package engine

import (
	"fmt"
	"testing"
	"time"
)

// wsCore is a core of the side for notePaths alone.
func wsCore(side wcSide) *wcore {
	c := newWcore(wcConfig{side: side, stage: 1, need: wtNeed, maxWin: wtMax, minWin: wtMin}, func() float64 { return 0.5 })
	c.tBase = 100 * time.Millisecond
	return c
}

// wsPaths feeds notePaths an interval every 2 s for d: the link socket's
// min_rtt and srtt, the record of the sender's socket (sndSrtt 0: none came)
// with min_rtt sndMin, and the streams. It returns the route changes logged.
func wsPaths(c *wcore, now *time.Time, d, minRTT, srtt, sndSrtt, sndMin time.Duration, streams ...wcStream) int {
	n := 0
	for end := now.Add(d); now.Before(end); *now = now.Add(2 * time.Second) {
		in := wcIn{now: *now, minRTT: minRTT, srtt: srtt, poolFree: -1, streams: streams}
		if sndSrtt > 0 {
			in.snd = wcSnd{fresh: true, srtt: sndSrtt, minRTT: sndMin, segsOut: 1000}
		}
		var ev []wcEvent
		c.notePaths(in, &ev)
		for _, e := range ev {
			if e.kind == wcEvRoute {
				n++
			}
		}
	}
	return n
}

// Step 2, the field's false route change (build a372b1301121 on a real Iran
// edge and Kharej exit, a 72 ms path): once a link's fast download ended,
// its window turnarounds (~75 ms) left turnBase's 60 s floor, and what was
// left were the turnarounds of streams whose source sends slowly -- the time
// their sender took to reach the limit it had before the update, 0.2 to
// 4 s, nothing of the path -- while the exit's records still read the 72 ms
// path with no queue. notePaths took that floor for a longer path ("the
// path's round trip grew to ~2199 ms (a route change)", 53-82 s after each
// download ended, and on links with only users' slow traffic) and T_base
// followed it. In the field such a stream was even counted fast at times
// ("downloads fill the path at ~14 Mbit/s"). A real route change (both ends'
// min_rtt stale for 5 minutes, the sender's srtt on the longer path) is
// still followed. Both sides have the record of their sender's socket, as
// every core that runs does: the edge leaves OFF only on the exit's records,
// and the exit's core gets the edge's record in step 3.
func TestWcoreS2SlowStreamsAreNoRouteChange(t *testing.T) {
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	fast := func(turn time.Duration) wcStream {
		return wcStream{id: 1, read: 1 << 20, upd: 4, hit: 4, turnMin: turn}
	}
	slow := func(turn time.Duration) wcStream { return wcStream{id: 3, read: 1 << 20, upd: 4, turnMin: turn} }
	for _, side := range []wcSide{wcEdge, wcExit} {
		name := map[wcSide]string{wcEdge: "edge", wcExit: "exit"}[side]
		for _, tc := range []struct {
			turn  time.Duration
			slowF bool // the slow stream is counted fast (F)
		}{{ms(206), false}, {ms(499), false}, {ms(2199), false}, {ms(2199), true}} {
			c := wsCore(side)
			now := wtT0
			wsPaths(c, &now, time.Minute, ms(72), ms(73), ms(74), ms(72), fast(ms(75)))
			if c.tBase != ms(72) {
				t.Fatalf("%s: during the download T_base %v, want 72ms", name, c.tBase)
			}
			c.inF = map[uint32]bool{3: tc.slowF}
			if n := wsPaths(c, &now, 90*time.Second, ms(72), ms(73), ms(74), ms(72), slow(tc.turn)); n > 0 || c.tBase != ms(72) {
				t.Errorf("%s: slow streams' turnarounds of %v (counted fast: %v): %d route change(s), T_base %v; want none and 72ms",
					name, tc.turn, tc.slowF, n, c.tBase)
			}
		}
		// a real route change, 72 → 300 ms, with a fast download on and a
		// slow stream beside it
		c := wsCore(side)
		now := wtT0
		wsPaths(c, &now, time.Minute, ms(72), ms(73), ms(74), ms(72), fast(ms(75)))
		c.inF = map[uint32]bool{1: true}
		n := wsPaths(c, &now, 80*time.Second, ms(72), ms(302), ms(302), ms(72), fast(ms(303)), slow(ms(2199)))
		if n == 0 || c.tBase < ms(290) || c.tBase > ms(310) {
			t.Errorf("%s: a real route change 72 → 300 ms: %d route change(s), T_base %v; want one and ~300ms", name, n, c.tBase)
		}
		// the sender's records stop for 25 s after it: the longer path is
		// kept, and no new rise is taken while they are away
		wsPaths(c, &now, 26*time.Second, ms(72), ms(302), 0, 0, fast(ms(303)))
		if c.tBase < ms(290) {
			t.Errorf("%s: the records stopped after the route change: T_base %v, want the longer path kept (~300ms)", name, c.tBase)
		}
	}
}

