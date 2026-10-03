package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// dgFakeCarrier is an in-memory datagram Carrier: SendFrame delivers straight to
// the paired peer's ReadFrame queue. It lets the pool logic be tested without
// sockets. It can be told it is not warm yet, and can drop.
type dgFakeCarrier struct {
	peer   *dgFakeCarrier
	in     chan frame
	closed chan struct{}
	once   sync.Once
	warm   atomic.Bool
	sent   atomic.Uint64
	rx     atomic.Int64 // LastRx, unix ns (0: cannot say)
}

// LastRx is what a test set (zero: the carrier cannot say, never silent).
func (c *dgFakeCarrier) LastRx() time.Time {
	if v := c.rx.Load(); v != 0 {
		return time.Unix(0, v)
	}
	return time.Time{}
}

type frame struct {
	ft byte
	p  []byte
}

func newDgFakePair() (*dgFakeCarrier, *dgFakeCarrier) {
	a := &dgFakeCarrier{in: make(chan frame, 4096), closed: make(chan struct{})}
	b := &dgFakeCarrier{in: make(chan frame, 4096), closed: make(chan struct{})}
	a.peer, b.peer = b, a
	a.warm.Store(true)
	b.warm.Store(true)
	return a, b
}

func (c *dgFakeCarrier) SendFrame(ft byte, p []byte) error {
	c.sent.Add(1)
	b := append([]byte(nil), p...)
	select {
	case c.peer.in <- frame{ft, b}:
		return nil
	case <-c.peer.closed:
		return errDgFakeClosed
	case <-c.closed:
		return errDgFakeClosed
	}
}

func (c *dgFakeCarrier) ReadFrame() (byte, []byte, error) {
	select {
	case f := <-c.in:
		return f.ft, f.p, nil
	case <-c.closed:
		return 0, nil, errDgFakeClosed
	}
}

func (c *dgFakeCarrier) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}
func (c *dgFakeCarrier) Warm() bool { return c.warm.Load() }

var errDgFakeClosed = context.Canceled

// dgFakeDialer hands out one side of a fresh pair each dial and delivers the
// other side to a paired listener, so a pool of edge carriers connects to a
// pool of exit carriers, all in memory.
type dgFakeDialer struct {
	accept chan Carrier
}

func newDgFakeLink() (*dgFakeDialer, *dgFakeAccepter) {
	ch := make(chan Carrier, 64)
	return &dgFakeDialer{accept: ch}, &dgFakeAccepter{accept: ch}
}

