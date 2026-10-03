package engine

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// Regression tests for the failure modes the 300-link review found, each in
// the shape it was measured in.

// Review #1: after an outage that killed every link one by one, the exit's
// slots that must retire used to queue at the dial gate ahead of the few
// that really dial — at ~10 starts/s that put the first link ~17-29 s after
// the path came back. With the production-like spacing (100 ms a start) the
// first link must now be back within 3 s.
func TestRegressExitFirstLinkSoonAfterOutage(t *testing.T) {
	if testing.Short() {
		t.Skip("real time")
	}
	var gap atomic.Int64
	gap.Store(int64(time.Millisecond))
	sp := newScaleTestPool(t, 1, 300, 0)
	sp.gate = newDialGate(gateInflight, func() time.Duration { return time.Duration(gap.Load()) })
	sp.setTarget(8)
	sp.setTarget(300)
	eventually(t, "300 live links", func() bool { return sp.liveCount() == 300 })
	gap.Store(int64(100 * time.Millisecond)) // ~10 starts a second, as in production
	sp.down.Store(true)
	// The path is black-holed: links die over ~2 s, not at once.
	sp.mu.Lock()
	live := append([]*fakeCarrier(nil), sp.live...)
	sp.mu.Unlock()
	for i, c := range live {
		c.endWith("read: connection timed out")
		if i%30 == 29 {
			time.Sleep(200 * time.Millisecond)
		}
	}
	time.Sleep(3 * time.Second) // the outage goes on
	sp.down.Store(false)
	back := time.Now()
	deadline := back.Add(10 * time.Second)
	for sp.liveCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no link 10 s after the path came back")
		}
		time.Sleep(20 * time.Millisecond)
	}
	took := time.Since(back)
	t.Logf("first link %s after the path came back", took.Round(100*time.Millisecond))
	if took > 3*time.Second {
		t.Fatalf("first link only %s after the path came back (want < 3 s)", took)
	}
}

// flakyDialer fails every nth handshake (a few % reset handshakes are normal
// on Iranian paths).
type flakyDialer struct {
	n     int32
	calls atomic.Int32
	ok    atomic.Int32
}

func (d *flakyDialer) DialLink(ctx context.Context) (Link, error) {
	if c := d.calls.Add(1); c%d.n == 0 {
		return nil, fmt.Errorf("tls handshake: connection reset by peer")
	}
	d.ok.Add(1)
	return newMeteredFake(), nil
}

// Review #2: a single failed handshake used to throw away every queued dial,
// so a ramp with 1-5% failures stalled. With 1 in 20 failing the pool must
// still reach its target within a few ticks' worth of dials.
func TestRegressDirectRampWithFailedHandshakes(t *testing.T) {
	d := &flakyDialer{n: 20}
	m, _, _ := newV2Manager(d, 1, 300, false)
	ctx := context.Background()
	const T = 120
	m.target.Store(T) // as the autopilot publishes it before reconcile
	for tick := 0; tick < 60; tick++ {
		m.reconcile(ctx, T)
		settle(m)
		if S, _ := poolCounts(m); S >= T {
			t.Logf("reached %d serving in %d ticks, %d dials (%d failed)", S, tick+1, d.calls.Load(), d.calls.Load()-d.ok.Load())
			return
		}
	}
	S, _ := poolCounts(m)
	t.Fatalf("only %d of %d serving after 60 ticks (%d dials, %d failed)", S, T, d.calls.Load(), d.calls.Load()-d.ok.Load())
}

// Review #4: the reverse edge's accept cap counted links that were already
// dead, so an exit redialing them was refused in a storm (46,721 handshakes
// with an older exit). It counts live links only, up to 2×max+8.
func TestRegressAcceptCapCountsLiveLinks(t *testing.T) {
	m, _, _ := newV2Manager(nil, 1, 4, true)
	for i := 0; i < 30; i++ { // dead links the reaper has not removed yet
		f := newMeteredFake()
		f.alive.Store(false)
		addManaged(m, f)
	}
	for i := 0; i < 10; i++ {
		addManaged(m, newMeteredFake())
	}
	if n, c := m.alive(), reverseAcceptCap(m.max); n >= c {
		t.Fatalf("%d counted against a cap of %d with 10 live links", n, c)
	}
	for i := 0; i < 6; i++ {
		addManaged(m, newMeteredFake())
	}
	if n, c := m.alive(), reverseAcceptCap(m.max); n < c {
		t.Fatalf("%d live links, cap %d: the cap should be reached", n, c)
	}
}
