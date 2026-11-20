package engine

import (
	"context"
	"testing"
)

// zdu review 5 (M): reconcile during the mix phase. Both generations serve
// (no routing mark), so the old reconcile counted O + N against T and retired
// the idlest - the new links (the old ones still hold draining flows).

// zduMixPool: O old + N new links, stepped phase on, old links busy.
func zduMixPool(t *testing.T, o, n int) *LinkManager {
	t.Helper()
	zduPhaseKeyOn(t)
	zduResetGauge(t)
	m, _ := zduPool(o, n)
	m.accept = true
	m.max = 40
	zduRefresh(m)
	m.mu.Lock()
	for _, ml := range m.links {
		if linkZDUInst(ml.link) == zduInstOld {
			ml.open = 1
		}
	}
	m.mu.Unlock()
	return m
}

func zduRetired(m *LinkManager) (newR, oldR int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, ml := range m.links {
		if !ml.retiring {
			continue
		}
		if linkZDUInst(ml.link) == zduInstNew {
			newR++
		} else {
			oldR++
		}
	}
	return
}

// Slow arrival: T = O = 8 and the new links come in one tick at a time from
// N = 4 (mix: need 4, full 8) to 7; reconcile runs every tick. No new link
// may be retired (drainTick would close it, the new Kharej would redial).
func TestZDUReconcileMixSlowArrival(t *testing.T) {
	m := zduMixPool(t, 8, 4)
	if m.zduMixNow() == nil {
		t.Fatal("8 old + 4 new: want the mix phase")
	}
	for n := 4; n < 8; n++ {
		if n > 4 {
			zduAddLinks(m, zduLink(zduInstNew))
			zduRefresh(m)
		}
		if m.zduMixNow() == nil {
			t.Fatalf("N=%d: left the mix phase", n)
		}
		for i := 0; i < 3; i++ { // several ticks at the same N (> 4 s)
			m.reconcile(context.Background(), 8)
		}
		if nr, or := zduRetired(m); nr != 0 || or != 0 {
			t.Fatalf("mix N=%d O=8 T=8: reconcile retired %d new and %d old links, want 0 and 0", n, nr, or)
		}
	}
	// both generations still serve while mixing (the flag is not the mark)
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, ml := range m.links {
		if !ml.serving() {
			t.Fatalf("mix: link %d (old=%v) not serving", ml.id, linkZDUInst(ml.link) == zduInstOld)
		}
	}
}

// The same with the mix fix off retires new links: the test sees the bug.
func TestZDUReconcileMixFixOffRetiresNew(t *testing.T) {
	old := zduReconMixFix
	zduReconMixFix = false
	t.Cleanup(func() { zduReconMixFix = old })
	m := zduMixPool(t, 8, 5)
	m.reconcile(context.Background(), 8)
	if nr, _ := zduRetired(m); nr == 0 {
		t.Fatal("mix fix off: expected the old behaviour (new links retired); the test does not reach the bug")
	}
}

// Revert from full: the new process dies, its links go; the old links are
// current again and count in reconcile (a smaller T retires old links).
func TestZDUReconcileRevertFromFull(t *testing.T) {
	m := zduMixPool(t, 4, 4)
	if m.zduMixNow() != nil || m.zduPeerOld() != 4 {
		t.Fatalf("4 old + 4 new: want full with 4 peerOld, mix=%v peerold=%d", m.zduMixNow() != nil, m.zduPeerOld())
	}
	m.reconcile(context.Background(), 4)
	if nr, or := zduRetired(m); nr != 0 || or != 0 {
		t.Fatalf("full: retired %d new %d old, want 0 0", nr, or)
	}
	zduDropInst(m, zduInstNew)
	zduRefresh(m)
	if m.zduPeerOld() != 0 {
		t.Fatalf("after revert: peerold %d, want 0", m.zduPeerOld())
	}
	m.reconcile(context.Background(), 2)
	if _, or := zduRetired(m); or != 2 {
		t.Fatalf("after revert reconcile(2) over 4 old links retired %d, want 2 (old links counted again)", or)
	}
}

// Revert from mix: the flag goes with the new generation.
func TestZDUReconcileRevertFromMix(t *testing.T) {
	m := zduMixPool(t, 8, 5)
	if m.zduMixNow() == nil {
		t.Fatal("want mix")
	}
	zduDropInst(m, zduInstNew)
	zduRefresh(m)
	m.mu.RLock()
	for _, ml := range m.links {
		if ml.zduMixOld || ml.zduOld {
			m.mu.RUnlock()
			t.Fatalf("after revert from mix: link %d still flagged (mixOld=%v old=%v)", ml.id, ml.zduMixOld, ml.zduOld)
		}
	}
	m.mu.RUnlock()
	m.reconcile(context.Background(), 6)
	if _, or := zduRetired(m); or != 2 {
		t.Fatalf("after revert reconcile(6) over 8 old links retired %d, want 2", or)
	}
}

// Key off: no flag, reconcile unchanged (counts every link).
func TestZDUReconcileMixKeyOff(t *testing.T) {
	setZDU(t, false)
	zduPhaseOn.Store(true)
	zduResetGauge(t)
	m, _ := zduPool(8, 5)
	m.accept = true
	m.max = 40
	zduRefresh(m)
	m.reconcile(context.Background(), 8)
	nr, or := zduRetired(m)
	if nr+or != 5 {
		t.Fatalf("key off: retired %d + %d, want 5 (unchanged)", nr, or)
	}
}

// zdu review 5 (L): pickStormFP's fallback takes a healthy retiring link of
// the current generation before a healthy older-generation link (as
// pickLockedFP since g400).
func TestZDUStormPickRetiringBeforeOld(t *testing.T) {
	setZDU(t, true)
	m, _ := zduPool(4, 0)
	zduAddLinks(m, zduLink(zduInstNew), zduLink(zduInstNew))
	m.mu.Lock()
	var cur []*managedLink
	for _, ml := range m.links {
		if !ml.zduOld {
			ml.retiring = true
			cur = append(cur, ml)
		}
	}
	m.mu.Unlock()
	if len(cur) != 2 {
		t.Fatalf("current links %d, want 2", len(cur))
	}
	for i := 0; i < 40; i++ {
		m.mu.Lock()
		ml, _ := m.pickStormFP(0)
		m.mu.Unlock()
		if ml == nil || ml.zduOld {
			t.Fatalf("pick %d: got an old-generation link (or none) while a current retiring link was healthy", i)
		}
	}
	m.mu.Lock()
	for _, ml := range cur {
		ml.degraded = true
	}
	ml, _ := m.pickStormFP(0)
	m.mu.Unlock()
	if ml == nil || !ml.zduOld {
		t.Fatal("current links degraded: the healthy old link must be the fallback")
	}
}
