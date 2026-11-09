package engine

import (
	"context"
	"testing"
	"time"
)

func withCutDrain(t *testing.T) {
	t.Helper()
	old := lbCutDrain
	lbCutDrain = true
	t.Cleanup(func() { lbCutDrain = old })
}

func cutLink(m *LinkManager, path string, open int) *managedLink {
	f := newMeteredFake()
	f.m.path = path
	ml := addManaged(m, f)
	m.mu.Lock()
	ml.open = open
	m.mu.Unlock()
	return ml
}

func TestCutPathsFromReport(t *testing.T) {
	r := LBReport{Auto: true, Paths: []LBPeerPath{
		{Local: "k1", Remote: "i1", Target: 2, Cut: true},
		{Local: "", Remote: "i2", Target: 0, Cut: true},
		{Local: "k1", Remote: "i3", Target: 40},
	}}
	c := cutPathsFromReport(r)
	if len(c) != 2 || c["i1>k1"] != 2 || c["i2>*"] != lb2Keep {
		t.Fatalf("cut map %v", c)
	}
	if n, ok := cutKeep(c, "i2>k9"); !ok || n != lb2Keep {
		t.Fatal("default-route path not matched by its edge IP")
	}
	if _, ok := cutKeep(c, "i3>k1"); ok {
		t.Fatal("a good path matched as cut")
	}
	r.Paths[2].Cut = true
	if c := cutPathsFromReport(r); c != nil {
		t.Fatalf("every path cut: want no drain, got %v", c)
	}
}

// The production case: a cut path keeps 12 busy links for a target of 2. With
// the key on, the 10 beyond the target retire at once (busy or not), the 2
// with the fewest connections stay as probes, the exit is asked for 10
// replacements, the retired ones never come back while the path is cut, and
// once it is no longer cut they are ordinary retiring links again.
func TestCutDrainRetiresBusySurplus(t *testing.T) {
	withCutDrain(t)
	m, clk, _ := newV2Manager(nil, 1, 64, true)
	cut := map[string]int{"i1>k1": 2}
	m.cutPaths = func() map[string]int { return cut }
	var bad, good []*managedLink
	for i := 0; i < 12; i++ {
		bad = append(bad, cutLink(m, "i1>k1", 20+i))
	}
	for i := 0; i < 12; i++ {
		good = append(good, cutLink(m, "i2>k1", 5))
	}
	ctx := context.Background()
	clk.Advance(time.Second)
	m.reconcile(ctx, 24)
	S, R := poolCounts(m)
	if S != 14 || R != 10 {
		t.Fatalf("after the cut: %d serving, %d retiring; want 14, 10", S, R)
	}
	if isRetiring(m, bad[0]) || isRetiring(m, bad[1]) || !isRetiring(m, bad[11]) {
		t.Fatal("kept the wrong links: want the two with the fewest connections")
	}
	for _, ml := range good {
		if isRetiring(m, ml) {
			t.Fatal("a good path's link retired")
		}
	}
	m.mu.Lock()
	rep := m.replacingLocked()
	m.mu.Unlock()
	if rep != 10 {
		t.Fatalf("replacements asked of the exit: %d, want 10", rep)
	}
	// still short of 24 serving: the cut-drained links must not come back
	m.reconcile(ctx, 24)
	if S, R := poolCounts(m); S != 14 || R != 10 {
		t.Fatalf("cut-drained links came back: %d serving, %d retiring", S, R)
	}
	// the path recovers: they are retiring links like any other and come back
	cut = nil
	m.reconcile(ctx, 24)
	if S, R := poolCounts(m); S != 24 || R != 0 {
		t.Fatalf("after recovery: %d serving, %d retiring; want 24, 0", S, R)
	}
	m.mu.Lock()
	rep = m.replacingLocked()
	m.mu.Unlock()
	if rep != 0 {
		t.Fatalf("replacements still asked after recovery: %d", rep)
	}
}

// Key off: nothing changes (the cut path keeps its links).
func TestCutDrainOff(t *testing.T) {
	old := lbCutDrain
	lbCutDrain = false
	t.Cleanup(func() { lbCutDrain = old })
	m, clk, _ := newV2Manager(nil, 1, 64, true)
	m.cutPaths = func() map[string]int { return map[string]int{"i1>k1": 2} }
	for i := 0; i < 6; i++ {
		cutLink(m, "i1>k1", 3)
	}
	clk.Advance(time.Second)
	m.reconcile(context.Background(), 6)
	if S, R := poolCounts(m); S != 6 || R != 0 {
		t.Fatalf("key off: %d serving, %d retiring; want 6, 0", S, R)
	}
}

