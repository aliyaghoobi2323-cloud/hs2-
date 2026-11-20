package engine

import (
	"testing"
	"time"
)

func zduPhaseKeyOn(t *testing.T) {
	setZDU(t, true)
	zduPhaseOn.Store(true)
}

func TestZDUPhaseOf(t *testing.T) {
	for _, c := range []struct {
		n, o, w int
		age     time.Duration
		want    zduPhase
	}{
		{0, 10, 10, 0, zduPhaseNone},
		{3, 0, 10, 0, zduPhaseNone},
		{1, 10, 10, 0, zduPhaseHold},
		{4, 10, 10, 0, zduPhaseHold},
		{5, 10, 10, 0, zduPhaseMix},
		{9, 10, 10, 0, zduPhaseMix},
		{9, 10, 10, time.Minute, zduPhaseFull},
		{5, 10, 10, 31 * time.Second, zduPhaseMix},  // review L4: half the old count is not enough at 30 s
		{8, 10, 10, 31 * time.Second, zduPhaseFull}, // 3/4 of full
		{5, 10, 10, 61 * time.Second, zduPhaseFull}, // hard cap 2 x zduMixMax
		{10, 10, 10, 0, zduPhaseFull},
		{2, 10, 3, 0, zduPhaseMix},  // need min(5, 3) = 3? no: 2 < 3 -> see next
		{3, 10, 3, 0, zduPhaseFull}, // full = min(10, 3)
		{1, 1, 0, 0, zduPhaseFull},
	} {
		if c.n == 2 && c.w == 3 {
			c.want = zduPhaseHold
		}
		if got := zduPhaseOf(c.n, c.o, c.w, c.age); got != c.want {
			t.Errorf("n %d o %d w %d age %v: %v want %v", c.n, c.o, c.w, c.age, got, c.want)
		}
	}
}

// Review issue 3: Iran does not move every new conn to a new exit at its
// first link. Below min(old/2, warm) new links the old generation stays
// current (new links marked, nothing lands there); between that and the old
// count both serve and new conns follow the link ratio; at the old count the
// new generation is current. The old code made the first new link current.
func TestZDUPhaseHoldMixFull(t *testing.T) {
	zduPhaseKeyOn(t)
	m, _ := zduPool(10, 0)
	m.target.Store(10)
	zduAddLinks(m, zduLink(zduInstNew), zduLink(zduInstNew))
	zduRefresh(m)
	if cur := m.zduCurrent(); cur != zduInstOld {
		t.Fatalf("2 of 10 new links: current %v, want the old generation (hold)", cur)
	}
	if n := m.zduPeerOld(); n != 2 {
		t.Fatalf("hold: %d links marked, want the 2 new ones", n)
	}
	e := zduNewEdge(m, 1)
	for i := 0; i < 50; i++ {
		if got := e.zduTarget(); got != zduInstOld {
			t.Fatalf("hold: a new conn aimed at %v", got)
		}
	}
	// 6 new links: mix, nothing marked, ratio 6:10
	for i := 0; i < 4; i++ {
		zduAddLinks(m, zduLink(zduInstNew))
	}
	zduRefresh(m)
	if m.zduMixNow() == nil || m.zduPeerOld() != 0 {
		t.Fatalf("6 of 10: mix %v marked %d", m.zduMixNow(), m.zduPeerOld())
	}
	nNew := 0
	const N = 4000
	for i := 0; i < N; i++ {
		if zduNewEdge(m, uint64(i+2)).zduTarget() == zduInstNew {
			nNew++
		}
	}
	if r := float64(nNew) / N; r < 0.30 || r > 0.45 { // 6/16 = 0.375
		t.Fatalf("mix: %.3f of new conns to the new generation, want ~0.375", r)
	}
	// 10 new: full, the old ones are peerOld
	for i := 0; i < 4; i++ {
		zduAddLinks(m, zduLink(zduInstNew))
	}
	zduRefresh(m)
	if cur := m.zduCurrent(); cur != zduInstNew || m.zduMixNow() != nil || m.zduPeerOld() != 10 {
		t.Fatalf("full: current %v mix %v marked %d", cur, m.zduMixNow(), m.zduPeerOld())
	}
}

// The mix phase is bounded: a new pool that never reaches full takes over
// after zduMixMax.
func TestZDUPhaseMixBounded(t *testing.T) {
	zduPhaseKeyOn(t)
	defer func(d time.Duration) { zduMixMax = d }(zduMixMax)
	zduMixMax = 100 * time.Millisecond
	m, _ := zduPool(10, 6)
	m.target.Store(10)
	zduRefresh(m)
	if m.zduMixNow() == nil {
		t.Fatal("no mix")
	}
	time.Sleep(150 * time.Millisecond)
	zduRefresh(m)
	if m.zduMixNow() == nil { // review L4: 6 of 10 is below 3/4 of full: still mixing
		t.Fatalf("after zduMixMax with 6/10 links: mix ended (current %v)", m.zduCurrent())
	}
	time.Sleep(100 * time.Millisecond)
	zduRefresh(m)
	if cur := m.zduCurrent(); cur != zduInstNew || m.zduPeerOld() != 10 {
		t.Fatalf("after 2 x zduMixMax: current %v marked %d", cur, m.zduPeerOld())
	}
}

