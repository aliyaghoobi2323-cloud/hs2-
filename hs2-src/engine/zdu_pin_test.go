package engine

import (
	"context"
	"errors"
	"testing"
)

// ZDU generation pin (zdu_pin.go, zdu350.md §7 S3): a fake Iran pool with
// links of two peer instances.

var (
	zduInstOld = zduInst{Start: 1000, Rand: 7}
	zduInstNew = zduInst{Start: 2000, Rand: 3}
)

// zduLink is an mconn link whose exit announced ZDU with inst (none: a peer
// without ZDU) and the role-6 end notice.
func zduLink(inst zduInst) *mcLink {
	l := newMCLink(capPortTags | capMConn)
	pi := *l.mtr.peerInfo.Load()
	pi.Ext = capExtMConnRstEnd
	if inst.known() {
		pi.Ext |= capExtZDU
		pi.Inst = inst
	}
	l.mtr.peerInfo.Store(&pi)
	return l
}

// zduPool: nOld links of zduInstOld then nNew of zduInstNew.
func zduPool(nOld, nNew int) (*LinkManager, []*mcLink) {
	var ls []*mcLink
	for i := 0; i < nOld; i++ {
		ls = append(ls, zduLink(zduInstOld))
	}
	for i := 0; i < nNew; i++ {
		ls = append(ls, zduLink(zduInstNew))
	}
	m := mcManager(ls...)
	for _, l := range ls {
		m.zduNoteInfo(l.mtr.peerInfo.Load())
	}
	return m, ls
}

// zduAddLinks adds links as a new peer process's links arrive.
func zduAddLinks(m *LinkManager, ls ...*mcLink) {
	m.mu.Lock()
	for _, l := range ls {
		m.links = append(m.links, &managedLink{link: l, id: len(m.links), mtr: l.mtr})
	}
	m.b327Bump()
	m.mu.Unlock()
	for _, l := range ls {
		m.zduNoteInfo(l.mtr.peerInfo.Load())
	}
}

// zduDropInst removes every link of inst (the old process exited).
func zduDropInst(m *LinkManager, inst zduInst) {
	m.mu.Lock()
	kept := m.links[:0]
	for _, ml := range m.links {
		if linkZDUInst(ml.link) != inst {
			kept = append(kept, ml)
		}
	}
	m.links = kept
	m.b327Bump()
	m.mu.Unlock()
}

// zduStreamsOn: streams opened on links of inst / of other instances.
func zduStreamsOn(ls []*mcLink, inst zduInst) (on, off int) {
	for _, l := range ls {
		n := len(l.streams())
		if linkZDUInst(l) == inst {
			on += n
		} else {
			off += n
		}
	}
	return on, off
}

func zduNewEdge(m *LinkManager, id uint64) *mconnEdge {
	return &mconnEdge{ctx: context.Background(), lm: m, port: 443, id: id}
}

func TestZDUCurrentIsNewest(t *testing.T) {
	setZDU(t, true)
	m, _ := zduPool(2, 0)
	if c := m.zduCurrent(); c != zduInstOld {
		t.Fatalf("current %+v, want old", c)
	}
	zduAddLinks(m, zduLink(zduInstNew))
	if c := m.zduCurrent(); c != zduInstNew {
		t.Fatalf("current %+v after the new link, want new", c)
	}
	zduDropInst(m, zduInstNew) // the new process died in probation
	if c := m.zduCurrent(); c != zduInstOld {
		t.Fatalf("current %+v after the new links died, want old again", c)
	}
	mp, _ := zduPool(0, 0)
	zduAddLinks(mp, zduLink(zduInst{}))
	if c := mp.zduCurrent(); c.known() {
		t.Fatalf("pre-ZDU peer: current %+v, want none", c)
	}
}

