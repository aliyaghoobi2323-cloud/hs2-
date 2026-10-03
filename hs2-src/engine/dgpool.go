package engine

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/udpcarrier"
)

// Datagram tun pool: a routed TUN carried over a POOL of datagram carriers
// (udpcarrier over udp/icmp/gre/ipip/ipx, or the auto transport), sized by the
// SAME autopilot controller as the stream pool (engine/autopilot.go).
//
// Why a pool of datagram carriers, not a pool of TCP streams: each carrier is a
// datagram pipe with its own crypto, FEC and delay-based pacing, so an IP
// packet from the TUN crosses the path as ONE sealed datagram. The only TCP in
// the picture is the user's own connection running THROUGH the tunnel — carried
// as ordinary IP packets — so there is no TCP-in-TCP and no retransmission
// meltdown under loss. Several carriers exist for the same reason the stream
// pool has several links: a per-flow throttle (a DPI box policing each 5-tuple)
// caps each carrier, and N carriers carry up to N× the cap; the carriers'
// own delay-based pacing keeps each one from bloating the path.
//
// Placement is per flow, by rendezvous hashing (highest-random-weight), exactly
// like the stream pool: a flow keeps its carrier while that carrier lives, so a
// carrier's death only moves its own flows and nothing else reorders. A packet
// is never blocked behind a slow carrier — a full per-carrier queue drops it
// (the inner TCP reads that as ordinary loss and backs off) and marks the
// carrier PRESSED, which is what tells the autopilot the path is the limit.
//
// The autopilot sees the same signals it sees for streams: flows per carrier,
// throughput per carrier, and pressure. The TUN is opened once and never closed
// because a carrier failed (the engine's invariant); carriers come and go under
// it. In reverse only the exit dials, so the edge publishes its serving target
// down one carrier as a TypePoolCtl frame and the exit runs a matching set of
// dial slots.

const (
	// dgQueueLen / dgSojourn bound one carrier's send queue: short, and in
	// time, so a slow carrier drops rather than buffering seconds of latency.
	dgQueueLen = 256
	dgSojourn  = 50 * time.Millisecond
	// dgWarmGrace: a carrier's backpressure is not read as path pressure for
	// this long after it joins (its rate model is still ramping). The carrier's
	// own Warm() lifts this sooner when it has a capacity estimate.
	dgWarmGrace = 4 * time.Second
	// dgPoolCtlEvery: how often the reverse edge (re)publishes its target.
	dgPoolCtlEvery = 3 * time.Second
	// dgSilentDead: a carrier that has received nothing for this long is dead
	// — its peer sends feedback every 100 ms while it lives — once a fresh
	// carrier proves the path works (a restarted peer forgets its carriers
	// and drops their packets silently; they used to linger 15 s).
	dgSilentDead = 3 * time.Second
	// dgScoutEvery: while every carrier is silent, the dialing side tries one
	// new carrier this often to find out whether the peer is back.
	dgScoutEvery = 5 * time.Second
	// dgInfoEvery: how often a DIRECT edge tells the exit its ceiling (display
	// only, so slow; jittered so it is not a fixed beat). A report older than
	// dgPeerMaxStale reads as unknown (the peer stopped, or was downgraded).
	dgInfoEvery    = 15 * time.Second
	dgPeerMaxStale = 3 * dgInfoEvery
	dgDialBudget   = 4 // most carriers dialed per health tick (edge, direct)
	// dgDownStatsStale: a download-stats report older than this is ignored, so
	// an exit that stops reporting (or an older one that never does) falls back
	// to upload-only sizing rather than holding a stale pressure reading.
	dgDownStatsStale = 3 * healthTick
)

// dgCarrier is a datagram Carrier that can also report whether its rate model
// has warmed up. Real carriers (udpcarrier.Conn) satisfy Warm(); a carrier
// without it is always treated as warm.
type dgCarrier interface {
	Carrier
	Warm() bool
}

// warmOf reports a carrier's warm state (true when it cannot say).
func warmOf(c Carrier) bool {
	if w, ok := c.(interface{ Warm() bool }); ok {
		return w.Warm()
	}
	return true
}

// DgDialer dials one datagram carrier to the peer. Reconnection calls it again.
type DgDialer interface {
	Dial(ctx context.Context) (Carrier, error)
}

// DgListener accepts datagram carriers the peer dials in.
type DgListener interface {
	Accept(ctx context.Context) (Carrier, error)
	Close() error
}

// dgLink is one carrier in the pool: a datagram pipe with a bounded send queue,
// its own writer goroutine, and the per-flow and throughput bookkeeping the
// autopilot reads.
type dgLink struct {
	car  Carrier
	id   uint32 // rendezvous seed
	q    chan qpkt
	done chan struct{}
	once sync.Once
	dead atomic.Bool

	// icmp echo-shaping (B5): set once in add(), before the pumps start. When a
	// carrier runs over the icmp encapsulation its wire packets are echo
	// requests/replies that a stateful classifier expects to be ~1:1, so the
	// pool keeps this carrier's frames sent close to frames received with cheap
	// fillers (echoBalance). echoDial is this side's role: true = it sends echo
	// REQUESTS (the dial side), false = it sends echo REPLIES (the listen side).
	echoShaped bool
	echoDial   bool

	born         time.Time
	servingSince time.Time
	// peerRetiring: the OTHER side is retiring this carrier (TypeClose
	// closeRetire), so new flowlets avoid it here too and it can empty.
	peerRetiring atomic.Bool
	// retiringAt: unix ns since when either side retires this carrier (0 =
	// serving). After dgRetireForce its sticky flows are moved off it too.
	retiringAt   atomic.Int64
	retireSentAt time.Time // when this side last told the peer it retires it (under pool.mu)
	retiring     bool      // set under pool.mu
	retireSince  time.Time
	bornSpare    bool // reverse: arrived while the pool already had its target

	// measurement (bytesUp/Down by the data paths; the rest under pool.mu in
	// the sampler)
	bytesUp   atomic.Uint64
	bytesDown atomic.Uint64
	sentPkts  atomic.Uint64
	droppedAt atomic.Int64 // unixnano of the last queue-full drop (pressure)

	// echo-shaping frame counts (icmp only): every frame this side sent
	// (writeLoop data, sendVia control, fillers) and received (readLoop), so
	// echoBalance can tell which direction is light and top it up toward 1:1.
	rxFrames atomic.Uint64
	txFrames atomic.Uint64

	// sampler state (pool goroutine only)
	prevUp, prevDown uint64
	rate             float64
	rates            [5]float64
	doms             [3]float64
	nSamples         int
	rate10           float64
	sustained        float64
	pressed          bool

	flowMu sync.Mutex
	flows  map[uint32]*dgFlow

	// ro puts each TCP flow's segments from this carrier back in order before
	// the TUN (see reorder.go). Set by the pool when the carrier joins.
	ro *reorderer
}

// dgFlow is one L3 flow's activity on a carrier.
type dgFlow struct {
	last  time.Time
	bytes uint64
	prev  uint64
	ewma  float64
}

func newDgLink(car Carrier, now time.Time) *dgLink {
	l := &dgLink{
		car: car, id: randSeed(), q: make(chan qpkt, dgQueueLen), done: make(chan struct{}),
		born: now, servingSince: now, flows: map[uint32]*dgFlow{},
	}
	return l
}

func (l *dgLink) alive() bool { return !l.dead.Load() && l.car != nil }

func (l *dgLink) markDead() {
	l.once.Do(func() {
		l.dead.Store(true)
		close(l.done)
		l.car.Close()
	})
}

// lastRx is when the carrier last received anything (zero when it cannot
// say — such a carrier is never taken for silent).
func (l *dgLink) lastRx() time.Time {
	if r, ok := l.car.(interface{ LastRx() time.Time }); ok {
		return r.LastRx()
	}
	return time.Time{}
}

// silent reports whether the carrier has received nothing for dgSilentDead.
func (l *dgLink) silent(now time.Time) bool {
	t := l.lastRx()
	return !t.IsZero() && now.Sub(t) >= dgSilentDead
}

// setRetiring records when this carrier started retiring (either side) or
// that it serves again.
func (l *dgLink) setRetiring(now time.Time, on bool) {
	switch {
	case !on:
		l.retiringAt.Store(0)
	case l.retiringAt.Load() == 0:
		l.retiringAt.Store(now.UnixNano())
	}
}

// retireForced reports whether the carrier has been retiring for
// dgRetireForce: its flows then move off it even mid-burst.
func (l *dgLink) retireForced(now time.Time) bool {
	at := l.retiringAt.Load()
	return at != 0 && now.UnixNano()-at >= int64(dgRetireForce)
}

