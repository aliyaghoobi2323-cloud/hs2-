package engine

import (
	"testing"
	"time"
)

// dlp217 (HS2_FX212_DL_PRIM=2): a download's DL_COPIES open grades the
// copies' links: no primary first, then idle primaries, then the fewest hot
// primaries — at most fx212DlPrimHotK of the connection's shared subs on hot
// links. Uploads keep their pick.
func TestDlp217Tiers(t *testing.T) {
	old, oldS, oldC, oldF, oldH := stripeK.Load(), stripeShare, stripeShareCopies.Load(), stripeLinkCopies, stripeLinkPrimHot
	oldD, oldM, oldK := fx212DlPrim, fx212DlPrimMode, fx212DlPrimHotK
	t.Cleanup(func() {
		stripeK.Store(old)
		stripeShare = oldS
		stripeShareCopies.Store(oldC)
		stripeLinkCopies = oldF
		stripeLinkPrimHot = oldH
		fx212DlPrim, fx212DlPrimMode, fx212DlPrimHotK = oldD, oldM, oldK
	})
	stripeK.Store(4)
	stripeShare = true
	stripeShareCopies.Store(true)
	fx212DlPrim, fx212DlPrimMode, fx212DlPrimHotK = true, 2, 1
	caps := uint8(capMConn | capMConnRaw)
	ls := []*mcLink{newMCLink(caps), newMCLink(caps), newMCLink(caps), newMCLink(caps), newMCLink(caps)}
	m := mcManager(ls...)
	copies := map[Link]int{}
	type ph struct{ p, h int }
	prim := map[Link]ph{
		Link(ls[0]): {2, 2}, // hot
		Link(ls[1]): {3, 0}, // idle primaries
		Link(ls[2]): {2, 1}, // hot, fewer
		Link(ls[3]): {0, 0}, // copies only
		Link(ls[4]): {1, 1}, // the download's own link (avoided)
	}
	for _, l := range ls {
		copies[Link(l)] = 1
	}
	copies[Link(ls[3])] = 3 // more copies: the old order would rank it last
	stripeLinkCopies = func(l Link) int { return copies[l] }
	stripeLinkPrimHot = func(l Link, _ time.Duration) (int, int) { return prim[l].p, prim[l].h }
	pick := func(avoid ...any) (*managedLink, stripeShareWhy) {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.pickStripeSharedWhyLocked(avoid, m.exitInfo.Load())
	}
	dl := func(extra ...any) []any {
		return append([]any{stripeCopyUpHint{}, stripeCopyDlHint{}, Link(ls[4])}, extra...)
	}
	p0 := fx212DlPrimStat.picks.Load()
	t1, t2, hr, nl := fx212DlPrimStat.tier1.Load(), fx212DlPrimStat.tier2.Load(), fx212DlPrimStat.hotRef.Load(), fx212DlPrimStat.noLink.Load()

	// upload: the old order (fewest copies: link 0), tiers do not apply
	if ml, why := pick(stripeCopyUpHint{}, Link(ls[4])); ml != m.links[0] || why.tier != 0 {
		t.Fatalf("upload got %v why=%+v, want link 0", ml, why)
	}
	// tier 0 (no primary) beats fewer copies
	if ml, why := pick(dl()...); ml != m.links[3] || why.tier != 0 || why.dlQuiet != 1 || why.dlHot != 2 {
		t.Fatalf("download got %v why=%+v, want link 3 tier 0", ml, why)
	}
	// no tier 0: the idle primaries' link
	prim[Link(ls[3])] = ph{1, 1}
	if ml, why := pick(dl()...); ml != m.links[1] || why.tier != 1 {
		t.Fatalf("download got %v why=%+v, want link 1 tier 1", ml, why)
	}
	// only hot links: the fewest hot primaries (link 2 or 3 at 1; link 2 has fewer copies)
	prim[Link(ls[1])] = ph{3, 3}
	ml, why := pick(dl()...)
	if ml != m.links[2] || why.tier != 2 {
		t.Fatalf("download got %v why=%+v, want link 2 tier 2", ml, why)
	}
	// HOTK spent: the connection's own link 4 carries a hot-tier shared sub
	m.links[4].stShared.Add(1)
	m.links[4].stDlHot.Add(1)
	if ml, why = pick(dl()...); ml != nil || why.ownHot != 1 || why.dlHotRef != 4 {
		t.Fatalf("HOTK spent got %v why=%+v, want none, 4 refused", ml, why)
	}
	// ... but an idle link is still taken
	prim[Link(ls[0])] = ph{2, 0}
	if ml, why = pick(dl()...); ml != m.links[0] || why.tier != 1 {
		t.Fatalf("HOTK spent got %v why=%+v, want link 0 tier 1", ml, why)
	}
	// a shared sub not on the hot tier does not spend HOTK
	m.links[4].stDlHot.Add(-1)
	prim[Link(ls[0])] = ph{2, 2}
	if ml, why = pick(dl()...); ml != m.links[2] || why.ownHot != 0 {
		t.Fatalf("own non-hot got %v why=%+v, want link 2", ml, why)
	}
	// HOTK 0: never a hot link
	fx212DlPrimHotK = 0
	m.links[4].stShared.Add(-1)
	if ml, why = pick(dl()...); ml != nil || why.dlHotRef != 4 {
		t.Fatalf("HOTK 0 got %v why=%+v, want none", ml, why)
	}
	if fx212DlPrimStat.picks.Load()-p0 != 5 || fx212DlPrimStat.tier1.Load()-t1 != 2 || fx212DlPrimStat.tier2.Load()-t2 != 2 ||
		fx212DlPrimStat.hotRef.Load()-hr != 11 || fx212DlPrimStat.noLink.Load()-nl != 2 {
		t.Fatalf("stats: %s", fx212DlPrimStatString())
	}
	// mode 1: the old refusal (only a copies-only link; none here)
	fx212DlPrimMode = 1
	stripeLinkPrims = func(l Link) int { return prim[l].p }
	t.Cleanup(func() { stripeLinkPrims = linkPrimaries })
	if ml, why = pick(dl()...); ml != nil || why.dlPrim != 4 {
		t.Fatalf("mode 1 got %v why=%+v, want none", ml, why)
	}
}

