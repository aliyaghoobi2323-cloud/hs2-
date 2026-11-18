package engine

import (
	"math/rand"
	"testing"
	"time"
)

func b309Stick(l *l3Link, now int64) {
	l.stall.checkAt.Store(0)
	b308Stick(l, now)
}

func b309Clear(l *l3Link) {
	l.txStat = nil
	l.stall.checkAt.Store(0)
	l.stall.stuck.Store(false)
}

func b309Key(t testing.TB, on bool) {
	o := b309On.Load()
	b309On.Store(on)
	t.Cleanup(func() { b309On.Store(o) })
}

// b309Ref is b309Scan by brute force: every valid link on another path,
// sorted by rank, light first.
func b309Ref(links []*l3Link, h uint32, first *l3Link, a, b string, now int64) *l3Link {
	for _, heavy := range []bool{false, true} {
		var best *l3Link
		var bw uint32
		for _, l := range links {
			if l == first || l.path == "" || l.path == first.path || l.path == a || l.path == b ||
				l.isHeavy() != heavy || !l.Alive() || !l.canDup() || l3OnCut(l) || l.stall.stuck.Load() {
				continue
			}
			if w := mix32(h ^ l.id); best == nil || w > bw {
				best, bw = l, w
			}
		}
		if best != nil {
			return best
		}
	}
	return nil
}

// The scan returns the best-ranked valid other-path link for random sets of
// stuck, heavy, cut and avoided paths, the same as brute force.
func TestB309ScanMatchesRankOrder(t *testing.T) {
	b303Keys(t, true, true)
	withL3CutAvoid(t, true)
	clock := int64(time.Hour)
	links := b303Links(130, 12)
	s := laneSet(&clock, links...)
	r := rand.New(rand.NewSource(309))
	for it := 0; it < 300; it++ {
		for _, l := range links {
			if r.Intn(3) == 0 {
				b309Stick(l, clock)
			} else {
				b309Clear(l)
			}
			if r.Intn(4) == 0 {
				l.heavy.Store(1)
			} else {
				l.heavy.Store(0)
			}
		}
		var cut []string
		for p := 0; p < 12; p++ {
			if r.Intn(4) == 0 {
				cut = append(cut, links[p].path)
			}
		}
		setCut(&s.cut, cut...)
		first := links[r.Intn(len(links))]
		a, b := links[r.Intn(12)].path, ""
		if r.Intn(2) == 0 {
			b = links[r.Intn(12)].path
		}
		h := r.Uint32()
		if got, want := b309Scan(links, h, first, a, b, clock), b309Ref(links, h, first, a, b, clock); got != want {
			t.Fatalf("it %d: scan %v, rank order %v", it, got, want)
		}
	}
}

// b278's pick on the first's path whose single top other-path link is stuck:
// b309 takes the next valid other-path link; key off it stays on the first's
// path (ab308 b303 R1A t=330).
func TestB309SecondOffFirstPathPastStuckTop(t *testing.T) {
	b303Keys(t, true, true)
	clock := int64(time.Hour)
	links := b303Links(60, 6)
	first := links[0]
	hits := 0
	for h := uint32(0); h < 4096 && hits < 50; h++ {
		for _, l := range links {
			b309Clear(l)
		}
		second := dupSecond(links, h, first)
		if second.path != first.path {
			continue
		}
		top := dupSecondWhere(links, h, first, func(l *l3Link) bool { return l.path != first.path })
		b309Stick(top, clock)
		b309Key(t, false)
		off, _ := b278SecondMoved(links, h, first, second, clock)
		b309Key(t, true)
		on, moved := b278SecondMoved(links, h, first, second, clock)
		if off == nil || off.path != first.path {
			continue // old logic found another way; not this case
		}
		hits++
		if on == nil || on.path == first.path || on == top || !moved {
			t.Fatalf("h %d: second %v moved %v (first %s, stuck top %v)", h, on, moved, first.path, top)
		}
	}
	if hits == 0 {
		t.Fatal("no hash hit the single-candidate case: the test does not bite")
	}
}

// A dark on the SECOND copy's path: once its kept link is stuck the flow's
// second copy never goes back to that path (its idle links read healthy) nor
// onto the first's path for the hold; after the hold the plain pick again.
func TestB309SecondPathDarkAvoided(t *testing.T) {
	b303Keys(t, true, true)
	b308Key(t, true)
	b309Key(t, true)
	clock := int64(time.Hour)
	links := b303Links(60, 6)
	s := laneSet(&clock, links...)
	first := links[0]
	n := 0
	for h := uint32(0); h < 256; h++ {
		for _, l := range links {
			b309Clear(l)
		}
		e := &s.lane.flows[h&(l3LaneSlots-1)]
		*e = l3Flow{key: h, used: true, inter: true}
		sec := s.dupSecondFor(e, h, first, clock)
		if sec == nil || sec.path == first.path {
			continue
		}
		n++
		dark := sec.path
		t0 := clock
		b309Stick(sec, clock)
		for i := 0; i < 40; i++ {
			clock += int64(50 * time.Millisecond)
			got := s.dupSecondFor(e, h, first, clock)
			if got == nil || got.path == dark || got.path == first.path {
				t.Fatalf("h %d packet %d: second on %v (dark %s, first %s)", h, i, got, dark, first.path)
			}
		}
		clock = t0 + b308Hold + int64(time.Second)
		if got := s.dupSecondFor(e, h, first, clock); got == nil || got.path == first.path {
			t.Fatalf("h %d after hold: %v", h, got)
		}
	}
	if n < 100 {
		t.Fatalf("only %d flows tested", n)
	}
}