// dgRetireForce: a retiring carrier keeps its sticky flows (moving a flow
// mid-burst reorders it) — but a flow that never pauses would hold it, and
// the shrink, forever (download on a busy pool: the review saw carriers stay
// retiring as long as the download ran). After this long the flows move,
// each with one reorder, and the carrier drains. A variable for tests.
var dgRetireForce = 30 * time.Second

// TypeClose payload (one byte; old peers ignore the frame entirely, so it
// needs no negotiation): the carrier is closing now, the sender is retiring
// it (no new flowlets), or it serves again.
const (
	closeBye    = 0
	closeRetire = 1
	closeServe  = 2
)

// enqueue hands a packet to the carrier's writer without ever blocking. A full
// queue drops the packet and records pressure.
func (l *dgLink) enqueue(b *[]byte, flow uint32, now time.Time) bool {
	select {
	case l.q <- qpkt{b, now}:
		l.noteFlowSend(flow, len(*b), now)
		return true
	default:
		l.droppedAt.Store(now.UnixNano())
		return false
	}
}

func (l *dgLink) noteFlowSend(flow uint32, n int, now time.Time) {
	l.bytesUp.Add(uint64(n))
	l.flowMu.Lock()
	f := l.flows[flow]
	if f == nil {
		f = &dgFlow{}
		l.flows[flow] = f
	}
	f.bytes += uint64(n)
	f.last = now
	l.flowMu.Unlock()
}

func (l *dgLink) noteFlowRecv(flow uint32, n int, now time.Time) {
	l.bytesDown.Add(uint64(n))
	l.flowMu.Lock()
	f := l.flows[flow]
	if f == nil {
		f = &dgFlow{}
		l.flows[flow] = f
	}
	f.bytes += uint64(n)
	f.last = now
	l.flowMu.Unlock()
}

// writeLoop is the only goroutine that sends on the carrier. Each queued IP
// packet is one datagram; the carrier does its own pacing and framing, so
// there is nothing to coalesce (unlike the TLS l3 path).
func (l *dgLink) writeLoop(pool *sync.Pool, drops, aged, sent *atomic.Uint64) {
	for {
		select {
		case <-l.done:
			return
		case p := <-l.q:
			if time.Since(p.t) > dgSojourn {
				// The packet aged out behind a writer blocked in the pacer: the
				// carrier could not drain its queue within the sojourn, so it is
				// at its limit just as surely as on a queue-full drop. Record it
				// as pressure, or a starved carrier reads as "not at its limit"
				// and the pool shrinks under exactly the load that needs it.
				l.droppedAt.Store(time.Now().UnixNano())
				drops.Add(1)
				aged.Add(1)
				pool.Put(p.b)
				continue
			}
			err := l.car.SendFrame(core.TypeData, *p.b)
			pool.Put(p.b)
			if err != nil {
				l.markDead()
				return
			}
			l.sentPkts.Add(1)
			sent.Add(1)
			if l.echoShaped {
				l.txFrames.Add(1)
			}
		}
	}
}

// randSeed is a non-zero random 32-bit rendezvous seed.
func randSeed() uint32 {
	for {
		if v := rand.Uint32(); v != 0 {
			return v
		}
	}
}

// dgPool is a set of datagram carriers under one TUN, sized by the autopilot.
// The zero value is not usable; use newDgPool.
type dgPool struct {
	dev    tunWriter
	dialer DgDialer // direct edge / reverse exit: dials carriers; nil on the accepting side
	accept bool     // reverse edge: carriers arrive via Add, never dialed here
	// revExit (reverse exit): the edge chooses which carriers retire and
	// closes them; the exit only stops redialing above the target, never
	// retires on its own (two ends retiring different carriers would cut
	// serving far below the target and churn redials).
	revExit    bool
	hadCarrier bool // reverse exit: a carrier was up since the target last fell back
	// Dials take turns from the process's dial gate; dialing counts those
	// queued or in progress (they count toward the target), and a failed dial
	// bumps dialEpoch so dials queued before it are dropped unattempted.
	gate       *dialGate
	dialing    atomic.Int32
	dialEpoch  atomic.Uint64
	failN      atomic.Int32
	failStreak atomic.Int32
	failLog    atomic.Int64 // unix ns of the last failure line
	min        int
	max        int
	perLink    int
	log        func(string, ...any)
	clock      func() time.Time

	ap    *autopilot
	mu    sync.RWMutex
	set   []*dgLink
	pool  sync.Pool     // *[]byte packet buffers
	drops atomic.Uint64 // no carrier, queue full, or waited too long

	target       atomic.Int32 // serving carriers wanted (published to a reverse exit)
	lastSampleAt time.Time
	dec          apDecision
	stats        atomic.Pointer[PoolStats]
	growable     bool

	// reverse edge: the carrier currently carrying pool-control, and the exit's
	// clamps learned from it.
	poolCtlMu sync.Mutex
	poolCtl   *dgLink

	// Download-stats backchannel. The pool is sized by the EDGE (direct: it
	// dials; reverse: it publishes the target), which is the download RECEIVER
	// and so never sees the download sender's send-queue pressure — a download-
	// bound pool would shrink to min and throttle the download to a few
	// carriers. The EXIT (the download sender) measures that pressure and
	// reports it to the edge as a TypeLinkStats frame; the edge folds it into
	// sampleHealth so the autopilot sizes for the busier of the two directions.
	// A peer that predates the frame drops it unread, so it is safe to send.
	downSender bool          // EXIT: measure and report; EDGE: consume and fold in
	dnPressed  atomic.Uint32 // EDGE: exit's serving carriers pressed on download
	dnServing  atomic.Uint32 // EDGE: exit's serving carriers (for clamping)
	dnStatsAt  atomic.Int64  // EDGE: unixnano the last report arrived (0 = none)

	// The other server's link-pool ceiling, for display only (both servers
	// show the effective ceiling and which side limits it). It rides the two
	// control frames that already flow, as two trailing bytes their older
	// parsers never read: the exit's on TypeLinkStats, the edge's on
	// TypePoolCtl (sent in direct mode too, slowly, where the exit ignores the
	// target). No new frame type, so the carrier is untouched; nothing here
	// feeds sizing. peerMaxAt is unixnano of the last report (0 = none).
	peerMax   atomic.Uint32
	peerMaxAt atomic.Int64

	// gov watches the pool as a whole for a policer on the path to the peer
	// (all carriers share its IP) and caps the pool's total send rate.
	gov *udpcarrier.Governor

	// sticky maps a live flow to the carrier it is on. Rendezvous hashing
	// alone moves ~1/n of the flows whenever the serving set changes (a carrier
	// added, retired or un-retired), and every move reorders that flow's
	// packets — which its inner TCP reads as loss. So a flow stays on its
	// carrier until the carrier dies or the flow pauses; only then is it
	// hashed again. Written by pumpTun only; pruned by drainTick.
	stickyMu sync.Mutex
	sticky   map[uint32]*stickyFlow

	// packet accounting for the status file (what a field test needs to see
	// where loss happens): read from the tun, handed to carriers, received from
	// carriers, written to the tun, and dropped by reason.
	tunRead, tunWritten, sentPkts, recvPkts atomic.Uint64
	dropNoCarrier, dropQueueFull, dropAged  atomic.Uint64

	// upLog / retireLog fold per-carrier lines into one summary once a burst
	// passes a few lines (a 300-carrier rebuild was ~300 lines per side).
	upLog, retireLog, byeLog *burstLog

	// scout (dialing side, pool goroutine only): the next time a scout dial
	// may go while every carrier is silent, and whether this silence was
	// logged.
	nextScout   time.Time
	scoutLogged bool

	fecCeilLogged bool      // "FEC at its ceiling" was logged and not yet cleared
	fecCeilRun    int       // consecutive samples at (+) / below (-) the ceiling
	phaseLabel    string    // fixed phase for the snapshot (the direct exit: "listening")
	gHist         []float64 // throughput per published tick, for the last minute's peak
}

type stickyFlow struct {
	l    *dgLink
	last time.Time
}