// New connections go to the newest instance, every sub of them too; old
// connections keep the old instance for every later sub; both directions
// never cross. Light and bulk joins, b312 locked and unlocked join paths.
func TestZDUPinNewConnsNewestOldStay(t *testing.T) {
	setLite(t, false)
	setZDU(t, true)
	x0 := zduStat.crossgen.Load()
	for _, nolock := range []bool{true, false} {
		setOpenNoLock(t, nolock)
		m, ls := zduPool(4, 0)
		var old []*mconnEdge
		for i := 0; i < 6; i++ {
			e := zduNewEdge(m, uint64(100+i))
			if _, _, err := e.open(planeLight, nil); err != nil {
				t.Fatal(err)
			}
			if p := e.zduPin.Load(); p == nil || *p != zduInstOld {
				t.Fatalf("nolock=%v old conn pin %v", nolock, p)
			}
			old = append(old, e)
		}
		newLs := []*mcLink{zduLink(zduInstNew), zduLink(zduInstNew), zduLink(zduInstNew), zduLink(zduInstNew)}
		zduAddLinks(m, newLs...)
		var fresh []*mconnEdge
		for i := 0; i < 6; i++ {
			e := zduNewEdge(m, uint64(200+i))
			if _, _, err := e.open(planeLight, nil); err != nil {
				t.Fatal(err)
			}
			if p := e.zduPin.Load(); p == nil || *p != zduInstNew {
				t.Fatalf("nolock=%v new conn pin %v, want the newest", nolock, p)
			}
			fresh = append(fresh, e)
		}
		if on, off := zduStreamsOn(newLs, zduInstNew); on != 6 || off != 0 {
			t.Fatalf("nolock=%v new conns' first subs: %d on new links, want 6", nolock, on)
		}
		oldOpens := 0
		for _, l := range ls {
			oldOpens += len(l.streams())
		}
		for r := 0; r < 5; r++ {
			for _, p := range []mPlane{planeLight, planeBulk} {
				for _, e := range old {
					_, l, err := e.open(p, nil)
					if err != nil {
						t.Fatal(err)
					}
					if in := linkZDUInst(l.(Link)); in != zduInstOld {
						t.Fatalf("nolock=%v old conn sub on %+v", nolock, in)
					}
					oldOpens++
				}
				for _, e := range fresh {
					_, l, err := e.open(p, nil)
					if err != nil {
						t.Fatal(err)
					}
					if in := linkZDUInst(l.(Link)); in != zduInstNew {
						t.Fatalf("nolock=%v new conn sub on %+v", nolock, in)
					}
				}
			}
		}
		n := 0
		for _, l := range ls {
			n += len(l.streams())
		}
		if n != oldOpens {
			t.Fatalf("nolock=%v old links carry %d streams, want exactly the old conns' %d", nolock, n, oldOpens)
		}
		if x := zduStat.crossgen.Load() - x0; x != 0 {
			t.Fatalf("nolock=%v zdu_crossgen_reject +%d: a picker missed the mark", nolock, x)
		}
		for _, e := range append(old, fresh...) {
			e.end()
		}
		if u := m.users.Load(); u != 0 {
			t.Fatalf("nolock=%v users %d after end", nolock, u)
		}
	}
}