// The kept second link expires (b303Refresh) while its socket went stuck
// since the last stuck read (cached verdict false): the hold is still set,
// so the next refresh does not return to an idle link of the dark path
// (ab309 R2B t=210).
func TestB309HoldOnRefreshStaleVerdict(t *testing.T) {
	b303Keys(t, true, true)
	b308Key(t, true)
	b309Key(t, true)
	clock := int64(time.Hour)
	links := b303Links(60, 6)
	s := laneSet(&clock, links...)
	first := links[0]
	n := 0
	for h := uint32(0); h < 256; h++ {
		for _, l := range links {
			b309Clear(l)
		}
		e := &s.lane.flows[h&(l3LaneSlots-1)]
		*e = l3Flow{key: h, used: true, inter: true}
		sec := s.dupSecondFor(e, h, first, clock)
		if sec == nil || sec.path == first.path {
			continue
		}
		n++
		dark := sec.path
		// stuck now, but the last read (just above) said not stuck
		sec.txStat = func() (tcpStat, bool) { return tcpStat{unacked: 3, backoff: 1, rttUs: 80000}, true }
		clock += b303Refresh + int64(10*time.Millisecond)
		for i := 0; i < 50; i++ {
			got := s.dupSecondFor(e, h, first, clock)
			if got == nil || got.path == dark || got.path == first.path {
				t.Fatalf("h %d packet %d: second on %v (dark %s, first %s)", h, i, got, dark, first.path)
			}
			clock += int64(50 * time.Millisecond)
		}
	}
	if n < 100 {
		t.Fatalf("only %d flows tested", n)
	}
}

// Cut paths and avoided paths are skipped; a heavy valid link beats a cut
// light one.
func TestB309SkipsCutPrefersLight(t *testing.T) {
	b303Keys(t, true, true)
	withL3CutAvoid(t, true)
	clock := int64(time.Hour)
	links := b303Links(4, 4) // p0..p3, one link each
	s := laneSet(&clock, links...)
	first := links[0]
	setCut(&s.cut, "p1")
	links[2].heavy.Store(1)
	if got := b309Scan(links, 7, first, "p3", "", clock); got != links[2] {
		t.Fatalf("got %v, want the heavy p2 link", got)
	}
	if got := b309Scan(links, 7, first, "", "", clock); got != links[3] {
		t.Fatalf("got %v, want the light p3 link", got)
	}
}

// Nothing valid elsewhere (two paths, the other all stuck): the old pick
// stands and is never nil.
func TestB309FallbackNeverStalls(t *testing.T) {
	b303Keys(t, true, true)
	b308Key(t, true)
	b309Key(t, true)
	clock := int64(time.Hour)
	links := b303Links(8, 2)
	s := laneSet(&clock, links...)
	first := links[0]
	for _, l := range links {
		if l.path != first.path {
			b309Stick(l, clock)
		}
	}
	for h := uint32(0); h < 256; h++ {
		e := &s.lane.flows[h&(l3LaneSlots-1)]
		*e = l3Flow{key: h, used: true, inter: true}
		for i := 0; i < 3; i++ {
			clock += int64(10 * time.Millisecond)
			if got := s.dupSecondFor(e, h, first, clock); got == nil {
				t.Fatalf("h %d: second copy dropped", h)
			}
		}
	}
}

// The scan allocates nothing.
func TestB309ScanNoAlloc(t *testing.T) {
	b303Keys(t, true, true)
	clock := int64(time.Hour)
	links := b303Links(130, 12)
	for i, l := range links {
		if i%3 == 0 {
			b309Stick(l, clock)
		}
	}
	first := links[0]
	if a := testing.AllocsPerRun(200, func() {
		b309Scan(links, 12345, first, "p3", "p5", clock)
		b309Fix(links, 12345, first, links[12], "p3", "p5", clock)
	}); a != 0 {
		t.Fatalf("%v allocs per scan", a)
	}
}

func BenchmarkB309Scan(b *testing.B) {
	b303Keys(b, true, true)
	clock := int64(time.Hour)
	links := b303Links(130, 12)
	first := links[0]
	for i := 0; i < b.N; i++ {
		b309Scan(links, uint32(i), first, "p3", "", clock)
	}
}
