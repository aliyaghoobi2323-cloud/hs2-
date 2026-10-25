package engine

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xtaci/smux"
)

// The window core's runner (WINDOW_CORE §2.2, §2.5, §7).
//
// Every edge's smux session that can grow a window past NeedStreamBuffer
// gets a core for the direction it receives (its downloads), registered with
// wcores where newSession registers the session with the wedge guard. Its
// core leaves OFF only once the exit sent an extended stats record (srtt and
// min_rtt != 0, wcore.sndSeen): with an old exit, or one on Linux before 4.6,
// it stays OFF for the link's life. The
// exit's sessions get one for their uploads only with WcoreUpload (a lab
// knob until step 3, WINDOW_CORE §11). One goroutine wakes every
// wcRampTick (100 ms) and steps each link whose turn has come (wcOut.next):
// one TCP_INFO of the link's own socket, one CoreSnapshot of its session
// (the runner is the session's only caller: a snapshot restarts the
// turnaround samples), the window pool's free bytes and, on the edge, the
// exit's latest stats record, the control ping's round trips and the
// autopilot's probing flag.
// wcBuilder turns them into wcIn, the pure controller (wcore.step) decides,
// and the runner applies it: each stream's ceiling by id, none for a stream
// the snapshot left out, and the session's turnaround reference. A link
// whose core is OFF and that read next to nothing is not even snapshotted
// (one atomic load), and an idle one is looked at only every wcTickMin.
// Nothing is sent on the wire for the core, and no lock of the data path is
// taken past the snapshot's copy of the session's stream list.
//
// The runner also keeps what the rest of the engine reads of a link's core
// (atomics: linkStallNote, the autopilot's grownRate, the status line),
// writes its log lines (§7.1, each with its repeat cap) and, in the lab, a
// JSON line per evaluation (HS2_TUNE_WCORE_TRACE).
//
// It also runs the exit's pacing net (pacenet.go): every exit link socket is
// registered for it (with no core unless WcoreUpload), whatever the edge
// runs, and read on each wake (one TCP_INFO); its cap is set with
// SO_MAX_PACING_RATE only when the net engages, moves or lifts it.

// Lab knobs (cmd/hs2 applyTuning: HS2_TUNE_WCORE*), read as a session
// registers; WcoreQTarget and WcoreProbeQ when the first one does.
var (
	// Wcore 0 turns the window core off: no session gets one, and every
	// window stays within NeedStreamBuffer as without it (the field safety
	// switch).
	Wcore = 1
	// WcoreUpload 1 gives the exit's sessions a core for their uploads
	// too (lab only, off by default, until step 3 sends the exit a record
	// of the edge's socket). The exit's core has no record of the sender:
	// it cannot see a policer's signature, and its growth lifts the edge's
	// BBR pacing past the policer for good (app-limited samples never lower
	// BBR's max-bw filter): behind a policer at 60% of the path 8 of 96 of
	// the red team's model runs drained the link (WINDOW_CORE §2.2, §8.1).
	WcoreUpload = 0
	// WcoreStage 2 lets ceilings go below NeedStreamBuffer once the core
	// found the path full (§4.4).
	WcoreStage = 1
	// WcoreQTarget, WcoreProbeQ (ms, > 0): the standing queue HOLD keeps and
	// the queue a probe adds, both ends.
	WcoreQTarget = 0
	WcoreProbeQ  = 0
	// WcoreTrace: a file the runner appends one JSON line to per evaluation
	// per link (lab only).
	WcoreTrace = ""
	// WcoreFixedWin (bytes, > 0): no controller; every large stream's
	// ceiling is this window, with no growth brake, the rest traced as usual
	// (lab only: calibrating the signals under fixed windows).
	WcoreFixedWin = 0
	// WcorePaceCap (> 0): lab experiment, off by default. Each end caps its
	// link socket's pacing (SO_MAX_PACING_RATE) at this many times the
	// largest delivery rate its own TCP_INFO reported in the last second,
	// or, while its own round trip shows no queue, the windows its peer
	// grants per min_rtt if that is more (wcPaceRate), refreshed on the
	// runner's wake, never under wcPaceFloor, and lifted
	// while the socket sends nothing. A window-limited BBR sender (in its
	// STARTUP for good) sends each window update's credit, and after its
	// PROBE_RTT the whole window, at 3.9 times the path's rate: on a shallow
	// queue that burst is the loss the receiver's window core cannot prevent.
	// With WcorePaceCap > 0 a link is registered even with Wcore 0, and the
	// exit's without WcoreUpload (the windows as without the core, the
	// sender's pace capped).
	WcorePaceCap = 0.0
	// WcoreBurstShare (> 0): wcBurstShare (B held where a burst after the
	// sender's PROBE_RTT fits Q_cap); lab knob.
	WcoreBurstShare = 0.0
)

const (
	// wcPaceFloor: the pacing cap is never under this (bytes/s).
	wcPaceFloor = 1 << 20
	// wcPaceIdle: a socket that had less than this acknowledged since the
	// previous wake sends nothing: its cap is lifted.
	wcPaceIdle = 64 << 10
	// wcPaceSpan: the cap follows the largest delivery rate of this span.
	wcPaceSpan = time.Second
	// wcHitFrames: a stream's hit gap is this many of its own frames' time
	// at its rate (smux.Stream.SetCoreHitGap), at most wcHitGapMax.
	wcHitFrames = 3
	wcHitGapMax = 50 * time.Millisecond
)

const (
	// wcIdleBytes: a link whose core is OFF and that read less than this
	// since its last snapshot has no fast stream to find.
	wcIdleBytes = 128 << 10
	// The log lines' repeat caps (§7.1): per link, and past wcAggLinks
	// links a minute one line for the rest.
	wcLogFilledEvery = time.Minute
	wcLogDrainEvery  = 30 * time.Second
	wcLogRareEvery   = 10 * time.Minute // random loss, a window at its ceiling, a route change
	wcLogPoolEvery   = 5 * time.Minute  // per process
	wcLogNetEvery    = 5 * time.Minute  // the pacing net held a link's pacing
	wcAggLinks       = 8
	wcAggEvery       = time.Minute
	// wcKneeMove: a HOLD budget this far from the last "fill the path"
	// line's is a knee that moved: logged as one.
	wcKneeMove = 0.25
	// wcPoolSeen: the links that met a full pool this recently count in
	// its log line.
	wcPoolSeen = 10 * time.Second
)

