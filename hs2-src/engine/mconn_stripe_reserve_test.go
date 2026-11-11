package engine

import (
	"testing"
	"time"
)

// up56: HS2_MCONN_STRIPE_FAST_RESERVE parsing and gating.
func TestStripeFastReserveEnv(t *testing.T) {
	for _, c := range []struct {
		v    string
		want int64
	}{{"", 3}, {"0", 0}, {"2", 2}, {"4", 4}, {"9", 6}, {"-1", 3}, {"x", 3}} {
		if got := stripeFastReserveFromEnv(c.v); got != c.want {
			t.Errorf("reserve(%q) = %d, want %d", c.v, got, c.want)
		}
	}
	old, oldR := stripeFast.Load(), stripeFastReserve.Load()
	defer func() { stripeFast.Store(old); stripeFastReserve.Store(oldR) }()
	stripeFastReserve.Store(3)
	stripeFast.Store(false)
	if stripeReserveN() != 0 {
		t.Fatal("reserve without HS2_MCONN_STRIPE_FAST")
	}
	stripeFast.Store(true)
	if stripeReserveN() != 3 {
		t.Fatal("reserve with HS2_MCONN_STRIPE_FAST")
	}
}

// up56: a stripe's want raises the pool's target at once (FAST only), by the
// new links alone, never past max_links, and not at all when pinned.
func TestStripeWantRaiseBumpsTarget(t *testing.T) {
	old := stripeFast.Load()
	defer stripeFast.Store(old)
	now := time.Now()
	stripeFast.Store(true)
	lm := &LinkManager{max: 10}
	lm.target.Store(5)
	lm.stripeWantRaiseLocked(now, 3)
	if lm.stripeWant != 3 || lm.target.Load() != 8 {
		t.Fatalf("want=%d target=%d, want 3/8", lm.stripeWant, lm.target.Load())
	}
	lm.stripeWantRaiseLocked(now, 2) // less than wanted: nothing
	if lm.stripeWant != 3 || lm.target.Load() != 8 {
		t.Fatalf("lower want moved it: want=%d target=%d", lm.stripeWant, lm.target.Load())
	}
	lm.stripeWantRaiseLocked(now, 7) // +4, capped by max
	if lm.stripeWant != 7 || lm.target.Load() != 10 {
		t.Fatalf("want=%d target=%d, want 7/10", lm.stripeWant, lm.target.Load())
	}
	if !lm.stripeWantUntil.After(now) {
		t.Fatal("want not renewed")
	}
	lm.stripeReserveRefillLocked(now) // one replacement, not the chain's hint again
	if lm.stripeWant != 8 {
		t.Fatalf("refill: want=%d, want 8", lm.stripeWant)
	}

	stripeFast.Store(false)
	lm2 := &LinkManager{max: 10}
	lm2.target.Store(5)
	lm2.stripeWantRaiseLocked(now, 3)
	if lm2.stripeWant != 3 || lm2.target.Load() != 5 {
		t.Fatalf("FAST off: want=%d target=%d, want 3/5", lm2.stripeWant, lm2.target.Load())
	}
	stripeFast.Store(true)
	lm3 := &LinkManager{max: 10}
	lm3.target.Store(5)
	lm3.pin.Store(4)
	lm3.stripeWantRaiseLocked(now, 3)
	if lm3.target.Load() != 5 {
		t.Fatalf("pinned target moved: %d", lm3.target.Load())
	}
}
