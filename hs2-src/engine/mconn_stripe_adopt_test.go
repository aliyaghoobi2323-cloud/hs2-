package engine

import (
	"context"
	"testing"
)

// diag60 (HS2_MCONN_STRIPE_ADOPT_BACK): when the stripe asked for a link and
// the pool meets the raised target with a retiring link, an idle retiring
// link comes back first and is set aside for the stripe; the stripe pick
// then takes it. Key off: the busiest retiring link comes back, as before,
// and the stripe finds nothing.
func TestStripeAdoptBack(t *testing.T) {
	old, oldA := stripeK.Load(), stripeAdoptBack.Load()
	t.Cleanup(func() { stripeK.Store(old); stripeAdoptBack.Store(oldA) })
	stripeK.Store(4)
	for _, on := range []bool{false, true} {
		stripeAdoptBack.Store(on)
		caps := uint8(capMConn | capMConnRaw)
		ls := []*mcLink{newMCLink(caps), newMCLink(caps), newMCLink(caps), newMCLink(caps)}
		m := mcManager(ls...)
		for i, ml := range m.links {
			ml.users.Store(int32(20 + i))
			ml.open = 20 + i
		}
		busyR, idleR := m.links[2], m.links[3]
		busyR.retiring, idleR.retiring = true, true
		busyR.open = 30
		idleR.users.Store(0)
		idleR.open = 0
		now := m.now()
		m.mu.Lock()
		m.stripeAskLocked(now, 0) // the stripe found no link
		m.mu.Unlock()
		if m.stripeWant != 1 {
			t.Fatalf("want %d, expected 1", m.stripeWant)
		}
		m.reconcile(context.Background(), 3)
		if on {
			if idleR.retiring || idleR.stripeRes.IsZero() || !stripeOn(idleR) {
				t.Fatalf("key on: idle retiring link not set aside (retiring=%v res=%v)", idleR.retiring, idleR.stripeRes)
			}
			if !busyR.retiring {
				t.Fatal("key on: the busy retiring link came back too (target 3)")
			}
			if m.stripeWant != 0 {
				t.Fatalf("key on: want %d after the adoption, expected 0", m.stripeWant)
			}
			l, rel, ok := m.pickStripe(nil)
			if !ok || l != Link(ls[3]) {
				t.Fatalf("key on: pick %v ok=%v, want the set-aside link 3", l, ok)
			}
			rel()
		} else {
			if busyR.retiring || !idleR.retiring {
				t.Fatalf("key off: busy retiring=%v idle retiring=%v, want the busy one back", busyR.retiring, idleR.retiring)
			}
			if !idleR.stripeRes.IsZero() {
				t.Fatal("key off: a link was set aside")
			}
			if _, _, ok := m.pickStripe(nil); ok {
				t.Fatal("key off: the stripe found a link in a pool where every serving link has users")
			}
		}
		stripeHeld.mu.Lock()
		stripeHeld.n = nil
		stripeHeld.mu.Unlock()
	}
}