func newDgPool(dev tunWriter, min, max, perLink int, logf func(string, ...any)) *dgPool {
	if min < 1 {
		min = 1
	}
	if max < min {
		max = min
	}
	if perLink < 1 {
		perLink = 8
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	p := &dgPool{dev: dev, min: min, max: max, perLink: perLink, log: logf,
		ap: newAutopilot(min, max, perLink), growable: true, gov: udpcarrier.NewGovernor(logf),
		sticky: map[uint32]*stickyFlow{}}
	p.pool.New = func() any { b := make([]byte, 0, 2048); return &b }
	p.target.Store(int32(warmSize(min, max)))
	logp := func(f string, a ...any) { p.log(f, a...) } // p.log may be swapped (tests)
	p.upLog = newBurstLog("dg: ", "carriers up", logp)
	p.retireLog = newBurstLog("dg: ", "carriers retired", logp)
	p.byeLog = newBurstLog("dg: ", "carriers closed by the other server", logp)
	return p
}

func (p *dgPool) now() time.Time {
	if p.clock != nil {
		return p.clock()
	}
	return time.Now()
}

// Target is the serving-carrier count the autopilot wants (for the reverse
// exit and the live monitor).
func (p *dgPool) Target() int { return int(p.target.Load()) }

// poolProbe lets tests observe the most recent edge pool. Nil in production;
// set only from RunDgEdge when the test build tag is not used — kept tiny and
// harmless. (Tests read it; production never does.)
var poolProbe atomic.Pointer[*dgPool]

// countsLive returns the live serving/retiring counts (test helper).
func (p *dgPool) countsLive() (serving, retiring int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.countsLocked()
}

func (p *dgPool) count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.set)
}

func (p *dgPool) countsLocked() (serving, retiring int) {
	for _, l := range p.set {
		if !l.alive() {
			continue
		}
		if l.retiring {
			retiring++
		} else {
			serving++
		}
	}
	return serving, retiring
}

// add installs a carrier and starts its read/write pumps. Used by the dialer
// (direct edge, reverse exit) and by the accept loop (reverse edge). It returns
// the new link.
func (p *dgPool) add(ctx context.Context, car Carrier, from string) *dgLink {
	now := p.now()
	l := newDgLink(car, now)
	l.echoShaped = carrierEchoShaped(car)
	l.echoDial = p.dialer != nil // this side dials => it sends echo requests
	if a, ok := car.(interface{ AttachGovernor(*udpcarrier.Governor) }); ok {
		a.AttachGovernor(p.gov)
	}
	// Set ro BEFORE publishing the link to p.set: carrierStats reads l.ro under
	// p.mu, so assigning it after the append below raced with that read (a
	// pre-existing data race the -race detector flags on TestDgPoolReverse).
	l.ro = newReorderer(dgReorderHold, func(b []byte) {
		if _, err := p.dev.Write(b); err == nil {
			p.tunWritten.Add(1)
		}
	})
	p.mu.Lock()
	if s, _ := p.countsLocked(); p.accept && s >= int(p.target.Load()) {
		l.retiring, l.retireSince, l.bornSpare = true, now, true
		l.retireSentAt = now
		l.setRetiring(now, true)
	}
	// A carrier just came up, so the path works: any carrier that has heard
	// nothing for dgSilentDead is dead — its peer restarted (and drops its
	// packets) or its path is gone — and would black-hole the flows on it
	// until its 15 s timeout.
	var zombies []*dgLink
	rxNow := time.Now()
	for _, o := range p.set {
		if o.alive() && o.silent(rxNow) {
			zombies = append(zombies, o)
		}
	}
	p.set = append(p.set, l)
	n := len(p.set)
	p.mu.Unlock()
	go l.writeLoop(&p.pool, &p.drops, &p.dropAged, &p.sentPkts)
	go p.readLoop(ctx, l)
	if l.bornSpare {
		p.sendOp(l, closeRetire)
	}
	if len(zombies) > 0 {
		for _, z := range zombies {
			z.markDead()
		}
		p.log("dg: dropped %d carrier(s) silent for %s+ when carrier %d came up — the other server restarted, or their path died; their flows move to live carriers", len(zombies), fmtDur(dgSilentDead), l.id)
	}
	if l.bornSpare {
		p.upLog.log("dg: carrier %d %s up (now %d) — spare: pattern needs %d serving", l.id, from, n, p.Target())
	} else {
		p.upLog.log("dg: carrier %d %s up (now %d)", l.id, from, n)
	}
	return l
}

// readLoop pumps received frames from one carrier into the TUN until it dies.
func (p *dgPool) readLoop(ctx context.Context, l *dgLink) {
	defer l.markDead()
	defer l.ro.Close() // release anything still held behind a gap
	for ctx.Err() == nil {
		ft, payload, err := l.car.ReadFrame()
		if err != nil {
			return
		}
		// Count only REAL frames (data, pool control) toward the echo balance —
		// never a received filler (Ping/Pong). If a received filler were counted,
		// this side would owe a reply to it and the peer would owe one back, so
		// two balancers could trade fillers without end. Anchoring on real traffic
		// keeps the data-heavy direction ahead (it never fills) and only the light
		// one catching up, so fillers stay bounded by the real data they match.
		if l.echoShaped && ft != core.TypePing && ft != core.TypePong {
			l.rxFrames.Add(1)
		}
		switch ft {
		case core.TypeData:
			now := p.now()
			p.recvPkts.Add(1)
			l.noteFlowRecv(flowHash(payload), len(payload), now)
			if l.ro != nil {
				l.ro.Push(payload) // in TCP order, or held briefly behind a gap
			} else if _, err := p.dev.Write(payload); err == nil {
				p.tunWritten.Add(1)
			}
		case core.TypeClose:
			if p.onCloseFrame(l, payload) {
				return // the peer closed it
			}
		case core.TypePing, core.TypePong:
			// Inert in the datagram pool — the carrier handles its own liveness.
			// A Ping/Pong here is the peer's echo-shaping filler; it is neither
			// counted (above) nor answered, so fillers can never chain.
		case core.TypePoolCtl:
			p.onPoolCtl(l, payload)
		case core.TypeLinkStats:
			p.onLinkStats(payload)
		}
		if l.echoShaped {
			p.echoBalance(l)
		}
	}
}

// sendVia sends a control frame on a carrier (pong, pool control, echo filler),
// off the data queue so it is timely.
func (p *dgPool) sendVia(l *dgLink, ft byte, payload []byte) {
	if err := l.car.SendFrame(ft, payload); err != nil {
		l.markDead()
		return
	}
	if l.echoShaped {
		l.txFrames.Add(1)
	}
}

// sendOp tells the peer what this side does with carrier l (TypeClose).
func (p *dgPool) sendOp(l *dgLink, op byte) {
	if l.alive() {
		p.sendVia(l, core.TypeClose, []byte{op})
	}
}

// closeLink closes a carrier on purpose: the peer is told first (twice — a
// control datagram can be lost), so it stops placing flows on it at once
// instead of black-holing them until its 15 s timeout.
func (p *dgPool) closeLink(l *dgLink) {
	if l.alive() {
		l.car.SendFrame(core.TypeClose, []byte{closeBye})
		l.car.SendFrame(core.TypeClose, []byte{closeBye})
	}
	l.markDead()
}

// onCloseFrame handles the peer's TypeClose on carrier l; true when the peer
// closed it (the read loop ends).
func (p *dgPool) onCloseFrame(l *dgLink, payload []byte) bool {
	op := byte(closeBye)
	if len(payload) > 0 {
		op = payload[0]
	}
	switch op {
	case closeBye:
		p.byeLog.log("dg: carrier %d closed by the other server", l.id)
		l.markDead()
		return true
	case closeRetire:
		l.peerRetiring.Store(true)
		l.setRetiring(p.now(), true)
	case closeServe:
		l.peerRetiring.Store(false)
		l.setRetiring(p.now(), false)
	}
	return false
}

// encapICMP is the ICMP encapsulation kind (encap.KindICMP), named here so the
// engine need not import encap for one comparison.
const encapICMP = "icmp"

// echoFillerPad is a shared, read-only zero buffer the balancer slices for
// filler payloads, so a line-rate carrier allocates nothing per filler. Sealing
// only reads the payload, so one backing array is safe across concurrent sends.
var echoFillerPad [96]byte

// carrierEchoShaped reports whether a carrier runs over the icmp encapsulation,
// so its echo requests/replies should be kept ~1:1 (B5). It mirrors the
// optional-interface style of warmOf/AttachGovernor; a carrier that cannot say
// (a test fake, a non-udpcarrier transport) is treated as not echo-shaped, so
// udp/gre/ipip/ipx carriers and the rest of the pool are completely unaffected.
func carrierEchoShaped(c Carrier) bool {
	e, ok := c.(interface{ Encap() string })
	// Match encap's own kind normalization (lower-cased, trimmed): a config of
	// "ICMP" or " icmp " opens a fully working ICMP socket, so it must be shaped
	// too — an exact compare here would silently leave the balancer off and put
	// the one-directional fingerprint back, with no error.
	return ok && strings.EqualFold(strings.TrimSpace(e.Encap()), encapICMP)
}

