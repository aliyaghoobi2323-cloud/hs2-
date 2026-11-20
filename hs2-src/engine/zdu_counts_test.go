package engine

import (
	"strings"
	"testing"
)

// ZDU B3 (zdu_counts.go, zdu350.md §7 S3b): the pool counts only the
// current peer generation.

func zduRefresh(m *LinkManager) {
	m.mu.Lock()
	m.zduRefreshLocked()
	m.mu.Unlock()
}

func zduCounts(m *LinkManager) (alive, serving, retiring int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, r := m.countsLocked()
	return m.aliveLocked(), s, r
}

func zduResetGauge(t *testing.T) {
	t.Helper()
	p, c, s := zduGauge.peerOld.Load(), zduGauge.oldConns.Load(), zduSeen.Load()
	zduGauge.peerOld.Store(0)
	zduGauge.oldConns.Store(0)
	zduSeen.Store(0)
	t.Cleanup(func() { zduGauge.peerOld.Store(p); zduGauge.oldConns.Store(c); zduSeen.Store(s) })
	// Every other global on zduCountsLine too (the key-on run leaked
	// zduExitBootN from TestZDUExitBootAtCeiling into TestZDUGauges).
	zduResetExitStat(t)
	b, es, ep := zduExitBootN.Load(), zduExitSeen.Load(), zduExitPeers.Swap(nil)
	zduExitBootN.Store(0)
	zduExitSeen.Store(false)
	t.Cleanup(func() { zduExitBootN.Store(b); zduExitSeen.Store(es); zduExitPeers.Store(ep) })
}

// Mixed generations: the old links are up but not capacity; the plain
// picker and the mconn pin keep new connections off them; old conns still
// place subs on them (up()).
func TestZDUCountsExcludeOld(t *testing.T) {
	setZDU(t, true)
	zduResetGauge(t)
	m, ls := zduPool(3, 2)
	m.max = 5
	if a, s, r := zduCounts(m); a != 2 || s != 2 || r != 0 {
		t.Fatalf("alive/serving/retiring = %d/%d/%d, want 2/2/0", a, s, r)
	}
	if n := m.zduPeerOld(); n != 3 {
		t.Fatalf("peerold %d, want 3", n)
	}
	m.mu.RLock()
	for _, ml := range m.links {
		old := linkZDUInst(ml.link) == zduInstOld
		if ml.zduOld != old || ml.serving() == old || !ml.up() {
			t.Fatalf("link %d: zduOld=%v serving=%v up=%v (old=%v)", ml.id, ml.zduOld, ml.serving(), ml.up(), old)
		}
	}
	m.mu.RUnlock()
	for i := 0; i < 20; i++ { // the plain picker: never an old link while new ones serve
		l, rel, ok := m.Pick()
		if !ok || linkZDUInst(l) != zduInstNew {
			t.Fatalf("Pick %d -> %v ok=%v", i, linkZDUInst(l), ok)
		}
		rel()
	}
	_ = ls
	// the new process died in probation: its links go, the old count again
	zduDropInst(m, zduInstNew)
	zduRefresh(m)
	if a, s, _ := zduCounts(m); a != 3 || s != 3 || m.zduPeerOld() != 0 {
		t.Fatalf("after the new links left: alive %d serving %d peerold %d, want 3 3 0", a, s, m.zduPeerOld())
	}
}

// The autopilot sizes for the new generation: old links neither fill the
// target nor the dial room, and are not in its sample.
func TestZDUAutopilotNoExtraDial(t *testing.T) {
	setZDU(t, true)
	zduResetGauge(t)
	m, _ := zduPool(5, 5)
	m.max = 5
	m.target.Store(5)
	if m.wantsDial() {
		t.Fatal("5 new serving links = target 5: the old 5 must not make it dial more")
	}
	if r := m.dialRoom(); r != 0 {
		t.Fatalf("dialRoom %d with a full new pool, want 0", r)
	}
	if got := m.ctlTarget(); got != 5 {
		t.Fatalf("ctlTarget %d, want 5", got)
	}
	m.sampleHealth()
	if n := len(m.sample.links); n != 5 {
		t.Fatalf("autopilot sample has %d links, want the 5 new", n)
	}
	for _, l := range m.sample.links {
		if !l.serving {
			t.Fatalf("sample link %d not serving", l.id)
		}
	}
	// half the new pool up: it dials the rest, as for an empty pool
	m2, _ := zduPool(5, 2)
	m2.max = 5
	m2.target.Store(5)
	if !m2.wantsDial() || m2.dialRoom() != 3 {
		t.Fatalf("2 of 5 new: wantsDial %v dialRoom %d, want true 3", m2.wantsDial(), m2.dialRoom())
	}
	// key off: the same pool counts both generations (today's behaviour)
	setZDU(t, false)
	m3, _ := zduPool(5, 2)
	m3.max = 5
	m3.target.Store(5)
	if m3.wantsDial() {
		t.Fatal("key off: 7 serving links >= target 5, no dial")
	}
}