// A link set retiring without a time (a recovered drain) no longer logs
// "retiring 2562047h47m": its age counts from the tick that finds it.
func TestDrainTickZeroRetireSince(t *testing.T) {
	m, clk, lg := newV2Manager(nil, 1, 8, false)
	ml := cutLink(m, "", 0)
	m.mu.Lock()
	ml.retiring, ml.open = true, 1
	ml.users.Store(1)
	m.mu.Unlock()
	clk.Advance(time.Second)
	m.drainTick()
	m.mu.Lock()
	z := ml.retireSince.IsZero()
	m.mu.Unlock()
	if z {
		t.Fatal("retireSince left zero")
	}
	clk.Advance(heldLogFirst + time.Second)
	m.drainTick()
	if lg.count("2562047h") > 0 || lg.count("retiring ") == 0 {
		t.Fatalf("want a held line without an overflowed age: %s", lg)
	}
}

// The exit's rebalance keeps a cut path's last lb2Keep idle links (its probes)
// with HS2_LB_CUT_DRAIN: in the lab it retired them one by one to zero.
func TestCutDrainRebalanceKeepsProbes(t *testing.T) {
	withAuto2(t)
	old := lbCutDrain
	t.Cleanup(func() { lbCutDrain = old })
	for _, tc := range []struct {
		on        bool
		idle, ret int
	}{{true, 2, 0}, {true, 3, 1}, {false, 2, 1}} {
		lbCutDrain = tc.on
		b, _ := NewLB(twoPaths(50, 50), nil)
		b.SetAuto(true)
		retired := 0
		for i := 0; i < 8+tc.idle; i++ {
			j := 0
			if i >= 8 {
				j = 1
			}
			b.mu.Lock()
			b.links[j]++ // as acquire would, on the chosen path
			b.mu.Unlock()
			k := new(int)
			b.result(j, false, k, nil)
			b.attach(k, lbHooks{retire: func() bool { retired++; b.release(k); return true }})
		}
		b.mu.Lock()
		b.links[1] += 6 // busy cut-drained links: counted, never idle
		b.mult[1] = 0.1
		b.judge = newQ2Judge(lb2Floor, "local", "remote", b.log)
		b.judge.p[b.paths[1].Name()] = &q2Path{cut: true}
		b.mu.Unlock()
		b.rebalance2()
		if retired != tc.ret {
			t.Fatalf("on=%v idle=%d: retired %d, want %d", tc.on, tc.idle, retired, tc.ret)
		}
	}
}

// At the exit's ceiling no replacement can come: only as many links drain as
// there is room for (the lab at max_links lost 33 serving slots), and a cut
// path whose probes the exit retired gets cut-drained links back in service.
func TestCutDrainRoomAndProbes(t *testing.T) {
	withCutDrain(t)
	old := cutDrainSlackPct
	cutDrainSlackPct = 0
	t.Cleanup(func() { cutDrainSlackPct = old })
	m, clk, _ := newV2Manager(nil, 1, 27, true) // ceiling 27: room for 3
	cut := map[string]int{"i1>k1": 2}
	m.cutPaths = func() map[string]int { return cut }
	var bad []*managedLink
	for i := 0; i < 12; i++ {
		bad = append(bad, cutLink(m, "i1>k1", 20+i))
	}
	for i := 0; i < 12; i++ {
		cutLink(m, "i2>k1", 5)
	}
	ctx := context.Background()
	clk.Advance(time.Second)
	m.reconcile(ctx, 24)
	if S, R := poolCounts(m); S != 21 || R != 3 {
		t.Fatalf("at the ceiling: %d serving, %d retiring; want 21, 3", S, R)
	}
	// The exit retires the two idle probes; two cut-drained links come back.
	for _, ml := range bad[:9] {
		ml.link.Close()
	}
	clk.Advance(time.Second)
	m.reconcile(ctx, 24)
	n := 0
	for _, ml := range bad[9:] {
		if !isRetiring(m, ml) {
			n++
		}
	}
	if n != lb2Keep {
		t.Fatalf("cut path serving %d after its probes went, want %d", n, lb2Keep)
	}
}
