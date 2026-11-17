package engine

import (
	"strings"
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

// diag60: the shared pick says why it found no link (production logged
// "pick: none (free=N chosen=false)" with no reason), and the reason
// classification picks the same link as before.
func TestPickStripeSharedWhy(t *testing.T) {
	old, oldS := stripeK.Load(), stripeShare
	t.Cleanup(func() { stripeK.Store(old); stripeShare = oldS })
	stripeK.Store(4)
	stripeShare = true
	caps := uint8(capMConn | capMConnRaw)
	ls := []*mcLink{newMCLink(caps), newMCLink(caps), newMCLink(caps), newMCLink(caps)}
	m := mcManager(ls...)
	for i, ml := range m.links {
		ml.users.Store(int32(10 + i))
		ml.flowing = 1
		ml.rate10 = float64(1000 * (i + 1))
	}
	m.links[0].heavy = 1
	m.links[1].pressed = true
	m.mu.Lock()
	ml, why := m.pickStripeSharedWhyLocked([]any{Link(ls[2])}, m.exitInfo.Load())
	m.mu.Unlock()
	if ml != m.links[3] {
		t.Fatalf("picked %v, want link 3 (the only candidate)", ml)
	}
	if why.serving != 4 || why.busy != 1 || why.pressed != 1 || why.avoided != 1 || why.cand != 1 {
		t.Fatalf("why = %+v", why)
	}
	m.links[3].pressed = true
	m.mu.Lock()
	ml, why = m.pickStripeSharedWhyLocked([]any{Link(ls[2])}, m.exitInfo.Load())
	m.mu.Unlock()
	if ml != nil || why.cand != 0 || why.pressed != 2 {
		t.Fatalf("got %v why=%+v, want none, two pressed", ml, why)
	}
	if s := why.String(); !strings.Contains(s, "refused=no-candidate") || !strings.Contains(s, "pressed=2") {
		t.Fatalf("String() = %q", s)
	}
	// the wrapper gives the same answer
	m.mu.Lock()
	got := m.pickStripeSharedLocked([]any{Link(ls[2])}, m.exitInfo.Load())
	m.mu.Unlock()
	if got != nil {
		t.Fatalf("wrapper picked %v", got)
	}
}

// TestStripeShareCopies (adopt61): copy-only streams keep every link out of
// the shared pick with the key off (the production "copies=29 cand=0"); with
// HS2_MCONN_STRIPE_SHARE_COPIES the fewest copies go first and a call's link
// stays out.
func TestStripeShareCopies(t *testing.T) {
	old, oldS, oldC, oldF := stripeK.Load(), stripeShare, stripeShareCopies.Load(), stripeLinkCopies
	t.Cleanup(func() {
		stripeK.Store(old)
		stripeShare = oldS
		stripeShareCopies.Store(oldC)
		stripeLinkCopies = oldF
	})
	stripeK.Store(4)
	stripeShare = true
	caps := uint8(capMConn | capMConnRaw)
	ls := []*mcLink{newMCLink(caps), newMCLink(caps), newMCLink(caps), newMCLink(caps)}
	m := mcManager(ls...)
	copies := map[Link]int{Link(ls[0]): 3, Link(ls[1]): 1, Link(ls[2]): 2, Link(ls[3]): 4}
	stripeLinkCopies = func(l Link) int { return copies[l] }
	for _, ml := range m.links {
		ml.users.Store(20)
	}
	pick := func() (*managedLink, stripeShareWhy) {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.pickStripeSharedWhyLocked([]any{stripeCopyUpHint{}}, m.exitInfo.Load()) // an upload's (share62)
	}
	stripeShareCopies.Store(false)
	if ml, why := pick(); ml != nil || why.copies != 4 || why.cand != 0 {
		t.Fatalf("key off: got %v why=%+v, want none (copies=4)", ml, why)
	}
	stripeShareCopies.Store(true)
	ml, why := pick()
	if ml != m.links[1] || why.cand != 4 || why.copyOK != 4 {
		t.Fatalf("key on: got %v why=%+v, want link 1 (fewest copies)", ml, why)
	}
	if !strings.Contains(why.String(), "copy-ok=4") {
		t.Fatalf("String() = %q", why.String())
	}
	// a call rides link 1: the next fewest copies (link 2)
	callLinks.mu.Lock()
	if callLinks.at == nil {
		callLinks.at = map[*linkMeter]time.Time{}
	}
	callLinks.at[m.links[1].mtr] = time.Now().Add(time.Minute)
	callLinks.mu.Unlock()
	t.Cleanup(func() {
		callLinks.mu.Lock()
		delete(callLinks.at, m.links[1].mtr)
		callLinks.mu.Unlock()
	})
	if ml, why = pick(); ml != m.links[2] || why.call != 1 {
		t.Fatalf("call on link 1: got %v why=%+v, want link 2", ml, why)
	}
	// flowing user streams still rank first
	m.links[2].flowing = 1
	if ml, _ = pick(); ml != m.links[0] {
		t.Fatalf("got %v, want link 0 (no flowing, 3 copies)", ml)
	}
	// HS2_MCONN_STRIPE_SHARE_COPIES_K: this connection already has one
	// shared sub (on link 3, which it avoids) and the cap is 1
	oldK := stripeShareCopiesK
	t.Cleanup(func() { stripeShareCopiesK = oldK })
	stripeShareCopiesK = 1
	m.links[3].stShared.Store(1)
	t.Cleanup(func() { m.links[3].stShared.Store(0) })
	m.mu.Lock()
	ml, why = m.pickStripeSharedWhyLocked([]any{Link(ls[3]), stripeCopyUpHint{}}, m.exitInfo.Load())
	m.mu.Unlock()
	if ml != nil || !why.kCap || why.own != 1 {
		t.Fatalf("copies-k: got %v why=%+v, want refused", ml, why)
	}
}

// TestStripeShareCopiesUpOnly (share62): the key serves an upload's stripe
// only — a download's open (no stripeCopyUpHint) keeps the copies rule and
// the call marks out of its pick; among an upload's candidates the links the
// TUN lane used lately go last (HS2_MCONN_STRIPE_SHARE_COPIES_LANEQ).
func TestStripeShareCopiesUpOnly(t *testing.T) {
	old, oldS, oldC, oldF, oldQ := stripeK.Load(), stripeShare, stripeShareCopies.Load(), stripeLinkCopies, stripeShareCopiesLaneQ
	t.Cleanup(func() {
		stripeK.Store(old)
		stripeShare = oldS
		stripeShareCopies.Store(oldC)
		stripeLinkCopies = oldF
		stripeShareCopiesLaneQ = oldQ
	})
	stripeK.Store(4)
	stripeShare = true
	stripeShareCopies.Store(true)
	stripeShareCopiesLaneQ = true
	caps := uint8(capMConn | capMConnRaw)
	ls := []*mcLink{newMCLink(caps), newMCLink(caps), newMCLink(caps), newMCLink(caps)}
	m := mcManager(ls...)
	copies := map[Link]int{Link(ls[0]): 3, Link(ls[1]): 1, Link(ls[2]): 2, Link(ls[3]): 0}
	stripeLinkCopies = func(l Link) int { return copies[l] }
	for _, ml := range m.links {
		ml.users.Store(20)
		ml.mtr.laneAt.Store(0)
	}
	m.links[3].heavy = 1 // the only copy-free link is a download's
	pick := func(avoid []any) (*managedLink, stripeShareWhy) {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.pickStripeSharedWhyLocked(avoid, m.exitInfo.Load())
	}
	// a download (no hint, or another hint): as with the key off
	for _, av := range [][]any{nil, {stripeDirHint{up: false}}} {
		if ml, why := pick(av); ml != nil || why.copies != 3 || why.copyOK != 0 {
			t.Fatalf("download %v: got %v why=%+v, want none (copies=3)", av, ml, why)
		}
	}
	up := []any{stripeCopyUpHint{}}
	if ml, why := pick(up); ml != m.links[1] || why.copyOK != 3 || why.laneBusy != 0 {
		t.Fatalf("upload: got %v why=%+v, want link 1", ml, why)
	}
	// the game's lane rides link 1 and link 2: link 0 (no lane) first
	now := ctrlNow()
	m.links[1].mtr.laneAt.Store(now)
	m.links[2].mtr.laneAt.Store(now)
	n0 := stripeShareCopiesStat.laneSkipped.Load()
	ml, why := pick(up)
	if ml != m.links[0] || why.laneBusy != 2 || why.cand != 3 {
		t.Fatalf("lane on 1,2: got %v why=%+v, want link 0", ml, why)
	}
	if stripeShareCopiesStat.laneSkipped.Load() != n0+1 {
		t.Fatalf("laneSkipped not counted")
	}
	if !strings.Contains(why.String(), "lane-busy=2") {
		t.Fatalf("String() = %q", why.String())
	}
	// every candidate is a lane link: still a pick (never refused), fewest copies
	m.links[0].mtr.laneAt.Store(now)
	if ml, _ = pick(up); ml != m.links[1] {
		t.Fatalf("all lane: got %v, want link 1", ml)
	}
	// a lane stamp older than stripeLaneQuiet does not count
	m.links[0].mtr.laneAt.Store(now - int64(stripeLaneQuiet) - 1)
	if ml, _ = pick(up); ml != m.links[0] {
		t.Fatalf("stale lane on 0: got %v, want link 0", ml)
	}
	// LANEQ=0: the order of adopt61 (fewest copies)
	stripeShareCopiesLaneQ = false
	if ml, why = pick(up); ml != m.links[1] || why.laneBusy != 0 {
		t.Fatalf("laneq off: got %v why=%+v, want link 1", ml, why)
	}
	// the hint: an upload's growth on Iran only, and only with the key
	mc := &mConn{iran: true, stGrowUp: true}
	if !stripeCopyUp(mc.stripeCopyUpHintLocked(nil)) {
		t.Fatal("upload growth: no hint")
	}
	mc.stGrowUp = false
	if stripeCopyUp(mc.stripeCopyUpHintLocked(nil)) {
		t.Fatal("download growth: hint")
	}
	mc.stGrowUp, mc.iran = true, false
	if stripeCopyUp(mc.stripeCopyUpHintLocked(nil)) {
		t.Fatal("exit side: hint")
	}
	mc.iran = true
	stripeShareCopies.Store(false)
	if stripeCopyUp(mc.stripeCopyUpHintLocked(nil)) {
		t.Fatal("key off: hint")
	}
}
