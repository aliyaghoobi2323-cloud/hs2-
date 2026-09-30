package engine

import (
	"context"
	"encoding/binary"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
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
	dgDialBudget   = 4 // most carriers dialed per health tick (edge, direct)
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

	born         time.Time
	servingSince time.Time
	retiring     bool // set under pool.mu
	retireSince  time.Time
	bornSpare    bool // reverse: arrived while the pool already had its target

	// measurement (bytesUp/Down by the data paths; the rest under pool.mu in
	// the sampler)
	bytesUp   atomic.Uint64
	bytesDown atomic.Uint64
	droppedAt atomic.Int64 // unixnano of the last queue-full drop (pressure)

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
func (l *dgLink) writeLoop(pool *sync.Pool, drops *atomic.Uint64) {
	for {
		select {
		case <-l.done:
			return
		case p := <-l.q:
			if time.Since(p.t) > dgSojourn {
				drops.Add(1)
				pool.Put(p.b)
				continue
			}
			err := l.car.SendFrame(core.TypeData, *p.b)
			pool.Put(p.b)
			if err != nil {
				l.markDead()
				return
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
		ap: newAutopilot(min, max, perLink), growable: true}
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
	p.mu.Lock()
	if s, _ := p.countsLocked(); p.accept && s >= int(p.target.Load()) {
		l.retiring, l.retireSince, l.bornSpare = true, now, true
	}
	p.set = append(p.set, l)
	n := len(p.set)
	p.mu.Unlock()
	go l.writeLoop(&p.pool, &p.drops)
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
	for ctx.Err() == nil {
		ft, payload, err := l.car.ReadFrame()
		if err != nil {
			return
		}
		switch ft {
		case core.TypeData:
			now := p.now()
			l.noteFlowRecv(flowHash(payload), len(payload), now)
			p.dev.Write(payload)
		case core.TypePing:
			p.sendVia(l, core.TypePong, nil)
		case core.TypePoolCtl:
			p.onPoolCtl(l, payload)
		case core.TypePong, core.TypeClose:
		}
	}
}

// sendVia sends a control frame on a carrier (pong, pool control), off the
// data queue so it is timely.
func (p *dgPool) sendVia(l *dgLink, ft byte, payload []byte) {
	if err := l.car.SendFrame(ft, payload); err != nil {
		l.markDead()
	}
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
		flow := flowHash(*bp)
		if l := p.pick(flow); l == nil || !l.enqueue(bp, flow, p.now()) {
			p.pool.Put(bp)
			p.drops.Add(1)
		}
	}
}

// pick maps a flow to a live serving carrier by rendezvous hashing, so a flow
// keeps its carrier for as long as it lives. A retiring carrier still carries
// the flows already hashed to it (they move only when it dies or they pause),
// so retiring never reorders a live flow.
func (p *dgPool) pick(flow uint32) *dgLink {
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

	p.mu.Lock()
	defer p.mu.Unlock()
	s := apSample{now: now, growable: p.growable}
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
		l.pressed = !l.retiring && warm && dropped != 0 && now.Sub(time.Unix(0, dropped)) <= dt
		s.links = append(s.links, apLink{
			id: int(l.id), serving: l.alive() && !l.retiring, retiring: l.retiring,
			servingSince: l.servingSince, pressed: l.pressed,
			rate: l.rate, rate10: l.rate10, sustained: l.sustained,
			flowing: flowing, open: open,
		})
		s.G += l.rate
		s.flowing += flowing
		s.open += open
	}
	return s
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
		p.log("dg: %s", d.note)
	}
	return d.target
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
		Reason: p.dec.reason, MbitPerS: mbitps(s.G),
	}
	p.stats.Store(&ps)
}

// Stats returns the last published snapshot.
func (p *dgPool) Stats() PoolStats {
	if ps := p.stats.Load(); ps != nil {
		return *ps
	}
	return PoolStats{Min: p.min, Max: p.max, Target: p.Target(), Phase: "starting"}
}

// --- pool control (reverse) -------------------------------------------------

// onPoolCtl handles a TypePoolCtl frame received on a carrier. On the reverse
// EXIT it is the edge's desired serving count; the exit clamps it to its own
// [min,max] and reconciles. (The edge never receives it.)
func (p *dgPool) onPoolCtl(l *dgLink, payload []byte) {
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
		var b [2]byte
		binary.BigEndian.PutUint16(b[:], uint16(T))
		p.sendVia(l, core.TypePoolCtl, b[:])
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
	if cfg.OnStart != nil {
		cfg.OnStart(p.Stats)
	}
	go p.pumpTun(ctx)
	go p.reapLoop(ctx)
	if cfg.Reverse {
		go p.acceptLoop(ctx, cfg.Listener)
		go p.publishTarget(ctx)
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
	if cfg.OnStart != nil {
		cfg.OnStart(p.Stats)
	}
	go p.pumpTun(ctx)
	go p.reapLoop(ctx)
	if !cfg.Reverse {
		// Direct exit: accept carriers, no autopilot (the edge decides).
		if err := ctx.Err(); err != nil {
			return nil
		}
		go p.acceptLoop(ctx, cfg.Listener)
		<-ctx.Done()
		p.closeAll()
		return nil
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
			p.publishStats(p.sampleHealth())
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
