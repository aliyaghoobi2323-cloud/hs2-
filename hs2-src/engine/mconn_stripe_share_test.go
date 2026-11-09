package engine

import (
	"testing"
	"time"
)

// HS2_MCONN_STRIPE_SHARE: in a pool where every link has a user, a stripe sub
// takes the least loaded light link (never a download's, a call copy's or a
// stripe's), and stops once half the pool would be heavy or striped.
func TestPickStripeShared(t *testing.T) {
	old, oldS := stripeK.Load(), stripeShare
	t.Cleanup(func() { stripeK.Store(old); stripeShare = oldS })
	stripeK.Store(4)
	caps := uint8(capMConn | capMConnRaw)
	ls := []*mcLink{newMCLink(caps), newMCLink(caps), newMCLink(caps), newMCLink(caps), newMCLink(caps), newMCLink(caps)}
	m := mcManager(ls...)
	for i, ml := range m.links {
		ml.users.Store(int32(10 + i))
		ml.flowing = 2
		ml.rate10 = float64(1000 * (i + 1))
	}
	m.links[0].heavy = 1      // a download
	m.links[1].pressed = true // its sender is blocked
	stripeShare = false
	if _, _, ok := m.pickStripe(nil); ok {
		t.Fatal("key off: stripe took a link a user rides")
	}
	stripeShare = true
	l, rel, ok := m.pickStripe(nil)
	if !ok || l != Link(ls[2]) {
		t.Fatalf("picked %v ok=%v, want link 2 (the lowest rate of the light ones)", l, ok)
	}
	if !stripeOn(m.links[2]) {
		t.Fatal("the shared link is not held")
	}
	// 6 serving: the download (0) and the stripe (2) are 2; one more makes 3
	// of 6 (half); a fourth would be over half
	l2, rel2, ok := m.pickStripe([]any{l})
	if !ok || l2 != Link(ls[3]) {
		t.Fatalf("second pick %v ok=%v, want link 3", l2, ok)
	}
	if _, _, ok := m.pickStripe([]any{l, l2}); ok {
		t.Fatal("took a link past half of the pool")
	}
	rel2()
	rel()
	if stripeOn(m.links[2]) || stripeOn(m.links[3]) {
		t.Fatal("release left a link held")
	}
}

// A fast striped connection keeps its quiet stripe subs (Iran's idle rule
// skips them) and warms them with a STRIPE record; a slow one does not.
func TestStripeWarm(t *testing.T) {
	oldS := stripeShare
	t.Cleanup(func() { stripeShare = oldS })
	stripeShare = true
	m := &mConn{iran: true}
	p := &msub{m: m, kick: make(chan struct{}, 1), done: make(chan struct{})}
	s := &msub{m: m, kick: make(chan struct{}, 1), done: make(chan struct{}), stripe: true}
	m.subs = []*msub{p, s}
	m.primary.Store(p)
	m.stN = 1
	now := time.Now()
	m.stripeNoteRateLocked(now, stripeWarmRate/2)
	m.stripeWarmLocked(now)
	if len(s.q) != 0 || m.stripeWarmKeepLocked(s, now) {
		t.Fatal("a slow connection kept or warmed its stripe sub")
	}
	m.stripeNoteRateLocked(now, stripeWarmRate*2)
	if !m.stripeWarmKeepLocked(s, now) || m.stripeWarmKeepLocked(p, now) {
		t.Fatal("keep: want the stripe sub only")
	}
	m.stripeWarmLocked(now)
	if len(s.q) != 1 || s.q[0].kind != mRecStripe || len(p.q) != 0 {
		t.Fatalf("warm: stripe sub queue %d, primary %d; want one STRIPE record on the stripe sub", len(s.q), len(p.q))
	}
	m.stripeWarmLocked(now.Add(time.Second))
	if len(s.q) != 1 {
		t.Fatal("warmed again before stripeWarmEvery (or with a record queued)")
	}
	if m.stripeWarmKeepLocked(s, now.Add(4*mLitePoll)) {
		t.Fatal("kept past the warm window")
	}
}