// echoBalance keeps an icmp carrier's frames sent close to frames received, so
// its echo requests/replies stay ~1:1 like a real ping (B5). Called once per
// received frame: when this side has sent fewer frames than it has received, it
// emits one cheap filler to catch up — a TypePong (a reply) on the side that
// answers echo requests, a TypePing (a request) on the side that sends them.
//
// Data (writeLoop) and control frames also count toward txFrames, so a side
// already sending enough on its own emits no filler; the filler only fills the
// gap the LIGHT direction leaves (upload replies on a download-heavy reverse
// edge; upload requests on a download-heavy direct edge). Received fillers are
// not counted (see readLoop), so a filler never begets a filler and the HEAVY
// direction (txFrames > rxFrames) never fills — no feedback loop whatsoever. It
// is purely additive: it never gates, delays or drops a
// data frame, so the carrier's throughput is unchanged. The ratio is only
// approximate (FEC fans one data frame into several wire shards, feedback rides
// below the engine), which is all a real-ping ratio check needs.
func (p *dgPool) echoBalance(l *dgLink) {
	if l.txFrames.Load() >= l.rxFrames.Load() {
		return
	}
	ft := byte(core.TypePong) // listen side: answer a request with a reply
	if l.echoDial {
		ft = core.TypePing // dial side: add a request to pull the peer's replies
	}
	// A small, varied pad (like a keepalive) so fillers are not all one size.
	// math/rand, not crypto/rand: the size is cosmetic (the payload is masked
	// and AEAD-sealed) and this runs at roughly the heavy direction's packet
	// rate, so a per-packet CSPRNG draw would be wasted work.
	p.sendVia(l, ft, echoFillerPad[:rand.IntN(len(echoFillerPad))])
}

// pumpTun reads IP packets from the TUN and places each on its flow's carrier
// by rendezvous hashing. It never blocks: no carrier or a full queue drops the
// packet (the inner TCP resends), which also marks the carrier pressed.
func (p *dgPool) pumpTun(ctx context.Context) {
	size := p.dev.MTU() + 128
	for ctx.Err() == nil {
		bp, _ := p.pool.Get().(*[]byte)
		if bp == nil || cap(*bp) < size {
			b := make([]byte, size)
			bp = &b
		}
		b := (*bp)[:size]
		n, err := p.dev.Read(b)
		if err != nil {
			p.pool.Put(bp)
			if ctx.Err() != nil {
				return
			}
			continue
		}
		*bp = b[:n]
		p.tunRead.Add(1)
		flow := flowHash(*bp)
		now := p.now()
		l := p.pick(flow, now)
		switch {
		case l == nil:
			p.pool.Put(bp)
			p.dropNoCarrier.Add(1)
			p.drops.Add(1)
		case !l.enqueue(bp, flow, now):
			p.pool.Put(bp)
			p.dropQueueFull.Add(1)
			p.drops.Add(1)
		}
	}
}

// pick returns the carrier for a flow: the one it is already on, while that
// carrier lives (serving or retiring) and the flow has not paused — so neither
// a pool resize nor retiring ever reorders a live flow; otherwise a live serving
// carrier by rendezvous hashing, which is then remembered.
func (p *dgPool) pick(flow uint32, now time.Time) *dgLink {
	p.stickyMu.Lock()
	if sf := p.sticky[flow]; sf != nil {
		if sf.l.alive() && now.Sub(sf.last) <= flowletGap && !sf.l.retireForced(now) {
			sf.last = now
			p.stickyMu.Unlock()
			return sf.l
		}
		delete(p.sticky, flow)
	}
	p.stickyMu.Unlock()
	l := p.pickHash(flow)
	if l != nil {
		p.stickyMu.Lock()
		p.sticky[flow] = &stickyFlow{l: l, last: now}
		p.stickyMu.Unlock()
	}
	return l
}

// pruneSticky forgets flows that paused or whose carrier died (drainTick).
func (p *dgPool) pruneSticky(now time.Time) {
	p.stickyMu.Lock()
	for f, sf := range p.sticky {
		if !sf.l.alive() || now.Sub(sf.last) > flowletGap {
			delete(p.sticky, f)
		}
	}
	p.stickyMu.Unlock()
}

// pickHash maps a flow to a live serving carrier by rendezvous hashing.
func (p *dgPool) pickHash(flow uint32) *dgLink {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var best *dgLink
	var bestW uint32
	var bestRetiring *dgLink
	var bestRW uint32
	for _, l := range p.set {
		if !l.alive() {
			continue
		}
		w := mix32(flow ^ l.id)
		if l.retiring || l.peerRetiring.Load() {
			if bestRetiring == nil || w > bestRW {
				bestRetiring, bestRW = l, w
			}
			continue
		}
		if best == nil || w > bestW {
			best, bestW = l, w
		}
	}
	if best != nil {
		return best
	}
	return bestRetiring // only retiring carriers left: better than dropping
}

// sampleHealth measures every carrier once per tick and builds the autopilot's
// sample: throughput per carrier and in total, active flows, and pressure (the
// carrier's send queue overflowing while it is warm — the path, not the tunnel,
// is the limit).
func (p *dgPool) sampleHealth() apSample {
	now := p.now()
	dt := healthTick
	if !p.lastSampleAt.IsZero() {
		if d := now.Sub(p.lastSampleAt); d > time.Millisecond {
			dt = d
		}
	}
	p.lastSampleAt = now
	secs := dt.Seconds()

	capped := p.gov.Capped()
	p.mu.Lock()
	defer p.mu.Unlock()
	s := apSample{now: now, growable: p.growable}
	var dnRate []float64 // this tick's RECEIVE (download) rate per s.links entry
	for _, l := range p.set {
		if !l.alive() {
			continue
		}
		up, down := l.bytesUp.Load(), l.bytesDown.Load()
		dUp, dDown := up-l.prevUp, down-l.prevDown
		l.prevUp, l.prevDown = up, down
		l.rate = float64(dUp+dDown) / secs
		dom := float64(max64(dUp, dDown)) / secs
		l.rates[l.nSamples%len(l.rates)] = l.rate
		l.doms[l.nSamples%len(l.doms)] = dom
		l.nSamples++
		l.rate10 = meanF(l.rates[:min(l.nSamples, len(l.rates))])
		l.sustained = 0
		if l.nSamples >= len(l.doms) {
			l.sustained = l.doms[0]
			for _, d := range l.doms[1:] {
				l.sustained = minF(l.sustained, d)
			}
		}
		flowing, open := l.flowStats(now, dt)
		// Pressure: the carrier dropped a packet for a full queue within the
		// last tick (offered faster than the path drains) AND it is warm (past
		// startup), so a still-ramping carrier is not mistaken for a full path.
		warm := warmOf(l.car) || now.Sub(l.servingSince) >= dgWarmGrace
		dropped := l.droppedAt.Load()
		// Under a policer cap a full queue is the cap, not the path: another
		// carrier would share the same budget, so it never counts as pressure.
		l.pressed = !capped && !l.retiring && warm && dropped != 0 && now.Sub(time.Unix(0, dropped)) <= dt
		s.links = append(s.links, apLink{
			id: int(l.id), serving: l.alive() && !l.retiring, retiring: l.retiring,
			servingSince: l.servingSince, pressed: l.pressed,
			rate: l.rate, rate10: l.rate10, sustained: l.sustained,
			flowing: flowing, open: open,
		})
		dnRate = append(dnRate, float64(dDown)/secs)
		s.G += l.rate
		s.flowing += flowing
		s.open += open
	}
	p.foldDownPressure(s.links, dnRate, now)
	return s
}

// foldDownPressure folds the download sender's reported pressure into this
// (edge) sample. The edge is the download RECEIVER, so its own send-queue
// pressure only reflects the UPLOAD direction; without this a download-bound
// pool would shrink to min and throttle the download to a few carriers. The
// exit reports how many of its serving carriers are pressed on the download
// path (TypeLinkStats); we mark that many of OUR busiest-by-download serving
// carriers pressed, so the autopilot sizes for the busier of the two
// directions (a carrier added for download also serves upload). We never lower
// the edge's own pressed count — the effective pressure is max(up, down). An
// absent or stale report (an older exit, or no download pressure) is a no-op,
// so mixed-version pairs keep today's behaviour.
func (p *dgPool) foldDownPressure(links []apLink, dnRate []float64, now time.Time) {
	if p.downSender { // only the sizer (edge) folds in; the exit produces it
		return
	}
	at := p.dnStatsAt.Load()
	if at == 0 || now.Sub(time.Unix(0, at)) > dgDownStatsStale {
		return
	}
	want := int(p.dnPressed.Load())
	if want <= 0 {
		return
	}
	have := 0
	for _, l := range links {
		if l.serving && l.pressed {
			have++
		}
	}
	extra := want - have
	if extra <= 0 {
		return
	}
	// Candidates: serving, not already pressed, actually carrying download this
	// tick — never fabricate pressure on an idle carrier. Busiest first, so the
	// per-carrier capacity the autopilot learns is the real download rate.
	cand := make([]int, 0, len(links))
	for i := range links {
		if links[i].serving && !links[i].pressed && dnRate[i] > 0 {
			cand = append(cand, i)
		}
	}
	for a := 1; a < len(cand); a++ { // insertion sort by download rate, desc
		for b := a; b > 0 && dnRate[cand[b]] > dnRate[cand[b-1]]; b-- {
			cand[b], cand[b-1] = cand[b-1], cand[b]
		}
	}
	for k := 0; k < extra && k < len(cand); k++ {
		links[cand[k]].pressed = true
	}
}

