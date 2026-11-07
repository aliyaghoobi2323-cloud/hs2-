package engine

import (
	"net"
	"testing"
)

func withDupPaths(t *testing.T, on bool) {
	t.Helper()
	old := dupPathsOn
	dupPathsOn = on
	t.Cleanup(func() { dupPathsOn = old })
}

// pathLinks is a pool of light links (all reading mconn) on the given paths.
func pathLinks(paths ...string) (*LinkManager, []*mcLink) {
	m, ls := pickLinks(make([]int, len(paths)), make([]bool, len(paths)))
	for i, p := range paths {
		ls[i].mtr.path = p
	}
	return m, ls
}

// pickSecond picks a light sub beside link first n times and returns how
// often each link got it.
func pickSecond(t *testing.T, m *LinkManager, ls []*mcLink, first, n int) []int {
	t.Helper()
	got := make([]int, len(ls))
	for i := 0; i < n; i++ {
		l, rel, ok := m.pickPlane(planeLight, []any{Link(ls[first])})
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

func TestDupPathsMConnOtherPath(t *testing.T) {
	withDupPaths(t, true)
	m, ls := pathLinks("a>x", "a>x", "a>x", "b>y", "b>y")
	before := Observed()
	got := pickSecond(t, m, ls, 0, 40)
	if got[1]+got[2] != 0 || got[3]+got[4] != 40 {
		t.Fatalf("second sub not on the other path: %v", got)
	}
	d := Observed().Sub(before)
	if d.MConnPathOther != 40 || d.MConnPathSame != 0 {
		t.Fatalf("counters other=%d same=%d, want 40/0", d.MConnPathOther, d.MConnPathSame)
	}
	// The first sub on path b: the second goes to a.
	got = pickSecond(t, m, ls, 3, 20)
	if got[4] != 0 {
		t.Fatalf("second sub beside b went to b: %v", got)
	}
}

func TestDupPathsMConnFallback(t *testing.T) {
	withDupPaths(t, true)
	// One path only: the usual pick (link 1 or 2), counted as same.
	m, ls := pathLinks("a>x", "a>x", "a>x")
	before := Observed()
	got := pickSecond(t, m, ls, 0, 10)
	if got[1]+got[2] != 10 {
		t.Fatalf("one path: %v", got)
	}
	if d := Observed().Sub(before); d.MConnPathSame != 10 || d.MConnPathOther != 0 {
		t.Fatalf("one path: counters other=%d same=%d", d.MConnPathOther, d.MConnPathSame)
	}

	// The other path is sick (most of its links degraded or suspect): no
	// preference, the usual pick (link 1 or b's one healthy link).
	m, ls = pathLinks("a>x", "a>x", "b>y", "b>y", "b>y")
	m.links[2].degraded, m.links[3].suspect = true, true
	if got := pickSecond(t, m, ls, 0, 40); got[1] == 0 || got[2]+got[3] != 0 {
		t.Fatalf("sick other path still preferred: %v", got)
	}
	// A healthy path, but its only healthy link is draining: not on b.
	m, ls = pathLinks("a>x", "a>x", "b>y")
	m.links[2].draining = true
	if got := pickSecond(t, m, ls, 0, 10); got[1] != 10 {
		t.Fatalf("draining link on the other path taken: %v", got)
	}
	// The balancer cooled b: no preference, the usual pick.
	m, ls = pathLinks("a>x", "a>x", "b>y", "b>y")
	m.pathCooled = func(l Link) bool { return l == Link(ls[2]) || l == Link(ls[3]) }
	before = Observed()
	if got := pickSecond(t, m, ls, 0, 30); got[1] == 0 {
		t.Fatalf("cooled path still preferred: %v", got)
	}
	if d := Observed().Sub(before); d.MConnPathSame == 0 {
		t.Fatal("cooled path: no pick counted as same")
	}
	// Unknown paths: the usual pick.
	m, ls = pathLinks("", "", "")
	if got := pickSecond(t, m, ls, 0, 10); got[1]+got[2] != 10 {
		t.Fatalf("unknown paths: %v", got)
	}
}

func TestDupPathsMConnOff(t *testing.T) {
	withDupPaths(t, false)
	m, ls := pathLinks("a>x", "a>x", "a>x", "b>y", "b>y")
	before := Observed()
	got := pickSecond(t, m, ls, 0, 60)
	if got[1]+got[2] == 0 {
		t.Fatalf("key off: never the same path, as if on: %v", got)
	}
	if d := Observed().Sub(before); d.MConnPathOther+d.MConnPathSame != 0 {
		t.Fatal("key off: counted")
	}
	// No avoid (a first sub) and the bulk plane: never filtered.
	withDupPaths(t, true)
	if _, rel, ok := m.pickPlane(planeLight, nil); !ok {
		t.Fatal("no first sub")
	} else {
		rel()
	}
	if _, rel, ok := m.pickPlane(planeBulk, []any{Link(ls[0])}); !ok {
		t.Fatal("no bulk sub")
	} else {
		rel()
	}
}

func pathDupLink(id uint32, heavy int32, path string) *l3Link {
	l := dupLink(id, heavy, true)
	l.path = path
	return l
}

func TestDupPathsL3Second(t *testing.T) {
	first := pathDupLink(1, 0, "a>x")
	sameLight := pathDupLink(2, 0, "a>x")
	otherHeavy := pathDupLink(3, 1, "b>y")
	links := []*l3Link{first, sameLight, otherHeavy}

	withDupPaths(t, true)
	for h := uint32(0); h < 200; h++ {
		if got := dupSecond(links, h*2654435761, first); got != otherHeavy {
			t.Fatalf("h=%d: second on %v, want the other path", h, got.path)
		}
	}
	// No capable live link on another path: as before.
	otherHeavy.dupOK = no
	for h := uint32(0); h < 50; h++ {
		if got := dupSecond(links, h*2654435761, first); got != sameLight {
			t.Fatal("fallback: not the same-path link")
		}
	}
	otherHeavy.dupOK = yes
	otherHeavy.dead.Store(true)
	if got := dupSecond(links, 7, first); got != sameLight {
		t.Fatal("dead other-path link used")
	}
	otherHeavy.dead.Store(false)

	// Key off: the light link always (it was preferred before).
	withDupPaths(t, false)
	for h := uint32(0); h < 50; h++ {
		if got := dupSecond(links, h*2654435761, first); got != sameLight {
			t.Fatal("key off: changed choice")
		}
	}
}

func TestDupPathsL3Counted(t *testing.T) {
	withDupPaths(t, true)
	a, b := pathDupLink(1, 0, "a>x"), pathDupLink(2, 0, "b>y")
	s := dupSet(a, b)
	before := Observed()
	for i := 0; i < 5; i++ {
		pumpOne(s, icmpPacket(0, byte(i)))
	}
	d := Observed().Sub(before)
	if d.L3DupPathOther == 0 || d.L3DupPathSame != 0 {
		t.Fatalf("counters other=%d same=%d", d.L3DupPathOther, d.L3DupPathSame)
	}
}

func TestConnPathKey(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer ln.Close()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if k := connPathKey(c); k != "127.0.0.1>127.0.0.1" {
		t.Fatalf("key %q", k)
	}
	if connPathKey(nil) != "" {
		t.Fatal("nil conn")
	}
}