// R6/H6: at the hourly storm the old Kharej holds ~80 links while the new
// one dials its pool; the accept gate (acceptReverseLinks: lm.alive() vs
// reverseAcceptCap(max)) must let the whole new pool in.
func TestZDUAcceptCapHeadroom(t *testing.T) {
	for _, mx := range []int{40, 80, 96} {
		for _, on := range []bool{true, false} {
			setZDU(t, on)
			m, _ := zduPool(80, 0)
			m.max = mx
			refused := 0
			for i := 0; i < 80; i++ {
				if m.alive() >= reverseAcceptCap(m.max) { // the gate, exactly
					refused++
					continue
				}
				l := zduLink(zduInstNew)
				zduAddLinks(m, l) // accepted, then its kindInfo answer arrives
			}
			switch {
			case on && refused != 0:
				t.Fatalf("max %d key on: %d of the 80 new links refused", mx, refused)
			case on && m.zduPeerOld() != 80:
				t.Fatalf("max %d: peerold %d, want 80", mx, m.zduPeerOld())
			case !on && mx == 40 && refused != 72:
				t.Fatalf("max 40 key off: refused %d, want 72 (cap 88 = 80 old + 8)", refused)
			}
		}
	}
	// a link accepted before its answer has no known generation and counts:
	// old links are marked only once a newer instance answered
	setZDU(t, true)
	m, _ := zduPool(4, 0)
	m.mu.Lock()
	m.links = append(m.links, &managedLink{link: newMCLink(capPortTags | capMConn), id: 99})
	m.b327Bump()
	m.mu.Unlock()
	zduRefresh(m)
	if a, _, _ := zduCounts(m); a != 5 || m.zduPeerOld() != 0 {
		t.Fatalf("no newer answer yet: alive %d peerold %d, want 5 0", a, m.zduPeerOld())
	}
}

// The gauges on the mem line.
func TestZDUGauges(t *testing.T) {
	setZDU(t, true)
	zduResetGauge(t)
	if s := zduCountsLine(); s != "" {
		t.Fatalf("no generation change yet: %q", s)
	}
	m, _ := zduPool(3, 1)
	m.mu.Lock()
	for _, ml := range m.links {
		if linkZDUInst(ml.link) == zduInstOld {
			ml.users.Store(4)
		}
	}
	m.zduRefreshLocked()
	m.mu.Unlock()
	if p, c := zduGauge.peerOld.Load(), zduGauge.oldConns.Load(); p != 3 || c != 12 {
		t.Fatalf("gauges peerold %d old_conns %d, want 3 12", p, c)
	}
	if s := zduCountsLine(); s != " zdu_peerold=3 zdu_old_conns=12" {
		t.Fatalf("line %q", s)
	}
	// a second pool adds, a drained pool goes back to 0 (line stays at 0)
	m2, _ := zduPool(2, 1)
	_ = m2
	if p := zduGauge.peerOld.Load(); p != 5 {
		t.Fatalf("two pools: peerold %d, want 5", p)
	}
	zduDropInst(m, zduInstOld)
	zduDropInst(m2, zduInstOld)
	zduRefresh(m)
	zduRefresh(m2)
	if s := zduCountsLine(); s != " zdu_peerold=0 zdu_old_conns=0" {
		t.Fatalf("drained: %q", s)
	}
	if !strings.Contains(memLogLine(), "zdu_peerold=0") {
		t.Fatal("mem line lacks the gauges")
	}
}

// Key off: no mark, counts as before, no allocation on the hot paths.
func TestZDUCountsOffIdentical(t *testing.T) {
	setZDU(t, false)
	zduResetGauge(t)
	m, _ := zduPool(3, 2)
	zduRefresh(m)
	if a, s, _ := zduCounts(m); a != 5 || s != 5 || m.zduPeerOld() != 0 || zduCountsLine() != "" {
		t.Fatalf("key off: alive %d serving %d peerold %d line %q", a, s, m.zduPeerOld(), zduCountsLine())
	}
	// key on with a peer without ZDU: nothing either
	setZDU(t, true)
	mp := mcManager(zduLink(zduInst{}), zduLink(zduInst{}))
	zduRefresh(mp)
	if a, s, _ := zduCounts(mp); a != 2 || s != 2 || mp.zduPeerOld() != 0 {
		t.Fatalf("pre-ZDU peer: alive %d serving %d", a, s)
	}
	setZDU(t, false)
	ml := m.links[0]
	if n := testing.AllocsPerRun(200, func() { _ = ml.serving() }); n != 0 {
		t.Fatalf("serving allocs %v", n)
	}
	if n := testing.AllocsPerRun(200, func() { zduRefresh(m) }); n != 0 {
		t.Fatalf("zduRefreshLocked allocs %v (key off)", n)
	}
	if n := testing.AllocsPerRun(200, func() { m.mu.RLock(); m.countsLocked(); m.aliveLocked(); m.mu.RUnlock() }); n != 0 {
		t.Fatalf("counts allocs %v", n)
	}
	if n := testing.AllocsPerRun(200, func() { m.mu.RLock(); m.pickLockedFP(0); m.mu.RUnlock() }); n != 0 {
		t.Fatalf("pickLockedFP allocs %v", n)
	}
	setZDU(t, true)
	mm, _ := zduPool(3, 2)
	if n := testing.AllocsPerRun(200, func() { zduRefresh(mm) }); n != 0 {
		t.Fatalf("zduRefreshLocked allocs %v (key on, cached current)", n)
	}
}

