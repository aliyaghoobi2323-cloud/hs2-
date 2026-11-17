package engine

import (
	"os"
	"testing"
	"time"
)

// gate74's defaults: the cut on a 10 ms base at 67 % of the target
func TestStripeUpSplitDefaults(t *testing.T) {
	if os.Getenv("HS2_MCONN_STRIPE_UP_CUT_BASE_MAX") == "" && stripeUpCutBaseMax != 10*time.Millisecond {
		t.Fatalf("cut base max %v, want 10ms", stripeUpCutBaseMax)
	}
	if os.Getenv("HS2_MCONN_STRIPE_UP_CUT_PCT") == "" && stripeUpCutPct != 67 {
		t.Fatalf("cut pct %d, want 67", stripeUpCutPct)
	}
	if os.Getenv("HS2_MCONN_STRIPE_UP_GROW_BASE_MAX") == "" && stripeUpGrowBaseMax != 10*time.Millisecond {
		t.Fatalf("grow base max %v, want 10ms", stripeUpGrowBaseMax)
	}
}

// gate74: growth reads the median excess over the higher learned base;
// the cut reads the latest sample over the strict base.
func TestStripeUpSplit(t *testing.T) {
	ms := time.Millisecond
	oldG, oldC := stripeUpGrowBaseMax, stripeUpCutBaseMax
	defer func() { stripeUpGrowBaseMax, stripeUpCutBaseMax = oldG, oldC }()
	stripeUpGrowBaseMax, stripeUpCutBaseMax = 10*ms, 2*ms
	pq := &stripeQ{light: true}
	pq.p10 = 4 * ms // a healthy standing queue of 4 ms
	m := &mConn{}
	// the healthy queue (4-5 ms): growth sees ~0, the strict cut sees 2-3 ms
	g, c, b := m.stripeUpSplitLocked(pq, 5*ms)
	if b != 4*ms || g != 1*ms || c != 3*ms {
		t.Fatalf("healthy: grow %v cut %v base %v", g, c, b)
	}
	m.stripeUpSplitLocked(pq, 5*ms)
	m.stripeUpSplitLocked(pq, 5*ms) // three healthy samples
	// one spike: the cut sample follows it at once, growth's median does not
	g, c, _ = m.stripeUpSplitLocked(pq, 20*ms)
	if c != 18*ms || g != 1*ms {
		t.Fatalf("spike: grow %v cut %v", g, c)
	}
	// a held queue: growth's median follows after two samples
	g, _, _ = m.stripeUpSplitLocked(pq, 20*ms)
	if g != 16*ms {
		t.Fatalf("held: grow %v", g)
	}
	// the grow base is capped
	pq.p10 = 30 * ms
	if _, _, b = m.stripeUpSplitLocked(pq, 30*ms); b != 10*ms {
		t.Fatalf("cap: base %v", b)
	}
}

// the learned base keeps the uncapped percentile next to the capped one
func TestStripeQP10(t *testing.T) {
	ms := time.Millisecond
	p := &stripeQ{light: true}
	for i := 0; i < stripeBaseMin; i++ {
		p.noteBaseLocked(6 * ms)
	}
	if p.p10Base() != 6*ms || p.bq != min(6*ms, stripeUpBaseMax) {
		t.Fatalf("p10 %v bq %v", p.p10, p.bq)
	}
}