// flowStats counts a carrier's flows and how many are actively moving data,
// ageing out idle ones. Pool goroutine only.
func (l *dgLink) flowStats(now time.Time, dt time.Duration) (flowing, open int) {
	alpha := flowAlpha(dt)
	l.flowMu.Lock()
	defer l.flowMu.Unlock()
	for k, f := range l.flows {
		if now.Sub(f.last) > flowRecent {
			delete(l.flows, k)
			continue
		}
		open++
		if f.bytes != f.prev && dt > 0 {
			rate := float64(f.bytes-f.prev) / dt.Seconds()
			f.ewma += alpha * (rate - f.ewma)
			f.prev = f.bytes
		} else if dt > 0 {
			f.ewma -= alpha * f.ewma
		}
		if f.ewma >= flowingRate {
			flowing++
		}
	}
	return flowing, open
}

// autoscale runs one autopilot tick and moves the pool toward its decision.
// Direct only; the reverse edge uses decide + publish (runAcceptLoop).
func (p *dgPool) autoscale(ctx context.Context) {
	s := p.sampleHealth()
	T := p.decide(s)
	p.reconcile(ctx, T)
	p.scoutIfSilent(ctx)
	p.publishStats(s)
}

// scoutIfSilent (dialing side, pool goroutine): when every carrier has heard
// nothing for dgSilentDead — the other server restarted and forgot them, or
// the path is down — one new carrier is dialed, every dgScoutEvery at most.
// If it comes up, add() drops the silent ones and the pool refills at once
// (they used to hold the pool, black-holing every flow, until their 15 s
// timeout); if it does not, nothing else is spent.
func (p *dgPool) scoutIfSilent(ctx context.Context) {
	if p.dialer == nil || p.dialing.Load() > 0 {
		return
	}
	now := time.Now()
	p.mu.RLock()
	n, silent := 0, 0
	for _, l := range p.set {
		if l.alive() {
			n++
			if l.silent(now) {
				silent++
			}
		}
	}
	p.mu.RUnlock()
	if n == 0 || silent < n {
		p.scoutLogged = false
		return
	}
	if now.Before(p.nextScout) {
		return
	}
	p.nextScout = now.Add(dgScoutEvery)
	if !p.scoutLogged {
		p.scoutLogged = true
		p.log("dg: all %d carrier(s) silent for %s+ — dialing a scout carrier to see whether the other server is back", n, fmtDur(dgSilentDead))
	}
	g := p.gate
	if g == nil {
		g = linkGate
	}
	p.dialing.Add(1)
	go func() {
		defer p.dialing.Add(-1)
		release, ok := g.acquire(ctx)
		if !ok {
			return
		}
		defer release()
		if car, err := p.dialer.Dial(ctx); err == nil {
			p.add(ctx, car, "dialed (scout)")
		}
	}()
}

// decide runs the controller and publishes the target.
func (p *dgPool) decide(s apSample) int {
	d := p.ap.decide(s)
	p.dec = d
	p.target.Store(int32(d.target))
	if d.note != "" {
		p.log("dg: %s%s", d.note, p.capNote(d.target))
	}
	return d.target
}

// capNote (display only): on the REVERSE edge a target above the ceiling the
// Kharej exit reported is clamped there by the exit — say so in the log line.
// Direct exits do not clamp. Nothing here changes the target.
func (p *dgPool) capNote(target int) string {
	if !p.accept {
		return ""
	}
	if pm := p.peerMaxNow(); pm > 0 && target > pm {
		return fmt.Sprintf(" — capped at %d by the Kharej server (its max_links), so at most %d carriers run", pm, pm)
	}
	return ""
}

// reconcile moves the pool to T serving carriers. Growing un-retires the
// busiest retiring carriers first (already up, flows stay put), then — direct
// only — dials the rest, at most dgDialBudget per tick. Shrinking marks the
// carriers that will empty soonest retiring; a retiring carrier is closed by
// drainTick once its flows have moved off. In reverse the exit dials any
// shortfall itself once it learns the target.
func (p *dgPool) reconcile(ctx context.Context, T int) {
	now := p.now()
	p.mu.Lock()
	var serving, retiring []*dgLink
	for _, l := range p.set {
		if !l.alive() {
			continue
		}
		if l.retiring {
			retiring = append(retiring, l)
		} else {
			serving = append(serving, l)
		}
	}
	need := T - len(serving)
	var toDial int
	var told []*dgLink // retired or back in service: the peer is told
	if need > 0 {
		// un-retire the busiest first
		sortByBusiest(retiring)
		for _, l := range retiring {
			if need == 0 {
				break
			}
			l.retiring = false
			l.servingSince = now
			l.setRetiring(now, false)
			told = append(told, l)
			need--
		}
		toDial = need
	} else if need < 0 && !p.revExit {
		// retire the ones that will empty soonest (fewest flows, then idlest)
		sortByEmptiest(serving, now)
		for i := 0; i < -need && i < len(serving); i++ {
			serving[i].retiring = true
			serving[i].retireSince = now
			serving[i].retireSentAt = now
			serving[i].setRetiring(now, true)
			told = append(told, serving[i])
		}
	}
	p.mu.Unlock()
	for _, l := range told {
		if l.retiring {
			p.sendOp(l, closeRetire)
		} else {
			p.sendOp(l, closeServe)
		}
	}

	if p.dialer != nil && toDial > 0 {
		toDial -= int(p.dialing.Load()) // already on their way
		budget := min(max(dgDialBudget, ceilDiv(T, 16)), 20)
		toDial = min(toDial, budget, p.max-p.count()-int(p.dialing.Load()))
		for i := 0; i < toDial; i++ {
			p.queueDial(ctx)
		}
	}
}

// dgDialFailRun: carrier dials failed in a row before the queued ones are
// dropped (one lost handshake is not a dead peer).
const dgDialFailRun = 3

// wantsDial reports whether a carrier dial still has a place: below max and
// serving below the target (the reverse exit's target is the edge's).
func (p *dgPool) wantsDial() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	serving, retiring := p.countsLocked()
	return serving+retiring < p.max && serving < p.Target()
}

// queueDial starts one carrier dial through the dial gate (direct edge /
// reverse exit).
func (p *dgPool) queueDial(ctx context.Context) {
	p.dialing.Add(1)
	epoch := p.dialEpoch.Load()
	go func() {
		defer p.dialing.Add(-1) // after add(): the count never dips
		p.dialOne(ctx, epoch)
	}()
}

// dialOne dials a carrier and adds it, after its turn at the gate; a dial
// queued before another one failed is dropped (the next tick asks again).
func (p *dgPool) dialOne(ctx context.Context, epoch uint64) {
	g := p.gate
	if g == nil {
		g = linkGate
	}
	valid := func() bool { return p.dialEpoch.Load() == epoch && p.wantsDial() }
	release, ok := g.acquireIf(ctx, valid)
	if !ok {
		return
	}
	defer release()
	if !valid() { // re-checked after the start spacing
		return
	}
	car, err := p.dialer.Dial(ctx)
	if err != nil {
		if p.failStreak.Add(1) >= dgDialFailRun || p.count() == 0 {
			p.dialEpoch.Add(1)
		}
		n := p.failN.Add(1)
		now := time.Now().UnixNano()
		if last := p.failLog.Load(); now-last >= int64(dialFailLogEvery) && p.failLog.CompareAndSwap(last, now) {
			p.failN.Add(-n)
			p.log("dg: carrier dial failed: %v (%d failed dial(s) since the last line)", err, n)
		}
		return
	}
	p.failStreak.Store(0)
	p.add(ctx, car, "dialed")
}

