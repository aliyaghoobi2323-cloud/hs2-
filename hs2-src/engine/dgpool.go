package engine

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
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
	retiring     bool // set under pool.mu
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
	dev     tunWriter
	dialer  DgDialer // direct edge / reverse exit: dials carriers; nil on the accepting side
	accept  bool     // reverse edge: carriers arrive via Add, never dialed here
	min     int
	max     int
	perLink int
	log     func(string, ...any)
	clock   func() time.Time

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

	fecCeilLogged bool   // "FEC at its ceiling" was logged and not yet cleared
	fecCeilRun    int    // consecutive samples at (+) / below (-) the ceiling
	phaseLabel    string // fixed phase for the snapshot (the direct exit: "listening")
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
	}
	p.set = append(p.set, l)
	n := len(p.set)
	p.mu.Unlock()
	go l.writeLoop(&p.pool, &p.drops, &p.dropAged, &p.sentPkts)
	go p.readLoop(ctx, l)
	if l.bornSpare {
		p.log("dg: carrier %s up (now %d) — spare: pattern needs %d serving", from, n, p.Target())
	} else {
		p.log("dg: carrier %s up (now %d)", from, n)
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
		case core.TypePing, core.TypePong, core.TypeClose:
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
		if sf.l.alive() && now.Sub(sf.last) <= flowletGap {
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
		if l.retiring {
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
	p.publishStats(s)
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
	if need > 0 {
		// un-retire the busiest first
		sortByBusiest(retiring)
		for _, l := range retiring {
			if need == 0 {
				break
			}
			l.retiring = false
			l.servingSince = now
			need--
		}
		toDial = need
	} else if need < 0 {
		// retire the ones that will empty soonest (fewest flows, then idlest)
		sortByEmptiest(serving, now)
		for i := 0; i < -need && i < len(serving); i++ {
			serving[i].retiring = true
			serving[i].retireSince = now
		}
	}
	p.mu.Unlock()

	if p.dialer != nil && toDial > 0 {
		if toDial > dgDialBudget {
			toDial = dgDialBudget
		}
		for i := 0; i < toDial; i++ {
			go p.dialOne(ctx)
		}
	}
}

// dialOne dials a carrier and adds it (direct edge / reverse exit).
func (p *dgPool) dialOne(ctx context.Context) {
	if p.count() >= p.max {
		return
	}
	car, err := p.dialer.Dial(ctx)
	if err != nil {
		p.log("dg: carrier dial failed: %v", err)
		return
	}
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
	p.mu.Unlock()
	for _, l := range closing {
		p.log("dg: carrier %d retired: its flows ended", l.id)
		go func(l *dgLink) { time.Sleep(closeJitter()); l.markDead() }(l)
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
	sortLinks(ls, func(a, b *dgLink) bool { return a.rate10 > b.rate10 })
}

// sortByEmptiest orders carriers that will empty soonest first (fewest recent
// flows, then lowest recent rate), so a shrink retires the least disruptive.
func sortByEmptiest(ls []*dgLink, now time.Time) {
	rec := func(l *dgLink) int {
		l.flowMu.Lock()
		n := 0
		for _, f := range l.flows {
			if now.Sub(f.last) <= flowRecent {
				n++
			}
		}
		l.flowMu.Unlock()
		return n
	}
	sortLinks(ls, func(a, b *dgLink) bool {
		ra, rb := rec(a), rec(b)
		if ra != rb {
			return ra < rb
		}
		return a.rate10 < b.rate10
	})
}

func sortLinks(ls []*dgLink, less func(a, b *dgLink) bool) {
	for i := 1; i < len(ls); i++ {
		for j := i; j > 0 && less(ls[j], ls[j-1]); j-- {
			ls[j], ls[j-1] = ls[j-1], ls[j]
		}
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
	}
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
	var b []byte
	for _, l := range p.set {
		if !l.alive() {
			continue
		}
		st := "serving"
		if l.retiring {
			st = "retiring"
		}
		if len(b) > 0 {
			b = append(b, ' ')
		}
		if c, ok := l.car.(statser); ok {
			s := c.Stats()
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
	binary.BigEndian.PutUint16(b[0:], uint16(pressed))
	binary.BigEndian.PutUint16(b[2:], uint16(serving))
	binary.BigEndian.PutUint16(b[4:], p.ceilingU16()) // display only; older edges read [0:4]
	p.sendVia(l, core.TypeLinkStats, b[:])
}

// poolCtlPayload is a TypePoolCtl frame: [target u16][this edge's ceiling u16].
// An older exit reads only the first two bytes.
func (p *dgPool) poolCtlPayload(target int) []byte {
	var b [4]byte
	binary.BigEndian.PutUint16(b[0:], uint16(target))
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
// current one so the target rides a stable path.
func (p *dgPool) poolCtlCarrier() *dgLink {
	p.poolCtlMu.Lock()
	defer p.poolCtlMu.Unlock()
	if p.poolCtl != nil && p.poolCtl.alive() {
		return p.poolCtl
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, l := range p.set {
		if l.alive() {
			p.poolCtl = l
			return l
		}
	}
	return nil
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
	// Initial fill (direct): bring the pool up warm, staggered.
	if !cfg.Reverse {
		warm := warmSize(cfg.Min, cfg.Max)
		for i := 0; i < warm && ctx.Err() == nil; i++ {
			if i > 0 {
				sleepCtx(ctx, jitterGap())
			}
			go p.dialOne(ctx)
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
			p.reconcile(ctx, p.Target()) // dials up to target; retires surplus
			p.drainTick()
			s := p.sampleHealth()
			p.publishDownStats(s) // tell the edge our download send pressure
			p.publishStats(s)
		}
	}
}

// reconcileReverseEdge retires carriers the exit dialed beyond the target
// (it cannot un-retire — only the exit dials), so the edge's target really
// shrinks the pool.
func (p *dgPool) reconcileReverseEdge() {
	now := p.now()
	T := p.Target()
	p.mu.Lock()
	defer p.mu.Unlock()
	var serving []*dgLink
	for _, l := range p.set {
		if l.alive() && !l.retiring {
			serving = append(serving, l)
		}
	}
	if len(serving) > T {
		sortByEmptiest(serving, now)
		for i := 0; i < len(serving)-T; i++ {
			serving[i].retiring = true
			serving[i].retireSince = now
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
		l.markDead()
	}
}