// wcLink is one session's window core: what the runner keeps for it and
// what it publishes.
type wcLink struct {
	sess  *smux.Session
	tcp   *net.TCPConn     // the link's socket (TCP_INFO); nil: none
	mtr   *linkMeter       // nil: none (bare sessions in tests)
	pool  *smux.WindowPool // nil: none
	side  wcSide
	fixed uint32 // WcoreFixedWin
	frame uint32 // smux MaxFrameSize (the hit gap)
	off   bool   // no core (Wcore 0, the exit's without WcoreUpload): registered for the pacing net or cap only
	pnet  bool   // the exit's link socket: the pacing net runs on it (pacenet.go)
	lab   bool   // WcorePaceCap as the session registered: the lab's always-on cap runs on it
	set   *wcSet // the registry that steps it
	name  atomic.Pointer[string]
	// peerKey: the peer's address (its host): the cores of the links to it
	// share a wcPeer (wcSet.peers)
	peerKey string
	// ap is the edge's LinkManager's apProbing (set when it adopts the link)
	ap atomic.Pointer[atomic.Bool]

	// the runner's own (one goroutine)
	core    *wcore
	bld     wcBuilder
	next    time.Time
	samples []smux.CoreSample
	snaps   []wcStreamSnap
	ceils   map[uint32]uint32       // this step's, by stream id
	sampled map[uint32]bool         // the streams of this snapshot
	ceiled  map[uint32]*smux.Stream // streams the runner gave a ceiling
	rdSeen  uint64
	rdOK    bool
	segs    wcWrap32
	ooo     wcWrap32
	pongN   uint64
	logAt   [wcLogKinds]time.Time
	heldB   uint64 // the budget of the latest "fill the path" line
	inStats wcStats
	// the hit gap: each stream's rate (bytes/s, smoothed over wakes) and the
	// gap set for it
	rateAt time.Time
	rates  map[uint32]float64
	gaps   map[uint32]time.Duration
	gapSet map[uint32]time.Duration
	// the pacing cap (WcorePaceCap)
	pcAcked uint64
	pcOK    bool
	pcDlv   []wcRate
	pcSet   uint64
	// the pacing net (pnet)
	net     wcNet
	netRtx  wcWrap32
	netSegs wcWrap32
}

// wcStats is what the engine reads of a link's core, at any time.
type wcStats struct {
	active    atomic.Bool   // the core left OFF at least once
	state     atomic.Uint32 // wcState
	grown     atomic.Uint64 // float64 bits: grownRate, bytes/s
	fastN     atomic.Int32  // streams with a ceiling past NeedStreamBuffer now
	fastWin   atomic.Int64  // their windows
	grow      atomic.Int32  // STARTUP ends and probes that paid
	drainQ    atomic.Int32
	drainLoss atomic.Int32
	drainRate atomic.Int32
	probeFail atomic.Int32
	undo      atomic.Int32  // drains for holes taken back on the exit's record (wcore.undoCut)
	oooRo     atomic.Uint64 // float64 bits: the path's reordering level now (wcore.roLevel)
	poolShort atomic.Int32  // windows the pool could not cover in full
	peakB     atomic.Uint64
	peakR     atomic.Uint64 // float64 bits, bytes/s
	lossy     atomic.Int64  // ns under random loss
	// the pacing net: times it held the socket's pacing, and the largest
	// resend share it saw (float64 bits)
	netHeld  atomic.Int32
	netShare atomic.Uint64
}

// wcWrap32 makes a cumulative 32-bit kernel counter (tcp_info's segs_in,
// rcv_ooopack) a 64-bit one.
type wcWrap32 struct {
	last uint32
	tot  uint64
	ok   bool
}

func (w *wcWrap32) add(v uint32) uint64 {
	if w.ok {
		w.tot += uint64(v - w.last)
	}
	w.last, w.ok = v, true
	return w.tot
}

// The log lines' kinds, each with its own cap.
const (
	wcLogFilled = iota
	wcLogDrain
	wcLogLossy
	wcLogCapped
	wcLogRoute
	wcLogNet
	wcLogUndo
	wcLogKinds
)

// wcSet is the process's registry of window cores; one goroutine steps
// them.
type wcSet struct {
	mu    sync.Mutex
	all   map[*wcLink]struct{}
	once  sync.Once
	tuned sync.Once
	logf  atomic.Pointer[func(string, ...any)]

	tracePath string // WcoreTrace as the first session registered

	// peers: what the cores of the links to each peer share (wcPeer), by
	// the peer's host (red team, round 3, finding 1: the links of the pool
	// to one exit share its path, and each core took the queue the others
	// held for the path's own). The map is under mu; each wcPeer is the
	// runner's own (it steps every link from one goroutine).
	peers map[string]*wcPeer

	// the runner's own
	list      []*wcLink
	trace     *os.File
	traceOpen bool
	agg       [wcAggKinds]wcAgg
	pool      map[*wcLink]wcPoolSeenAt
	poolLogAt time.Time
}

type wcPoolSeenAt struct {
	n  int
	at time.Time
}

// The kinds of per-link lines held to wcAggLinks links a minute (ws.agg):
// every line of a link's core, so a path all the links share that drains,
// turns lossy or gets longer writes a few lines and a summary, not one per
// link (red team, round 1: 300 links, 609 lines in 30 s).
const (
	wcAggFilled = iota
	wcAggDrain
	wcAggLossy
	wcAggRoute
	wcAggCapped
	wcAggNet
	wcAggUndo
	wcAggKinds
)

// wcAgg holds back one kind's per-link lines past wcAggLinks a minute and
// sums them up in one line once the minute is over.
type wcAgg struct {
	start time.Time
	n     int // lines logged this minute
	held  int // lines held back
	links map[*wcLink]bool
	conns int
	rate  float64
}

var wcores wcSet

// setWcoreLog sets where the window core's lines go (the tunnel's log).
func setWcoreLog(logf func(string, ...any)) {
	if logf != nil {
		wcores.logf.Store(&logf)
	}
}

// add gives sess a window core if the window core is on and its config can
// grow a window past NeedStreamBuffer (an adaptive window and a pool), and
// returns it (nil: none). server is the exit's end (it receives uploads):
// it gets a core only with WcoreUpload.
func (ws *wcSet) add(sess *smux.Session, cfg *smux.Config, tcp *net.TCPConn, mtr *linkMeter, server bool, remote net.Addr) *wcLink {
	l := newWcLink(sess, cfg, tcp, mtr, server, remote)
	if l == nil {
		return nil
	}
	ws.tuned.Do(func() {
		wcApplyKnobs()
		ws.tracePath = WcoreTrace
	})
	ws.once.Do(func() { go ws.run() })
	ws.put(l)
	return l
}

// put registers l with ws.
func (ws *wcSet) put(l *wcLink) {
	l.set = ws
	ws.mu.Lock()
	if ws.all == nil {
		ws.all = map[*wcLink]struct{}{}
	}
	ws.all[l] = struct{}{}
	if !l.off && l.core != nil {
		if ws.peers == nil {
			ws.peers = map[string]*wcPeer{}
		}
		p := ws.peers[l.peerKey]
		if p == nil {
			p = newWcPeer()
			ws.peers[l.peerKey] = p
		}
		p.refs++
		l.core.peer = p
	}
	ws.mu.Unlock()
}

// wcCoreOn says whether a session's end gets a core (more than the pacing
// cap's registration): the window core is on and, on the exit's end
// (server: it receives uploads), WcoreUpload too.
func wcCoreOn(server bool) bool {
	return Wcore != 0 && (!server || WcoreUpload != 0)
}

// wcNetOn says whether the exit's link sockets get the pacing net: PaceNet,
// and not the lab's always-on cap (WcorePaceCap) in its place.
func wcNetOn() bool { return PaceNet != 0 && WcorePaceCap <= 0 }

