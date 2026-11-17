package engine

import "testing"

// HS2_MCONN_STRIPE_UPG_IDLE: an upload's raw upgrade takes a lightly used
// link (the shared pick) before a link a download's stripe rides; with no
// shared link it still takes the cross-direction one (better than none).
// Without the hint (every later stripe sub, or the key off) the
// cross-direction pick comes first, as before.
func TestPickStripeUpgIdle(t *testing.T) {
	old, oldS, oldX := stripeK.Load(), stripeShare, stripeXDir
	t.Cleanup(func() { stripeK.Store(old); stripeShare = oldS; stripeXDir = oldX })
	stripeK.Store(4)
	stripeShare = false
	stripeXDir = true
	caps := uint8(capMConn | capMConnRaw)
	var ls []*mcLink
	for range 8 {
		ls = append(ls, newMCLink(caps))
	}
	m := mcManager(ls...)
	for i := range 6 {
		m.links[i].users.Store(int32(2 + i)) // light links
	}
	dn := []any{stripeDirHint{up: false}}
	l6, rel6, ok := m.pickStripe(dn)
	if !ok {
		t.Fatal("download pick")
	}
	l7, rel7, ok := m.pickStripe(append([]any{l6}, dn...))
	if !ok {
		t.Fatal("second download pick")
	}
	xd := func(l Link) bool { return l == l6 || l == l7 }
	up := []any{stripeDirHint{up: true}}
	upg := []any{stripeDirHint{up: true}, stripeUpgHint{}}
	stripeShare = true
	// no upgrade hint: the cross-direction link first (unchanged)
	l, rel, ok := m.pickStripe(up)
	if !ok || !xd(l) {
		t.Fatalf("plain upload pick %v ok=%v, want a download's stripe link", l, ok)
	}
	rel()
	// the upgrade: a light link
	d0 := stripeUpgStat.deferred.Load()
	l, rel, ok = m.pickStripe(upg)
	if !ok || xd(l) {
		t.Fatalf("upgrade pick %v ok=%v, want a lightly used link", l, ok)
	}
	if stripeUpgStat.deferred.Load() != d0+1 {
		t.Fatal("deferral not counted")
	}
	rel()
	// no shared pick: the cross-direction link as the last resort
	stripeShare = false
	x0 := stripeUpgStat.lastXdir.Load()
	l, rel, ok = m.pickStripe(upg)
	if !ok || !xd(l) || stripeUpgStat.lastXdir.Load() != x0+1 {
		t.Fatalf("last-resort pick %v ok=%v", l, ok)
	}
	rel()
	rel6()
	rel7()
}

// The hint is marked only for an upload with the key on, and never matches
// a link.
func TestStripeUpgHint(t *testing.T) {
	old := stripeUpgIdle.Load()
	t.Cleanup(func() { stripeUpgIdle.Store(old) })
	m := &mConn{}
	m.stGrowUp = true
	stripeUpgIdle.Store(false)
	if stripeUpgOf(m.stripeUpgHintLocked(nil)) {
		t.Fatal("key off: hinted")
	}
	stripeUpgIdle.Store(true)
	a := m.stripeUpgHintLocked(nil)
	if !stripeUpgOf(a) {
		t.Fatal("key on: no hint")
	}
	if avoided(Link(newMCLink(0)), a) {
		t.Fatal("the hint matched a link")
	}
	m.stGrowUp = false
	if stripeUpgOf(m.stripeUpgHintLocked(nil)) {
		t.Fatal("a download hinted")
	}
}

// With one free link left (the keep-free rule refuses it to a plain stripe
// sub) and no shared pick (HS2_MCONN_STRIPE_SHARE off or spent; with it, the
// shared pick prefers the free link anyway) the upgrade still takes it.
func TestPickStripeUpgIdleFree(t *testing.T) {
	old, oldS, oldX := stripeK.Load(), stripeShare, stripeXDir
	t.Cleanup(func() { stripeK.Store(old); stripeShare = oldS; stripeXDir = oldX })
	stripeK.Store(4)
	stripeShare = false
	stripeXDir = false
	caps := uint8(capMConn | capMConnRaw)
	var ls []*mcLink
	for range 7 {
		ls = append(ls, newMCLink(caps))
	}
	m := mcManager(ls...)
	for i := range 6 {
		m.links[i].users.Store(int32(2 + i)) // light links with flows; link 6 free
		m.links[i].flowing = 1
	}
	if l, _, ok := m.pickStripe([]any{stripeDirHint{up: true}}); ok {
		t.Fatalf("plain pick %v, want none (keep-free)", l)
	}
	n0 := stripeUpgStat.idle.Load()
	l, rel, ok := m.pickStripe([]any{stripeDirHint{up: true}, stripeUpgHint{}})
	if !ok || l != Link(ls[6]) || stripeUpgStat.idle.Load() != n0+1 {
		t.Fatalf("upgrade pick %v ok=%v, want the free link", l, ok)
	}
	rel()
}
