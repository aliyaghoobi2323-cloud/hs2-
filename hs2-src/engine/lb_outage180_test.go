package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// ou180: the outage rotation walks every path, the primary first, a
// different remote address before a different local one.
func TestLBOutageOrderInterleavesRemotes(t *testing.T) {
	paths := []LBPath{
		{Local: "k1", Remote: "i2", Weight: 10},
		{Local: "k1", Remote: "i1", Weight: 50, Primary: true},
		{Local: "k2", Remote: "i1", Weight: 10},
		{Local: "k2", Remote: "i2", Weight: 10},
		{Local: "k1", Remote: "i3", Weight: 10},
		{Local: "k1", Remote: "gone", Weight: 10, Gone: true},
	}
	lb, err := NewLB(paths, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := lb.outageOrderLocked()
	want := []int{1, 0, 4, 2, 3} // i1/k1 (primary), i2/k1, i3/k1, i1/k2, i2/k2
	if len(got) != len(want) {
		t.Fatalf("order %v, want %v", got, want)
	}
	for k := range want {
		if got[k] != want[k] {
			t.Fatalf("order %v, want %v", got, want)
		}
	}
	// Sequential failing dials with nothing up rotate over every path.
	seen := map[int]int{}
	for k := 0; k < 10; k++ {
		i, _ := lb.acquire()
		seen[i]++
		lb.result(i, false, nil, errors.New("connection refused"))
	}
	for _, j := range want {
		if seen[j] != 2 {
			t.Fatalf("10 outage dials per path %v, want 2 on each live path", seen)
		}
	}
	if seen[5] != 0 {
		t.Fatalf("a removed path was dialed: %v", seen)
	}
}

// ou180 (prod audit #1): a full outage while the primary path stays dead —
// the edge restarted and its primary IP is filtered — must not keep the
// tunnel down. The outage scout used to dial only the primary (the heaviest
// path wins pickLargestRemainder with no link anywhere); now it rotates and
// the pool is back within a few seconds through another path.
func TestLBOutageScoutFindsWorkingPath(t *testing.T) {
	paths := []LBPath{
		{Local: "10.0.0.1", Remote: "10.1.0.1", Addr: "10.1.0.1:443", Weight: 50, Primary: true},
		{Local: "10.0.0.1", Remote: "10.1.0.2", Addr: "10.1.0.2:443", Weight: 20},
		{Local: "10.0.0.2", Remote: "10.1.0.1", Addr: "10.1.0.1:443", Weight: 15},
		{Local: "10.0.0.2", Remote: "10.1.0.2", Addr: "10.1.0.2:443", Weight: 15},
	}
	lb, err := NewLB(paths, nil)
	if err != nil {
		t.Fatal(err)
	}
	sp := newScaleTestPool(t, 1, 64, time.Millisecond)
	var allDown, primDown atomic.Bool
	var tries [4]atomic.Int64
	inner := sp.exitPool.dial
	sp.exitPool.dial = func() (dialedLink, error) {
		v, err := lb.dialVia(func(p LBPath) (any, error) {
			for i, q := range paths {
				if q.Name() == p.Name() {
					tries[i].Add(1)
				}
			}
			if allDown.Load() || (p.Primary && primDown.Load()) {
				return nil, errors.New("connection refused")
			}
			return inner()
		})
		if err != nil {
			return nil, err
		}
		return v.(dialedLink), nil
	}
	serve := sp.exitPool.serve
	sp.exitPool.serve = func(ctx context.Context, c dialedLink) string {
		defer lb.release(c)
		return serve(ctx, c)
	}
	sp.setTarget(8)
	eventually(t, "8 live links", func() bool { return sp.liveCount() == 8 })

	// The edge goes down on every path; its primary IP stays filtered when it
	// comes back.
	allDown.Store(true)
	primDown.Store(true)
	sp.endAll()
	time.Sleep(2500 * time.Millisecond) // the outage settles: one scout dials
	for i := range tries {
		tries[i].Store(0)
	}
	allDown.Store(false)
	back := time.Now()
	if !waitFor(t, 8*time.Second, func() bool { return sp.liveCount() >= 1 }) {
		t.Fatalf("no link up 8s after the edge came back with only its primary path dead (dials per path %d/%d/%d/%d)",
			tries[0].Load(), tries[1].Load(), tries[2].Load(), tries[3].Load())
	}
	t.Logf("first link back %s after the edge returned", time.Since(back).Round(10*time.Millisecond))
	// The other slots resume once any path works: the pool refills (the
	// edge's target arrives on the first link back).
	sp.setTarget(8)
	if !waitFor(t, 15*time.Second, func() bool { return sp.liveCount() == 8 }) {
		t.Fatalf("pool did not refill after the outage: %d of 8", sp.liveCount())
	}
	if n := lb.Stats()[0].Links; n != 0 {
		t.Fatalf("%d links on the dead primary path", n)
	}
}