// No link of the pinned instance: today's "no link" (errMConnNoLink after
// the wait), never a sub on the other instance.
func TestZDUPinNoLinkNeverCrosses(t *testing.T) {
	setLite(t, false)
	setZDU(t, true)
	setOpenNoLock(t, true)
	m, ls := zduPool(2, 0)
	e := zduNewEdge(m, 7)
	if _, _, err := e.open(planeLight, nil); err != nil {
		t.Fatal(err)
	}
	newLs := []*mcLink{zduLink(zduInstNew), zduLink(zduInstNew)}
	zduAddLinks(m, newLs...)
	zduDropInst(m, zduInstOld)
	nl0, x0 := zduStat.noLink.Load(), zduStat.crossgen.Load()
	if _, _, err := e.open(planeLight, nil); !errors.Is(err, errMConnNoLink) {
		t.Fatalf("join with no old link: %v, want errMConnNoLink", err)
	}
	if _, _, err := e.open(planeStripe, nil); err == nil {
		t.Fatal("stripe with no old link opened")
	}
	if on, _ := zduStreamsOn(newLs, zduInstNew); on != 0 {
		t.Fatalf("%d streams crossed to the new instance", on)
	}
	if zduStat.noLink.Load() == nl0 {
		t.Fatal("pin_nolink not counted")
	}
	if zduStat.crossgen.Load() != x0 {
		t.Fatal("zdu_crossgen_reject counted: a picker missed the mark")
	}
	_ = ls
	// a new conn still opens (on the new instance)
	e2 := zduNewEdge(m, 8)
	if _, l, err := e2.open(planeLight, nil); err != nil || linkZDUInst(l.(Link)) != zduInstNew {
		t.Fatalf("new conn: %v on %v", err, l)
	}
	e.end()
	e2.end()
}

// The lite first sub (pickFirstLite: pickWait has no avoid list): a link of
// another instance gives way to the pinned placement.
func TestZDUPinLiteFirst(t *testing.T) {
	setLite(t, true)
	setZDU(t, true)
	m, _ := zduPool(3, 1)
	for _, ml := range m.links {
		if linkZDUInst(ml.link) == zduInstNew {
			ml.users.Store(9) // pickWait prefers the empty old links
		}
	}
	// S3b: with the peerOld marks (zdu_counts.go) pickWait's first tier no
	// longer offers an old link at all: no move needed.
	mv0, x0 := zduStat.liteMoved.Load(), zduStat.crossgen.Load()
	for i := 0; i < 4; i++ {
		e := zduNewEdge(m, uint64(200+i))
		_, l, err := e.open(planeLight, nil)
		if err != nil {
			t.Fatal(err)
		}
		if in := linkZDUInst(l.(Link)); in != zduInstNew {
			t.Fatalf("marked: lite first sub on %+v, want new", in)
		}
		e.end()
	}
	if zduStat.liteMoved.Load() != mv0 {
		t.Fatal("marked: pickWait chose an old link")
	}
	// The move is the guard for a link not marked yet (between a newer
	// answer and the refresh): clear the marks.
	m.mu.Lock()
	for _, ml := range m.links {
		ml.zduOld = false
	}
	m.mu.Unlock()
	for i := 0; i < 8; i++ {
		e := zduNewEdge(m, uint64(300+i))
		_, l, err := e.open(planeLight, nil)
		if err != nil {
			t.Fatal(err)
		}
		if in := linkZDUInst(l.(Link)); in != zduInstNew {
			t.Fatalf("lite first sub on %+v, want new", in)
		}
		e.end()
	}
	if zduStat.crossgen.Load() != x0 {
		t.Fatal("zdu_crossgen_reject counted")
	}
	if zduStat.liteMoved.Load() == mv0 {
		t.Fatal("pickWait never chose an old link: the test does not cover the move")
	}
}