// newWcLink is the core of one session, not registered (nil: the session
// gets none). The exit's end (server) gets none without WcoreUpload: only
// the pacing net's registration (its link socket, whatever its config) and
// the pacing cap's if that is on.
func newWcLink(sess *smux.Session, cfg *smux.Config, tcp *net.TCPConn, mtr *linkMeter, server bool, remote net.Addr) *wcLink {
	off := !wcCoreOn(server)
	pnet := server && tcp != nil && wcNetOn()
	grow := cfg.NeedStreamBuffer > 0 && cfg.MinStreamBuffer > 0 && cfg.WindowPool != nil && cfg.NeedStreamBuffer < cfg.MaxStreamBuffer
	switch {
	case grow && (!off || WcorePaceCap > 0):
	case pnet:
		off = true
	default:
		return nil
	}
	side, name := wcEdge, "link to "
	if server {
		side, name = wcExit, "link from "
	}
	if remote != nil {
		name += remote.String()
	} else {
		name = "link"
	}
	l := &wcLink{sess: sess, tcp: tcp, mtr: mtr, pool: cfg.WindowPool, side: side, frame: uint32(cfg.MaxFrameSize), off: off, pnet: pnet, lab: WcorePaceCap > 0,
		ceils: map[uint32]uint32{}, sampled: map[uint32]bool{}, ceiled: map[uint32]*smux.Stream{},
		rates: map[uint32]float64{}, gaps: map[uint32]time.Duration{}, gapSet: map[uint32]time.Duration{}}
	if WcoreFixedWin > 0 {
		l.fixed = uint32(min(WcoreFixedWin, cfg.MaxStreamBuffer))
	}
	l.name.Store(&name)
	l.peerKey = wcPeerKey(remote)
	l.core = newWcore(wcConfig{side: side, stage: WcoreStage, need: uint32(cfg.NeedStreamBuffer),
		maxWin: uint32(cfg.MaxStreamBuffer), minWin: uint32(cfg.MinStreamBuffer), noCover: cfg.NoCover}, rand.Float64)
	return l
}

// wcPeerKey is the key of a link's peer in wcSet.peers: its host (links
// to one exit over other ports or carriers still share its path); "" with
// no address.
func wcPeerKey(remote net.Addr) string {
	if remote == nil {
		return ""
	}
	s := remote.String()
	if h, _, err := net.SplitHostPort(s); err == nil {
		return h
	}
	return s
}

// wcApplyKnobs sets the core's queue targets from the lab knobs.
func wcApplyKnobs() {
	if WcoreQTarget > 0 {
		wcQTarget = time.Duration(WcoreQTarget) * time.Millisecond
		wcQTargetExit = wcQTarget
	}
	if WcoreProbeQ > 0 {
		wcProbeQ = time.Duration(WcoreProbeQ) * time.Millisecond
		wcProbeQExit = wcProbeQ
	}
	if WcoreBurstShare > 0 {
		wcBurstShare = WcoreBurstShare
	}
}

func (ws *wcSet) run() {
	t := time.NewTicker(wcRampTick)
	defer t.Stop()
	for range t.C {
		ws.wakeAll()
	}
}

// wakeAll steps every live link whose turn has come, and drops the ended
// ones.
func (ws *wcSet) wakeAll() {
	ws.mu.Lock()
	ws.list = ws.list[:0]
	for l := range ws.all {
		if l.sess.IsClosed() {
			delete(ws.all, l)
			l.ceiled, l.samples = nil, nil // what it reads of the link stays (wcStats)
			if p := l.core.peer; p != nil {
				p.drop(l.core)
				if p.refs--; p.refs <= 0 {
					delete(ws.peers, l.peerKey)
				}
			}
			continue
		}
		ws.list = append(ws.list, l)
	}
	ws.mu.Unlock()
	for i, l := range ws.list {
		l.wake(time.Now())
		ws.list[i] = nil
	}
	ws.flush(time.Now())
}

// setName names the link in the log (the edge's LinkManager: "link 3").
func (l *wcLink) setName(s string) { l.name.Store(&s) }

func (l *wcLink) label() string { return *l.name.Load() }

// wake steps the link if its turn has come, and reports whether it did.
func (l *wcLink) wake(now time.Time) bool {
	if l.lab {
		l.paceCap(now)
	} else if l.pnet {
		l.paceNet(now)
	}
	if l.off || now.Before(l.next) {
		return false
	}
	if l.idle() {
		l.next = now.Add(wcTickMin)
		return false
	}
	c := l.core
	pub, drainLoss := c.bPub, c.cnt.drainLoss
	in := l.bld.build(l.snap(now))
	l.noteRates(now, in)
	out := c.step(in)
	if l.fixed > 0 {
		l.pin(&out)
	}
	l.apply(out)
	l.publish(now)
	if c.cnt.drainLoss > drainLoss && !wcHasDrain(out.events) && c.B < pub {
		// halved for loss between evaluations (wcore.cut)
		out.events = append(out.events, wcEvent{kind: wcEvDrain, why: wcWhyLoss, sig: wcSigOoo, from: pub, B: c.B, n: len(c.inF), T: c.tBase, q: -1})
	}
	if ws := l.set; ws != nil {
		if c.lastAt.Equal(now) || len(out.events) > 0 { // an evaluation, not only a step of the ramp
			ws.traceLine(l, now, out)
		}
		if l.fixed == 0 { // the lab's fixed windows are no decisions of the core
			ws.logEvents(l, now, out)
		}
	}
	l.next = out.next
	return true
}

func wcHasDrain(ev []wcEvent) bool {
	for _, e := range ev {
		if e.kind == wcEvDrain {
			return true
		}
	}
	return false
}

// idle: the core is OFF and the link read under wcIdleBytes since its last
// snapshot.
func (l *wcLink) idle() bool {
	if l.mtr == nil || l.core.state != wcOff || !l.rdOK {
		return false
	}
	return l.mtr.rdBytes.Load()-l.rdSeen < wcIdleBytes
}

// snap takes one snapshot of the link.
func (l *wcLink) snap(now time.Time) wcLinkSnap {
	s := wcLinkSnap{now: now, poolFree: -1, tcpNone: true}
	if ts, ok := tcpStats(l.tcp); ok {
		s.tcpNone = false
		s.minRTT, s.srtt = wcMinRTTOf(ts.minRTT), usDur(uint64(ts.rtt))
		// (rcv_ooopack is Linux 5.4's: before it the field reads 0, which is
		// no clean path)
		s.segsIn, s.ooo, s.oooOK = l.segs.add(ts.segsIn), l.ooo.add(ts.rcvOOO), ts.oooOK
	}
	var cc smux.CoreCounters
	l.samples, cc = l.sess.CoreSnapshot(l.samples)
	s.rcvd, s.parks, s.borrows, s.brakes, s.poolShort = cc.Rcvd, cc.Parks, cc.Borrows, cc.Brakes, cc.PoolShort
	if l.pool != nil {
		// what the pool has left for this link's windows (it holds no more
		// than the pool has left for the others)
		s.poolFree = int64(l.pool.FreeFor(l.sess.Extra()))
	}
	l.snaps = l.snaps[:0]
	for _, x := range l.samples {
		l.snaps = append(l.snaps, wcStreamSnap{id: x.ID, readTot: x.Read, updN: x.Upds, hitN: x.Hits, lagN: x.Lags,
			turnMin: x.TurnMin, turnLast: x.TurnLast, win: x.Win, braked: x.Braked})
	}
	s.streams = l.snaps
	m := l.mtr
	if m == nil {
		return s
	}
	l.rdSeen, l.rdOK = m.rdBytes.Load(), true
	if l.side != wcEdge {
		return s
	}
	s.ctrlMin = usDur(m.rttMin.Load())
	if p := m.peerLoss.Load(); p != nil && p.n > l.pongN {
		ring := uint64(len(m.pongRTT))
		k := l.pongN
		if p.n > ring {
			k = max(k, p.n-ring) // the older ones are gone
		}
		for ; k < p.n; k++ {
			if r := m.pongRTT[k%ring].Load(); r > 0 {
				s.pongs = append(s.pongs, usDur(r))
			}
		}
		l.pongN = p.n
	}
	if r := m.peer.Load(); r != nil {
		s.sndAt = r.at
		s.sndSrtt, s.sndMinRTT = usDur(uint64(r.srtt)), wcMinRTTOf(r.minRTT)
		s.sndRetrans, s.sndSegsOut = r.retrans, r.segsOut
	}
	if p := l.ap.Load(); p != nil {
		s.apProbing = p.Load()
	}
	return s
}

