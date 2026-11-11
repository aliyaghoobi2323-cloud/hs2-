package engine

import (
	"testing"
	"time"
)

// TestStripeUpPathQ: a queue on one path of three (a third of the light
// links) leaves the overall median low; with HS2_MCONN_STRIPE_UP_PATHQ the
// gate reads that path's queue.
func TestStripeUpPathQ(t *testing.T) {
	mk := func() *stripeQ {
		p := &stripeQ{links: map[*linkMeter]*stripeLinkQ{}, light: true}
		for i, path := range []string{"a", "b", "c"} {
			for j := 0; j < 6; j++ {
				q := 300 * time.Microsecond
				if i == 0 {
					q = 12 * time.Millisecond // the stripe's path
				}
				p.links[&linkMeter{}] = &stripeLinkQ{path: path, active: true, q: q}
			}
		}
		// too few links on a path: not counted on its own
		p.links[&linkMeter{}] = &stripeLinkQ{path: "d", active: true, q: 50 * time.Millisecond}
		return p
	}
	old := stripeUpPathQOn.Load()
	defer stripeUpPathQOn.Store(old)

	stripeUpPathQOn.Store(false)
	p := mk()
	if q := p.pickLocked(time.Now(), nil, false); q != 300*time.Microsecond {
		t.Fatalf("off: q=%v, want the overall median 300us", q)
	}
	stripeUpPathQOn.Store(true)
	p = mk()
	if q := p.pickLocked(time.Now(), nil, false); q != 12*time.Millisecond {
		t.Fatalf("on: q=%v, want path a's 12ms", q)
	}
	// own links are left out, as for the overall rule
	p = mk()
	var own []*linkMeter
	for m, l := range p.links {
		if l.path == "a" {
			own = append(own, m)
		}
	}
	if q := p.pickLocked(time.Now(), own, false); q != 300*time.Microsecond {
		t.Fatalf("own left out: q=%v", q)
	}
}