// Stripes (pickStripeWhy, both b312 paths) and the role-6 end notice
// (pickPlaneCall from sendEndNotice) of an old conn stay on the old instance.
func TestZDUPinStripeAndEndNotice(t *testing.T) {
	setLite(t, false)
	setZDU(t, true)
	for _, nolock := range []bool{true, false} {
		setOpenNoLock(t, nolock)
		x0 := zduStat.crossgen.Load()
		m, oldLs := zduPool(3, 0)
		e := zduNewEdge(m, 77)
		if _, _, err := e.open(planeLight, nil); err != nil {
			t.Fatal(err)
		}
		newLs := []*mcLink{zduLink(zduInstNew), zduLink(zduInstNew), zduLink(zduInstNew), zduLink(zduInstNew)}
		zduAddLinks(m, newLs...)
		got := 0
		for i := 0; i < 2; i++ {
			_, l, err := e.open(planeStripe, nil)
			if err != nil {
				continue // keep-free rule: no free old link left is fine
			}
			got++
			if in := linkZDUInst(l.(Link)); in != zduInstOld {
				t.Fatalf("nolock=%v stripe on %+v", nolock, in)
			}
		}
		if got == 0 {
			t.Fatalf("nolock=%v no stripe opened at all", nolock)
		}
		before := 0
		for _, l := range oldLs {
			before += len(l.streams())
		}
		e.sendEndNotice()
		after := 0
		for _, l := range oldLs {
			after += len(l.streams())
		}
		if after != before+1 {
			t.Fatalf("nolock=%v end notice: old links %d -> %d streams", nolock, before, after)
		}
		if on, _ := zduStreamsOn(newLs, zduInstNew); on != 0 {
			t.Fatalf("nolock=%v %d streams on the new instance", nolock, on)
		}
		if x := zduStat.crossgen.Load() - x0; x != 0 {
			t.Fatalf("nolock=%v zdu_crossgen_reject +%d", nolock, x)
		}
		e.end()
		// and a new conn's stripe goes to the new instance
		e2 := zduNewEdge(m, 78)
		if _, _, err := e2.open(planeLight, nil); err != nil {
			t.Fatal(err)
		}
		if _, l, err := e2.open(planeStripe, nil); err == nil && linkZDUInst(l.(Link)) != zduInstNew {
			t.Fatalf("nolock=%v new conn stripe on old", nolock)
		}
		e2.end()
	}
}

// The last check inside the pick functions: a link of another instance is
// counted and refused, its placement released.
func TestZDUPickOKAssert(t *testing.T) {
	setZDU(t, true)
	l := zduLink(zduInstNew)
	released := 0
	x0 := zduStat.crossgen.Load()
	avoid := []any{zduPinMark{inst: zduInstOld}}
	if zduPickOK(l, func() { released++ }, avoid) {
		t.Fatal("cross-generation pick passed")
	}
	if released != 1 || zduStat.crossgen.Load() != x0+1 {
		t.Fatalf("released %d, crossgen +%d", released, zduStat.crossgen.Load()-x0)
	}
	if !zduPickOK(zduLink(zduInstOld), nil, avoid) || !zduPickOK(l, nil, nil) {
		t.Fatal("same instance / no pin refused")
	}
	if !avoided(l, avoid) || avoided(zduLink(zduInstOld), avoid) {
		t.Fatal("avoided() does not apply the pin")
	}
	if !avoided(zduLink(zduInst{}), avoid) {
		t.Fatal("a link of unknown instance passed a known pin")
	}
}