// dlp217: linkPrimHot counts open non-copy streams, and as hot those that
// moved bytes since the sampler's look or within the quiet window.
func TestDlp217LinkPrimHot(t *testing.T) {
	l := &mtcpLink{flows: map[*countedStream]struct{}{}}
	now := time.Now()
	add := func(copyOnly, done bool, moved bool, last time.Duration) {
		cs := &countedStream{link: l, lastActive: now.Add(-last)}
		cs.copyOnly.Store(copyOnly)
		cs.done.Store(done)
		cs.bytes.Store(100)
		cs.prevBytes = 100
		if moved {
			cs.bytes.Store(200)
		}
		l.flows[cs] = struct{}{}
	}
	add(true, false, true, 0)             // copy: never
	add(false, true, true, 0)             // done: never
	add(false, false, false, time.Minute) // idle primary
	add(false, false, true, time.Minute)  // moved since the last look: hot
	add(false, false, false, 100*time.Millisecond)
	if p, h := linkPrimHot(l, time.Second); p != 3 || h != 2 {
		t.Fatalf("prims=%d hot=%d, want 3 2", p, h)
	}
	if p, h := linkPrimHot(l, 50*time.Millisecond); p != 3 || h != 1 {
		t.Fatalf("short quiet: prims=%d hot=%d, want 3 1", p, h)
	}
	if p, h := linkPrimHot(newMCLink(0), time.Second); p != 0 || h != 0 {
		t.Fatalf("fake link: %d %d", p, h)
	}
}

func TestDlp217ModeFrom(t *testing.T) {
	for v, want := range map[string]int{"": 1, "1": 1, "0": 0, "2": 2, " 2 ": 2, "x": 1} {
		if got := fx212DlPrimModeFrom(v); got != want {
			t.Fatalf("%q: %d, want %d", v, got, want)
		}
	}
}

// dlp217: a hot-tier shared placement marks its link (stDlHot), which the
// connection's next pick counts against HOTK; the release clears it.
func TestDlp217PlaceMarksHot(t *testing.T) {
	old, oldS, oldC, oldF, oldH := stripeK.Load(), stripeShare, stripeShareCopies.Load(), stripeLinkCopies, stripeLinkPrimHot
	oldD, oldM, oldK := fx212DlPrim, fx212DlPrimMode, fx212DlPrimHotK
	t.Cleanup(func() {
		stripeK.Store(old)
		stripeShare = oldS
		stripeShareCopies.Store(oldC)
		stripeLinkCopies = oldF
		stripeLinkPrimHot = oldH
		fx212DlPrim, fx212DlPrimMode, fx212DlPrimHotK = oldD, oldM, oldK
	})
	stripeK.Store(4)
	stripeShare = true
	stripeShareCopies.Store(true)
	fx212DlPrim, fx212DlPrimMode, fx212DlPrimHotK = true, 2, 1
	caps := uint8(capMConn | capMConnRaw)
	ls := []*mcLink{newMCLink(caps), newMCLink(caps), newMCLink(caps), newMCLink(caps), newMCLink(caps), newMCLink(caps)}
	m := mcManager(ls...)
	for _, ml := range m.links {
		ml.users.Store(10)
		ml.flowing = 1
	}
	stripeLinkCopies = func(Link) int { return 1 }
	stripeLinkPrimHot = func(Link, time.Duration) (int, int) { return 2, 1 } // every link hot
	hint := []any{stripeCopyUpHint{}, stripeCopyDlHint{}}
	l, rel, ok := m.pickStripe(hint)
	if !ok || l == nil {
		t.Fatal("no hot-tier pick")
	}
	var hot *managedLink
	for _, ml := range m.links {
		if ml.link == l {
			hot = ml
		}
	}
	if hot.stDlHot.Load() != 1 || hot.stShared.Load() != 1 {
		t.Fatalf("placed link stDlHot=%d stShared=%d", hot.stDlHot.Load(), hot.stShared.Load())
	}
	// the same connection (its link in avoid): HOTK spent, no second hot link
	if _, _, ok := m.pickStripe(append(hint, l)); ok {
		t.Fatal("a second hot-tier link past HOTK")
	}
	rel()
	if hot.stDlHot.Load() != 0 || hot.stShared.Load() != 0 {
		t.Fatalf("release left stDlHot=%d stShared=%d", hot.stDlHot.Load(), hot.stShared.Load())
	}
}

