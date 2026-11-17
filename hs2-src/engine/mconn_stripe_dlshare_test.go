package engine

import "testing"

// HS2_MCONN_STRIPE_DL_SHARE: in a pool with no free link (every link has a
// light user or a stripe), a download's open shares the stripe link with the
// most room — never a link a light user rides, never one already shared,
// never one of its own — and only with the key and the download hint.
func TestPickStripeDlShare(t *testing.T) {
	oldK, oldS, oldD := stripeK.Load(), stripeShare, stripeDlShare.Load()
	t.Cleanup(func() { stripeK.Store(oldK); stripeShare = oldS; stripeDlShare.Store(oldD) })
	stripeK.Store(8)
	stripeShare = false
	caps := uint8(capMConn | capMConnRaw)
	ls := []*mcLink{newMCLink(caps), newMCLink(caps), newMCLink(caps), newMCLink(caps), newMCLink(caps), newMCLink(caps)}
	m := mcManager(ls...)
	for i, ml := range m.links {
		ml.users.Store(5) // light users everywhere
		ml.flowing = 1
		ml.rate10 = float64(1e6 * (10 - i))
	}
	// links 2..5 carry one stripe sub each and nothing else
	for _, i := range []int{2, 3, 4, 5} {
		m.links[i].users.Store(1)
		stripeHold(m.links[i], 1)
	}
	m.links[3].users.Store(2) // link 3: a light user too
	stripeHold(m.links[4], 1) // link 4: two stripe subs already
	m.links[4].users.Store(2)
	t.Cleanup(func() {
		for _, i := range []int{2, 3, 4, 5} {
			stripeHold(m.links[i], -1)
		}
		stripeHold(m.links[4], -1)
	})
	dl := []any{stripeDlShareHint{}}

	stripeDlShare.Store(false)
	if _, _, ok := m.pickStripe(dl); ok {
		t.Fatal("key off: a download took a stripe's link")
	}
	stripeDlShare.Store(true)
	if _, _, ok := m.pickStripe(nil); ok {
		t.Fatal("no download hint: took a stripe's link")
	}
	l, rel, ok := m.pickStripe(dl)
	if !ok || l != Link(ls[5]) {
		t.Fatalf("picked %v ok=%v, want link 5 (one stripe sub, no light user, least rate)", l, ok)
	}
	if got := m.links[5].users.Load(); got != 2 {
		t.Fatalf("link 5 users %d after the pick, want 2", got)
	}
	// link 5 now has two stripe subs: the next download pick takes link 2
	l2, rel2, ok := m.pickStripe(dl)
	if !ok || l2 != Link(ls[2]) {
		t.Fatalf("second pick %v ok=%v, want link 2", l2, ok)
	}
	// a connection's own links are avoided: nothing left
	if _, _, ok := m.pickStripe(append([]any{avoidKey(ls[2]), avoidKey(ls[5])}, dl...)); ok {
		t.Fatal("took a link with two stripe subs or a light user")
	}
	rel()
	rel2()
	if got := m.links[5].users.Load(); got != 1 {
		t.Fatalf("link 5 users %d after release, want 1", got)
	}
}

// The hint goes only on a download's growth while the connection is under
// stripeDlShareMax subs.
func TestStripeDlShareHint(t *testing.T) {
	old := stripeDlShare.Load()
	t.Cleanup(func() { stripeDlShare.Store(old) })
	m := &mConn{iran: true}
	has := func() bool { return stripeDlShareOf(m.stripeDlShareHintLocked(nil)) }
	stripeDlShare.Store(false)
	if has() {
		t.Fatal("key off: hint")
	}
	stripeDlShare.Store(true)
	if !has() {
		t.Fatal("a raw download: no hint")
	}
	m.stGrowUp = true
	if has() {
		t.Fatal("an upload: hint")
	}
	m.stGrowUp = false
	for i := 0; i < stripeDlShareMax-1; i++ {
		m.subs = append(m.subs, &msub{m: m, stripe: true})
	}
	if has() {
		t.Fatal("at stripeDlShareMax subs: hint")
	}
	m.iran = false
	if has() {
		t.Fatal("exit side: hint")
	}
}
