package engine

import "testing"

func withCs225(t *testing.T, on bool) {
	t.Helper()
	old := cs225CallPath.Load()
	cs225CallPath.Store(on)
	t.Cleanup(func() { cs225CallPath.Store(old) })
}

// pickCallSecond places a call's sub beside link first n times through
// HS2_MCONN_CALL_AVOID's branch and returns how often each link got it.
func pickCallSecond(t *testing.T, m *LinkManager, ls []*mcLink, first, n int) []int {
	t.Helper()
	got := make([]int, len(ls))
	for i := 0; i < n; i++ {
		l, rel, ok := m.pickPlaneCall(planeLight, []any{Link(ls[first]), callAvoidMark{}})
		if !ok {
			t.Fatal("no link picked")
		}
		got[linkIndex(ls, l)]++
		rel()
	}
	if got[first] != 0 {
		t.Fatalf("picked the avoided link: %v", got)
	}
	return got
}

// N2: the call's copy sub went to another link on the primary's path.
func TestCs225CallCopyOffPath(t *testing.T) {
	withDupPaths(t, false) // the production default: no HS2_DUP_PATHS
	withCs225(t, true)
	m, ls := pathLinks("a>x", "a>x", "a>x", "b>y", "b>y")
	s0, o0 := cs225Stat.same.Load(), cs225Stat.off.Load()
	got := pickCallSecond(t, m, ls, 0, 40)
	if got[1]+got[2] != 0 || got[3]+got[4] != 40 {
		t.Fatalf("call copy not on the other path: %v", got)
	}
	if cs225Stat.off.Load()-o0 != 40 || cs225Stat.same.Load()-s0 != 0 {
		t.Fatalf("counters off %d same %d", cs225Stat.off.Load()-o0, cs225Stat.same.Load()-s0)
	}

	// Every link on b carries a download: still b (another path beats a
	// light link on the primary's path).
	m, ls = pathLinks("a>x", "a>x", "b>y", "b>y")
	m.links[2].heavy, m.links[3].heavy = 1, 1
	if got := pickCallSecond(t, m, ls, 0, 20); got[2]+got[3] != 20 {
		t.Fatalf("heavy other path not taken: %v", got)
	}

	// b sick (most links degraded / suspect): no preference, the old pick
	// (link 1 on a or b's one healthy link 4, ties at random).
	m, ls = pathLinks("a>x", "a>x", "b>y", "b>y", "b>y")
	withCs286(t, false) // cs286 takes b's healthy link 4 (TestCs286SickOtherPath)
	m.links[2].degraded, m.links[3].suspect = true, true
	if got := pickCallSecond(t, m, ls, 0, 40); got[1] == 0 || got[2]+got[3] != 0 {
		t.Fatalf("sick other path preferred: %v", got)
	}

	// One path only: the old pick, counted as same.
	m, ls = pathLinks("a>x", "a>x", "a>x")
	s0 = cs225Stat.same.Load()
	if got := pickCallSecond(t, m, ls, 0, 10); got[1]+got[2] != 10 {
		t.Fatalf("one path: %v", got)
	}
	if cs225Stat.same.Load()-s0 != 10 {
		t.Fatal("one path: not counted as same")
	}
}

func TestCs225KeyOff(t *testing.T) {
	withDupPaths(t, false)
	withCs225(t, false)
	withCs286(t, false)
	m, ls := pathLinks("a>x", "a>x", "a>x", "b>y", "b>y")
	got := pickCallSecond(t, m, ls, 0, 200)
	if got[1]+got[2] == 0 {
		t.Fatalf("key off: never the primary's path (old pick ignores paths): %v", got)
	}
}