// Exit side: the live links counted by the Iran instance they announced.
func TestZDUExitGens(t *testing.T) {
	setZDU(t, true)
	mk := func(in zduInst) *linkMeter {
		m := &linkMeter{}
		pi := peerInfo{V2: true}
		if in.known() {
			pi.Ext, pi.Inst = capExtZDU, in
		}
		m.peerInfo.Store(&pi)
		return m
	}
	p := &linkPeers{}
	if c, a, b := p.zduGens(); c.known() || a != 0 || b != 0 {
		t.Fatal("empty set")
	}
	for i := 0; i < 3; i++ {
		p.add(mk(zduInstOld))
	}
	p.add(mk(zduInst{})) // pre-ZDU / no answer: in neither count
	if c, a, b := p.zduGens(); c != zduInstOld || a != 3 || b != 0 {
		t.Fatalf("one generation: %v %d %d", c, a, b)
	}
	n1, n2 := mk(zduInstNew), mk(zduInstNew)
	p.add(n1)
	p.add(n2)
	if c, a, b := p.zduGens(); c != zduInstNew || a != 2 || b != 3 {
		t.Fatalf("mixed: %v %d %d, want new 2 3", c, a, b)
	}
	old := zduExitPeers.Swap(p)
	seen := zduExitSeen.Load()
	t.Cleanup(func() { zduExitPeers.Store(old); zduExitSeen.Store(seen) })
	zduExitSeen.Store(false)
	if s := zduCountsLine(); !strings.Contains(s, " zdu_exit_cur=2 zdu_exit_old=3") {
		t.Fatalf("line %q", s)
	}
	p.remove(n1)
	p.remove(n2) // the new Iran process died: back to the old generation
	if c, a, b := p.zduGens(); c != zduInstOld || a != 3 || b != 0 {
		t.Fatalf("fallback: %v %d %d", c, a, b)
	}
	setZDU(t, false)
	if s := zduCountsLine(); s != "" {
		t.Fatalf("key off: %q", s)
	}
}

// The fast pickers (b325 storm pick, b327 list) follow the marks, also when
// they change after a list was built.
func TestZDUFastPickersSkipOld(t *testing.T) {
	setZDU(t, true)
	m, _ := zduPool(4, 0)
	m.mu.RLock()
	if k, _, ok := m.b327Key(m.links[0], ""); !ok {
		t.Fatalf("one generation: old link not a candidate (%v)", k)
	}
	m.mu.RUnlock()
	ep := m.b327.epoch.Load()
	zduAddLinks(m, zduLink(zduInstNew))
	if m.b327.epoch.Load() == ep {
		t.Fatal("no b327 bump")
	}
	ep = m.b327.epoch.Load()
	zduRefresh(m) // nothing changed: no bump
	if m.b327.epoch.Load() != ep {
		t.Fatal("refresh without change bumped the list")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, ml := range m.links {
		_, _, ok := m.b327Key(ml, "")
		if ok != !ml.zduOld {
			t.Fatalf("b327Key link %d ok=%v zduOld=%v", ml.id, ok, ml.zduOld)
		}
	}
	for i := 0; i < 10; i++ {
		if ml, _ := m.pickLockedFP(0); ml == nil || ml.zduOld {
			t.Fatal("pickLockedFP chose an old link")
		}
	}
}

// A pool that is done (Run returned) takes its share out of the gauges: the
// leak the tests showed (pools dropped with marks) cannot outlive a pool.
func TestZDUForgetGauge(t *testing.T) {
	setZDU(t, true)
	zduResetGauge(t)
	m, _ := zduPool(3, 1)
	if p := zduGauge.peerOld.Load(); p != 3 {
		t.Fatalf("peerold %d, want 3", p)
	}
	m.zduForget()
	if p, c := zduGauge.peerOld.Load(), zduGauge.oldConns.Load(); p != 0 || c != 0 {
		t.Fatalf("after forget: peerold %d old_conns %d", p, c)
	}
	zduRefresh(m) // a later refresh counts the pool again, exactly
	if p := zduGauge.peerOld.Load(); p != 3 {
		t.Fatalf("refresh after forget: peerold %d, want 3", p)
	}
	m.zduForget()
}
