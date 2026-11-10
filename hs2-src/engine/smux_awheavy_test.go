package engine

import "testing"

// HS2_STRIPE_AW_HEAVY: "1" (strict neighbour) and "2" (normal rule) only; off by default.
func TestStripeAWHeavyFromEnv(t *testing.T) {
	for v, want := range map[string]int{"": 0, "0": 0, "1": 1, " 1 ": 1, "2": 2, "3": 0, "yes": 0} {
		if got := stripeAWHeavyFromEnv(v); got != want {
			t.Errorf("%q: %v, want %v", v, got, want)
		}
	}
	if stripeAWHeavy != 0 {
		t.Fatal("on by default")
	}
}