func usDur(us uint64) time.Duration { return time.Duration(us) * time.Microsecond }

// wcMinRTTOf is a socket's tcpi_min_rtt (µs) as a round trip: Linux reports
// ~0U (71 minutes) until the socket's first round-trip sample (minmax_reset
// in tcp_init_sock), which is no round trip (0: unknown).
func wcMinRTTOf(us uint32) time.Duration {
	if us == ^uint32(0) {
		return 0
	}
	return usDur(uint64(us))
}

// pin puts the lab's fixed window (WcoreFixedWin) in place of the
// controller's ceilings: on every large stream of the snapshot (a window of
// need/4 or more), with no growth brake. (Only the fast streams before:
// one that left F under random loss or a slow reader went back to 2 MiB.)
func (l *wcLink) pin(o *wcOut) {
	big := map[uint32]bool{}
	for _, x := range l.samples {
		if x.Win >= l.core.cfg.need/4 {
			big[x.ID] = true
		}
	}
	for i := range o.ceils {
		o.ceils[i].ceil = 0
		if big[o.ceils[i].id] {
			o.ceils[i].ceil = l.fixed
		}
	}
	o.turnRef, o.qBrake, o.growOK = 0, 0, true
}

// noteRates keeps each stream's rate (smoothed over the wakes) and the hit
// gap it makes: wcHitFrames of its own frames' time at that rate, at least
// smux's 2 ms. Under ~85 Mbit/s a stream's own frames come more than 2 ms
// apart in clumps, and the rig counted 87 to 100% of such streams' cycles as
// hits with the window nowhere near the limit.
func (l *wcLink) noteRates(now time.Time, in wcIn) {
	dt := now.Sub(l.rateAt).Seconds()
	l.rateAt = now
	if dt <= 0 || dt > 1 || l.frame == 0 {
		return
	}
	seen := map[uint32]bool{}
	for _, s := range in.streams {
		seen[s.id] = true
		r := float64(s.read) / dt
		e, ok := l.rates[s.id]
		if ok {
			e += 0.3 * (r - e)
		} else {
			e = r
		}
		l.rates[s.id] = e
		g := wcHitGapMax
		if e > 0 {
			g = min(wcHitGapMax, time.Duration(wcHitFrames*float64(l.frame)/e*float64(time.Second)))
		}
		l.gaps[s.id] = g
	}
	for id := range l.rates {
		if !seen[id] {
			delete(l.rates, id)
			delete(l.gaps, id)
			delete(l.gapSet, id)
		}
	}
}

// paceCap is the lab's pacing-cap experiment (WcorePaceCap): the link
// socket's pacing at most WcorePaceCap times the largest delivery rate of
// the last wcPaceSpan, or the windows the peer grants the streams that sent
// (smux PeerWindows) per min_rtt if that is more, never under wcPaceFloor,
// and no cap while the socket sends nothing (the next transfer starts as
// without the cap).
func (l *wcLink) paceCap(now time.Time) {
	ts, ok := tcpStats(l.tcp)
	if !ok {
		return
	}
	acked := ts.bytesAcked - l.pcAcked
	first := !l.pcOK
	l.pcAcked, l.pcOK = ts.bytesAcked, true
	if first {
		return
	}
	if acked < wcPaceIdle {
		if l.pcSet != 0 {
			setMaxPacing(l.tcp, 0)
			l.pcSet = 0
		}
		l.pcDlv = l.pcDlv[:0]
		return
	}
	l.pcDlv = append(l.pcDlv, wcRate{now, float64(ts.deliveryRate)})
	cut := 0
	for cut < len(l.pcDlv)-1 && now.Sub(l.pcDlv[cut].at) > wcPaceSpan {
		cut++
	}
	l.pcDlv = l.pcDlv[cut:]
	r := 0.0
	for _, x := range l.pcDlv {
		r = math.Max(r, x.r)
	}
	win := uint64(0)
	if l.sess != nil {
		win = l.sess.PeerWindows()
	}
	v := wcPaceRate(r, win, time.Duration(ts.minRTT)*time.Microsecond, time.Duration(ts.rtt)*time.Microsecond)
	if l.pcSet == 0 || math.Abs(float64(v)-float64(l.pcSet)) > 0.05*float64(l.pcSet) {
		if setMaxPacing(l.tcp, v) {
			l.pcSet = v
		}
	}
}

// wcPaceSet sets a socket's pacing cap (setMaxPacing); a test stands in
// for it (atomic: the runner reads it as it steps).
var wcPaceSet atomic.Pointer[func(*net.TCPConn, uint64) bool]

func init() {
	f := setMaxPacing
	wcPaceSet.Store(&f)
}

// paceNet is the pacing net's wake (pacenet.go): one TCP_INFO of the link's
// socket, and its cap set, moved or lifted as the net decides.
func (l *wcLink) paceNet(now time.Time) {
	ts, ok := tcpStats(l.tcp)
	if !ok {
		return
	}
	pace := float64(ts.pacingRate)
	if ts.pacingRate == math.MaxUint64 {
		pace = 0 // no pacing: unknown
	}
	o := l.net.step(wcNetIn{now: now, retrans: l.netRtx.add(uint32(ts.retrans)), segsOut: l.netSegs.add(ts.segsOut),
		acked: ts.bytesAcked, dlv: float64(ts.deliveryRate), pace: pace, srtt: usDur(uint64(ts.rtt)), minRTT: wcMinRTTOf(ts.minRTT)})
	set := *wcPaceSet.Load()
	switch o.act {
	case wcNetEngage, wcNetMoveCap:
		if !set(l.tcp, o.cap) {
			l.net.cap = 0 // not capped after all: the next reading judges again
			return
		}
	case wcNetRelease:
		set(l.tcp, 0)
	}
	if o.act != wcNetEngage {
		return
	}
	st := &l.inStats
	st.netHeld.Add(1)
	if v := math.Float64frombits(st.netShare.Load()); o.share > v {
		st.netShare.Store(math.Float64bits(o.share))
	}
	if ws := l.set; ws != nil {
		ws.netLine(l, now, o)
	}
}