// drainTick reaps dead carriers and closes retiring carriers once their flows
// have moved off (empty), so shrinking never cuts a live flow.
func (p *dgPool) drainTick() {
	now := p.now()
	p.pruneSticky(now)
	p.mu.Lock()
	var closing []*dgLink
	kept := p.set[:0]
	for _, l := range p.set {
		if !l.alive() {
			continue // reaped
		}
		if l.retiring {
			l.flowMu.Lock()
			active := 0
			for _, f := range l.flows {
				if now.Sub(f.last) <= flowletGap {
					active++
				}
			}
			l.flowMu.Unlock()
			bornOK := !l.bornSpare || now.Sub(l.born) >= bornSpareGrace
			// Reverse edge: hold a retiring carrier long enough for the exit
			// to learn the lower target over pool control and stop redialing,
			// or a close/redial cycle churns the pool.
			heldOK := !p.accept || now.Sub(l.retireSince) >= dgRetireHold
			if active == 0 && bornOK && heldOK {
				closing = append(closing, l)
				continue
			}
		}
		kept = append(kept, l)
	}
	p.set = kept
	// Remind the peer of every carrier this side is still retiring (a lost
	// notice would leave it placing new flows there).
	var remind []*dgLink
	for _, l := range kept {
		if l.retiring && now.Sub(l.retireSentAt) >= dgPoolCtlEvery {
			l.retireSentAt = now
			remind = append(remind, l)
		}
	}
	p.mu.Unlock()
	for _, l := range remind {
		p.sendOp(l, closeRetire)
	}
	for _, l := range closing {
		p.retireLog.log("dg: carrier %d retired: its flows ended", l.id)
		go func(l *dgLink) { time.Sleep(closeJitter()); p.closeLink(l) }(l)
	}
}

// dgRetireHold: on the reverse edge, how long a retiring carrier is kept before
// it is closed, so the exit has time to learn the lower target and not redial it.
const dgRetireHold = 2 * dgPoolCtlEvery

// flowletGap: a retiring carrier keeps a flow until it has paused this long;
// moving a flow between packets costs nothing, moving it mid-burst reorders.
const flowletGap = 300 * time.Millisecond

func max64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}
func meanF(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}
func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// sortByBusiest orders carriers most-active first (un-retire the busy ones).
func sortByBusiest(ls []*dgLink) {
	sort.SliceStable(ls, func(i, j int) bool { return ls[i].rate10 > ls[j].rate10 })
}

// sortByEmptiest orders carriers that will empty soonest first (fewest recent
// flows, then lowest recent rate), so a shrink retires the least disruptive.
// Each carrier's flow count is taken once (its flow map is scanned under its
// own lock), not once per comparison: at 300 carriers the per-comparison scan
// held the pool lock for ~12 ms, stalling every packet's carrier pick.
func sortByEmptiest(ls []*dgLink, now time.Time) {
	type key struct {
		l    *dgLink
		recs int
	}
	ks := make([]key, len(ls))
	for i, l := range ls {
		l.flowMu.Lock()
		n := 0
		for _, f := range l.flows {
			if now.Sub(f.last) <= flowRecent {
				n++
			}
		}
		l.flowMu.Unlock()
		ks[i] = key{l, n}
	}
	sort.SliceStable(ks, func(i, j int) bool {
		if ks[i].recs != ks[j].recs {
			return ks[i].recs < ks[j].recs
		}
		return ks[i].l.rate10 < ks[j].l.rate10
	})
	for i := range ks {
		ls[i] = ks[i].l
	}
}

// publishStats stores a snapshot for the live monitor.
func (p *dgPool) publishStats(s apSample) {
	serving, retiring := 0, 0
	p.mu.RLock()
	serving, retiring = p.countsLocked()
	p.mu.RUnlock()
	ps := PoolStats{
		Links: serving + retiring, Serving: serving, Retiring: retiring,
		Target: p.Target(), Min: p.min, Max: p.max,
		Users: s.open, Flowing: s.flowing, Phase: p.dec.phase.String(),
		Reason: p.dec.reason, MbitPerS: mbitps(s.G), PeerMax: p.peerMaxNow(),
		Counted: true, // flows and throughput this side's carriers see
	}
	p.gHist = append(p.gHist, s.G)
	if len(p.gHist) > peakTicks {
		p.gHist = p.gHist[len(p.gHist)-peakTicks:]
	}
	ps.PeakMbit = mbitps(max(peakOf(p.gHist), s.G))
	if p.phaseLabel != "" {
		ps.Phase = p.phaseLabel
	}
	p.carrierStats(&ps)
	p.stats.Store(&ps)
}

// carrierStats fills the datagram fields of the snapshot from the live
// carriers and the governor, and logs FEC reaching / leaving its ceiling.
func (p *dgPool) carrierStats(ps *PoolStats) {
	type statser interface{ Stats() udpcarrier.Stats }
	ps.Datagram = true
	var parSum float64
	active := 0
	p.mu.RLock()
	for _, l := range p.set {
		if !l.alive() {
			continue
		}
		c, ok := l.car.(statser)
		if !ok {
			continue
		}
		st := c.Stats()
		ps.FECRecovered += st.Dec.Recovered
		ps.FECLost += st.Dec.Lost
		ps.PacerDropped += st.PacerDropped
		ps.RxDropped += st.RxDropped
		if l.rate < 1000 { // idle carriers say nothing about the path
			continue
		}
		active++
		parSum += st.ParityRatio
		if st.FECAtCeiling {
			ps.FECAtCeiling++
		}
		if lp := float64(st.LossPPM) / 1e4; lp > ps.MaxLossPct {
			ps.MaxLossPct = lp
		}
	}
	p.mu.RUnlock()
	if active > 0 {
		ps.ParityPct = round1f(parSum / float64(active) * 100)
	}
	ps.MaxLossPct = round1f(ps.MaxLossPct)
	if _, loss := p.gov.Last(); loss > 0 {
		ps.LossPct = round1f(loss * 100)
	}
	ps.TunDrops = p.drops.Load()
	ps.TunRead, ps.TunWritten = p.tunRead.Load(), p.tunWritten.Load()
	ps.SentPkts, ps.RecvPkts = p.sentPkts.Load(), p.recvPkts.Load()
	ps.DropNoCarrier, ps.DropQueueFull, ps.DropAged = p.dropNoCarrier.Load(), p.dropQueueFull.Load(), p.dropAged.Load()
	ps.Carriers = p.carrierLine()
	p.mu.RLock()
	for _, l := range p.set {
		if l.alive() && l.ro != nil {
			st := l.ro.Stats()
			ps.ReorderHeld += st.Held
			ps.ReorderFilled += st.Filled
			ps.ReorderTimedOut += st.TimedOut
		}
	}
	p.mu.RUnlock()
	ps.Policed, ps.PoliceConfirm = p.gov.Capped(), p.gov.Confirmed()
	ps.PoliceCapMbit = round1f(mbitps(p.gov.CapBytes()))
	// Hysteresis: at the ceiling for 2 samples in a row to say so, clear of it
	// for 5 to take it back — a bursty path would otherwise flap the log.
	if ps.FECAtCeiling > 0 {
		p.fecCeilRun = max(p.fecCeilRun, 0) + 1
	} else {
		p.fecCeilRun = min(p.fecCeilRun, 0) - 1
	}
	switch {
	case p.fecCeilRun >= 2 && !p.fecCeilLogged:
		p.fecCeilLogged = true
		p.log("dg: FEC at its ceiling on %d of %d carriers (parity %.0f%% of data) — loss %.1f%% (worst carrier %.1f%%) is more than it is sized to repair", ps.FECAtCeiling, active, ps.ParityPct, ps.LossPct, ps.MaxLossPct)
	case p.fecCeilRun <= -5 && p.fecCeilLogged:
		p.fecCeilLogged = false
		p.log("dg: FEC below its ceiling again (parity %.0f%% of data)", ps.ParityPct)
	}
}

func round1f(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }

