package engine

import (
	"testing"
	"time"
)

func b308Key(t testing.TB, on bool) {
	o := b308On.Load()
	b308On.Store(on)
	t.Cleanup(func() { b308On.Store(o) })
}

// b308Stick makes l's socket stuck (RTO backoff) and takes the verdict, as
// the lane does before it moves a flow off l.
func b308Stick(l *l3Link, now int64) {
	l.txStat = func() (tcpStat, bool) { return tcpStat{unacked: 3, backoff: 1, rttUs: 80000}, true }
	if !l.txStuck(now) {
		panic("not stuck")
	}
}

// b308DarkHash finds a flow hash whose full second-copy pick for newFirst
// lands on dark (a not-yet-stuck link of the path the first just left).
func b308DarkHash(t testing.TB, links []*l3Link, newFirst *l3Link, dark string, now int64) uint32 {
	for h := uint32(0); h < 1<<16; h++ {
		if l := b303Full(links, h, newFirst, now); l != nil && l.path == dark {
			return h
		}
	}
	t.Fatal("no dark hash")
	return 0
}

// After the first copy leaves a stuck link, the second copy goes to a third
// path, never onto the dark path or the first's path, until the hold ends.
func TestB308AvoidsLeftPath(t *testing.T) {
	b303Keys(t, true, true)
	b308Key(t, true)
	clock := int64(time.Hour)
	links := b303Links(60, 6)
	s := laneSet(&clock, links...)
	old, nf := links[0], links[1] // paths p0 and p1
	b308Stick(old, clock)
	h := b308DarkHash(t, links, nf, old.path, clock)
	e := &s.lane.flows[h&(l3LaneSlots-1)]
	*e = l3Flow{key: h, used: true, inter: true}
	// before the dark: first on old, second kept elsewhere
	s.dupSecondFor(e, h, old, clock)
	m0 := b308Moved.Load()
	for i := 0; i < 40; i++ {
		clock += int64(50 * time.Millisecond)
		got := s.dupSecondFor(e, h, nf, clock)
		if got == nil || got.path == old.path || got.path == nf.path {
			t.Fatalf("packet %d: second on %v (left %s, first %s)", i, got, old.path, nf.path)
		}
	}
	if b308Moved.Load() == m0 {
		t.Fatal("no re-pick counted")
	}
	// hold over: the plain b303 pick again
	clock += b308Hold
	if got, want := s.dupSecondFor(e, h, nf, clock), b303Full(links, h, nf, clock); got != want {
		t.Fatalf("after hold: %v, full pick %v", got, want)
	}
}

// The first leaving a healthy link (heavy, rebalanced) sets no hold.
func TestB308HealthyMoveNoHold(t *testing.T) {
	b303Keys(t, true, true)
	b308Key(t, true)
	clock := int64(time.Hour)
	links := b303Links(60, 6)
	s := laneSet(&clock, links...)
	old, nf := links[0], links[1]
	h := b308DarkHash(t, links, nf, old.path, clock)
	e := &s.lane.flows[h&(l3LaneSlots-1)]
	*e = l3Flow{key: h, used: true, inter: true}
	s.dupSecondFor(e, h, old, clock)
	if got, want := s.dupSecondFor(e, h, nf, clock), b303Full(links, h, nf, clock); got != want || e.dup2.avoid != "" {
		t.Fatalf("healthy move: %v, full pick %v, avoid %q", got, want, e.dup2.avoid)
	}
}

// Two paths only: no third path, the pick is left as it was (never nil,
// never both copies on one path when the other path has a link).
func TestB308TwoPaths(t *testing.T) {
	b303Keys(t, true, true)
	b308Key(t, true)
	clock := int64(time.Hour)
	links := b303Links(20, 2)
	s := laneSet(&clock, links...)
	old, nf := links[0], links[1]
	b308Stick(old, clock)
	h := b308DarkHash(t, links, nf, old.path, clock)
	e := &s.lane.flows[h&(l3LaneSlots-1)]
	*e = l3Flow{key: h, used: true, inter: true}
	s.dupSecondFor(e, h, old, clock)
	clock += int64(10 * time.Millisecond)
	got, want := s.dupSecondFor(e, h, nf, clock), b303Full(links, h, nf, clock)
	if got != want || got == nil || got.path == nf.path {
		t.Fatalf("two paths: %v, full pick %v", got, want)
	}
}

// A same-path fallback during the hold (the only other-path candidate is
// stuck) is moved to a third path; with none it stays.
func TestB308RepickSamePath(t *testing.T) {
	b303Keys(t, true, true)
	clock := int64(time.Hour)
	links := b303Links(30, 3) // paths p0 p1 p2
	// first and same both on p1
	first, same := links[1], links[4]
	if got := b308Repick(links, 9, first, same, "p0", clock); got == nil || got.path != "p2" {
		t.Fatalf("same path: %v, want p2", got)
	}
	if got := b308Repick(links, 9, first, links[0], "p0", clock); got == nil || got.path != "p2" {
		t.Fatalf("avoided path: %v, want p2", got)
	}
	ok := links[2] // p2
	if got := b308Repick(links, 9, first, ok, "p0", clock); got != ok {
		t.Fatalf("third path pick changed: %v", got)
	}
	for _, l := range links {
		if l.path == "p2" {
			b308Stick(l, clock)
		}
	}
	if got := b308Repick(links, 9, first, same, "p0", clock); got != same {
		t.Fatalf("no free third path: %v, want unchanged", got)
	}
}

// Key off: exactly b303.
func TestB308KeyOff(t *testing.T) {
	b303Keys(t, true, true)
	b308Key(t, false)
	clock := int64(time.Hour)
	links := b303Links(60, 6)
	s := laneSet(&clock, links...)
	old, nf := links[0], links[1]
	b308Stick(old, clock)
	h := b308DarkHash(t, links, nf, old.path, clock)
	e := &s.lane.flows[h&(l3LaneSlots-1)]
	*e = l3Flow{key: h, used: true, inter: true}
	s.dupSecondFor(e, h, old, clock)
	if got, want := s.dupSecondFor(e, h, nf, clock), b303Full(links, h, nf, clock); got != want || got.path != old.path {
		t.Fatalf("key off: %v, full pick %v", got, want)
	}
}
