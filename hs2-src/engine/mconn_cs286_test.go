package engine

import "testing"

func withCs286(t *testing.T, on bool) {
	t.Helper()
	old := cs286Apart.Load()
	cs286Apart.Store(on)
	t.Cleanup(func() { cs286Apart.Store(old) })
}

func lowLinks(t *testing.T, ls []*mcLink, idx ...int) {
	t.Helper()
	for _, i := range idx {
		k := Link(ls[i])
		rescueLow.Store(k, true)
		t.Cleanup(func() { rescueLow.Delete(k) })
	}
}

// sk280 SN4: the only other path is lowered (lb auto cut verdict): b280 put
// the call's copy beside the primary; cs286 puts it on the lowered path.
func TestCs286CallCopyLoweredOtherPath(t *testing.T) {
	withDupPaths(t, false)
	withCs226(t, true)
	withCs229(t, true)
	for _, on := range []bool{false, true} {
		withCs286(t, on)
		m, ls := pathLinks("a>x", "a>x", "a>x", "b>y", "b>y")
		lowLinks(t, ls, 3, 4)
		mv, lo := cs286Stat.moved.Load(), cs286Stat.moveLo.Load()
		got := pickCallSecond(t, m, ls, 0, 30)
		if !on {
			if got[1]+got[2] == 0 {
				t.Fatalf("off: expected the b280 same-path pick, got %v", got)
			}
			continue
		}
		if got[1]+got[2] != 0 || got[3]+got[4] != 30 {
			t.Fatalf("call copy beside the primary while a lowered path exists: %v", got)
		}
		// the original pick may land on b itself (kept); every same-path
		// pick was moved, onto the lowered path
		if d, l := cs286Stat.moved.Load()-mv, cs286Stat.moveLo.Load()-lo; d == 0 || d != l {
			t.Fatalf("moved %d low %d", d, l)
		}
	}
}

// The call placement's light pick finds nothing (every link carries a
// download): b280 fell back to the plain pickPlane with no path rule.
func TestCs286CallCopyPlainFallback(t *testing.T) {
	withDupPaths(t, false)
	withCs226(t, true)
	withCs229(t, true)
	withCs286(t, true)
	m, ls := pathLinks("a>x", "a>x", "a>x", "b>y", "b>y")
	for i := range m.links {
		m.links[i].heavy = 1
	}
	m.links[1].heavy, m.links[2].heavy = 0, 0 // the primary's path is the light one
	if got := pickCallSecond(t, m, ls, 0, 30); got[1]+got[2] != 0 {
		t.Fatalf("plain fallback put the copy beside the primary: %v", got)
	}
	// cs225 placement (MINPLACE off) too
	withCs229(t, false)
	withCs225(t, true)
	m, ls = pathLinks("a>x", "a>x", "a>x", "b>y", "b>y")
	lowLinks(t, ls, 3, 4)
	if got := pickCallSecond(t, m, ls, 0, 30); got[1]+got[2] != 0 {
		t.Fatalf("cs225 same-path fallback: %v", got)
	}
}

// Prefer a healthy other path over a lowered one; one path only: the pick
// stays (nocand); draining links on the other path are never used.
func TestCs286PreferenceAndNoCand(t *testing.T) {
	withDupPaths(t, false)
	withCs226(t, true)
	withCs229(t, true)
	withCs286(t, true)
	m, ls := pathLinks("a>x", "a>x", "b>y", "c>z")
	lowLinks(t, ls, 2)
	// (the original pick itself may still choose b now and then: kept)
	if got := pickCallSecond(t, m, ls, 0, 30); got[1] != 0 || got[3] < 20 {
		t.Fatalf("lowered path beat a healthy one: %v", got)
	}
	// b sick (degraded/suspect) beside one healthy b link: the healthy one
	m, ls = pathLinks("a>x", "a>x", "b>y", "b>y", "b>y")
	m.links[2].degraded, m.links[3].suspect = true, true
	if got := pickCallSecond(t, m, ls, 0, 30); got[1]+got[3] != 0 || got[4] < 20 {
		t.Fatalf("sick other path: %v", got)
	}
	m, ls = pathLinks("a>x", "a>x", "a>x")
	n0 := cs286Stat.nocand.Load()
	if got := pickCallSecond(t, m, ls, 0, 10); got[1]+got[2] != 10 {
		t.Fatalf("single path: %v", got)
	}
	if cs286Stat.nocand.Load()-n0 != 10 {
		t.Fatal("nocand not counted")
	}
	m, ls = pathLinks("a>x", "a>x", "b>y")
	m.links[2].draining = true
	if got := pickCallSecond(t, m, ls, 0, 10); got[2] != 0 {
		t.Fatalf("copy on a draining link: %v", got)
	}
}
