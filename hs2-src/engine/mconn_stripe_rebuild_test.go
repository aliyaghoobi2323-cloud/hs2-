package engine

import (
	"testing"
	"time"
)

// TestStripeRebuildWindow: HS2_MCONN_STRIPE_REBUILD opens a width-1 window
// when the pool falls under half of its recent peak (a mass kill), not on a
// slow decline past the peak's age, not when off, and not twice inside one
// window; the window ends after HS2_MCONN_STRIPE_REBUILD_MS. The window is
// the pool's own: another pool (a second tunnel) is not narrowed.
func TestStripeRebuildWindow(t *testing.T) {
	oldOn, oldFor := stripeRebuildOn.Load(), stripeRebuildFor
	defer func() { stripeRebuildOn.Store(oldOn); stripeRebuildFor = oldFor }()
	stripeRebuildFor = 5 * time.Second
	t0 := time.Unix(1_700_000_000, 0)

	stripeRebuildOn.Store(false)
	p := &poolPeak{}
	p.note(t0, 150)
	p.note(t0.Add(time.Second), 30)
	if c := p.rebuildCap(t0.Add(time.Second)); c != -1 {
		t.Fatalf("off: cap %d, want -1", c)
	}

	stripeRebuildOn.Store(true)
	var nilPool *poolPeak
	if c := nilPool.rebuildCap(t0); c != -1 {
		t.Fatalf("no pool: cap %d, want -1", c)
	}
	p = &poolPeak{}
	other := &poolPeak{} // a second tunnel's pool on the same server
	p.note(t0, 150)
	other.note(t0, 150)
	for i, n := 0, 149; n >= 76; i, n = i+1, n-1 { // a kill takes links one by one
		p.note(t0.Add(time.Duration(i)*time.Millisecond), n)
	}
	if c := p.rebuildCap(t0.Add(time.Second)); c != -1 {
		t.Fatalf("pool at 76 of 150: cap %d, want -1", c)
	}
	hits := stripeRebuildHits.Load()
	p.note(t0.Add(300*time.Millisecond), 74)
	if stripeRebuildHits.Load() != hits+1 {
		t.Fatal("no window counted")
	}
	if c := p.rebuildCap(t0.Add(time.Second)); c != 1 {
		t.Fatalf("pool under half: cap %d, want 1", c)
	}
	if c := other.rebuildCap(t0.Add(time.Second)); c != -1 {
		t.Fatalf("the other pool narrowed too: cap %d, want -1", c)
	}
	p.note(t0.Add(400*time.Millisecond), 40)
	if stripeRebuildHits.Load() != hits+1 {
		t.Fatal("a second window opened inside the first")
	}
	if c := p.rebuildCap(t0.Add(300*time.Millisecond + 5*time.Second + time.Millisecond)); c != -1 {
		t.Fatalf("after the window: cap %d, want -1", c)
	}

	// a slow decline: the peak ages out, so the pool never sits under half of it
	p = &poolPeak{}
	n := 150
	for i := 0; i < 120; i++ {
		p.note(t0.Add(time.Duration(i)*time.Second), n)
		n -= n / 15
	}
	if p.until.Load() != 0 {
		t.Fatal("a slow decline opened a window")
	}
	// one path of three cut at once (an LB cut): 150 -> 100, never under half
	p = &poolPeak{}
	p.note(t0, 150)
	for i, n := 0, 149; n >= 100; i, n = i+1, n-1 {
		p.note(t0.Add(time.Duration(i)*time.Millisecond), n)
	}
	if p.until.Load() != 0 {
		t.Fatal("a one-path cut opened a window")
	}
	// a small pool never opens one
	p = &poolPeak{}
	p.note(t0, 6)
	p.note(t0.Add(time.Second), 1)
	if p.until.Load() != 0 {
		t.Fatal("a pool under the minimum peak opened a window")
	}
}
