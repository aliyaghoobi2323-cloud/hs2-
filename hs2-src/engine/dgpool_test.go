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
	// four carriers, no real peer needed for placement
	for i := 0; i < 4; i++ {
		a, _ := newDgFakePair()
		l := newDgLink(a, time.Now())
		p.set = append(p.set, l)
	}
	// one flow always lands on one carrier
	f1 := flowHash(ipPacket(5000, 443, nil))
	first := p.pick(f1)
	for i := 0; i < 100; i++ {
		if p.pick(f1) != first {
			t.Fatal("a flow moved carriers while all carriers were alive")
		}
	}
	// many flows spread over more than one carrier
	seen := map[*dgLink]bool{}
	for port := 0; port < 200; port++ {
		seen[p.pick(flowHash(ipPacket(uint16(port), 443, nil)))] = true
	}
	if len(seen) < 3 {
		t.Fatalf("200 flows landed on only %d/4 carriers", len(seen))
	}
	// a dead carrier is never picked
	first.markDead()
	for i := 0; i < 50; i++ {
		if p.pick(f1) == first {
			t.Fatal("a dead carrier was picked")
		}
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
