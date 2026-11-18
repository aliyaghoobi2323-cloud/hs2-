package engine

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// b303Links is n dup-capable links spread over paths paths (no socket view:
// never stuck unless a test gives one).
func b303Links(n, paths int) []*l3Link {
	links := make([]*l3Link, 0, n)
	for i := 0; i < n; i++ {
		l := laneLink(uint32(i+1), 0)
		l.dupOK = func() bool { return true }
		l.path = fmt.Sprintf("p%d", i%paths)
		links = append(links, l)
	}
	return links
}

func b303Full(links []*l3Link, h uint32, first *l3Link, now int64) *l3Link {
	return b278Second(links, h, first, dupSecond(links, h, first), now)
}

// b303SamePathHash finds a flow hash whose dupSecond pick shares first's path.
func b303SamePathHash(t testing.TB, links []*l3Link, first *l3Link) uint32 {
	for h := uint32(0); h < 1<<16; h++ {
		if dupSecond(links, h, first).path == first.path {
			return h
		}
	}
	t.Fatal("no same-path hash")
	return 0
}

func b303Keys(t testing.TB, b278, b303 bool) {
	o1, o2 := b278On.Load(), b303On.Load()
	b278On.Store(b278)
	b303On.Store(b303)
	t.Cleanup(func() { b278On.Store(o1); b303On.Store(o2) })
}

// The kept link is the full pick's link for every flow, and a flow whose
// dupSecond winner shares the first's path is moved once, not per packet.
func TestB303KeepsFullPick(t *testing.T) {
	b303Keys(t, true, true)
	clock := int64(time.Hour)
	links := b303Links(130, 12)
	s := laneSet(&clock, links...)
	first := links[0]
	for h := uint32(0); h < 512; h++ {
		e := &s.lane.flows[h&(l3LaneSlots-1)]
		*e = l3Flow{key: h, used: true, inter: true}
		for i := 0; i < 5; i++ {
			if got, want := s.dupSecondFor(e, h, first, clock), b303Full(links, h, first, clock); got != want {
				t.Fatalf("h %d try %d: kept %v, full pick %v", h, i, got, want)
			}
		}
	}
	h := b303SamePathHash(t, links, first)
	e := &s.lane.flows[h&(l3LaneSlots-1)]
	*e = l3Flow{key: h, used: true, inter: true}
	m0, p0 := b278OtherPath.Load(), b303Pick.Load()
	var got *l3Link
	for i := 0; i < 100; i++ {
		clock += int64(5 * time.Millisecond)
		got = s.dupSecondFor(e, h, first, clock)
		if got == nil || got.path == first.path {
			t.Fatalf("second copy on %v, first on %s", got, first.path)
		}
	}
	if m, p := b278OtherPath.Load()-m0, b303Pick.Load()-p0; m != 1 || p != 1 {
		t.Fatalf("100 packets: %d moves, %d picks; want 1 and 1", m, p)
	}
	// the refresh re-picks the same link: no new move
	clock += b303Refresh
	if s.dupSecondFor(e, h, first, clock) != got || b278OtherPath.Load()-m0 != 1 || b303Pick.Load()-p0 != 2 {
		t.Fatalf("refresh: moves %d picks %d", b278OtherPath.Load()-m0, b303Pick.Load()-p0)
	}
}

// The kept link is left at once when it dies, turns heavy, gets stuck, lands
// on the first's path, the link list changes or the first copy's link
// changes; each re-pick equals the full pick.
func TestB303Leaves(t *testing.T) {
	b303Keys(t, true, true)
	clock := int64(time.Hour)
	links := b303Links(40, 6)
	s := laneSet(&clock, links...)
	first := links[0]
	h := uint32(7)
	e := &s.lane.flows[h&(l3LaneSlots-1)]
	*e = l3Flow{key: h, used: true, inter: true}
	check := func(what string, f *l3Link, old *l3Link) *l3Link {
		t.Helper()
		got := s.dupSecondFor(e, h, f, clock)
		if want := b303Full(s.links, h, f, clock); got != want {
			t.Fatalf("%s: kept %v, full pick %v", what, got, want)
		}
		if old != nil && got == old {
			t.Fatalf("%s: still on the old link", what)
		}
		return got
	}
	k := check("first", first, nil)
	k.dead.Store(true)
	k = check("dead", first, k)
	k.heavy.Store(1)
	k = check("heavy", first, k)
	var backoff atomic.Uint32
	backoff.Store(1)
	k.txStat = func() (tcpStat, bool) {
		return tcpStat{unacked: 3, backoff: uint8(backoff.Load()), rttUs: 80000}, true
	}
	stuck := k
	k = check("stuck", first, k)
	// b278 tries only the top-ranked other-path link; take the stuck one away
	// so the path move below has a free other-path link
	stuck.dead.Store(true)
	k.path = first.path
	k = check("on first's path", first, k)
	p0 := b303Pick.Load()
	s.add(b303Links(1, 6)[0])
	check("link added", first, nil)
	if b303Pick.Load() == p0 {
		t.Fatal("link added: no re-pick")
	}
	check("first changed", links[1], nil)
	if b303Pick.Load() < p0+2 {
		t.Fatal("first changed: no re-pick")
	}
}

// Key off (or b278 off): the full pick on every packet, nothing kept.
func TestB303KeyOff(t *testing.T) {
	for _, keys := range [][2]bool{{true, false}, {false, true}} {
		b303Keys(t, keys[0], keys[1])
		clock := int64(time.Hour)
		links := b303Links(60, 12)
		s := laneSet(&clock, links...)
		first := links[0]
		h := b303SamePathHash(t, links, first)
		e := &s.lane.flows[h&(l3LaneSlots-1)]
		*e = l3Flow{key: h, used: true, inter: true}
		m0, p0 := b278OtherPath.Load(), b303Pick.Load()
		for i := 0; i < 10; i++ {
			if got, want := s.dupSecondFor(e, h, first, clock), b303Full(links, h, first, clock); got != want {
				t.Fatalf("keys %v: %v, full pick %v", keys, got, want)
			}
		}
		if e.dup2.link != nil || b303Pick.Load() != p0 {
			t.Fatalf("keys %v: kept a link", keys)
		}
		if keys[0] && b278OtherPath.Load()-m0 != 20 { // 10 ours + 10 b303Full
			t.Fatalf("b278 on, b303 off: %d moves, want per packet", b278OtherPath.Load()-m0)
		}
	}
}

func BenchmarkB303DupSecond(b *testing.B) {
	for _, on := range []bool{false, true} {
		b.Run(fmt.Sprintf("dupstick=%v", on), func(b *testing.B) {
			b303Keys(b, true, on)
			clock := int64(time.Hour)
			links := b303Links(130, 12)
			s := laneSet(&clock, links...)
			first := links[0]
			h := b303SamePathHash(b, links, first)
			e := &s.lane.flows[h&(l3LaneSlots-1)]
			*e = l3Flow{key: h, used: true, inter: true}
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				s.dupSecondFor(e, h, first, clock)
			}
		})
	}
}
