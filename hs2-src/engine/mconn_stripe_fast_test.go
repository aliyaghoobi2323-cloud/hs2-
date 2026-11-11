package engine

import (
	"testing"
	"time"
)

// Tests of HS2_MCONN_STRIPE_FAST (mconn_stripe_fast.go).

func setStripeFast(t *testing.T, on bool) {
	t.Helper()
	old, oldRate := stripeFast.Load(), stripeFastRate.Load()
	stripeFast.Store(on)
	t.Cleanup(func() { stripeFast.Store(old); stripeFastRate.Store(oldRate) })
}

// An upload stripes after one hot sample with the key; a download still
// waits stripeHotTicks; without the key the upload waits too.
func TestStripeFastHot(t *testing.T) {
	old := stripeSubRate.Load()
	t.Cleanup(func() { stripeSubRate.Store(old) })
	stripeSubRate.Store(100)
	if stripeHotTicks.Load() < 2 {
		t.Skip("stripeHotTicks < 2")
	}
	run := func(fast, up bool) bool {
		setStripeFast(t, fast)
		m := &mConn{iran: true}
		m.stK = 8
		m.stActive = true
		now := time.Now()
		var sent, recvd uint64
		m.stripeTickLocked(now, sent, recvd) // the first sample
		now = now.Add(mLitePoll)
		if up {
			sent += 1 << 20
		} else {
			recvd += 1 << 20
		}
		return m.stripeTickLocked(now, sent, recvd)
	}
	if !run(true, true) {
		t.Fatal("fast upload: no growth after one hot sample")
	}
	if run(true, false) {
		t.Fatal("fast: a download grew after one hot sample")
	}
	if run(false, true) {
		t.Fatal("key off: an upload grew after one hot sample")
	}
}

// The plan doubles the width while every sub carries stripeFastRate, the
// first growth goes 1 → 3, and K caps it; slower subs grow by one.
func TestStripeFastPlan(t *testing.T) {
	setStripeFast(t, true)
	stripeFastRate.Store(1000)
	m := &mConn{iran: true}
	m.stK = 8
	cases := []struct {
		n    int
		rate float64
		up   bool
		want int
	}{
		{1, 5000, true, 1},  // 1 → 3
		{2, 5000, true, 1},  // 2 → 4
		{4, 5000, true, 3},  // 4 → 8
		{6, 6000, true, 1},  // 6 → 8 (K)
		{4, 3000, true, 0},  // under the per-sub rate: one at a time
		{4, 5000, false, 0}, // a download: unchanged
	}
	for _, c := range cases {
		if got := m.stripeFastPlanLocked(c.n, c.rate, c.up); got != c.want {
			t.Errorf("n=%d rate=%v up=%v: %d more, want %d", c.n, c.rate, c.up, got, c.want)
		}
	}
	setStripeFast(t, false)
	if got := m.stripeFastPlanLocked(4, 5000, true); got != 0 {
		t.Errorf("key off: %d more", got)
	}
}

// A want hint asks the pool for that many links at once and a retried pick
// does not add up; without a hint each ask adds one.
func TestStripeFastAsk(t *testing.T) {
	m := &LinkManager{}
	now := time.Now()
	m.stripeAskLocked(now, 4)
	m.stripeAskLocked(now, 4)
	if m.stripeWant != 4 {
		t.Fatalf("want %d after two hinted asks, want 4", m.stripeWant)
	}
	m.stripeAskLocked(now, 0)
	if m.stripeWant != 5 {
		t.Fatalf("want %d after a plain ask, want 5", m.stripeWant)
	}
	m.stripeAskLocked(now, 1000)
	if m.stripeWant != stripeWantCap() {
		t.Fatalf("want %d, want the cap %d", m.stripeWant, stripeWantCap())
	}
	if stripeWantOf([]any{1, stripeWantHint{n: 3}}) != 3 || stripeWantOf([]any{1}) != 0 {
		t.Fatal("stripeWantOf")
	}
}

// The retry backoff: 250-500 ms for a fast upload, 2 s otherwise; a failed
// open ends the chain.
func TestStripeFastBackoff(t *testing.T) {
	setStripeFast(t, true)
	m := &mConn{iran: true}
	now := time.Now()
	m.stGrowUp, m.stFastMore = true, 3
	d := m.stripeRetryLocked(now).Sub(now)
	if d < stripeFastBackoff || d >= stripeFastBackoff+stripeFastBackoffJit || m.stFastMore != 0 {
		t.Fatalf("fast backoff %v (chain %d)", d, m.stFastMore)
	}
	m.stGrowUp = false
	if d := m.stripeRetryLocked(now).Sub(now); d != mReopenBackoff {
		t.Fatalf("download backoff %v", d)
	}
}