// wcPaceRate is the pacing cap for a socket whose largest delivery rate of
// the last wcPaceSpan is r (bytes/s) and whose streams that sent hold win of
// the peer's windows: WcorePaceCap·r, never under wcPaceFloor, and, where
// the socket's own round trip shows no queue (srtt within max(5 ms,
// min_rtt/10) of min_rtt: the path has room), never under what those
// windows allow per min_rtt. A sender held by its windows, capped at 1.25
// times its own delivery rate, could not leave a slower turnaround regime
// (z12 P2: the windows without the core on 300/150 stayed at 55 Mbit/s, 2
// MiB in two round trips, where without the cap they ran at 77–90); where
// the windows already fill the path (100/50: 2 MiB is 3.4 times what
// fits), the window's rate would undo the cap, which took the resends of
// the windows without the core there from 20% to 0.03%.
func wcPaceRate(r float64, win uint64, minRTT, srtt time.Duration) uint64 {
	v := WcorePaceCap * r
	if minRTT > 0 && srtt > 0 && srtt-minRTT <= max(wcDqMin, minRTT/10) {
		v = math.Max(v, float64(win)/minRTT.Seconds())
	}
	return uint64(math.Max(v, wcPaceFloor))
}

// apply hands the decision to smux: the session's reference first (a
// ceiling past need lets a window grow only as the reference allows), then
// each sampled stream's ceiling by id, and none to a stream that left the
// snapshot.
func (l *wcLink) apply(o wcOut) {
	l.sess.SetCoreRef(o.turnRef, o.qBrake, o.growOK)
	l.sess.SetCover(!o.noCover)
	clear(l.ceils)
	for _, c := range o.ceils {
		l.ceils[c.id] = c.ceil
	}
	clear(l.sampled)
	for i := range l.samples {
		x := &l.samples[i]
		l.sampled[x.ID] = true
		c := l.ceils[x.ID]
		if c != x.Ceil {
			x.Stream.SetCoreCeil(c)
		}
		if g, ok := l.gaps[x.ID]; ok {
			if s := l.gapSet[x.ID]; s == 0 || math.Abs(float64(g-s)) > 0.1*float64(s) {
				x.Stream.SetCoreHitGap(g)
				l.gapSet[x.ID] = g
			}
		}
		if c > 0 {
			l.ceiled[x.ID] = x.Stream
		} else {
			delete(l.ceiled, x.ID)
		}
	}
	for id, st := range l.ceiled {
		if !l.sampled[id] {
			st.SetCoreCeil(0)
			delete(l.ceiled, id)
		}
	}
}

// publish keeps what the engine reads of the core.
func (l *wcLink) publish(now time.Time) {
	c, st := l.core, &l.inStats
	if c.state != wcOff {
		st.active.Store(true)
	}
	st.state.Store(uint32(c.state))
	st.grow.Store(int32(c.cnt.grow + c.cnt.probeOK))
	st.drainQ.Store(int32(c.cnt.drainQ))
	st.drainLoss.Store(int32(c.cnt.drainLoss))
	st.drainRate.Store(int32(c.cnt.drainRate))
	st.probeFail.Store(int32(c.cnt.probeFail))
	st.undo.Store(int32(c.cnt.undo))
	st.oooRo.Store(math.Float64bits(c.roLevel(now)))
	st.poolShort.Store(int32(c.cnt.poolShort))
	st.peakB.Store(c.cnt.peakB)
	st.peakR.Store(math.Float64bits(c.cnt.peakR))
	st.lossy.Store(int64(c.cnt.lossy))
	st.grown.Store(math.Float64bits(l.grownRate(now)))
	n, w := int32(0), int64(0)
	for i := range l.samples {
		x := &l.samples[i]
		if l.ceils[x.ID] > c.cfg.need {
			n++
			w += int64(x.Win)
		}
		x.Stream = nil // no hold on a stream past this wake
	}
	st.fastN.Store(n)
	st.fastWin.Store(w)
}

// grownRate is what the link's fast streams carry past what windows of
// NeedStreamBuffer would carry them at their turnaround: max(0, R_F −
// n_F·Need/turn) (§6.3). The autopilot leaves it out of what the pool's
// size is measured by.
func (l *wcLink) grownRate(now time.Time) float64 {
	c := l.core
	m := c.last
	if c.state == wcOff || len(m.f) == 0 || m.rF <= 0 {
		return 0
	}
	t := c.turnMins.min(now, wcTurnWin)
	if t <= 0 {
		t = c.tBase
	}
	if t <= 0 {
		return 0
	}
	return math.Max(0, m.rF-float64(len(m.f))*float64(c.cfg.need)/t.Seconds())
}

// wcGrownOf is a link's grownRate (0: no core).
func wcGrownOf(m *linkMeter) float64 {
	if m == nil || m.wc == nil {
		return 0
	}
	return math.Float64frombits(m.wc.inStats.grown.Load())
}

// wcoreNote says, for the log line of a link's end, what its window core
// did ("" if it never left OFF): the largest budget and rate, and how often
// it drained and the pool held it.
func wcoreNote(m *linkMeter) string {
	if m == nil || m.wc == nil || !m.wc.inStats.active.Load() {
		return ""
	}
	st := &m.wc.inStats
	s := fmt.Sprintf("window core: up to %s at %.0f Mbit/s, drained %d time(s) for queue, %d for loss",
		wcMiB(st.peakB.Load()), mbitps(math.Float64frombits(st.peakR.Load())), st.drainQ.Load(), st.drainLoss.Load())
	if r := st.drainRate.Load(); r > 0 {
		s += fmt.Sprintf(", %d for a slower path", r)
	}
	if u := st.undo.Load(); u > 0 {
		// (each a drain for holes the exit's record showed were reordering:
		// counted in "for loss" above too)
		s += fmt.Sprintf(", %d for holes taken back (reordering level %.0f%%)", u, 100*math.Float64frombits(st.oooRo.Load()))
	}
	return s + fmt.Sprintf(", held by the window pool %d time(s)", st.poolShort.Load())
}

// fillStats adds the process's window cores to a pool snapshot (the status
// line, §7.3).
func (ws *wcSet) fillStats(st *PoolStats) {
	ws.mu.Lock()
	n, w := 0, int64(0)
	for l := range ws.all {
		n += int(l.inStats.fastN.Load())
		w += l.inStats.fastWin.Load()
	}
	ws.mu.Unlock()
	st.FastConns, st.FastWinMiB = n, float64(w)/(1<<20)
	st.PoolMiB = min(max(SmuxWindowPool, 0), 1<<30) >> 20
	if windowPoolUp.Load() {
		p := windowPool()
		st.Covers, st.CoverMiB = p.CoverStreams(), float64(p.Covers())/(1<<20)
	}
}

// ---- log lines (§7.1) ----

// dir is what the link's core receives: "download" on the edge.
func (l *wcLink) dir() string {
	if l.side == wcExit {
		return "upload"
	}
	return "download"
}