// After a real route change the exit's records stop for a while (a stats
// poll that times out, a busy exit): the core keeps the longer path as
// T_base while they are away. Taking the stale min_rtt again for the gap
// gave each of these runs a queue drain on the longer path and two of them
// lost 7-8% of their rate (the independent judge of step 2's route fix).
// What T_base does once they are back (in one of these runs it fell back 4 s
// later, until the route change was kept for good) is TestWcoreS2RouteSticky's.
func TestWcoreS2RouteHeldThroughRecordGap(t *testing.T) {
	for _, x := range []struct {
		rate         float64
		rtt, q, to   int
		offAt, offTo int
	}{{300, 100, 50, 300, 85, 110}, {300, 150, 50, 600, 85, 115}} {
		for seed := uint64(1); seed <= 2; seed++ {
			c := wmPath(x.rate, wmMS(x.rtt), wmMS(x.q))
			c.seed, c.dur, c.mode = seed, 120*time.Second, wmCore
			c.rttAt = []wmChange{{20 * time.Second, float64(x.to)}}
			c.recOff = [][2]time.Duration{{time.Duration(x.offAt) * time.Second, time.Duration(x.offTo) * time.Second}}
			from, to := time.Duration(x.offAt)*time.Second, time.Duration(x.offTo)*time.Second
			moved := time.Duration(0)
			var fell []time.Duration
			c.check = func(at time.Duration, k *wcore, in wcIn, o wcOut) {
				long := float64(k.tBase) >= 0.9*float64(wmMS(x.to))
				if moved == 0 && long {
					moved = at
				}
				if moved > 0 && moved < from && at >= from && at <= to && !long && len(fell) < 3 {
					fell = append(fell, at)
				}
			}
			r := wmRun(c)
			if moved == 0 || moved >= from {
				t.Errorf("%v/%d→%d s%d: T_base did not follow the route change before the records stopped (at %v)", x.rate, x.rtt, x.to, seed, moved)
				continue
			}
			if len(fell) > 0 {
				t.Errorf("%v/%d→%d, records off %d-%d s, s%d: T_base took the longer path at %v and fell back during the gap at %v (%.1f Mbit, drains %v)",
					x.rate, x.rtt, x.to, x.offAt, x.offTo, seed, moved, fell, r.mbit, r.drains)
			}
		}
	}
}

// A route change, once taken, stays until the path is seen to change back
// (notePaths: the turnaround's floor back within 1.3·min_rtt, the link
// socket's min_rtt renewed or lower, the exit's srtt back under the longer
// path). notePaths took the longer path anew at each evaluation, and only
// while q_snd stayed within Δq: a queue on the longer path (the core's own
// growth, a probe) sent T_base back to the stale min_rtt, where the core read
// the whole rise as queue and drained, and Δq shrank with T_base, so it came
// back only once the queue was gone (300/100 → 300 ms at 20 s, seed 1: back
// to 100 ms at 90.4 and 108.4 s; seed 2 at 114.3 and 128.5 s, and with the
// exit's records off 85-110 s at 114.3 s, 4 s after they came back). Nor
// may a queue on the longer path be taken for path: T_base never goes past
// the path the route change took.
func TestWcoreS2RouteSticky(t *testing.T) {
	for _, x := range []struct {
		rate       float64
		rtt, q, to int
		off        [2]int // the exit's records off (s); none if 0
	}{{300, 100, 50, 300, [2]int{85, 110}}, {300, 150, 50, 600, [2]int{85, 115}},
		{300, 100, 50, 300, [2]int{}}, {300, 150, 50, 600, [2]int{}}} {
		for seed := uint64(1); seed <= 2; seed++ {
			c := wmPath(x.rate, wmMS(x.rtt), wmMS(x.q))
			c.seed, c.dur, c.mode = seed, 160*time.Second, wmCore
			c.rttAt = []wmChange{{20 * time.Second, float64(x.to)}}
			if x.off[1] > 0 {
				c.recOff = [][2]time.Duration{{time.Duration(x.off[0]) * time.Second, time.Duration(x.off[1]) * time.Second}}
			}
			moved, top := time.Duration(0), time.Duration(0)
			var fell []time.Duration
			c.check = func(at time.Duration, k *wcore, in wcIn, o wcOut) {
				long := float64(k.tBase) >= 0.9*float64(wmMS(x.to))
				if moved == 0 && long {
					moved = at
				}
				if moved > 0 && !long && len(fell) < 3 {
					fell = append(fell, at)
				}
				top = max(top, k.tBase)
			}
			r := wmRun(c)
			name := fmt.Sprintf("%v/%d→%d records off %v s%d", x.rate, x.rtt, x.to, x.off, seed)
			switch {
			case moved == 0:
				t.Errorf("%s: T_base never took the longer path", name)
			case len(fell) > 0:
				t.Errorf("%s: T_base took the longer path at %v and fell back at %v (%.1f Mbit, drains %v)", name, moved, fell, r.mbit, r.drains)
			case float64(top) > 1.1*float64(wmMS(x.to)):
				t.Errorf("%s: T_base up to %v on a %d ms path: a queue taken for path", name, top, x.to)
			default:
				t.Logf("%s: T_base the longer path from %v to the end (%.1f Mbit, drains %v)", name, moved, r.mbit, r.drains)
			}
		}
	}
}

