package engine

import "testing"

// HS2_MCONN_STRIPE_XDIR: in a pool with no free link, an upload's stripe sub
// takes a link only a download's stripe rides (never one with a user, one
// with an upload stripe already, or a plain light link), and the other way
// round; release gives the count back. Key off: nothing.
func TestPickStripeXDir(t *testing.T) {
	old, oldS, oldX := stripeK.Load(), stripeShare, stripeXDir
	t.Cleanup(func() { stripeK.Store(old); stripeShare = oldS; stripeXDir = oldX })
	stripeK.Store(4)
	stripeShare = false
	caps := uint8(capMConn | capMConnRaw)
	ls := []*mcLink{newMCLink(caps), newMCLink(caps), newMCLink(caps), newMCLink(caps)}
	m := mcManager(ls...)
	m.links[0].users.Store(5) // a light link
	m.links[1].users.Store(3) // a light link
	// links 2 and 3: a download's stripe links (no user)
	stripeXDir = true
	dn := []any{stripeDirHint{up: false}}
	l2, rel2, ok := m.pickStripe(dn)
	if !ok || l2 != Link(ls[2]) && l2 != Link(ls[3]) {
		t.Fatalf("download pick %v ok=%v, want a free link", l2, ok)
	}
	l3, rel3, ok := m.pickStripe(append([]any{l2}, dn...))
	if !ok || l3 == l2 {
		t.Fatalf("second download pick %v ok=%v", l3, ok)
	}
	if m.links[2].stDnN.Load() != 1 || m.links[3].stDnN.Load() != 1 {
		t.Fatalf("download counts %d %d, want 1 1", m.links[2].stDnN.Load(), m.links[3].stDnN.Load())
	}
	// a third download stripe: no free link, none with an upload stripe
	if _, _, ok := m.pickStripe(append([]any{l2, l3}, dn...)); ok {
		t.Fatal("a download stripe took a download's stripe link or a light link")
	}
	// key off: the upload finds nothing
	stripeXDir = false
	if _, _, ok := m.pickStripe(nil); ok {
		t.Fatal("key off: an upload stripe found a link")
	}
	stripeXDir = true
	up := []any{stripeDirHint{up: true}}
	lu, relu, ok := m.pickStripe(up)
	if !ok || lu != l2 && lu != l3 {
		t.Fatalf("upload pick %v ok=%v, want a download's stripe link", lu, ok)
	}
	if stripeXDirPicks.Load() == 0 {
		t.Fatal("no xdir pick counted")
	}
	var ul *managedLink
	for _, ml := range m.links {
		if ml.link == lu {
			ul = ml
		}
	}
	if ul.stUpN.Load() != 1 {
		t.Fatalf("upload count %d, want 1", ul.stUpN.Load())
	}
	// a second upload stripe: the other download link, never the same one
	lu2, relu2, ok := m.pickStripe(append([]any{lu}, up...))
	if !ok || lu2 == lu || lu2 != l2 && lu2 != l3 {
		t.Fatalf("second upload pick %v ok=%v", lu2, ok)
	}
	if _, _, ok := m.pickStripe(append([]any{lu, lu2}, up...)); ok {
		t.Fatal("a third upload stripe found a link (both download links carry an upload)")
	}
	relu()
	relu2()
	rel2()
	rel3()
	for i, ml := range m.links {
		if ml.stUpN.Load() != 0 || ml.stDnN.Load() != 0 || stripeOn(ml) {
			t.Fatalf("link %d: release left up=%d dn=%d held=%v", i, ml.stUpN.Load(), ml.stDnN.Load(), stripeOn(ml))
		}
	}
}

// The hint never matches a link (avoided) and is found by stripeDirOf.
func TestStripeDirHint(t *testing.T) {
	l := newMCLink(uint8(capMConn))
	av := []any{stripeDirHint{up: true}}
	if avoided(l, av) {
		t.Fatal("the hint avoided a link")
	}
	if h, ok := stripeDirOf(av); !ok || !h.up {
		t.Fatalf("hint %v %v", h, ok)
	}
	if _, ok := stripeDirOf([]any{l}); ok {
		t.Fatal("a hint found where none is")
	}
}