// logEvents writes the lines of a step's events, each within its cap.
func (ws *wcSet) logEvents(l *wcLink, now time.Time, o wcOut) {
	lp := ws.logf.Load()
	if lp == nil {
		return
	}
	ws.flushAgg(now) // a minute's summary goes before the next minute's lines
	logf := *lp
	c := l.core
	filled := false
	for _, e := range o.events {
		switch e.kind {
		case wcEvFilled:
			// (the knee may be under the least budget, the windows of F at
			// c_min: that is what is held)
			filled = true
			l.heldB = max(e.B, uint64(max(e.n, 1))*uint64(c.cmin()))
			ws.filled(l, now, logf, e.R, e.T, l.heldB, e.n, e.q)
		case wcEvDrain:
			if !l.due(wcLogDrain, now, wcLogDrainEvery) || !ws.agg[wcAggDrain].admit(l, now, e.n, e.R) {
				continue
			}
			logf("mtcp: %s: %s — %s windows %s → %s", l.label(), wcDrainCause(e), l.dir(),
				wcMiB(e.from), wcMiB(e.B))
		case wcEvLossy:
			if !l.due(wcLogLossy, now, wcLogRareEvery) || !ws.agg[wcAggLossy].admit(l, now, 0, 0) {
				continue
			}
			loss := e.loss
			if loss <= 0 {
				loss = c.last.ooo / 10 // the exit side's reading: ~a hole per ten packets behind it
			}
			logf("mtcp: %s: %.1f%% loss with no queue — windows cover the resends (+%d ms)", l.label(), 100*loss, e.q.Milliseconds())
		case wcEvCapped:
			if e.pool {
				ws.poolHeld(l, now, e.n) // the pool had less than a step of a window left
				continue
			}
			if e.n > 0 && e.B/uint64(e.n) >= uint64(c.cfg.maxWin) {
				if !l.due(wcLogCapped, now, wcLogRareEvery) || !ws.agg[wcAggCapped].admit(l, now, e.n, 0) {
					continue
				}
				if e.n == 1 {
					logf("mtcp: %s: one connection's window is at its %d MiB ceiling (round trip %d ms)", l.label(), c.cfg.maxWin>>20, e.T.Milliseconds())
				} else {
					logf("mtcp: %s: %d connections' windows are at their %d MiB ceiling (round trip %d ms)", l.label(), e.n, c.cfg.maxWin>>20, e.T.Milliseconds())
				}
				continue
			}
			// capped by the pool with a step or more of it left: no line
		case wcEvPool:
			ws.poolHeld(l, now, e.n)
		case wcEvRoute:
			if l.due(wcLogRoute, now, wcLogRareEvery) && ws.agg[wcAggRoute].admit(l, now, 0, 0) {
				logf("mtcp: %s: the path's round trip grew to ~%d ms (a route change) — the window core follows it", l.label(), e.T.Milliseconds())
			}
		case wcEvUndo:
			// (the drain line for holes before it says why it came; this one
			// that the exit's record took it back: wcore.undoCut)
			if l.due(wcLogUndo, now, wcLogDrainEvery) && ws.agg[wcAggUndo].admit(l, now, e.n, 0) {
				logf("mtcp: %s: the other server resent %.3f%% — the holes were reordering, not loss; %s windows back %s → %s",
					l.label(), 100*e.loss, l.dir(), wcMiB(e.from), wcMiB(e.B))
			}
		}
	}
	// a knee that moved by more than wcKneeMove in HOLD
	if !filled && c.state == wcHold && c.lastAt.Equal(now) && l.heldB > 0 && !c.lossDrain &&
		math.Abs(float64(c.B)-float64(l.heldB)) > wcKneeMove*float64(l.heldB) {
		l.heldB = c.B
		ws.filled(l, now, logf, c.rhatF(now, c.last), c.tBase, c.B, len(c.inF), c.qTarget(now))
	}
}

// filled is the "fill the path" line.
func (ws *wcSet) filled(l *wcLink, now time.Time, logf func(string, ...any), r float64, t time.Duration, b uint64, n int, q time.Duration) {
	if !l.due(wcLogFilled, now, wcLogFilledEvery) || !ws.agg[wcAggFilled].admit(l, now, n, r) {
		return
	}
	logf("mtcp: %s: %ss fill the path at ~%.0f Mbit/s (round trip %d ms): window held at %s for %d fast connection(s), queue ~%d ms",
		l.label(), l.dir(), mbitps(r), t.Milliseconds(), wcMiB(b), n, q.Milliseconds())
}

// wcNetNote says, for the exit's link-end lines, what the pacing net did
// on the link ("" if it never engaged).
func wcNetNote(m *linkMeter) string {
	if m == nil || m.wc == nil {
		return ""
	}
	st := &m.wc.inStats
	n := st.netHeld.Load()
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("pacing held %d time(s) for resends (up to %.1f%%)", n, 100*math.Float64frombits(st.netShare.Load()))
}

// wcoreEndNote is what the exit's link-end lines end with: " (while up:
// window core: …; pacing held …; streams covered …)", or "" (the exit's
// covers: SmuxCover 2, lab only, and only on a link with a core).
func wcoreEndNote(m *linkMeter) string {
	var parts []string
	cover := ""
	if m != nil && m.wc != nil {
		cover = smuxCoverNote(m.wc.sess)
	}
	for _, n := range []string{wcoreNote(m), wcNetNote(m), cover} {
		if n != "" {
			parts = append(parts, n)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return wcEndNoteSep + strings.Join(parts, "; ") + ")"
}

const wcEndNoteSep = " (while up: "

// cutEndNote splits a link's end reason from the note wcoreEndNote put after
// it.
func cutEndNote(s string) (why, note string) {
	if i := strings.Index(s, wcEndNoteSep); i >= 0 {
		return s[:i], s[i:]
	}
	return s, ""
}

func wcMiB(b uint64) string { return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20)) }

// wcDrainCause says why a drain came, for its line: the signal that set it
// off and its value past its reference.
func wcDrainCause(e wcEvent) string {
	ms := max(e.q, 0).Milliseconds()
	switch {
	case e.why == wcWhyRate:
		return fmt.Sprintf("path slowed to ~%.0f Mbit/s with its queue growing", mbitps(e.R))
	case e.why == wcWhyLoss && e.q < 0:
		return "packets arrived behind holes on a clean path"
	case e.why == wcWhyLoss && e.sig == wcSigOoo:
		return "packets arrived behind holes as the windows grew"
	case e.why == wcWhyLoss && e.loss > 0 && e.q > 0 && e.sig != wcSigLoss:
		return fmt.Sprintf("%s (resent %.1f%%)", wcQueueCause(e.sig, ms), 100*e.loss)
	case e.why == wcWhyLoss && e.loss > 0:
		return fmt.Sprintf("packets were lost as the windows grew (resent %.1f%%)", 100*e.loss)
	case e.why == wcWhyLoss:
		return "packets were lost as the windows grew"
	}
	return wcQueueCause(e.sig, ms)
}

// wcQueueCause names the queue signal of a drain and how far past its
// reference it was.
func wcQueueCause(sig wcSig, ms int64) string {
	switch sig {
	case wcSigSnd:
		return fmt.Sprintf("the exit's queue grew by %d ms", ms)
	case wcSigOwn:
		return fmt.Sprintf("path queue grew by %d ms (the link's own round trip)", ms)
	case wcSigCtrl:
		return fmt.Sprintf("control pings came back %d ms late", ms)
	}
	return fmt.Sprintf("path queue grew by %d ms", ms)
}

// netLine is the pacing net's line as it engages on a link, within its cap.
func (ws *wcSet) netLine(l *wcLink, now time.Time, o wcNetOut) {
	lp := ws.logf.Load()
	if lp == nil {
		return
	}
	ws.flushAgg(now)
	if !l.due(wcLogNet, now, wcLogNetEvery) || !ws.agg[wcAggNet].admit(l, now, 0, 0) {
		return
	}
	(*lp)("mtcp: %s: resending %.1f%% at a pacing rate %.1f× what the path delivers — pacing held at %.4g× (~%.0f Mbit/s) until the resends stop",
		l.label(), 100*o.share, o.ratio, o.k, mbitps(float64(o.cap)))
}

// due says whether a line of kind k may go out for the link now, and takes
// its turn if so.
func (l *wcLink) due(k int, now time.Time, every time.Duration) bool {
	if !l.logAt[k].IsZero() && now.Sub(l.logAt[k]) < every {
		return false
	}
	l.logAt[k] = now
	return true
}

