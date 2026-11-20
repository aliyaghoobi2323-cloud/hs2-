package engine

import (
	"context"
	"testing"
)

// g401 (zdu_recon.go): with the key on, reconcile measures the target against
// the current generation's links only. S9d: after a reconnect storm while the
// old Kharej drains, the new links are as idle as the old ones (here: idler,
// the old links still hold a drained flow each), and the old reconcile retired
// new links to make room for the old pool - the new Kharej redialed each one.
func zduReconRun(t *testing.T, key bool) (newRetired, oldRetired int) {
	t.Helper()
	setZDU(t, key)
	zduResetGauge(t)
	m, _ := zduPool(4, 6)
	m.accept = true
	m.max = 20
	zduRefresh(m)
	m.mu.Lock()
	for _, ml := range m.links {
		if linkZDUInst(ml.link) == zduInstOld {
			ml.open = 1 // a draining flow (the S9d UDP flows)
		}
	}
	m.mu.Unlock()
	m.reconcile(context.Background(), 6)
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, ml := range m.links {
		if !ml.retiring {
			continue
		}
		if linkZDUInst(ml.link) == zduInstNew {
			newRetired++
		} else {
			oldRetired++
		}
	}
	return
}

func TestZDUReconcileSkipsOldGen(t *testing.T) {
	if n, o := zduReconRun(t, true); n != 0 || o != 0 {
		t.Fatalf("key on: reconcile(6) with 6 new + 4 peerOld links retired %d new and %d old links, want 0 and 0", n, o)
	}
}

// Key off: no link is peerOld, reconcile counts all 10 against 6 as before
// (the 4 idlest go, the new ones here).
func TestZDUReconcileKeyOff(t *testing.T) {
	if n, o := zduReconRun(t, false); n+o != 4 || n != 4 {
		t.Fatalf("key off: retired %d new + %d old, want 4 + 0 (unchanged)", n, o)
	}
}

// The traced S9d sequence (g401 A/B): 4 old + 4 new links, T = 4. In the hold
// phase the NEW links carry the mark; at the flip to full the OLD ones do. The
// old reconcile retired the new links in the hold (counted them anyway) and,
// after the flip, saw the 4 old links as serving == T: the new links stayed
// retiring for the whole drain (drainTick closes each once empty -> redial).
func TestZDUReconcileHoldFlip(t *testing.T) {
	setZDU(t, true)
	zduResetGauge(t)
	m, _ := zduPool(4, 4)
	m.accept = true
	m.max = 20
	mark := func(oldGenMarked bool) {
		m.mu.Lock()
		for _, ml := range m.links {
			isOld := linkZDUInst(ml.link) == zduInstOld
			ml.zduOld = isOld == oldGenMarked
			if isOld {
				ml.open = 1 // draining flows on the old Kharej
			}
		}
		m.mu.Unlock()
	}
	mark(false) // hold: the new links are marked
	m.reconcile(context.Background(), 4)
	mark(true) // full: the old links are marked
	m.reconcile(context.Background(), 4)
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, ml := range m.links {
		if linkZDUInst(ml.link) == zduInstNew && (ml.retiring || !ml.serving()) {
			t.Fatalf("after hold -> full: new link %d retiring=%v serving=%v, want serving", ml.id, ml.retiring, ml.serving())
		}
	}
}
