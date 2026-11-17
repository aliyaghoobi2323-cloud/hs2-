package engine

import "testing"

func withL3CutAvoid(t *testing.T, on bool) {
	t.Helper()
	old := l3CutAvoid
	l3CutAvoid = on
	t.Cleanup(func() { l3CutAvoid = old })
}

// setCut stores the cut paths of one tunnel's set (nil: none).
func setCut(c *l3CutSet, cut ...string) {
	var m map[string]bool
	if len(cut) > 0 {
		m = map[string]bool{}
		for _, p := range cut {
			m[p] = true
		}
	}
	c.p.Store(&m)
}

// HS2_LB_RESCUE_CUT_L3: a ping flow on a link of a path that is then cut
// moves to a link off the cut paths at once; key off it stays.
func TestL3LaneLeavesCutPath(t *testing.T) {
	for _, on := range []bool{false, true} {
		a, b := laneLink(1, 0), laneLink(2, 0)
		a.path, b.path = "x>1", "x>2"
		var clock int64 = 1
		s := laneSet(&clock, a, b)
		withL3CutAvoid(t, on)
		onA := 0
		var pkts [][]byte
		for i := 0; i < 400; i++ {
			p := icmpPacket(byte(i>>8), byte(i))
			if s.pick(p) == a {
				onA++
				pkts = append(pkts, p)
			}
		}
		if onA < 50 {
			t.Fatalf("only %d flows on a: the test does not bite", onA)
		}
		setCut(&s.cut, "x>1")
		for _, p := range pkts {
			got := s.pick(p)
			if on && got != b || !on && got != a {
				t.Fatalf("key %v: flow on %p after a path was cut (a %p)", on, got, a)
			}
		}
	}
}

// stuck60: the cut set is per tunnel. Two tunnels in one process whose links
// share a path key: a cut published by tunnel 1's judge moves tunnel 1's
// flows only, and tunnel 2's judge publishing "nothing cut" does not undo it.
func TestL3CutPathsPerTunnel(t *testing.T) {
	withL3CutAvoid(t, true)
	var clock int64 = 1
	a1, b1 := laneLink(1, 0), laneLink(2, 0)
	a2, b2 := laneLink(3, 0), laneLink(4, 0)
	a1.path, b1.path, a2.path, b2.path = "x>1", "x>2", "x>1", "x>2"
	s1, s2 := laneSet(&clock, a1, b1), laneSet(&clock, a2, b2)
	if a1.cut != &s1.cut || a2.cut != &s2.cut {
		t.Fatal("add did not point the links at their set")
	}
	j1 := &q2Judge{p: map[string]*q2Path{"x>1": {have: true, cut: true}, "x>2": {have: true}}, cutOut: &s1.cut}
	j2 := &q2Judge{p: map[string]*q2Path{"x>1": {have: true}, "x>2": {have: true}}, cutOut: &s2.cut}
	j1.log = func(string, ...any) {}
	j2.log = j1.log
	j1.publishCutPaths()
	j2.publishCutPaths() // after j1: a process-wide set would be cleared here
	if !l3OnCut(a1) || l3OnCut(b1) {
		t.Fatal("tunnel 1: its cut path not held")
	}
	if l3OnCut(a2) || s2.cut.any() {
		t.Fatal("tunnel 2 reads tunnel 1's cut path")
	}
	for i := 0; i < 400; i++ {
		p := icmpPacket(byte(i>>8), byte(i))
		if s1.pick(p) == a1 {
			t.Fatal("tunnel 1: a flow picked the cut path")
		}
	}
	on2 := 0
	for i := 0; i < 400; i++ {
		if s2.pick(icmpPacket(byte(i>>8), byte(i))) == a2 {
			on2++
		}
	}
	if on2 < 50 {
		t.Fatalf("tunnel 2: only %d flows on x>1, which it does not hold cut", on2)
	}
	// a judge without a set (no TUN) publishes nowhere and does not panic
	(&q2Judge{p: j1.p, log: j1.log}).publishCutPaths()
}

// The judge publishes its cut paths, nothing when every path is cut.
func TestL3CutPathsPublish(t *testing.T) {
	withL3CutAvoid(t, true)
	var c l3CutSet
	j := &q2Judge{p: map[string]*q2Path{"a": {have: true, cut: true}, "b": {have: true}}, cutOut: &c}
	j.log = func(string, ...any) {}
	j.publishCutPaths()
	if !c.has("a") || c.has("b") {
		t.Fatal("cut set")
	}
	j.p["b"].cut = true
	j.publishCutPaths()
	if c.any() {
		t.Fatal("every path cut: published")
	}
}
