package engine

import (
	"math"
	"testing"
	"time"
)

type lbFakeLink struct{ id int }

// addLinks places n links through the balancer and returns them by path.
func addLinks(b *LB, n int) map[int][]*lbFakeLink {
	out := map[int][]*lbFakeLink{}
	for k := 0; k < n; k++ {
		i, probe := b.acquire()
		l := &lbFakeLink{id: k}
		b.result(i, probe, l, nil)
		out[i] = append(out[i], l)
	}
	return out
}

// A lossy path's factor falls and a clean one's rises, at most 5 share points
// per step, until the clamp (75/25 from 50/50); equal paths never move.
func TestLBAutoAdapt(t *testing.T) {
	b, _ := NewLB(twoPaths(50, 50), nil)
	b.SetAuto(true)
	b.qual[0] = lbQual{loss: 0.01, have: true}
	b.qual[1] = lbQual{loss: 0.08, have: true}
	prev := lbShares(b.paths, b.mult)
	for step := 0; step < 10; step++ {
		b.adapt()
		sh := lbShares(b.paths, b.mult)
		if d := math.Abs(sh[0] - prev[0]); d > lbMaxStepPct+1e-6 {
			t.Fatalf("step %d moved %.2f points", step, d)
		}
		prev = sh
	}
	// It stops within the hysteresis of the clamp (1.5/0.5 → 75/25).
	if prev[0] < 70 || prev[0] > 75.01 || b.mult[0] > lbMultMax || b.mult[1] < lbMultMin {
		t.Fatalf("settled at %.1f/%.1f (mult %v), want 70–75/25–30", prev[0], prev[1], b.mult)
	}
	st := b.Stats()
	if st[0].EffPct < 70 || st[1].LossPct != 8 {
		t.Fatalf("stats %+v", st)
	}

	eq, _ := NewLB(twoPaths(50, 50), nil)
	eq.SetAuto(true)
	eq.qual[0] = lbQual{loss: 0.03, queueMs: 10, have: true}
	eq.qual[1] = lbQual{loss: 0.035, queueMs: 12, have: true} // within hysteresis
	for step := 0; step < 10; step++ {
		eq.adapt()
	}
	if eq.mult[0] != 1 || eq.mult[1] != 1 {
		t.Fatalf("equal paths moved: %v", eq.mult)
	}
}

// Placement follows the effective weights; a live path keeps two links.
func TestLBAutoPlacement(t *testing.T) {
	b, _ := NewLB(twoPaths(50, 50), nil)
	b.SetAuto(true)
	b.mult = []float64{1.5, 0.5}
	byPath := addLinks(b, 20)
	if len(byPath[0]) != 15 || len(byPath[1]) != 5 {
		t.Fatalf("placed %d/%d, want 15/5", len(byPath[0]), len(byPath[1]))
	}
	c, _ := NewLB(twoPaths(90, 10), nil)
	c.SetAuto(true)
	c.mult = []float64{1.5, 0.5}
	byPath = addLinks(c, 6)
	if len(byPath[1]) != 2 {
		t.Fatalf("weak path got %d of 6 links, want the minimum 2", len(byPath[1]))
	}
}

// The rebalance retires one idle link of an over-share path, never a busy one,
// and does nothing when the split is already within a link of the shares.
func TestLBRebalanceIdleOnly(t *testing.T) {
	b, _ := NewLB(twoPaths(50, 50), nil)
	// path 1 cooled: every link lands on path 0
	b.until[1] = b.now().Add(lbCooldown)
	byPath := addLinks(b, 8)
	if len(byPath[0]) != 8 {
		t.Fatalf("setup: %d on path 0", len(byPath[0]))
	}
	b.until[1] = time.Time{} // back (as after its probe link came up)
	busy := map[*lbFakeLink]bool{}
	closed := map[*lbFakeLink]bool{}
	for k, l := range byPath[0] {
		l := l
		busy[l] = k < 7 // only the last one is idle
		b.attach(l, lbHooks{retire: func() bool {
			if busy[l] {
				return false
			}
			closed[l] = true
			b.release(l)
			return true
		}})
	}
	if !b.rebalance() {
		t.Fatal("no rebalance with 8/0 against 50/50")
	}
	if len(closed) != 1 || !closed[byPath[0][7]] {
		t.Fatalf("closed %v, want only the idle link", closed)
	}
	if b.rebalance() {
		t.Fatal("rebalanced again with every remaining link busy")
	}
	// The replacement goes to the path under its share.
	if i, _ := b.acquire(); i != 1 {
		t.Fatalf("replacement on path %d, want 1", i)
	}

	ok, _ := NewLB(twoPaths(50, 50), nil)
	addLinks(ok, 5) // 3/2: within one link
	if ok.rebalance() {
		t.Fatal("rebalanced a split already within one link")
	}
}

func TestLBUpdateQual(t *testing.T) {
	q := lbUpdateQual(lbQual{}, 100, 50, nil, 0, 0)
	if q.have {
		t.Fatal("reading from too few segments")
	}
	q = lbUpdateQual(q, 1000, 20, []float64{5, 40, 10}, 4, 1)
	if !q.have || q.loss != 0.02 || q.queueMs != 10 || q.fail != 0.25 {
		t.Fatalf("%+v", q)
	}
	q = lbUpdateQual(q, 1000, 60, nil, 0, 0)
	if math.Abs(q.loss-0.04) > 1e-9 {
		t.Fatalf("smoothing: %+v", q)
	}
	if g1, g8 := lbGoodness(lbQual{loss: 0.01}), lbGoodness(lbQual{loss: 0.08}); g1 <= g8 {
		t.Fatalf("goodness %v %v", g1, g8)
	}
}

// A reverse link carrying only the TUN stream is retirable; a user is not.
// (l3mtcp: every link has an L3 stream, so counting it as a user kept the
// reverse rebalance from ever moving a link.)
func TestLBRetirableIgnoresL3(t *testing.T) {
	m := &linkMeter{}
	m.l3s.Add(1)
	if !lbRetirable(m) {
		t.Fatal("a link with only the TUN stream is held against the rebalance")
	}
	m.users.Add(1)
	if lbRetirable(m) {
		t.Fatal("a link with a user connection was retirable")
	}
}