// Key off: no pin, no mark, the mixed pool is one pool as today; the pick
// hot path allocates nothing more than without the ZDU wrapper.
func TestZDUPinOffIdentical(t *testing.T) {
	setLite(t, false)
	setZDU(t, false)
	setOpenNoLock(t, true)
	m, _ := zduPool(3, 3)
	e := zduNewEdge(m, 5)
	avoid := []any{avoidKey(m.links[0].link)}
	if got := e.zduAvoid(avoid); &got[0] != &avoid[0] || len(got) != 1 {
		t.Fatal("key off: zduAvoid changed the avoid list")
	}
	if a := testing.AllocsPerRun(200, func() { _ = e.zduAvoid(avoid) }); a != 0 {
		t.Fatalf("key off: zduAvoid allocates %.1f", a)
	}
	if a := testing.AllocsPerRun(200, func() { _ = avoided(m.links[1].link, avoid) }); a != 0 {
		t.Fatalf("key off: avoided allocates %.1f", a)
	}
	if !zduPickOK(zduLink(zduInstNew), nil, []any{zduPinMark{inst: zduInstOld}}) {
		t.Fatal("key off: the assert acted")
	}
	pick := func() {
		l, rel, ok := m.pickPlaneCall(planeLight, avoid)
		_ = l
		if ok {
			rel()
		}
	}
	core := func() {
		l, rel, ok := m.pickPlaneCallSkip(planeLight, avoid)
		_ = l
		if ok {
			rel()
		}
	}
	if a, b := testing.AllocsPerRun(300, pick), testing.AllocsPerRun(300, core); a != b {
		t.Fatalf("key off: pickPlaneCall allocs %.1f, without the ZDU wrapper %.1f", a, b)
	}
	spick := func() {
		_, rel, ok, _ := m.pickStripeWhy(nil)
		if ok {
			rel()
		}
	}
	score := func() {
		_, rel, ok, _ := m.pickStripeWhyCore(nil)
		if ok {
			rel()
		}
	}
	if a, b := testing.AllocsPerRun(300, spick), testing.AllocsPerRun(300, score); a != b {
		t.Fatalf("key off: pickStripeWhy allocs %.1f, without the ZDU wrapper %.1f", a, b)
	}
	// both generations take subs: no pin. Each conn stays open until the
	// end, and the walk runs twice, on the pool as built (old links first)
	// and on the same pool with the new links first: the picker's first link
	// takes most subs (about 130 of 160 in either order), so each order
	// reaches the generation listed first on every run. The old check (one
	// pool, conns ended at once, both generations seen) depended on where the
	// other 30-odd subs fell and saw only the old generation 1 run in 30.
	seen := map[zduInst]int{}
	for _, newFirst := range []bool{false, true} {
		pm, _ := zduPool(3, 3)
		if newFirst {
			pm.mu.Lock()
			pm.links = append(pm.links[3:], pm.links[:3]...)
			for i, ml := range pm.links {
				ml.id = i
			}
			pm.b327Bump()
			pm.mu.Unlock()
		}
		got := zduWalkOff(t, pm)
		first := linkZDUInst(pm.links[0].link)
		if got[first] == 0 {
			t.Fatalf("key off: first link's generation %v took no sub (%v)", first, got)
		}
		for k, v := range got {
			seen[k] += v
		}
	}
	if seen[zduInstOld] == 0 || seen[zduInstNew] == 0 {
		t.Fatalf("key off: subs only on %v", seen)
	}
}

// zduWalkOff opens 40 conns (one light and three bulk subs each) on m with
// the key off, keeps them open to the end, and counts the subs per peer
// instance. No conn may carry a pin.
func zduWalkOff(t *testing.T, m *LinkManager) map[zduInst]int {
	t.Helper()
	got := map[zduInst]int{}
	var edges []*mconnEdge
	defer func() {
		for _, e := range edges {
			e.end()
		}
	}()
	for i := 0; i < 40; i++ {
		e := zduNewEdge(m, uint64(400+i))
		edges = append(edges, e)
		_, l, err := e.open(planeLight, nil)
		if err != nil {
			t.Fatal(err)
		}
		got[linkZDUInst(l.(Link))]++
		for j := 0; j < 3; j++ {
			if _, l, err := e.open(planeBulk, nil); err == nil {
				got[linkZDUInst(l.(Link))]++
			}
		}
		if e.zduPin.Load() != nil {
			t.Fatal("key off: conn pinned")
		}
	}
	return got
}

// Key on, peer without ZDU: no instance anywhere, so no mark and no pin.
func TestZDUPinPeerWithoutZDU(t *testing.T) {
	setLite(t, false)
	setZDU(t, true)
	m, _ := zduPool(0, 0)
	zduAddLinks(m, zduLink(zduInst{}), zduLink(zduInst{}))
	e := zduNewEdge(m, 6)
	avoid := []any{1}
	if got := e.zduAvoid(avoid); len(got) != 1 {
		t.Fatal("mark added with no ZDU peer")
	}
	if _, _, err := e.open(planeLight, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.open(planeBulk, nil); err != nil {
		t.Fatal(err)
	}
	if e.zduPin.Load() != nil {
		t.Fatal("pinned with no ZDU peer")
	}
	e.end()
}
