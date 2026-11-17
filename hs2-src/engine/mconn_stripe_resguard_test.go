package engine

import (
	"testing"
	"time"
)

// resGuardPool: two ordinary links pressed by a burst's recent picks and one
// link set aside for the stripe (dr116 arm L: the crowd's arrival).
func resGuardPool(t *testing.T, guard bool) (*LinkManager, []*managedLink) {
	t.Helper()
	oldK, oldG := stripeK.Load(), stripeResGuard.Load()
	stripeK.Store(2)
	stripeResGuard.Store(guard)
	m, ls := pickPool(
		func(ml *managedLink) { ml.picks = 8; ml.users.Store(3) },
		func(ml *managedLink) { ml.picks = 8; ml.users.Store(3) },
		nil,
	)
	ls[2].stripeRes = time.Now()
	stripeHold(ls[2], 1)
	t.Cleanup(func() {
		stripeHold(ls[2], -1)
		stripeK.Store(oldK)
		stripeResGuard.Store(oldG)
	})
	return m, ls
}

func TestStripeResGuardOffLightTakesReserve(t *testing.T) {
	m, ls := resGuardPool(t, false)
	m.mu.Lock()
	ml := m.pickLocked(0)
	m.mu.Unlock()
	if ml != ls[2] {
		t.Fatalf("without the guard the unpressed set-aside link should win (got %v)", ml)
	}
}

func TestStripeResGuardKeepsReserve(t *testing.T) {
	m, ls := resGuardPool(t, true)
	for i := 0; i < 5; i++ {
		l, rel, ok := m.Pick()
		if !ok || l == ls[2].link {
			t.Fatalf("pick %d: a light user took the set-aside link (ok=%v)", i, ok)
		}
		defer rel()
	}
	if !stripeOn(ls[2]) || ls[2].stripeRes.IsZero() || ls[2].users.Load() != 0 {
		t.Fatal("the set-aside link lost its hold or got a user")
	}
}

func TestStripeResGuardHoldCap(t *testing.T) {
	m, ls := resGuardPool(t, true)
	m.mu.Lock()
	defer m.mu.Unlock()
	// every ordinary link at the hold's cap: the waiter waits, not the reserve
	if ml := m.pickLocked(3); ml != nil {
		t.Fatalf("under the cap a light user went to link %d", ml.id)
	}
	stripeResGuard.Store(false)
	if ml := m.pickLocked(3); ml != ls[2] {
		t.Fatalf("guard off: want the set-aside link under the cap, got %v", ml)
	}
	stripeResGuard.Store(true)
	// only the stripe link alive: the plain pick still takes it
	ls[0].link.(*fakeLink).alive = false
	ls[1].link.(*fakeLink).alive = false
	if ml := m.pickLocked(0); ml != ls[2] {
		t.Fatalf("the only link left must still take a user, got %v", ml)
	}
}

// An mconn's light sub (the crowd's own pick) ranks a held stripe link as
// pressed with the guard, as before without it.
func TestStripeResGuardPlaneKey(t *testing.T) {
	m, ls := resGuardPool(t, true)
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.planeKeyLocked(planeLight, ls[2]).pk.pressed {
		t.Fatal("guard on: a set-aside link must rank as pressed for a light sub")
	}
	if m.planeKeyLocked(planeLight, ls[2]).less(m.planeKeyLocked(planeLight, ls[0])) {
		t.Fatal("guard on: the set-aside link must not beat a pressed ordinary link")
	}
	stripeResGuard.Store(false)
	if m.planeKeyLocked(planeLight, ls[2]).pk.pressed {
		t.Fatal("guard off: an idle set-aside link is not pressed")
	}
}
