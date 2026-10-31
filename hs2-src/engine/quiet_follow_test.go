package engine

import (
	"testing"
	"time"
)

// QuietFollow (quietfollow.go): the two ends of a tunnel need not set the
// QUIET count alike.

func TestQuietFollowMath(t *testing.T) {
	defer func(x int) { QuietExtra = x }(QuietExtra)
	QuietExtra = 1
	for _, c := range []struct{ want, bulk, peer, out int }{
		{4, 8, 0, 4},  // no ceiling heard yet: its own
		{4, 8, 12, 4}, // an exit of the same count
		{4, 8, 10, 2}, // j3g's exit (2 past 5 links)
		{2, 8, 12, 2}, // an exit of more: its own
		{2, 1, 2, 1},  // one link, an exit of one QUIET link
		{2, 1, 3, 2},
		{4, 8, 8, 1}, // an exit with no room past the BULK links: one still
		{4, 8, 5, 1}, // an exit of a smaller max_links
		{1, 8, 5, 1}, // one: nothing to fit
		{3, 30, 300, 3},
	} {
		if got := followWant(c.want, c.bulk, c.peer); got != c.out {
			t.Errorf("followWant(%d, %d, %d) = %d, want %d", c.want, c.bulk, c.peer, got, c.out)
		}
	}
	withTune(t, QuietReplace, 0) // (its room: TestQuietReplaceRoom)
	for _, c := range []struct {
		max, own   int
		follow     bool
		report, hi int
	}{
		{8, 4, true, 12, 12},
		{8, 2, true, 10, 12},  // room for an edge of 4
		{8, 2, false, 10, 10}, // before QuietFollow
		{1, 1, true, 2, 5},
		{1, 2, true, 3, 5},
		{8, 6, true, 14, 14}, // its own above the room
	} {
		r, h := exitRevCeilings(c.max, c.own, c.follow)
		if r != c.report || h != c.hi {
			t.Errorf("exitRevCeilings(%d, %d, %v) = %d %d, want %d %d", c.max, c.own, c.follow, r, h, c.report, c.hi)
		}
	}
	QuietExtra = 0 // within max_links (lab): no room
	if r, h := exitRevCeilings(8, 2, true); r != 8 || h != 8 {
		t.Errorf("QuietExtra 0: %d %d, want 8 8", r, h)
	}
}

// linkSeqs is how many links the edge took so far, QUIET and BULK: a pool
// that dials again and again (churn) raises it.
func (p *quietPool) linkSeqs() int {
	p.lm.mu.RLock()
	n := p.lm.linkSeq
	p.lm.mu.RUnlock()
	p.q.mu.Lock()
	n += p.q.seq
	p.q.mu.Unlock()
	return n
}

// The reverse pool of two ends of different QUIET counts, both directions,
// with ends of before QuietFollow too: the edge ends with every BULK link and
// the QUIET links both can hold, settles (no churn), and carries a bulk
// connection that moves. "before" is the failure the switch closes.
func TestQuietFollowMixed(t *testing.T) {
	if QuietExtra == 0 {
		t.Skip("QuietExtra 0 (lab)")
	}
	type tc struct {
		name         string
		max          int
		mix          quietMix
		wantQ, wantB int
		ask          int // what the edge asks of the exit (0: wantQ + wantB)
	}
	cases := []tc{
		{"8/edge4-exit4", 8, quietMix{edgeWant: 4, exitOwn: 4}, 4, 8, 0},
		{"8/edge4-exit2", 8, quietMix{edgeWant: 4, exitOwn: 2}, 2, 8, 0},
		{"8/edge2-exit4", 8, quietMix{edgeWant: 2, exitOwn: 4}, 2, 8, 0},
		{"8/olderedge4-exit2", 8, quietMix{edgeWant: 4, edgeNoFollow: true, exitOwn: 2}, 4, 8, 0},
		{"8/edge4-olderexit2", 8, quietMix{edgeWant: 4, exitOwn: 2, exitNoRoom: true}, 2, 8, 0},
		{"8/before-edge4-exit2", 8, quietMix{edgeWant: 4, edgeNoFollow: true, exitOwn: 2, exitNoRoom: true}, 4, 6, 12},
		{"1/edge2-exit1", 1, quietMix{edgeWant: 2, exitOwn: 1}, 1, 1, 0},
		{"1/edge1-exit2", 1, quietMix{edgeWant: 1, exitOwn: 2}, 1, 1, 0},
		{"1/edge2-olderexit1", 1, quietMix{edgeWant: 2, exitOwn: 1, exitNoRoom: true}, 1, 1, 0},
		{"1/olderedge2-exit1", 1, quietMix{edgeWant: 2, edgeNoFollow: true, exitOwn: 1}, 2, 1, 0},
		{"1/before-edge2-exit1", 1, quietMix{edgeWant: 2, edgeNoFollow: true, exitOwn: 1, exitNoRoom: true}, 2, 0, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ask := c.ask
			if ask == 0 {
				ask = c.wantQ + c.wantB
			}
			p := startQuietPoolMix(t, false, c.max, c.mix)
			t0 := time.Now()
			p.waitFor("the classes settle", 30*time.Second, func() bool {
				return p.q.count() == c.wantQ && p.serving() == c.wantB && p.lm.ctlTarget() == ask
			})
			took := time.Since(t0)
			// no churn: nothing more dialed over three health ticks
			s0 := p.linkSeqs()
			time.Sleep(3 * healthTick)
			if s1 := p.linkSeqs(); s1 != s0 || p.q.count() != c.wantQ || p.serving() != c.wantB {
				t.Fatalf("not settled: links taken %d then %d, QUIET %d, BULK %d (%s)", s0, s1, p.q.count(), p.serving(), p.q.line())
			}
			if c.wantB > 0 {
				moved0 := p.q.moved.Load()
				p.echoBulk(2 << 20)
				p.waitFor("the bulk connection moved", 10*time.Second, func() bool { return p.q.moved.Load() == moved0+1 })
			} else {
				p.echoBulk(64 << 10) // QUIET alone still carries it
			}
			t.Logf("settled in %s, links taken %d: %s", took.Round(time.Millisecond), s0, p.q.line())
		})
	}
}

// Direct: the edge dials its QUIET links and the exit takes every one, so
// the edge's own count holds whatever the exit's build counts.
func TestQuietFollowDirect(t *testing.T) {
	if QuietExtra == 0 {
		t.Skip("QuietExtra 0 (lab)")
	}
	p := startQuietPoolMix(t, true, 2, quietMix{edgeWant: 3})
	if p.q.follow {
		t.Fatal("a direct edge follows")
	}
	p.waitFor("three QUIET and two BULK", 30*time.Second, func() bool { return p.q.count() == 3 && p.serving() == 2 })
	moved0 := p.q.moved.Load()
	p.echoBulk(2 << 20)
	p.waitFor("the bulk connection moved", 10*time.Second, func() bool { return p.q.moved.Load() == moved0+1 })
	t.Log(p.q.line())
}