// carrierLine is one compact line per live carrier for the status file:
// id:state:sent/loss% rate/btlBw(Mbit) flags — enough to see which carrier
// loses AND whether one is pinned at a low rate while the path is healthy (the
// after-idle ramp-stall signature). Flags: P=pushing (offered its allowance),
// S=startup (still ramping). A carrier stuck at a low rate with P set and loss
// ~0 is the sender throttling itself, not the path.
func (p *dgPool) carrierLine() string {
	type statser interface{ Stats() udpcarrier.Stats }
	p.mu.RLock()
	defer p.mu.RUnlock()
	// Above carrierLineMax carriers the line lists the counts and only the
	// carriers with the most loss (a 300-carrier line would be ~15 KB every
	// 2 s and unreadable).
	set := make([]*dgLink, 0, len(p.set))
	for _, l := range p.set {
		if l.alive() {
			set = append(set, l)
		}
	}
	// One Stats() per carrier (it takes the carrier's locks), not one per
	// comparison while sorting.
	type snap struct {
		l  *dgLink
		s  udpcarrier.Stats
		ok bool
	}
	snaps := make([]snap, len(set))
	for i, l := range set {
		snaps[i].l = l
		if c, ok := l.car.(statser); ok {
			snaps[i].s, snaps[i].ok = c.Stats(), true
		}
	}
	var b []byte
	if len(snaps) > carrierLineMax {
		serving, pushing, startup := 0, 0, 0
		for _, x := range snaps {
			if !x.l.retiring {
				serving++
			}
			if x.s.Pushing {
				pushing++
			}
			if x.s.Startup {
				startup++
			}
		}
		sort.SliceStable(snaps, func(i, j int) bool { return snaps[i].s.LossPPM > snaps[j].s.LossPPM })
		b = fmt.Appendf(b, "%d carriers (%d serving, %d retiring; P=%d S=%d); most loss:", len(snaps), serving, len(snaps)-serving, pushing, startup)
		snaps = snaps[:carrierLineWorst]
	}
	for _, x := range snaps {
		l := x.l
		st := "serving"
		if l.retiring {
			st = "retiring"
		}
		if len(b) > 0 {
			b = append(b, ' ')
		}
		if x.ok {
			s := x.s
			flags := ""
			if s.Pushing {
				flags += "P"
			}
			if s.Startup {
				flags += "S"
			}
			if flags == "" {
				flags = "-"
			}
			b = append(b, []byte(fmt.Sprintf("%d:%s:%d/%.1f%% r%.1f/bw%.1f %s",
				l.id, st, l.sentPkts.Load(), float64(s.LossPPM)/1e4,
				mbitps(s.RateBytes), mbitps(s.BtlBwBytes), flags))...)
		} else {
			b = append(b, []byte(fmt.Sprintf("%d:%s:%d", l.id, st, l.sentPkts.Load()))...)
		}
	}
	return string(b)
}

// carrierLineMax / carrierLineWorst: see carrierLine.
const (
	carrierLineMax   = 32
	carrierLineWorst = 10
)

// Stats returns the last published snapshot.
func (p *dgPool) Stats() PoolStats {
	if ps := p.stats.Load(); ps != nil {
		return *ps
	}
	return PoolStats{Min: p.min, Max: p.max, Target: p.Target(), Phase: "starting", PeerMax: p.peerMaxNow()}
}

// --- pool control (reverse) -------------------------------------------------

// onPoolCtl handles a TypePoolCtl frame received on a carrier. On the reverse
// EXIT it is the edge's desired serving count; the exit clamps it to its own
// [min,max] and reconciles. (The edge never receives it.)
func (p *dgPool) onPoolCtl(l *dgLink, payload []byte) {
	// [target u16][edge's ceiling u16]: the ceiling (display only) is taken on
	// any exit, direct or reverse, before the target guard below — a direct
	// exit has no dialer and ignores the target, as it always did.
	if p.downSender && len(payload) >= 4 {
		p.storePeerMax(binary.BigEndian.Uint16(payload[2:]))
	}
	if len(payload) < 2 || p.accept || p.dialer == nil {
		return
	}
	want := int(binary.BigEndian.Uint16(payload))
	if want < p.min {
		want = p.min
	}
	if want > p.max {
		want = p.max
	}
	if int(p.target.Load()) != want {
		p.target.Store(int32(want))
		p.log("dg: exit target %d carriers (edge asked)", want)
	}
}

// onLinkStats handles a TypeLinkStats frame: the download sender (exit) reports
// how many of its serving carriers are pressed on the download path, and how
// many are serving. The edge (the sizer) stores it to fold into sampleHealth.
// (The exit never acts on one; it only produces them.)
func (p *dgPool) onLinkStats(payload []byte) {
	if p.downSender || len(payload) < 4 {
		return
	}
	p.dnPressed.Store(uint32(binary.BigEndian.Uint16(payload[0:])))
	p.dnServing.Store(uint32(binary.BigEndian.Uint16(payload[2:])))
	p.dnStatsAt.Store(p.now().UnixNano())
	if len(payload) >= 6 { // [pressed][serving][exit's ceiling]: display only
		p.storePeerMax(binary.BigEndian.Uint16(payload[4:]))
	}
}

// storePeerMax records the other server's ceiling and when it arrived.
func (p *dgPool) storePeerMax(v uint16) {
	p.peerMax.Store(uint32(v))
	p.peerMaxAt.Store(p.now().UnixNano())
}

// peerMaxNow is the other server's ceiling, or 0 when no report is fresh.
func (p *dgPool) peerMaxNow() int {
	at := p.peerMaxAt.Load()
	if at == 0 || p.now().Sub(time.Unix(0, at)) > dgPeerMaxStale {
		return 0
	}
	return int(p.peerMax.Load())
}

// ceilingU16 is this pool's max as a u16 for the control frames.
func (p *dgPool) ceilingU16() uint16 {
	if p.max > 0xffff {
		return 0xffff
	}
	if p.max < 0 {
		return 0
	}
	return uint16(p.max)
}

// publishDownStats (the EXIT, direct or reverse) reports the download
// direction's send-side pressure to the edge over one carrier, so the edge —
// which only sees its own upload pressure — can size the shared pool for the
// download too. It is a no-op on the edge. A pre-TypeLinkStats peer drops the
// frame unread, so this is always safe to send.
func (p *dgPool) publishDownStats(s apSample) {
	if !p.downSender {
		return
	}
	serving, pressed := 0, 0
	for _, l := range s.links {
		if !l.serving {
			continue
		}
		serving++
		if l.pressed {
			pressed++
		}
	}
	l := p.poolCtlCarrier()
	if l == nil {
		return
	}
	var b [6]byte
	binary.BigEndian.PutUint16(b[0:], uint16(min(pressed, 0xffff)))
	binary.BigEndian.PutUint16(b[2:], uint16(min(serving, 0xffff)))
	binary.BigEndian.PutUint16(b[4:], p.ceilingU16()) // display only; older edges read [0:4]
	p.sendVia(l, core.TypeLinkStats, b[:])
}

// poolCtlPayload is a TypePoolCtl frame: [target u16][this edge's ceiling u16].
// An older exit reads only the first two bytes.
func (p *dgPool) poolCtlPayload(target int) []byte {
	var b [4]byte
	binary.BigEndian.PutUint16(b[0:], uint16(min(target, 0xffff)))
	binary.BigEndian.PutUint16(b[2:], p.ceilingU16())
	return b[:]
}

// publishInfo (DIRECT edge) tells the exit this edge's ceiling, for display:
// soon after the first carrier is up, then every ~dgInfoEvery (jittered). It
// reuses TypePoolCtl, which a direct exit has always ignored as a target (it
// has no dialer), so an older exit drops it harmlessly and the carrier needs
// no new frame type.
func (p *dgPool) publishInfo(ctx context.Context) {
	wait := time.Second
	for {
		if !sleepCtx(ctx, wait) {
			return
		}
		l := p.poolCtlCarrier()
		if l == nil {
			wait = time.Second // no carrier yet: look again soon
			continue
		}
		p.sendVia(l, core.TypePoolCtl, p.poolCtlPayload(p.Target()))
		wait = time.Duration(float64(dgInfoEvery) * (0.8 + 0.4*rand.Float64()))
	}
}