// notePaths keeps a route change it took (72 → 300 ms) while a queue builds
// on the longer path, standing past the turnaround's 60 s floor or swinging
// (T_base stays the path's: neither the stale min_rtt nor the path with the
// queue, and q_snd reads the queue), and drops it when the path is seen to
// change back: the turnaround's floor back near min_rtt, or the exit's srtt
// back under the longer path while that floor still holds the longer one.
func TestWcoreS2RouteKeptUntilBack(t *testing.T) {
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	fast := func(turn time.Duration) wcStream {
		return wcStream{id: 1, read: 1 << 20, upd: 4, hit: 4, turnMin: turn}
	}
	near := func(d, want time.Duration) bool { return d >= want-ms(10) && d <= want+ms(10) }
	for _, side := range []wcSide{wcEdge, wcExit} {
		name := map[wcSide]string{wcEdge: "edge", wcExit: "exit"}[side]
		route := func() (*wcore, time.Time) {
			c := wsCore(side)
			now := wtT0
			wsPaths(c, &now, time.Minute, ms(72), ms(73), ms(74), ms(72), fast(ms(75)))
			c.inF = map[uint32]bool{1: true}
			if n := wsPaths(c, &now, 80*time.Second, ms(72), ms(302), ms(302), ms(72), fast(ms(303))); n != 1 || !near(c.tBase, ms(300)) {
				t.Fatalf("%s: the route change 72 → 300 ms: %d logged, T_base %v", name, n, c.tBase)
			}
			return c, now
		}
		// a standing queue of 100 ms for 90 s: the turnaround's floor rises
		// with it once it has stood for 60 s
		c, now := route()
		for end := now.Add(90 * time.Second); now.Before(end); {
			wsPaths(c, &now, 2*time.Second, ms(72), ms(402), ms(402), ms(72), fast(ms(403)))
			if !near(c.tBase, ms(300)) {
				t.Fatalf("%s: a standing queue of 100 ms on the longer path, %v on: T_base %v, want the path's (~300ms)", name, now.Sub(wtT0), c.tBase)
			}
		}
		if c.qSnd < ms(90) {
			t.Errorf("%s: a standing queue of 100 ms: q_snd %v", name, c.qSnd)
		}
		// a queue that swings 0-150 ms
		for i := 0; i < 30; i++ {
			q := ms(50 * (i % 4))
			wsPaths(c, &now, 2*time.Second, ms(72), ms(302)+q, ms(302)+q, ms(72), fast(ms(303)+q))
			if !near(c.tBase, ms(300)) {
				t.Fatalf("%s: a queue of %v on the longer path: T_base %v, want ~300ms", name, q, c.tBase)
			}
		}
		// the path back: the turnaround and the exit's srtt at 72 ms again
		wsPaths(c, &now, 4*time.Second, ms(72), ms(73), ms(74), ms(72), fast(ms(75)))
		if c.tBase != ms(72) || c.routeLog {
			t.Errorf("%s: the path back to 72 ms: T_base %v (route %v)", name, c.tBase, c.routeLog)
		}
		// the exit's srtt back at 74 ms while no turnaround came under the
		// longer path's yet: the longer path is gone
		c, now = route()
		wsPaths(c, &now, 4*time.Second, ms(72), ms(73), ms(74), ms(72), fast(ms(303)))
		if c.tBase != ms(72) {
			t.Errorf("%s: the exit's srtt back at 74 ms: T_base %v, want the route dropped (72ms)", name, c.tBase)
		}
		// the link socket's min_rtt renewed on the longer path: T_base is it
		c, now = route()
		wsPaths(c, &now, 4*time.Second, ms(300), ms(302), ms(302), ms(72), fast(ms(303)))
		if c.tBase != ms(300) {
			t.Errorf("%s: min_rtt renewed at 300 ms: T_base %v", name, c.tBase)
		}
	}
}
