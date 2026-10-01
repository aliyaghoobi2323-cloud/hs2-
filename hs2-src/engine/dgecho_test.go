package engine

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
)

// kindCarrier is a fake carrier that reports an encap kind, to exercise the
// icmp detection carrierEchoShaped keys off.
type kindCarrier struct {
	*dgFakeCarrier
	kind string
}

func (c kindCarrier) Encap() string { return c.kind }

// waitCount spins until the counter reaches want or the deadline passes.
func waitCount(t *testing.T, c *atomic.Uint64, want int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for c.Load() < uint64(want) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
}

// echoReadyLink builds a hand-injected link whose read path is wired like a real
// one (a reorderer delivering to the pool's TUN), so p.readLoop can run on it.
func echoReadyLink(p *dgPool, car Carrier, dial bool) *dgLink {
	l := newDgLink(car, p.now())
	l.echoShaped, l.echoDial = true, dial
	l.ro = newReorderer(dgReorderHold, func(b []byte) { p.dev.Write(b) })
	p.mu.Lock()
	p.set = append(p.set, l)
	p.mu.Unlock()
	return l
}

// Only an icmp carrier is echo-shaped; udp/gre/"" and a carrier that cannot
// report its kind (any non-udpcarrier transport, a test fake) are not, so the
// rest of the pool is untouched by the shaping.
func TestCarrierEchoShaped(t *testing.T) {
	a, _ := newDgFakePair()
	for _, tc := range []struct {
		kind string
		want bool
	}{
		{"icmp", true},
		{"ICMP", true},   // encap lower-cases the kind, so this opens ICMP too
		{" icmp ", true}, // ...and trims it
		{"udp", false}, {"", false}, {"gre", false}, {"ipip", false},
	} {
		if got := carrierEchoShaped(kindCarrier{dgFakeCarrier: a, kind: tc.kind}); got != tc.want {
			t.Fatalf("carrierEchoShaped(kind=%q) = %v, want %v", tc.kind, got, tc.want)
		}
	}
	if carrierEchoShaped(a) {
		t.Fatal("a carrier without Encap() must not be echo-shaped")
	}
}

// B5 reverse: the listen side (the reverse edge) answers each received request
// with exactly one reply. With no upload data queued, every reply is a filler
// TypePong, so N download requests draw N pong replies and frames-sent equals
// frames-received — the ~1:1 a real-ping ratio check wants — without gating the
// data path.
func TestDgEchoReverseEdgeReplyPerRequest(t *testing.T) {
	p := newDgPool(newFakeTUN(1400), 2, 8, 8, func(string, ...any) {})
	a, b := newDgFakePair()
	l := echoReadyLink(p, a, false) // reverse edge == listen side (answers requests)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.readLoop(ctx, l)

	const n = 32
	for i := 0; i < n; i++ {
		if err := b.SendFrame(core.TypeData, ipPacket(5555, 80, []byte("download"))); err != nil {
			t.Fatalf("feed request %d: %v", i, err)
		}
	}
	waitCount(t, &a.sent, n, 2*time.Second)
	if got := a.sent.Load(); got != uint64(n) {
		t.Fatalf("edge sent %d replies for %d requests, want exactly %d", got, n, n)
	}
	if rx, tx := l.rxFrames.Load(), l.txFrames.Load(); rx != tx {
		t.Fatalf("frames unbalanced: received %d, sent %d (want equal for ~1:1)", rx, tx)
	}
	for i := 0; i < n; i++ {
		ft, _, err := b.ReadFrame()
		if err != nil {
			t.Fatalf("draining reply %d: %v", i, err)
		}
		if ft != core.TypePong {
			t.Fatalf("reply %d is type %d, want filler TypePong (%d)", i, ft, core.TypePong)
		}
	}
}

// B5 direct: the dial side (the direct edge) adds one filler request per
// received download reply when its own upload is not filling the gap, so the
// heavy download-on-replies direction gets matching echo requests on the wire.
// The fillers are TypePing.
func TestDgEchoDirectEdgePullsRequests(t *testing.T) {
	p := newDgPool(newFakeTUN(1400), 2, 8, 8, func(string, ...any) {})
	a, b := newDgFakePair()
	l := echoReadyLink(p, a, true) // direct edge == dial side (sends requests)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.readLoop(ctx, l)

	const n = 24
	for i := 0; i < n; i++ {
		if err := b.SendFrame(core.TypeData, ipPacket(80, 5555, []byte("download-reply"))); err != nil {
			t.Fatalf("feed reply %d: %v", i, err)
		}
	}
	waitCount(t, &a.sent, n, 2*time.Second)
	if got := a.sent.Load(); got != uint64(n) {
		t.Fatalf("edge sent %d filler requests for %d replies, want exactly %d", got, n, n)
	}
	for i := 0; i < n; i++ {
		ft, _, err := b.ReadFrame()
		if err != nil {
			t.Fatalf("draining filler %d: %v", i, err)
		}
		if ft != core.TypePing {
			t.Fatalf("filler %d is type %d, want TypePing (%d)", i, ft, core.TypePing)
		}
	}
}

