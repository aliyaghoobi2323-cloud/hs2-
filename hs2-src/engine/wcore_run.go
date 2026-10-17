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
// Every smux session that can grow a window past NeedStreamBuffer gets a
// core for the direction its end receives (the edge's downloads, the exit's
// uploads), registered with wcores where newSession registers the session
// with the wedge guard. One goroutine wakes every wcRampTick (100 ms) and
// steps each link whose turn has come (wcOut.next): one TCP_INFO of the
// link's own socket, one CoreSnapshot of its session (the runner is the
// session's only caller: a snapshot restarts the turnaround samples), the
// window pool's free bytes and, on the edge, the exit's latest stats
// record, the control ping's round trips and the autopilot's probing flag.
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

// Lab knobs (cmd/hs2 applyTuning: HS2_TUNE_WCORE*), read as a session
// registers; WcoreQTarget and WcoreProbeQ when the first one does.
var (
	// Wcore 0 turns the window core off: no session gets one, and every
	// window stays within NeedStreamBuffer as without it (the field safety
	// switch).
	Wcore = 1
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
	// WcoreFixedWin (bytes, > 0): no controller; every fast stream's ceiling
	// is this window, with no growth brake, the rest traced as usual (lab
	// only: calibrating the signals under fixed windows).
	WcoreFixedWin = 0
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
	set   *wcSet // the registry that steps it
	name  atomic.Pointer[string]
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
	poolShort atomic.Int32 // windows the pool could not cover in full
	peakB     atomic.Uint64
	peakR     atomic.Uint64 // float64 bits, bytes/s
	lossy     atomic.Int64  // ns under random loss
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

	// the runner's own
	list      []*wcLink
	trace     *os.File
	traceOpen bool
	agg       [2]wcAgg // filled, drain
	pool      map[*wcLink]wcPoolSeenAt
	poolLogAt time.Time
}

type wcPoolSeenAt struct {
	n  int
	at time.Time
}

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
// returns it (nil: none). server is the exit's end (it receives uploads).
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
	ws.mu.Unlock()
}

