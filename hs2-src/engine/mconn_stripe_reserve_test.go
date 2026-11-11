package engine

import "testing"

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