// noStripeShare: the test runs with HS2_MCONN_STRIPE_SHARE off.
func noStripeShare(t *testing.T) {
	old := stripeShare
	t.Cleanup(func() { stripeShare = old })
	stripeShare = false
}

// HS2_MCONN_STRIPE_SHARE_TOTAL: the pool's shared stripe subs, every
// connection together, stop at the budget; a release frees a place.
func TestPickStripeSharedTotal(t *testing.T) {
	old, oldS, oldK, oldT := stripeK.Load(), stripeShare, stripeShareK, stripeShareTotal
	t.Cleanup(func() { stripeK.Store(old); stripeShare = oldS; stripeShareK = oldK; stripeShareTotal = oldT })
	stripeK.Store(4)
	stripeShare, stripeShareK, stripeShareTotal = true, 8, 2
	caps := uint8(capMConn | capMConnRaw)
	var ls []*mcLink
	for i := 0; i < 10; i++ {
		ls = append(ls, newMCLink(caps))
	}
	m := mcManager(ls...)
	for i, ml := range m.links {
		ml.users.Store(10)
		ml.flowing = 2
		ml.rate10 = float64(1000 * (i + 1))
	}
	l1, rel1, ok1 := m.pickStripe(nil) // connection A
	l2, rel2, ok2 := m.pickStripe(nil) // connection B
	if !ok1 || !ok2 || l1 == l2 {
		t.Fatalf("two connections: %v %v ok=%v %v", l1, l2, ok1, ok2)
	}
	if _, _, ok := m.pickStripe(nil); ok {
		t.Fatal("a third shared sub past the pool budget of 2")
	}
	rel1()
	l3, rel3, ok := m.pickStripe(nil)
	if !ok {
		t.Fatal("no shared sub after a release freed the budget")
	}
	_ = l3
	rel3()
	rel2()
}

// The shared stripe's bounds: no link the TUN lane's interactive flows rode
// lately, the fewest flowing streams first, and at most stripeShareK shared
// subs per connection.
func TestPickStripeSharedBounds(t *testing.T) {
	old, oldS, oldK, oldL := stripeK.Load(), stripeShare, stripeShareK, stripeShareLane
	t.Cleanup(func() { stripeK.Store(old); stripeShare = oldS; stripeShareK = oldK; stripeShareLane = oldL })
	stripeK.Store(4)
	stripeShare, stripeShareK, stripeShareLane = true, 1, true
	caps := uint8(capMConn | capMConnRaw)
	ls := []*mcLink{newMCLink(caps), newMCLink(caps), newMCLink(caps), newMCLink(caps), newMCLink(caps), newMCLink(caps)}
	m := mcManager(ls...)
	for i, ml := range m.links {
		ml.users.Store(10)
		ml.flowing = 3
		ml.rate10 = float64(1000 * (i + 1))
		if ml.mtr == nil {
			ml.mtr = &linkMeter{}
		}
	}
	m.links[0].mtr.laneAt.Store(ctrlNow()) // a ping rides link 0
	m.links[4].flowing = 1                 // fewest streams moving data
	l, rel, ok := m.pickStripe(nil)
	if !ok || l != Link(ls[4]) {
		t.Fatalf("picked %v ok=%v, want link 4 (fewest flowing, no lane)", l, ok)
	}
	if _, _, ok := m.pickStripe([]any{l}); ok {
		t.Fatal("a second shared sub past stripeShareK=1")
	}
	// another connection (its own avoid list) still gets one, not link 0
	l2, rel2, ok := m.pickStripe(nil)
	if !ok || l2 == Link(ls[0]) || l2 == Link(ls[4]) {
		t.Fatalf("other connection picked %v ok=%v", l2, ok)
	}
	rel2()
	rel()
	if m.links[4].stShared.Load() != 0 {
		t.Fatal("release left the shared count")
	}
}