// admit counts a per-link line toward this minute's wcAggLinks; past them
// the line is held back for the minute's summary.
func (a *wcAgg) admit(l *wcLink, now time.Time, conns int, rate float64) bool {
	if a.start.IsZero() || now.Sub(a.start) >= wcAggEvery {
		*a = wcAgg{start: now} // (flush wrote the summary of the minute before)
	}
	if a.n < wcAggLinks {
		a.n++
		return true
	}
	if a.links == nil {
		a.links = map[*wcLink]bool{}
	}
	a.held++
	a.links[l] = true
	a.conns += conns
	a.rate += rate
	return false
}

// flush writes the summaries of the minutes that are over, and the pool's
// line (after a wake of every link: one line for the links it held).
func (ws *wcSet) flush(now time.Time) {
	ws.flushAgg(now)
	lp := ws.logf.Load()
	if len(ws.pool) == 0 || now.Sub(ws.poolLogAt) < wcLogPoolEvery {
		return
	}
	conns, links := 0, 0
	for l, p := range ws.pool {
		if now.Sub(p.at) > wcPoolSeen {
			delete(ws.pool, l)
			continue
		}
		conns += p.n
		links++
	}
	if links == 0 {
		return
	}
	ws.poolLogAt = now
	clear(ws.pool)
	if lp != nil {
		(*lp)("mtcp: window pool full (%d MiB): %d fast connection(s) on %d link(s) held below what their path carries",
			min(max(SmuxWindowPool, 0), 1<<30)>>20, conns, links)
	}
}

// flushAgg writes the summaries of the minutes that are over.
func (ws *wcSet) flushAgg(now time.Time) {
	lp := ws.logf.Load()
	for i := range ws.agg {
		a := &ws.agg[i]
		if a.start.IsZero() || now.Sub(a.start) < wcAggEvery {
			continue
		}
		if lp != nil && a.held > 0 {
			switch i {
			case wcAggFilled:
				(*lp)("mtcp: window core: %d more link(s) found the window that fills their path in the last minute (%d fast connection(s), ~%.0f Mbit/s)",
					len(a.links), a.conns, mbitps(a.rate))
			case wcAggDrain:
				(*lp)("mtcp: window core: %d more link(s) drained their windows %d time(s) in the last minute (a queue grew, or loss came, on their path)",
					len(a.links), a.held)
			case wcAggLossy:
				(*lp)("mtcp: window core: %d more link(s) saw loss with no queue in the last minute (windows cover the resends)", len(a.links))
			case wcAggRoute:
				(*lp)("mtcp: window core: %d more link(s) saw their round trip grow in the last minute (a route change; the window core follows it)", len(a.links))
			case wcAggCapped:
				(*lp)("mtcp: window core: %d more link(s) had a window at its ceiling in the last minute (%d connection(s))",
					len(a.links), a.conns)
			case wcAggNet:
				(*lp)("mtcp: pacing net: %d more link(s) resent past %.4g%% and had their pacing held in the last minute",
					len(a.links), 100*wcNetLossOf())
			case wcAggUndo:
				(*lp)("mtcp: window core: %d more link(s) took back %d drain(s) for holes in the last minute (the other server resent nothing: reordering)",
					len(a.links), a.held)
			}
		}
		*a = wcAgg{}
	}
}

// poolHeld notes a link whose core the window pool held back.
func (ws *wcSet) poolHeld(l *wcLink, now time.Time, n int) {
	if ws.pool == nil {
		ws.pool = map[*wcLink]wcPoolSeenAt{}
	}
	ws.pool[l] = wcPoolSeenAt{n: max(n, 1), at: now}
}

// ---- lab trace (HS2_TUNE_WCORE_TRACE) ----

// wcTraceLine is one evaluation of one link: all its inputs, what the core
// made of them, and what it decided.
type wcTraceLine struct {
	T         string        `json:"t"`
	Link      string        `json:"link"`
	Side      string        `json:"side"` // edge (downloads) | exit (uploads)
	State     string        `json:"state"`
	B         uint64        `json:"b"`     // the budget decided
	BPub      uint64        `json:"b_pub"` // ... and published (the ramp's)
	Fixed     uint32        `json:"fixed,omitempty"`
	Dt        float64       `json:"dt_s"`
	NF        int           `json:"n_f"`
	RF        float64       `json:"r_f"` // bytes/s
	RLink     float64       `json:"r_link"`
	RHat      float64       `json:"r_hat"` // R̂ for F
	Grown     float64       `json:"grown"`
	WF        float64       `json:"w_f"`
	HStep     float64       `json:"h_step"`
	Hit       float64       `json:"hit"`
	TBase     float64       `json:"t_base_ms"`
	TurnBase  float64       `json:"turn_base_ms"`
	Tau       float64       `json:"tau_hold_ms"`
	QTurn     float64       `json:"q_turn_ms"`
	QTurnOK   bool          `json:"q_turn_ok"`
	QRef      float64       `json:"q_ref_ms"`
	QSnd      float64       `json:"q_snd_ms"`
	QSndOK    bool          `json:"q_snd_ok"`
	QOwn      float64       `json:"q_own_ms"`
	QCtrl     float64       `json:"q_ctrl_ms"` // the longest pong over rttMin in the interval (0: none)
	CtrlMin   float64       `json:"ctrl_min_ms"`
	Pongs     []float64     `json:"pongs_ms,omitempty"`
	MinRTT    float64       `json:"min_rtt_ms"` // the link socket's own
	SRTT      float64       `json:"srtt_ms"`
	SegsIn    uint64        `json:"segs_in"`
	Ooo       float64       `json:"ooo"`
	OooOK     bool          `json:"ooo_ok"`
	Snd       float64       `json:"snd"` // the exit's resend share (a fresh record)
	SndOK     bool          `json:"snd_ok"`
	SndSRTT   float64       `json:"snd_srtt_ms,omitempty"`
	SndMinRTT float64       `json:"snd_min_rtt_ms,omitempty"`
	Spread    float64       `json:"spread_ms"`
	QTarget   float64       `json:"q_target_ms"`
	ProbeQ    float64       `json:"probe_q_ms"`
	Qcap      float64       `json:"q_cap_ms,omitempty"`
	Parks     uint32        `json:"parks"`
	Brakes    uint32        `json:"brakes"`
	PoolFree  int64         `json:"pool_free"`
	PoolShort uint32        `json:"pool_short"`
	APProbing bool          `json:"ap_probing"`
	TurnRef   float64       `json:"turn_ref_ms"`
	QBrake    float64       `json:"q_brake_ms"`
	GrowOK    bool          `json:"grow_ok"`
	NoCover   bool          `json:"no_cover,omitempty"` // the session's streams hold no cover (smux Session.SetCover)
	Cover     *wcTraceCover `json:"cover,omitempty"`    // what the session's covers did (none granted or held back: absent)
	// the path's reordering level now (wcore.roLevel) and the drains for
	// holes taken back so far (wcore.undoCut)
	OooRo float64 `json:"ooo_ro,omitempty"`
	Undo  int     `json:"undo,omitempty"`
	// the links to the same peer (wcPeer): another core is on (grp), the
	// path's queue by every link's records (median) and what of it stands
	// (lower quartile), and the fair share and rAvg the shared rule uses
	Grp     bool            `json:"grp,omitempty"`
	QPath   float64         `json:"q_path_ms,omitempty"`
	QStand  float64         `json:"q_stand_ms,omitempty"`
	Fair    float64         `json:"fair,omitempty"` // bytes/s (0: no shared rule this interval)
	RAvg    float64         `json:"r_avg,omitempty"`
	Streams []wcTraceStream `json:"streams"`
	Events  []string        `json:"events,omitempty"`
}

