package engine

import (
	"testing"
	"time"
)

// TestStripeRebuildWindow: HS2_MCONN_STRIPE_REBUILD opens a width-1 window
// when the pool falls under half of its recent peak (a mass kill), not on a
// slow decline past the peak's age, not when off, and not twice inside one
// window; the window ends after HS2_MCONN_STRIPE_REBUILD_MS.
func TestStripeRebuildWindow(t *testing.T) {
	oldOn, oldFor := stripeRebuildOn, stripeRebuildFor
	defer func() { stripeRebuildOn, stripeRebuildFor = oldOn, oldFor; stripeRebuildUntil.Store(0) }()
	stripeRebuildFor = 5 * time.Second
	t0 := time.Unix(1_700_000_000, 0)

	stripeRebuildOn = false
	stripeRebuildUntil.Store(0)
	var p poolPeak
	p.note(t0, 150)
	p.note(t0.Add(time.Second), 30)
	if c := stripeRebuildCap(t0.Add(time.Second)); c != -1 {
		t.Fatalf("off: cap %d, want -1", c)
	}

	stripeRebuildOn = true
	stripeRebuildUntil.Store(0)
	p = poolPeak{}
	p.note(t0, 150)
	for i, n := 0, 149; n >= 76; i, n = i+1, n-1 { // a kill takes links one by one
		p.note(t0.Add(time.Duration(i)*time.Millisecond), n)
	}
	if c := stripeRebuildCap(t0.Add(time.Second)); c != -1 {
		t.Fatalf("pool at 76 of 150: cap %d, want -1", c)
	}
	p.note(t0.Add(300*time.Millisecond), 74)
	hits := stripeRebuildHits.Load()
	if c := stripeRebuildCap(t0.Add(time.Second)); c != 1 {
		t.Fatalf("pool under half: cap %d, want 1", c)
	}
	p.note(t0.Add(400*time.Millisecond), 40)
	if stripeRebuildHits.Load() != hits {
		t.Fatal("a second window opened inside the first")
	}
	if c := stripeRebuildCap(t0.Add(300*time.Millisecond + 5*time.Second + time.Millisecond)); c != -1 {
		t.Fatalf("after the window: cap %d, want -1", c)
	}

	// a slow decline: the peak ages out, so the pool never sits under half of it
	stripeRebuildUntil.Store(0)
	p = poolPeak{}
	n := 150
	for i := 0; i < 120; i++ {
		p.note(t0.Add(time.Duration(i)*time.Second), n)
		n -= n / 15
	}
	if stripeRebuildUntil.Load() != 0 {
		t.Fatal("a slow decline opened a window")
	}
	// a small pool never opens one
	p = poolPeak{}
	p.note(t0, 6)
	p.note(t0.Add(time.Second), 1)
	if stripeRebuildUntil.Load() != 0 {
		t.Fatal("a pool under the minimum peak opened a window")
	}
}