// dlp217 (HS2_FX212_DL_SHAREQ_MS, exit): a stripe sub on a link other users
// ride takes no chunk while that link's cached queue is over the bound; a
// stale look, an unshared link, the primary and the key off are unchanged.
func TestDlp217ShareQHold(t *testing.T) {
	old, oldQ := stripeQTarget.Load(), dlp217ShareQ
	t.Cleanup(func() { stripeQTarget.Store(old); dlp217ShareQ = oldQ })
	stripeQTarget.Store(0) // no scavenger width: every sub may take chunks
	withStripeSteer(t, false)
	m, p := gateConn(3)
	m.primary.Store(p)
	now := time.Now()
	for _, s := range m.subs {
		s.rw = exitCountedStream{c: &exitConn{mtr: &linkMeter{}}}
		s.lastPush = now
		s.lastRecv.Store(monoNano(now))
	}
	sh := m.subs[2]
	mtr := m.subMeterOf(sh)
	mtr.users.Store(5) // the stripe sub and 4 light users
	dlp217NoteQ(mtr, 5*time.Millisecond, now)
	send := func(n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			c := getChunk(mRecData)
			c.n = 100
			if r := m.stripeSendLocked(c); r != stripeSent {
				t.Fatalf("chunk %d: %d, want sent", i, r)
			}
			c.unref()
		}
	}
	drain := func() {
		for _, s := range m.subs {
			for _, c := range s.q {
				c.unref()
			}
			s.q, s.qBytes = nil, 0
		}
	}
	// key off: the shared sub takes its share
	dlp217ShareQ = 0
	send(30)
	if len(sh.q) == 0 {
		t.Fatal("key off: the shared sub got nothing")
	}
	drain()
	// key on, queue 5 ms > 2 ms: held
	dlp217ShareQ = 2 * time.Millisecond
	h0 := dlp217Stat.held.Load()
	send(30)
	if len(sh.q) != 0 || dlp217Stat.held.Load() == h0 {
		t.Fatalf("over the bound: the shared sub got %d chunks", len(sh.q))
	}
	drain()
	// queue under the bound: used again
	dlp217NoteQ(mtr, time.Millisecond, now)
	send(30)
	if len(sh.q) == 0 {
		t.Fatal("under the bound: the shared sub got nothing")
	}
	drain()
	// stale look: not held
	dlp217NoteQ(mtr, 5*time.Millisecond, now.Add(-2*dlp217QStale))
	send(30)
	if len(sh.q) == 0 {
		t.Fatal("stale look held the sub")
	}
	drain()
	// a link no other user rides: not held
	dlp217NoteQ(mtr, 5*time.Millisecond, now)
	mtr.users.Store(1)
	send(30)
	if len(sh.q) == 0 {
		t.Fatal("unshared link held")
	}
	drain()
	// the primary on a shared, queued link still sends
	pm := m.subMeterOf(p)
	pm.users.Store(5)
	dlp217NoteQ(pm, 9*time.Millisecond, now)
	if !m.dlp217HoldLocked(p, now) {
		t.Fatal("hold check itself must see the primary's link as over the bound")
	}
	for _, s := range m.subs {
		if s != p {
			mm := m.subMeterOf(s)
			mm.users.Store(5)
			dlp217NoteQ(mm, 9*time.Millisecond, now)
		}
	}
	send(5)
	if len(p.q) != 5 {
		t.Fatalf("every stripe sub held: primary got %d of 5", len(p.q))
	}
	drain()
	// Iran never holds
	m.iran = true
	if m.dlp217HoldLocked(sh, now) {
		t.Fatal("Iran held a sub")
	}
}
