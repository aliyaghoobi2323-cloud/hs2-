package engine

import "testing"

// HS2_MCONN_STRIPE_HEADROOM: the stripe's links go over max_links only by
// the headroom and only as many as the stripe holds or wants.
func TestStripeHeadroomTarget(t *testing.T) {
	old := stripeHeadroomPct.Load()
	t.Cleanup(func() { stripeHeadroomPct.Store(old) })

	stripeHeadroomPct.Store(0) // off: clamped at max as before
	if tg, over := stripeHeadroomTarget(200, 5, 200); tg != 200 || over != 0 {
		t.Fatalf("off: got %d over %d, want 200 over 0", tg, over)
	}
	stripeHeadroomPct.Store(10)
	cases := []struct{ t, x, max, want, over int }{
		{200, 5, 200, 205, 5},   // full pool: the 5 stripe links come on top
		{200, 40, 200, 220, 20}, // at most 10 % of max over it
		{150, 5, 200, 155, 0},   // room under max: no over
		{198, 5, 200, 203, 3},   // partly over
		{300, 50, 300, 330, 30}, // the user's 300: at most 330
		{5, 3, 5, 6, 1},         // tiny pool: at least 1
	}
	for _, c := range cases {
		if tg, over := stripeHeadroomTarget(c.t, c.x, c.max); tg != c.want || over != c.over {
			t.Errorf("t=%d x=%d max=%d: got %d over %d, want %d over %d", c.t, c.x, c.max, tg, over, c.want, c.over)
		}
	}
	stripeHeadroomPct.Store(500) // bounded
	if h := stripeHeadroom(200); h != 100 {
		t.Fatalf("pct 500: headroom %d, want 100", h)
	}
}

// The exit follows the edge above its ceiling only with the key.
func TestExitPoolHeadroomBounds(t *testing.T) {
	old := stripeHeadroomPct.Load()
	t.Cleanup(func() { stripeHeadroomPct.Store(old) })
	p := &exitPool{min: 10, max: 200}
	if _, hi := p.bounds(); hi != 200 {
		t.Fatalf("off: hi %d", hi)
	}
	stripeHeadroomPct.Store(10)
	if _, hi := p.bounds(); hi != 220 {
		t.Fatalf("on: hi %d, want 220", hi)
	}
}