// icmpFakeDialer / icmpFakeAccepter produce in-memory carriers that report the
// icmp encap kind, so a full pool run turns the echo balancer ON at both ends —
// the case where TWO balancers face each other, which the single-link tests
// above cannot exercise.
type icmpFakeDialer struct{ accept chan Carrier }

func (d *icmpFakeDialer) Dial(ctx context.Context) (Carrier, error) {
	e, x := newDgFakePair()
	select {
	case d.accept <- kindCarrier{dgFakeCarrier: x, kind: "icmp"}:
		return kindCarrier{dgFakeCarrier: e, kind: "icmp"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type icmpFakeAccepter struct{ accept chan Carrier }

func (a *icmpFakeAccepter) Accept(ctx context.Context) (Carrier, error) {
	select {
	case c := <-a.accept:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (a *icmpFakeAccepter) Close() error { return nil }

func newICMPFakeLink() (*icmpFakeDialer, *icmpFakeAccepter) {
	ch := make(chan Carrier, 64)
	return &icmpFakeDialer{ch}, &icmpFakeAccepter{ch}
}

// With the echo balancer active on BOTH ends (icmp carriers), data must still
// flow both ways in direct AND reverse, and fillers must stay bounded: a filler
// storm would fill the carrier channels, stall the pumps and time out a recv.
// This is the end-to-end check the single-link balancer tests cannot give — two
// balancers facing each other through the real pool run loops.
func TestDgPoolICMPShapedBothModes(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		name := "direct"
		if reverse {
			name = "reverse"
		}
		t.Run(name, func(t *testing.T) {
			edgeTUN, exitTUN := newFakeTUN(1400), newFakeTUN(1400)
			dialer, accepter := newICMPFakeLink()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			logf := func(string, ...any) {}
			if reverse { // reverse: exit dials, edge accepts
				go RunDgEdge(ctx, DgConfig{Dev: edgeTUN, Min: 2, Max: 8, PerLink: 8, Reverse: true, Listener: accepter, Log: logf})
				go RunDgExit(ctx, DgConfig{Dev: exitTUN, Min: 2, Max: 8, PerLink: 8, Reverse: true, Dialer: dialer, Log: logf})
			} else { // direct: edge dials, exit accepts
				go RunDgEdge(ctx, DgConfig{Dev: edgeTUN, Min: 2, Max: 8, PerLink: 8, Dialer: dialer, Log: logf})
				go RunDgExit(ctx, DgConfig{Dev: exitTUN, Min: 2, Max: 8, PerLink: 8, Listener: accepter, Log: logf})
			}
			waitTunUp(t, edgeTUN, exitTUN)
			drain(edgeTUN)
			drain(exitTUN)
			for i := 0; i < 20; i++ {
				exitTUN.inject(ipPacket(80, 5555, []byte("down")))
				if edgeTUN.recv(2*time.Second) == nil {
					t.Fatalf("%s: exit->edge packet %d lost (balancer stall?)", name, i)
				}
				edgeTUN.inject(ipPacket(5555, 80, []byte("up")))
				if exitTUN.recv(2*time.Second) == nil {
					t.Fatalf("%s: edge->exit packet %d lost (balancer stall?)", name, i)
				}
			}
		})
	}
}

// The heavy direction (frames sent already ahead of frames received) emits no
// filler, so a filler never begets a filler across the two ends — there is no
// request/reply runaway. Upload data already sent counts toward txFrames, so an
// end that sends enough on its own stays silent.
func TestDgEchoHeavySideNoFiller(t *testing.T) {
	p := newDgPool(newFakeTUN(1400), 2, 8, 8, func(string, ...any) {})
	a, _ := newDgFakePair()
	l := newDgLink(a, p.now())
	l.echoShaped, l.echoDial = true, true
	p.set = append(p.set, l)

	l.txFrames.Store(100)
	l.rxFrames.Store(100) // caught up: nothing owed
	p.echoBalance(l)
	l.rxFrames.Store(101)
	l.txFrames.Store(150) // one more received, but sends are well ahead
	p.echoBalance(l)
	if got := a.sent.Load(); got != 0 {
		t.Fatalf("heavy side emitted %d fillers, want 0", got)
	}
}