// publishTarget (reverse EDGE) periodically sends the autopilot's serving
// target to the exit over one carrier, and on every change, until ctx ends.
func (p *dgPool) publishTarget(ctx context.Context) {
	t := time.NewTicker(dgPoolCtlEvery)
	defer t.Stop()
	last := -1
	send := func() {
		T := p.Target()
		l := p.poolCtlCarrier()
		if l == nil {
			last = -1
			return
		}
		p.sendVia(l, core.TypePoolCtl, p.poolCtlPayload(T))
		last = T
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			send()
		default:
			if p.Target() != last {
				send()
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
}

// poolCtlCarrier returns a live carrier to carry pool-control, preferring the
// current one so the target rides a stable path — unless it has gone silent
// (a dead carrier the peer forgot), when one that hears the peer is used.
func (p *dgPool) poolCtlCarrier() *dgLink {
	p.poolCtlMu.Lock()
	defer p.poolCtlMu.Unlock()
	now := time.Now()
	if p.poolCtl != nil && p.poolCtl.alive() && !p.poolCtl.silent(now) {
		return p.poolCtl
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	var any *dgLink
	for _, l := range p.set {
		if !l.alive() {
			continue
		}
		if !l.silent(now) {
			p.poolCtl = l
			return l
		}
		if any == nil {
			any = l
		}
	}
	if any != nil {
		p.poolCtl = any
	}
	return any
}

// --- run entry points -------------------------------------------------------

// DgConfig configures one side of the datagram tun pool.
type DgConfig struct {
	Dev      tunWriter
	Min, Max int
	PerLink  int
	Reverse  bool
	Log      func(string, ...any)
	OnStart  func(StatsFn) // publish the live snapshot for monitoring

	// Direct edge / reverse exit: dials carriers.
	Dialer DgDialer
	// Reverse edge / direct exit: accepts carriers the peer dials in.
	Listener DgListener

	// WarmLinks (edge, > 0): come up at this many carriers instead of
	// warmStartLinks — the target before a restart (see IranConfig.WarmLinks).
	WarmLinks int
}

// warmCount is how many carriers the edge comes up at (and the autopilot's
// starting target).
func (p *dgPool) warmCount(cfg DgConfig) int {
	if cfg.WarmLinks > 0 {
		return min(max(cfg.WarmLinks, p.min), p.max)
	}
	return warmSize(p.min, p.max)
}

// RunDgEdge runs the EDGE (iran) side of the datagram tun pool: the side that
// sizes the pattern from the traffic. Direct: it dials the pool and the
// autopilot moves it. Reverse: it accepts carriers the exit dials and publishes
// the target to the exit, which dials to match.
func RunDgEdge(ctx context.Context, cfg DgConfig) error {
	logf := cfg.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	p := newDgPool(cfg.Dev, cfg.Min, cfg.Max, cfg.PerLink, logf)
	if !cfg.Reverse {
		p.dialer = cfg.Dialer
	} else {
		p.accept = true
	}
	if w := p.warmCount(cfg); w != warmSize(p.min, p.max) {
		p.ap.T = w
		p.target.Store(int32(w))
	}
	pp := p
	poolProbe.Store(&pp)
	if cfg.Listener != nil {
		// Closed HERE, before this returns: closing releases what the listener
		// holds in the kernel (the icmp reply rule), and once Run returns the
		// process may exit at once — a close left to a goroutine never ran.
		defer cfg.Listener.Close()
	}
	if cfg.OnStart != nil {
		cfg.OnStart(p.Stats)
	}
	go p.pumpTun(ctx)
	go p.reapLoop(ctx)
	go p.gov.Run(ctx)
	if cfg.Reverse {
		go p.acceptLoop(ctx, cfg.Listener)
		go p.publishTarget(ctx)
	} else {
		go p.publishInfo(ctx) // direct: the exit learns our ceiling (display only)
	}
	// Initial fill (direct): bring the pool up warm; the dial gate spaces the
	// handshakes.
	if !cfg.Reverse {
		for i := p.warmCount(cfg); i > 0; i-- {
			p.queueDial(ctx)
		}
	}
	p.runLoop(ctx, !cfg.Reverse)
	return nil
}

// RunDgExit runs the EXIT (kharej) side. Direct: it accepts carriers the edge
// dials and pumps them under the TUN (no sizing — the edge sizes). Reverse: it
// dials a pool whose count the edge drives over pool control.
func RunDgExit(ctx context.Context, cfg DgConfig) error {
	logf := cfg.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	p := newDgPool(cfg.Dev, cfg.Min, cfg.Max, cfg.PerLink, logf)
	p.downSender = true // the exit sends the downloads; it reports that pressure
	if cfg.Listener != nil {
		defer cfg.Listener.Close() // before returning: see RunDgEdge
	}
	if cfg.OnStart != nil {
		cfg.OnStart(p.Stats)
	}
	go p.pumpTun(ctx)
	go p.reapLoop(ctx)
	go p.gov.Run(ctx)
	if !cfg.Reverse {
		// Direct exit: accept carriers, no autopilot (the edge decides).
		if err := ctx.Err(); err != nil {
			return nil
		}
		go p.acceptLoop(ctx, cfg.Listener)
		// no sizing here, but the live snapshot (links, loss, parity, policer)
		// is this side's: it sends the downloads
		p.phaseLabel = "listening"
		t := time.NewTicker(healthTick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				p.closeAll()
				return nil
			case <-t.C:
				s := p.sampleHealth()
				p.publishDownStats(s)
				p.publishStats(s)
			}
		}
	}
	// Reverse exit: dial to match the edge's target (learned via pool control).
	p.dialer = cfg.Dialer
	p.revExit = true
	p.phaseLabel = "following" // the edge decides the size; this side dials to match
	p.target.Store(int32(warmSize(cfg.Min, cfg.Max)))
	p.runReverseExit(ctx)
	return nil
}

// runLoop is the edge's health/scale loop.
func (p *dgPool) runLoop(ctx context.Context, direct bool) {
	tick := time.NewTicker(healthTick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			p.closeAll()
			return
		case <-tick.C:
			if direct {
				p.autoscale(ctx)
			} else {
				// reverse edge: decide + publish (publishTarget sends it), and
				// retire surplus carriers the exit dialed beyond the target.
				s := p.sampleHealth()
				p.decide(s)
				p.reconcileReverseEdge()
				p.publishStats(s)
			}
			p.drainTick()
		}
	}
}

// runReverseExit keeps the exit's dial count at the edge's target.
func (p *dgPool) runReverseExit(ctx context.Context) {
	tick := time.NewTicker(healthTick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			p.closeAll()
			return
		case <-tick.C:
			// Every carrier gone (the edge restarted, or the path dropped):
			// hold the warm size until the edge speaks again on the first
			// carrier back — a restarted edge must not be handed the old
			// target as spares.
			if w := warmSize(p.min, p.max); p.count() == 0 && p.Target() > w && p.hadCarrier {
				p.target.Store(int32(w))
			}
			if p.count() > 0 {
				p.hadCarrier = true
			}
			p.reconcile(ctx, p.Target()) // dials up to target
			p.scoutIfSilent(ctx)
			p.drainTick()
			s := p.sampleHealth()
			p.publishDownStats(s) // tell the edge our download send pressure
			p.publishStats(s)
		}
	}
}

// reconcileReverseEdge moves the reverse edge's serving set to the target:
// carriers the exit dialed beyond it retire (the emptiest first), and when
// the target rises again, retiring and born-spare carriers serve again (the
// busiest first) before the exit has to dial anything — the exit only dials
// below the target, and never while those carriers are alive, so without
// this the edge stayed below its target after any shrink. The exit is told
// either way, so its new flowlets follow.
func (p *dgPool) reconcileReverseEdge() {
	now := p.now()
	T := p.Target()
	p.mu.Lock()
	var serving, retiring, told []*dgLink
	for _, l := range p.set {
		if !l.alive() {
			continue
		}
		if l.retiring {
			retiring = append(retiring, l)
		} else {
			serving = append(serving, l)
		}
	}
	switch {
	case len(serving) > T:
		sortByEmptiest(serving, now)
		for i := 0; i < len(serving)-T; i++ {
			serving[i].retiring = true
			serving[i].retireSince = now
			serving[i].retireSentAt = now
			serving[i].setRetiring(now, true)
			told = append(told, serving[i])
		}
	case len(serving) < T && len(retiring) > 0:
		sortByBusiest(retiring)
		for i := 0; i < T-len(serving) && i < len(retiring); i++ {
			l := retiring[i]
			l.retiring, l.bornSpare = false, false
			l.servingSince = now
			l.setRetiring(now, false)
			told = append(told, l)
		}
	}
	p.mu.Unlock()
	for _, l := range told {
		if l.retiring {
			p.sendOp(l, closeRetire)
		} else {
			p.sendOp(l, closeServe)
		}
	}
}

// acceptLoop accepts carriers the peer dials in and adds them to the pool.
func (p *dgPool) acceptLoop(ctx context.Context, ln DgListener) {
	go func() { <-ctx.Done(); ln.Close() }()
	for ctx.Err() == nil {
		car, err := ln.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			sleepCtx(ctx, 50*time.Millisecond)
			continue
		}
		p.add(ctx, car, "accepted")
	}
}

// reapLoop drops carriers that died so pick/sample stop seeing them promptly.
func (p *dgPool) reapLoop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.mu.Lock()
			kept := p.set[:0]
			for _, l := range p.set {
				if l.alive() {
					kept = append(kept, l)
				}
			}
			p.set = kept
			p.mu.Unlock()
		}
	}
}

func (p *dgPool) closeAll() {
	p.mu.Lock()
	set := p.set
	p.set = nil
	p.mu.Unlock()
	for _, l := range set {
		p.closeLink(l) // a stopping server tells the other one at once
	}
}