type wcTraceStream struct {
	ID       uint32  `json:"id"`
	F        bool    `json:"f"` // a fast stream
	Ceil     uint32  `json:"ceil"`
	Win      uint32  `json:"win"`
	Read     uint64  `json:"read"`
	Upd      uint32  `json:"upd"`
	Hit      uint32  `json:"hit"`
	Lag      uint32  `json:"lag"`
	TurnMin  float64 `json:"turn_min_ms"`
	TurnLast float64 `json:"turn_last_ms"`
	Braked   bool    `json:"braked,omitempty"`
}

// wcTraceCover is smux.CoverCounters for the trace: the streams holding a
// cover now, and since the session began the covers granted, lost (and
// why) and held back by the covers' share of the window pool.
type wcTraceCover struct {
	Now    int    `json:"now"`
	Grants uint32 `json:"grants"`
	Reader uint32 `json:"lost_reader,omitempty"`
	Miss   uint32 `json:"lost_miss,omitempty"`
	Taken  uint32 `json:"lost_core,omitempty"`
	Back   uint32 `json:"back,omitempty"`
	Idle   uint32 `json:"lost_idle,omitempty"`
	Closed uint32 `json:"lost_closed,omitempty"`
	Capped uint32 `json:"capped,omitempty"`
}

// wcTraceCoverOf is the session's covers for the trace (nil: none granted
// or held back).
func wcTraceCoverOf(sess *smux.Session) *wcTraceCover {
	if sess == nil {
		return nil
	}
	c := sess.CoverStats()
	if c.Grants == 0 && c.Capped == 0 {
		return nil
	}
	return &wcTraceCover{Now: c.Now, Grants: c.Grants, Reader: c.Reader, Miss: c.Miss, Taken: c.Taken, Back: c.Back,
		Idle: c.Idle, Closed: c.Closed, Capped: c.Capped}
}

func msOf(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// traceLine writes the link's evaluation at now to the lab trace.
func (ws *wcSet) traceLine(l *wcLink, now time.Time, o wcOut) {
	if ws.tracePath == "" {
		return
	}
	if !ws.traceOpen {
		ws.traceOpen = true
		f, err := os.OpenFile(ws.tracePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			if lp := ws.logf.Load(); lp != nil {
				(*lp)("mtcp: window core trace: %v", err)
			}
			return
		}
		ws.trace = f
	}
	if ws.trace == nil {
		return
	}
	b, _ := json.Marshal(l.traceOf(now, o))
	ws.trace.Write(append(b, '\n'))
}

// traceOf is the link's evaluation at now (the core's input is still in
// c.acc, its measurements in c.last).
func (l *wcLink) traceOf(now time.Time, o wcOut) wcTraceLine {
	c, in, m := l.core, l.core.acc, l.core.last
	side := "edge"
	if l.side == wcExit {
		side = "exit"
	}
	t := wcTraceLine{T: now.UTC().Format(time.RFC3339Nano), Link: l.label(), Side: side, State: c.state.String(),
		B: c.B, BPub: c.bPub, Fixed: l.fixed, Dt: m.dt, NF: len(m.f), RF: m.rF, RLink: m.rLink,
		Grown: math.Float64frombits(l.inStats.grown.Load()), WF: m.wF, HStep: m.hStep, Hit: m.hit,
		TBase: msOf(c.tBase), TurnBase: msOf(c.turnMins.min(now, wcTurnWin)), Tau: msOf(c.tau),
		QTurn: msOf(m.qTurn), QTurnOK: m.qOK, QRef: msOf(c.qRef), QSnd: msOf(m.qSnd), QSndOK: m.qSndOK,
		QOwn: msOf(m.qOwn), CtrlMin: msOf(in.ctrlMin), MinRTT: msOf(in.minRTT), SRTT: msOf(in.srtt),
		SegsIn: in.segsIn, Ooo: m.ooo, OooOK: m.oooOK, Snd: m.snd, SndOK: m.sndOK, Spread: msOf(c.spread),
		QTarget: msOf(c.qTarget(now)), ProbeQ: msOf(c.probeQ(now)), Parks: in.parks, Brakes: in.brakes,
		PoolFree: in.poolFree, PoolShort: in.poolShort, APProbing: in.apProbing,
		TurnRef: msOf(o.turnRef), QBrake: msOf(o.qBrake), GrowOK: o.growOK, NoCover: o.noCover,
		OooRo: c.roLevel(now), Undo: c.cnt.undo, Cover: wcTraceCoverOf(l.sess)}
	if m.dt > 0 && c.tBase > 0 {
		t.RHat = c.rhatF(now, m)
	}
	if m.grp {
		t.Grp, t.QPath, t.QStand = true, msOf(m.qPath), msOf(m.qStand)
		if f, ok := c.fairShare(now, m); ok {
			t.Fair = f
		}
	}
	t.RAvg = c.rAvg
	if c.qcapKnown(now) {
		t.Qcap = msOf(c.qcap)
	}
	if in.snd.fresh {
		t.SndSRTT, t.SndMinRTT = msOf(in.snd.srtt), msOf(in.snd.minRTT)
	}
	for _, p := range in.pongs {
		t.Pongs = append(t.Pongs, msOf(p))
		if in.ctrlMin > 0 {
			t.QCtrl = math.Max(t.QCtrl, msOf(p-in.ctrlMin))
		}
	}
	ceil := map[uint32]uint32{}
	for _, x := range o.ceils {
		ceil[x.id] = x.ceil
	}
	for _, s := range in.streams {
		t.Streams = append(t.Streams, wcTraceStream{ID: s.id, F: c.inF[s.id], Ceil: ceil[s.id], Win: s.win, Read: s.read,
			Upd: s.upd, Hit: s.hit, Lag: s.lag, TurnMin: msOf(s.turnMin), TurnLast: msOf(s.turnLast), Braked: s.braked})
	}
	for _, e := range o.events {
		t.Events = append(t.Events, e.String())
	}
	return t
}

// String is an event for the trace.
func (e wcEvent) String() string {
	var b strings.Builder
	switch e.kind {
	case wcEvFilled:
		b.WriteString("filled")
	case wcEvDrain:
		b.WriteString([...]string{"drain:queue", "drain:loss", "drain:rate"}[e.why])
		b.WriteString([...]string{"/turn", "/own", "/snd", "/ctrl", "/loss", "/ooo", "/rate"}[e.sig])
	case wcEvLossy:
		b.WriteString("lossy")
	case wcEvPool:
		b.WriteString("pool")
	case wcEvCapped:
		b.WriteString("capped")
	case wcEvRoute:
		b.WriteString("route")
	case wcEvUndo:
		b.WriteString("undo")
	}
	fmt.Fprintf(&b, " from=%d b=%d n=%d r=%.0f t=%.1fms q=%.1fms loss=%.4f", e.from, e.B, e.n, e.R, msOf(e.T), msOf(e.q), e.loss)
	return b.String()
}