// newWcLink is the core of one session, not registered (nil: the session
// gets none).
func newWcLink(sess *smux.Session, cfg *smux.Config, tcp *net.TCPConn, mtr *linkMeter, server bool, remote net.Addr) *wcLink {
	if Wcore == 0 || cfg.NeedStreamBuffer <= 0 || cfg.MinStreamBuffer <= 0 || cfg.WindowPool == nil ||
		cfg.NeedStreamBuffer >= cfg.MaxStreamBuffer {
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
	l := &wcLink{sess: sess, tcp: tcp, mtr: mtr, pool: cfg.WindowPool, side: side,
		ceils: map[uint32]uint32{}, sampled: map[uint32]bool{}, ceiled: map[uint32]*smux.Stream{}}
	if WcoreFixedWin > 0 {
		l.fixed = uint32(min(WcoreFixedWin, cfg.MaxStreamBuffer))
	}
	l.name.Store(&name)
	l.core = newWcore(wcConfig{side: side, stage: WcoreStage, need: uint32(cfg.NeedStreamBuffer),
		maxWin: uint32(cfg.MaxStreamBuffer), minWin: uint32(cfg.MinStreamBuffer)}, rand.Float64)
	return l
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
	if now.Before(l.next) {
		return false
	}
	if l.idle() {
		l.next = now.Add(wcTickMin)
		return false
	}
	c := l.core
	pub, drainLoss := c.bPub, c.cnt.drainLoss
	out := c.step(l.bld.build(l.snap(now)))
	if l.fixed > 0 {
		l.pin(&out)
	}
	l.apply(out)
	l.publish(now)
	if c.cnt.drainLoss > drainLoss && !wcHasDrain(out.events) {
		// halved for loss between evaluations (wcore.cut)
		out.events = append(out.events, wcEvent{kind: wcEvDrain, why: wcWhyLoss, from: pub, B: c.B, n: len(c.inF), T: c.tBase, q: -1})
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
	s := wcLinkSnap{now: now, poolFree: -1}
	if ts, ok := tcpStats(l.tcp); ok {
		s.minRTT, s.srtt = usDur(uint64(ts.minRTT)), usDur(uint64(ts.rtt))
		s.segsIn, s.ooo, s.oooOK = l.segs.add(ts.segsIn), l.ooo.add(ts.rcvOOO), true
	}
	var cc smux.CoreCounters
	l.samples, cc = l.sess.CoreSnapshot(l.samples)
	s.rcvd, s.parks, s.borrows, s.brakes, s.poolShort = cc.Rcvd, cc.Parks, cc.Borrows, cc.Brakes, cc.PoolShort
	if l.pool != nil {
		s.poolFree = int64(l.pool.Free())
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
		for k := max(l.pongN, p.n-uint64(len(m.pongRTT))); k < p.n; k++ {
			if r := m.pongRTT[k%uint64(len(m.pongRTT))].Load(); r > 0 {
				s.pongs = append(s.pongs, usDur(r))
			}
		}
		l.pongN = p.n
	}
	if r := m.peer.Load(); r != nil {
		s.sndAt = r.at
		s.sndSrtt, s.sndMinRTT = usDur(uint64(r.srtt)), usDur(uint64(r.minRTT))
		s.sndRetrans, s.sndSegsOut = r.retrans, r.segsOut
	}
	if p := l.ap.Load(); p != nil {
		s.apProbing = p.Load()
	}
	return s
}

func usDur(us uint64) time.Duration { return time.Duration(us) * time.Microsecond }

// pin puts the lab's fixed window (WcoreFixedWin) in place of the
// controller's ceilings: on every fast stream, with no growth brake.
func (l *wcLink) pin(o *wcOut) {
	for i := range o.ceils {
		o.ceils[i].ceil = 0
		if l.core.inF[o.ceils[i].id] {
			o.ceils[i].ceil = l.fixed
		}
	}
	o.turnRef, o.qBrake, o.growOK = 0, 0, true
}

// apply hands the decision to smux: the session's reference first (a
// ceiling past need lets a window grow only as the reference allows), then
// each sampled stream's ceiling by id, and none to a stream that left the
// snapshot.
func (l *wcLink) apply(o wcOut) {
	l.sess.SetCoreRef(o.turnRef, o.qBrake, o.growOK)
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
	return s + fmt.Sprintf(", held by the window pool %d time(s)", st.poolShort.Load())
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
			filled = true
			l.heldB = e.B
			ws.filled(l, now, logf, e.R, e.T, e.B, e.n, e.q)
		case wcEvDrain:
			if !l.due(wcLogDrain, now, wcLogDrainEvery) || !ws.agg[1].admit(l, now, e.n, e.R) {
				continue
			}
			logf("mtcp: %s: %s — %s windows %s → %s", l.label(), wcDrainCause(e), l.dir(),
				wcMiB(e.from), wcMiB(e.B))
		case wcEvLossy:
			if !l.due(wcLogLossy, now, wcLogRareEvery) {
				continue
			}
			loss := e.loss
			if loss <= 0 {
				loss = c.last.ooo / 10 // the exit side's reading: ~a hole per ten packets behind it
			}
			logf("mtcp: %s: %.1f%% loss with no queue — windows cover the resends (+%d ms)", l.label(), 100*loss, e.q.Milliseconds())
		case wcEvCapped:
			if e.n > 0 && e.B/uint64(e.n) >= uint64(c.cfg.maxWin) {
				if !l.due(wcLogCapped, now, wcLogRareEvery) {
					continue
				}
				if e.n == 1 {
					logf("mtcp: %s: one connection's window is at its %d MiB ceiling (round trip %d ms)", l.label(), c.cfg.maxWin>>20, e.T.Milliseconds())
				} else {
					logf("mtcp: %s: %d connections' windows are at their %d MiB ceiling (round trip %d ms)", l.label(), e.n, c.cfg.maxWin>>20, e.T.Milliseconds())
				}
				continue
			}
			ws.poolHeld(l, now, e.n) // held by the pool, not the ceiling
		case wcEvPool:
			ws.poolHeld(l, now, e.n)
		case wcEvRoute:
			if l.due(wcLogRoute, now, wcLogRareEvery) {
				logf("mtcp: %s: the path's round trip grew to ~%d ms (a route change) — the window core follows it", l.label(), e.T.Milliseconds())
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
	if !l.due(wcLogFilled, now, wcLogFilledEvery) || !ws.agg[0].admit(l, now, n, r) {
		return
	}
	logf("mtcp: %s: %ss fill the path at ~%.0f Mbit/s (round trip %d ms): window held at %s for %d fast connection(s), queue ~%d ms",
		l.label(), l.dir(), mbitps(r), t.Milliseconds(), wcMiB(b), n, q.Milliseconds())
}

// wcoreEndNote is wcoreNote as the exit's link-end lines end: " (while up:
// window core: …)", or "".
func wcoreEndNote(m *linkMeter) string {
	if n := wcoreNote(m); n != "" {
		return wcEndNoteSep + n[len(wcEndNoteHead):] + ")"
	}
	return ""
}

const (
	wcEndNoteHead = "window core:"
	wcEndNoteSep  = " (while up: " + wcEndNoteHead
)

// cutEndNote splits a link's end reason from the note wcoreEndNote put after
// it.
func cutEndNote(s string) (why, note string) {
	if i := strings.Index(s, wcEndNoteSep); i >= 0 {
		return s[:i], s[i:]
	}
	return s, ""
}

func wcMiB(b uint64) string { return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20)) }

// wcDrainCause says why a drain came, for its line.
func wcDrainCause(e wcEvent) string {
	switch {
	case e.why == wcWhyRate:
		return fmt.Sprintf("path slowed to ~%.0f Mbit/s with its queue growing", mbitps(e.R))
	case e.why == wcWhyLoss && e.q < 0:
		return "packets arrived behind holes on a clean path"
	case e.why == wcWhyLoss && e.loss > 0 && e.q > 0:
		return fmt.Sprintf("path queue grew to %d ms (resent %.1f%%)", e.q.Milliseconds(), 100*e.loss)
	case e.why == wcWhyLoss && e.loss > 0:
		return fmt.Sprintf("packets were lost as the windows grew (resent %.1f%%)", 100*e.loss)
	case e.why == wcWhyLoss:
		return "packets arrived behind holes as the windows grew"
	}
	return fmt.Sprintf("path queue grew to %d ms", max(e.q, 0).Milliseconds())
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
			if i == 0 {
				(*lp)("mtcp: window core: %d more link(s) found the window that fills their path in the last minute (%d fast connection(s), ~%.0f Mbit/s)",
					len(a.links), a.conns, mbitps(a.rate))
			} else {
				(*lp)("mtcp: window core: %d more link(s) drained their windows %d time(s) in the last minute (a queue grew, or loss came, on their path)",
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
	T         string          `json:"t"`
	Link      string          `json:"link"`
	Side      string          `json:"side"` // edge (downloads) | exit (uploads)
	State     string          `json:"state"`
	B         uint64          `json:"b"`     // the budget decided
	BPub      uint64          `json:"b_pub"` // ... and published (the ramp's)
	Fixed     uint32          `json:"fixed,omitempty"`
	Dt        float64         `json:"dt_s"`
	NF        int             `json:"n_f"`
	RF        float64         `json:"r_f"` // bytes/s
	RLink     float64         `json:"r_link"`
	RHat      float64         `json:"r_hat"` // R̂ for F
	Grown     float64         `json:"grown"`
	WF        float64         `json:"w_f"`
	HStep     float64         `json:"h_step"`
	Hit       float64         `json:"hit"`
	TBase     float64         `json:"t_base_ms"`
	TurnBase  float64         `json:"turn_base_ms"`
	Tau       float64         `json:"tau_hold_ms"`
	QTurn     float64         `json:"q_turn_ms"`
	QTurnOK   bool            `json:"q_turn_ok"`
	QRef      float64         `json:"q_ref_ms"`
	QSnd      float64         `json:"q_snd_ms"`
	QSndOK    bool            `json:"q_snd_ok"`
	QOwn      float64         `json:"q_own_ms"`
	QCtrl     float64         `json:"q_ctrl_ms"` // the longest pong over rttMin in the interval (0: none)
	CtrlMin   float64         `json:"ctrl_min_ms"`
	Pongs     []float64       `json:"pongs_ms,omitempty"`
	MinRTT    float64         `json:"min_rtt_ms"` // the link socket's own
	SRTT      float64         `json:"srtt_ms"`
	SegsIn    uint64          `json:"segs_in"`
	Ooo       float64         `json:"ooo"`
	OooOK     bool            `json:"ooo_ok"`
	Snd       float64         `json:"snd"` // the exit's resend share (a fresh record)
	SndOK     bool            `json:"snd_ok"`
	SndSRTT   float64         `json:"snd_srtt_ms,omitempty"`
	SndMinRTT float64         `json:"snd_min_rtt_ms,omitempty"`
	Spread    float64         `json:"spread_ms"`
	QTarget   float64         `json:"q_target_ms"`
	ProbeQ    float64         `json:"probe_q_ms"`
	Qcap      float64         `json:"q_cap_ms,omitempty"`
	Parks     uint32          `json:"parks"`
	Brakes    uint32          `json:"brakes"`
	PoolFree  int64           `json:"pool_free"`
	PoolShort uint32          `json:"pool_short"`
	APProbing bool            `json:"ap_probing"`
	TurnRef   float64         `json:"turn_ref_ms"`
	QBrake    float64         `json:"q_brake_ms"`
	GrowOK    bool            `json:"grow_ok"`
	Streams   []wcTraceStream `json:"streams"`
	Events    []string        `json:"events,omitempty"`
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
		TurnRef: msOf(o.turnRef), QBrake: msOf(o.qBrake), GrowOK: o.growOK}
	if m.dt > 0 && c.tBase > 0 {
		t.RHat = c.rhatF(now, m)
	}
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
	case wcEvLossy:
		b.WriteString("lossy")
	case wcEvPool:
		b.WriteString("pool")
	case wcEvCapped:
		b.WriteString("capped")
	case wcEvRoute:
		b.WriteString("route")
	}
	fmt.Fprintf(&b, " from=%d b=%d n=%d r=%.0f t=%.1fms q=%.1fms loss=%.4f", e.from, e.B, e.n, e.R, msOf(e.T), msOf(e.q), e.loss)
	return b.String()
}