// Review issue 6: the exit marks its links of an older Iran instance once
// the new one has half as many, and the L3 picker then skips them.
func TestZDUExitMarkOldIran(t *testing.T) {
	setZDU(t, true)
	t.Cleanup(func() { zduExitMarked.Store(0) })
	p := &linkPeers{}
	mk := func(inst zduInst) *linkMeter {
		m := &linkMeter{}
		m.peerInfo.Store(&peerInfo{V2: true, Ext: capExtZDU, Inst: inst})
		p.add(m)
		return m
	}
	var old, nw []*linkMeter
	for i := 0; i < 6; i++ {
		old = append(old, mk(zduInstOld))
	}
	nw = append(nw, mk(zduInstNew), mk(zduInstNew))
	p.zduExitMark()
	for _, m := range old {
		if m.zduOld.Load() {
			t.Fatal("old Iran links marked at 2 of 6 new (one new link would take every TUN flow)")
		}
	}
	nw = append(nw, mk(zduInstNew))
	p.zduExitMark()
	for _, m := range old {
		if !m.zduOld.Load() {
			t.Fatal("old Iran link not marked at 3 of 6 new")
		}
	}
	for _, m := range nw {
		if m.zduOld.Load() {
			t.Fatal("new link marked")
		}
	}
	if zduExitMarked.Load() != 6 {
		t.Fatal(zduExitMarked.Load())
	}
	// the L3 set picks current links only
	s := &l3Set{}
	var lold, lnew []*l3Link
	for _, m := range old {
		l := &l3Link{zduOld: &m.zduOld}
		lold = append(lold, l)
		s.links = append(s.links, l)
	}
	for _, m := range nw {
		l := &l3Link{zduOld: &m.zduOld}
		lnew = append(lnew, l)
		s.links = append(s.links, l)
	}
	got, cut := s.zduLinks()
	if !cut || len(got) != len(lnew) {
		t.Fatalf("L3 picks from %d links (cut %v), want the %d current", len(got), cut, len(lnew))
	}
	// the new Iran gone: old links unmarked again
	for _, m := range nw {
		p.remove(m)
	}
	p.zduExitMark()
	if old[0].zduOld.Load() || zduExitMarked.Load() != 0 {
		t.Fatal("old links still marked with no new Iran")
	}
	_ = lold
}

func TestZDUExitMarkKeyOff(t *testing.T) {
	setZDU(t, false)
	p := &linkPeers{}
	for _, in := range []zduInst{zduInstOld, zduInstNew, zduInstNew} {
		m := &linkMeter{}
		m.peerInfo.Store(&peerInfo{V2: true, Ext: capExtZDU, Inst: in})
		p.add(m)
	}
	p.zduExitMark()
	for _, m := range p.meters() {
		if m.zduOld.Load() {
			t.Fatal("marked with the key off")
		}
	}
}

// Review issue 6: a draining process notes TUN packets from links; the drain
// end waits until none came for the grace.
func TestZDUL3Quiet(t *testing.T) {
	setZDU(t, true)
	t.Cleanup(func() { zduDraining.Store(false); zduL3RxAt.Store(0) })
	zduL3RxAt.Store(0)
	zduNoteL3Rx() // not draining: nothing noted
	if q, _ := ZDUL3Quiet(time.Now(), time.Second); !q {
		t.Fatal("not quiet with nothing noted")
	}
	zduDraining.Store(true)
	zduNoteL3Rx()
	if q, _ := ZDUL3Quiet(time.Now(), time.Second); q {
		t.Fatal("quiet right after a TUN packet")
	}
	if q, _ := ZDUL3Quiet(time.Now().Add(2*time.Second), time.Second); !q {
		t.Fatal("not quiet after the grace")
	}
}

// zdu389 S9c: after the new generation reached full, a dip of its link count
// below the old count (a recycled link, the storm) keeps it current: no fresh
// mix or hold sends new conns back to the draining exit. Before the fix the
// phase followed the counts and 592 new conns went to the old exit.
func TestZDUPhaseFullLatched(t *testing.T) {
	zduPhaseKeyOn(t)
	m, _ := zduPool(10, 10)
	m.target.Store(10)
	zduRefresh(m)
	if cur := m.zduCurrent(); cur != zduInstNew || m.zduMixNow() != nil {
		t.Fatalf("10 of 10: current %v mix %v, want full", cur, m.zduMixNow())
	}
	drop := func(k int) { // the last k links (new ones) leave
		m.mu.Lock()
		m.links = m.links[:len(m.links)-k]
		m.b327Bump()
		m.mu.Unlock()
		zduRefresh(m)
	}
	for _, k := range []int{1, 5} { // 9 new (was mix), then 4 new (was hold)
		drop(k)
		if cur := m.zduCurrent(); cur != zduInstNew || m.zduMixNow() != nil || m.zduPeerOld() != 10 {
			t.Fatalf("after dropping %d new links: current %v mix %v marked %d, want new current, old marked", k, cur, m.zduMixNow(), m.zduPeerOld())
		}
		for i := 0; i < 200; i++ {
			if got := zduNewEdge(m, uint64(1000*k+i)).zduTarget(); got != zduInstNew {
				t.Fatalf("after dropping %d: a new conn aimed at %v", k, got)
			}
		}
	}
	// the new generation dies: the old one is current again (revert unchanged)
	drop(4)
	if cur := m.zduCurrent(); cur != zduInstOld {
		t.Fatalf("no new link left: current %v, want the old generation", cur)
	}
}
