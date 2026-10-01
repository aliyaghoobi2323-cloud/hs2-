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