func (d *dgFakeDialer) Dial(ctx context.Context) (Carrier, error) {
	e, x := newDgFakePair()
	select {
	case d.accept <- x:
		return e, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type dgFakeAccepter struct{ accept chan Carrier }

func (a *dgFakeAccepter) Accept(ctx context.Context) (Carrier, error) {
	select {
	case c := <-a.accept:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (a *dgFakeAccepter) Close() error { return nil }

// A packet sent into the edge TUN comes out of the exit TUN unchanged, in both
// directions, over a small direct datagram pool.
func TestDgPoolRoundTrip(t *testing.T) {
	edgeTUN, exitTUN := newFakeTUN(1400), newFakeTUN(1400)
	dialer, accepter := newDgFakeLink()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go RunDgEdge(ctx, DgConfig{Dev: edgeTUN, Min: 2, Max: 8, PerLink: 8, Dialer: dialer, Log: t.Logf})
	go RunDgExit(ctx, DgConfig{Dev: exitTUN, Min: 2, Max: 8, PerLink: 8, Listener: accepter, Log: t.Logf})

	waitTunUp(t, edgeTUN, exitTUN)

	// edge -> exit
	for i := 0; i < 50; i++ {
		pkt := ipPacket(uint16(1000+i), 80, []byte("edge-to-exit"))
		edgeTUN.inject(pkt)
	}
	got := 0
	deadline := time.After(3 * time.Second)
	for got < 40 {
		select {
		case <-deadline:
			t.Fatalf("only %d/50 packets reached the exit", got)
		default:
		}
		if b := exitTUN.recv(50 * time.Millisecond); b != nil {
			got++
		}
	}
	// exit -> edge (return path)
	exitTUN.inject(ipPacket(80, 1234, []byte("exit-to-edge")))
	if b := edgeTUN.recv(2 * time.Second); b == nil {
		t.Fatal("return packet never reached the edge")
	}
}

// A flow keeps the same carrier across many packets (rendezvous), and distinct
// flows spread across carriers.
func TestDgPoolFlowPinning(t *testing.T) {
	dev := newFakeTUN(1400)
	p := newDgPool(dev, 4, 4, 8, t.Logf)
	now := time.Now()
	// four carriers, no real peer needed for placement
	for i := 0; i < 4; i++ {
		a, _ := newDgFakePair()
		l := newDgLink(a, now)
		p.set = append(p.set, l)
	}
	// one flow always lands on one carrier
	f1 := flowHash(ipPacket(5000, 443, nil))
	first := p.pick(f1, now)
	for i := 0; i < 100; i++ {
		if p.pick(f1, now) != first {
			t.Fatal("a flow moved carriers while all carriers were alive")
		}
	}
	// many flows spread over more than one carrier
	seen := map[*dgLink]bool{}
	for port := 0; port < 200; port++ {
		seen[p.pick(flowHash(ipPacket(uint16(port), 443, nil)), now)] = true
	}
	if len(seen) < 3 {
		t.Fatalf("200 flows landed on only %d/4 carriers", len(seen))
	}
	// a dead carrier is never picked
	first.markDead()
	for i := 0; i < 50; i++ {
		if p.pick(f1, now) == first {
			t.Fatal("a dead carrier was picked")
		}
	}
}

// A live flow stays on its carrier through every pool change that is not the
// carrier's death: a carrier added (rendezvous hashing alone would move ~1/n of
// the flows to it), the carrier retiring, a carrier un-retired. Each move
// reorders the flow's packets, which its TCP counts as loss (field: 40% of
// bytes retransmitted on a lossless path). Only a pause lets it be re-hashed.
func TestDgPoolStickyFlows(t *testing.T) {
	p := newDgPool(newFakeTUN(1400), 2, 8, 8, t.Logf)
	now := time.Now()
	add := func() *dgLink {
		a, _ := newDgFakePair()
		l := newDgLink(a, now)
		p.mu.Lock()
		p.set = append(p.set, l)
		p.mu.Unlock()
		return l
	}
	add()
	add()
	flows := make([]uint32, 300)
	home := map[uint32]*dgLink{}
	for i := range flows {
		flows[i] = flowHash(ipPacket(uint16(10000+i), 443, nil))
		home[flows[i]] = p.pick(flows[i], now)
	}
	check := func(what string) {
		moved := 0
		for _, f := range flows {
			if p.pick(f, now) != home[f] {
				moved++
			}
		}
		if moved != 0 {
			t.Fatalf("%s: %d of %d live flows changed carrier", what, moved, len(flows))
		}
	}
	// six more carriers: with plain rendezvous ~75% of the flows would move
	for i := 0; i < 6; i++ {
		add()
	}
	check("after adding carriers")
	// retire the carrier most flows are on
	p.mu.Lock()
	for _, l := range p.set {
		l.retiring = true
	}
	p.mu.Unlock()
	check("after retiring")
	p.mu.Lock()
	for _, l := range p.set {
		l.retiring = false
	}
	p.mu.Unlock()
	check("after un-retiring")
	// a new flow still lands on some serving carrier
	if p.pick(flowHash(ipPacket(1, 2, nil)), now) == nil {
		t.Fatal("a new flow got no carrier")
	}
	// the flow's carrier dies: the flow moves, once, and then sticks again
	victim := home[flows[0]]
	victim.markDead()
	l2 := p.pick(flows[0], now)
	if l2 == nil || l2 == victim || !l2.alive() {
		t.Fatal("a flow whose carrier died was not re-placed on a live carrier")
	}
	add()
	if p.pick(flows[0], now) != l2 {
		t.Fatal("the re-placed flow moved again when a carrier was added")
	}
	// a paused flow (idle past flowletGap) may be re-hashed; sticky forgets it
	later := now.Add(flowletGap + time.Second)
	p.pruneSticky(later)
	p.stickyMu.Lock()
	n := len(p.sticky)
	p.stickyMu.Unlock()
	if n != 0 {
		t.Fatalf("%d paused flows still pinned", n)
	}
}

// The pool grows toward the flow-count floor under sustained load and shrinks
// back toward min when the load stops — the autopilot, driven from datagram
// carriers.
func TestDgPoolScales(t *testing.T) {
	edgeTUN, exitTUN := newFakeTUN(1400), newFakeTUN(1400)
	dialer, accepter := newDgFakeLink()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunDgEdge(ctx, DgConfig{Dev: edgeTUN, Min: 2, Max: 32, PerLink: 8, Dialer: dialer, Log: func(string, ...any) {}})
	go RunDgExit(ctx, DgConfig{Dev: exitTUN, Min: 2, Max: 32, PerLink: 8, Listener: accepter, Log: func(string, ...any) {}})
	waitTunUp(t, edgeTUN, exitTUN)

	// 40 distinct flows moving data => floor ceil(40/8)=5 serving carriers.
	stop := make(chan struct{})
	go func() {
		tk := time.NewTicker(5 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				for f := 0; f < 40; f++ {
					edgeTUN.inject(ipPacket(uint16(20000+f), 443, make([]byte, 400)))
				}
			}
		}
	}()
	if !waitFor(t, 15*time.Second, func() bool { return countSetServing(ctx, edgeTUN) >= 5 }) {
		t.Fatalf("pool did not grow to the flow floor (serving=%d)", countSetServing(ctx, edgeTUN))
	}
	close(stop)
	// The shrink SCHEDULE (60s below target, then step) is the autopilot's own,
	// covered by autopilot_test.go with a fake clock. Here we only need the
	// pool to (a) feed the autopilot so it GROWS under load (asserted above)
	// and (b) act on a shrink decision — proven by TestDgReconcile.
}

// reconcile dials up to a raised target and retires down to a lowered one.
func TestDgReconcile(t *testing.T) {
	dev := newFakeTUN(1400)
	dialer, _ := newDgFakeLink()
	p := newDgPool(dev, 1, 16, 8, func(string, ...any) {})
	p.dialer = dialer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.reapLoop(ctx)
	// grow to 6 — reconcile dials at most dgDialBudget per call, like a tick,
	// so drive it until it converges.
	if !waitFor(t, 5*time.Second, func() bool {
		p.reconcile(ctx, 6)
		s, _ := p.countsLive()
		return s == 6
	}) {
		s, _ := p.countsLive()
		t.Fatalf("reconcile grow: serving=%d want 6", s)
	}
	// shrink to 2: retires 4 (they stay in the set until drained)
	p.reconcile(ctx, 2)
	if !waitFor(t, 2*time.Second, func() bool { s, _ := p.countsLive(); return s == 2 }) {
		s, r := p.countsLive()
		t.Fatalf("reconcile shrink: serving=%d retiring=%d want serving 2", s, r)
	}
	// drainTick closes the empty retiring carriers
	p.drainTick()
	if !waitFor(t, 3*time.Second, func() bool { return p.count() == 2 }) {
		t.Fatalf("retiring carriers were not drained: total=%d", p.count())
	}
}

// waitFor polls cond until true or the deadline.
func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.After(d)
	for {
		if cond() {
			return true
		}
		select {
		case <-deadline:
			return false
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// countSetServing reaches into the edge pool via a shared registry so the test
// can observe the serving count. Set by RunDgEdge in tests through poolProbe.
func countSetServing(ctx context.Context, dev *fakeTUN) int {
	if p := poolProbe.Load(); p != nil {
		s, _ := (*p).countsLive()
		return s
	}
	return 0
}

// A reverse pool: the exit dials, the edge accepts and drives the count. A
// packet crosses both ways.
func TestDgPoolReverse(t *testing.T) {
	edgeTUN, exitTUN := newFakeTUN(1400), newFakeTUN(1400)
	// reverse: exit dials -> edge accepts
	dialer, accepter := newDgFakeLink()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunDgEdge(ctx, DgConfig{Dev: edgeTUN, Min: 2, Max: 8, PerLink: 8, Reverse: true, Listener: accepter, Log: func(string, ...any) {}})
	go RunDgExit(ctx, DgConfig{Dev: exitTUN, Min: 2, Max: 8, PerLink: 8, Reverse: true, Dialer: dialer, Log: func(string, ...any) {}})
	waitTunUp(t, edgeTUN, exitTUN)
	exitTUN.inject(ipPacket(80, 5555, []byte("reverse-return")))
	if edgeTUN.recv(2*time.Second) == nil {
		t.Fatal("reverse: exit->edge packet lost")
	}
	edgeTUN.inject(ipPacket(5555, 80, []byte("reverse-forward")))
	if exitTUN.recv(2*time.Second) == nil {
		t.Fatal("reverse: edge->exit packet lost")
	}
}

// slowDgDialer counts dials and takes a while for each, like a real handshake.
type slowDgDialer struct {
	inner *dgFakeDialer
	n     atomic.Int32
	d     time.Duration
}

func (s *slowDgDialer) Dial(ctx context.Context) (Carrier, error) {
	s.n.Add(1)
	time.Sleep(s.d)
	return s.inner.Dial(ctx)
}

// Dials still in progress count toward the target: ticks that come while
// they are on their way do not dial the same carriers again.
func TestDgDialsInFlightCount(t *testing.T) {
	dev := newFakeTUN(1400)
	inner, acc := newDgFakeLink()
	go func() { // drain the fake exit side
		for {
			if _, err := acc.Accept(context.Background()); err != nil {
				return
			}
		}
	}()
	d := &slowDgDialer{inner: inner, d: 300 * time.Millisecond}
	p := newDgPool(dev, 1, 64, 8, func(string, ...any) {})
	p.dialer = d
	p.gate = instantGate()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < 5; i++ { // five ticks while the first dials are on their way
		p.reconcile(ctx, 10)
	}
	if !waitFor(t, 5*time.Second, func() bool { s, _ := p.countsLive(); return s == 10 }) {
		s, _ := p.countsLive()
		t.Fatalf("serving=%d, want 10", s)
	}
	time.Sleep(400 * time.Millisecond)
	if n := d.n.Load(); n != 10 {
		t.Fatalf("%d dials for a target of 10", n)
	}
}

// In reverse the exit never retires carriers on its own: the edge picks
// which ones retire and closes them; the exit only stops redialing.
func TestDgReverseExitLeavesRetiringToEdge(t *testing.T) {
	dev := newFakeTUN(1400)
	dialer, acc := newDgFakeLink()
	go func() {
		for {
			if _, err := acc.Accept(context.Background()); err != nil {
				return
			}
		}
	}()
	p := newDgPool(dev, 1, 16, 8, func(string, ...any) {})
	p.dialer = dialer
	p.revExit = true
	p.gate = instantGate()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !waitFor(t, 5*time.Second, func() bool { p.reconcile(ctx, 6); s, _ := p.countsLive(); return s == 6 }) {
		t.Fatal("did not reach 6")
	}
	p.reconcile(ctx, 2)
	if s, r := p.countsLive(); s != 6 || r != 0 {
		t.Fatalf("reverse exit retired on its own: serving=%d retiring=%d", s, r)
	}
}
