package engine

import "testing"

// rel54b: HS2_MCONN_STRIPE_FAST_SOLO — the fast rule only for an upload that
// stripes alone; the count follows growth and the slot's release.
func TestStripeFastSolo(t *testing.T) {
	oldF, oldS, oldN := stripeFast.Load(), stripeFastSolo.Load(), stripeUpActive.Load()
	defer func() { stripeFast.Store(oldF); stripeFastSolo.Store(oldS); stripeUpActive.Store(oldN) }()
	stripeFast.Store(true)
	stripeFastSolo.Store(true)
	stripeUpActive.Store(0)
	a, b := &mConn{}, &mConn{}
	if !a.stripeFastOnLocked(true) || a.stripeFastOnLocked(false) {
		t.Fatal("a lone upload: fast; a download: never")
	}
	a.stripeUpCountLocked(true)
	a.stripeUpCountLocked(true) // idempotent
	if stripeUpActive.Load() != 1 {
		t.Fatalf("count %d, want 1", stripeUpActive.Load())
	}
	if !a.stripeFastOnLocked(true) {
		t.Fatal("its own count must not hold the fast rule")
	}
	if b.stripeFastOnLocked(true) {
		t.Fatal("a second upload while another stripes: not fast")
	}
	b.stripeUpCountLocked(true)
	if a.stripeFastOnLocked(true) || b.stripeFastOnLocked(true) {
		t.Fatal("two uploads striping: neither is fast")
	}
	b.stFastMore = 3
	if b.stripeFastNextLocked() || b.stFastMore != 0 {
		t.Fatal("the doubling chain must stop when another upload stripes")
	}
	if b.stripeFastPlanLocked(2, 1e9, true) != 0 {
		t.Fatal("no doubling plan beside another upload")
	}
	b.stripeUpCountLocked(false) // b's last growth was a download / slot released
	if !a.stripeFastOnLocked(true) {
		t.Fatal("alone again: fast")
	}
	stripeFastSolo.Store(false)
	b.stripeUpCountLocked(true)
	if !a.stripeFastOnLocked(true) || !b.stripeFastOnLocked(true) {
		t.Fatal("SOLO=0: every upload fast (rel54)")
	}
	stripeFast.Store(false)
	if a.stripeFastOnLocked(true) {
		t.Fatal("FAST=0: never")
	}
	a.stripeUpCountLocked(false)
	b.stripeUpCountLocked(false)
	if stripeUpActive.Load() != 0 {
		t.Fatalf("count %d after release, want 0", stripeUpActive.Load())
	}
}

// rel54b: stripeEnded gives the upload count back with the slot.
func TestStripeFastSoloEnded(t *testing.T) {
	oldN := stripeUpActive.Load()
	defer stripeUpActive.Store(oldN)
	stripeUpActive.Store(0)
	m := &mConn{}
	m.stActive = true
	stripeStat.active.Add(1)
	m.stripeUpCountLocked(true)
	m.stripeEnded()
	if stripeUpActive.Load() != 0 || m.stUpCnt || m.stActive {
		t.Fatalf("after end: count %d cnt %v active %v", stripeUpActive.Load(), m.stUpCnt, m.stActive)
	}
}
