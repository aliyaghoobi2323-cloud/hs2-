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
// What T_base does once they are back is the code before this fix (in one
// of these runs it falls back 4 s later, as it did before): not judged here.
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

// Step 2, users' downloads whose source is slow, counted fast (the field,
// build a372b1301121 on a 72 ms path, an hour of users' traffic alone, 12
// times: "downloads fill the path at ~12–27 Mbit/s (round trip 73 ms):
// window held at 2.0 MiB for 1 fast connection(s)" and then "path queue grew
// by 99–1630 ms — download windows 3.0 MiB → 2.0 MiB"). A source of
// 14 Mbit/s fills a 2 MiB window's step (1 MiB) in 0.6 s: an update every
// other evaluation, each with a turnaround of 0.6 s that is no queue. From
// its first such evaluation its turnarounds are left out (q_turn has none:
// it was 527 ms past turnBase); an evaluation with no turnaround in it
// changes nothing, nor does one whose turnaround came with no update of its
// own; it leaves F once it looked slow for a record span with the exit's
// records clean (before, the test asked two updates in an evaluation and it
// never left); while the records show resends it stays (a sender in a loss
// recovery looks the same) and a bare turnaround takes it back; and a fast
// stream whose sender's PROBE_RTT stretched two turnarounds stays.
func TestWcoreS2SlowSourceFromFirstEvaluation(t *testing.T) {
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	const rate, dt = 1.75e6, 0.3 // 14 Mbit/s, an evaluation's measuring part on 72 ms
	core := func(clean int) *wcore {
		c := wtCore(1)
		c.tBase = ms(72)
		c.turnMins.add(wtT0, ms(73), wcTurnWin)
		c.cleanRecs = clean
		c.inF[3] = true
		return c
	}
	type step struct {
		at       time.Duration
		upd, hit uint32
		turn     time.Duration
		r        float64
		inF, qOK bool
		what     string
	}
	run := func(name string, c *wcore, win uint32, steps []step) {
		for _, x := range steps {
			s := wcStream{id: 3, read: uint64(x.r * dt), upd: x.upd, hit: x.hit, turnMin: x.turn, turnLast: x.turn, win: win}
			s.turns = wcWakeTurns(nil, s)
			now := wtT0.Add(x.at)
			c.qSnd, c.qSndAt = 0, now // the exit's latest record: no queue
			m := c.measure(wcIn{now: now, streams: []wcStream{s}}, dt)
			if (len(m.f) == 1) != x.inF || m.qOK != x.qOK {
				t.Errorf("%s, %s (%v): in F %v, q_turn %v (ok %v); want in F %v, q_turn ok %v",
					name, x.what, x.at, len(m.f) == 1, m.qTurn, m.qOK, x.inF, x.qOK)
			}
		}
	}
	slow := []step{
		{0, 1, 0, ms(600), rate, true, false, "the first update with a turnaround of 0.6 s"},
		{ms(300), 0, 0, 0, rate, true, false, "no update, no turnaround"},
		{ms(600), 1, 0, 0, rate, true, false, "an update, no turnaround"},
		{ms(900), 0, 0, ms(600), rate, true, false, "a turnaround with no update of its own"},
		{ms(3000), 1, 0, ms(600), rate, false, false, "a record span after the first"},
	}
	run("records clean", core(wcSndHist), wtNeed, slow)
	c := core(0)
	slow[4].inF = true
	run("records with resends", c, wtNeed, slow)
	run("records with resends", c, wtNeed, []step{{ms(3300), 2, 2, ms(75), 20e6, true, true, "its source fast again"}})
	// a fast stream of 8 MiB whose sender's PROBE_RTT (200 ms of 4 packets)
	// stretched two turnarounds, and the burst of its window after it
	run("PROBE_RTT", core(wcSndHist), 8*wtMiB, []step{
		{0, 1, 0, ms(290), 0.5e6, true, false, "a turnaround stretched"},
		{ms(300), 0, 0, ms(310), 0.2e6, true, false, "another, with no update of its own"},
		{ms(600), 6, 6, ms(74), 60e6, true, true, "the burst after it"},
	})
}

// Step 2, the turnaround floor the queue is judged against is the one
// notePaths took. Once a link had no fast download for a minute its
// turnaround floor was the turnarounds of streams whose source is slow (the
// field: 206 to 4230 ms on a 72 ms path); notePaths refuses that rise for a
// route change (the exit's srtt stays at the path) and keeps T_base, but
// the queue was still judged against the raw floor: a slow stream with a
// few hits joined F on the hint (a turnaround within Δq of "turnBase"), one
// in F was never past twice it, so never slow, and its turnarounds were a
// queue of 400 ms; the fast arm's turnRef was the raw floor too.
func TestWcoreS2SlowFloorIsNoTurnBase(t *testing.T) {
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	c := wsCore(wcEdge)
	now := wtT0
	wsPaths(c, &now, time.Minute, ms(72), ms(73), ms(74), ms(72), wcStream{id: 1, read: 1 << 20, upd: 4, hit: 4, turnMin: ms(75)})
	slowS := wcStream{id: 3, read: 1 << 20, upd: 2, turnMin: ms(600), turnLast: ms(600)}
	if n := wsPaths(c, &now, 70*time.Second, ms(72), ms(73), ms(74), ms(72), slowS); n > 0 || c.tBase != ms(72) {
		t.Fatalf("slow streams alone for 70 s: %d route change(s), T_base %v; want none and 72ms", n, c.tBase)
	}
	const rate, dt = 1.75e6, 0.3
	hinted := wcStream{id: 3, read: uint64(rate * dt), upd: 2, hit: 2, turnMin: ms(605), turnLast: ms(605), win: wtNeed}
	inF := wcStream{id: 5, read: uint64(rate * dt), upd: 2, turnMin: ms(1000), turnLast: ms(1000), win: 4 * wtMiB}
	hinted.turns, inF.turns = wcWakeTurns(nil, hinted), wcWakeTurns(nil, inF)
	c.inF = map[uint32]bool{5: true}
	if m := c.measure(wcIn{now: now, streams: []wcStream{hinted, inF}}, dt); len(m.f) != 1 || m.f[0] != 1 || m.qOK {
		t.Errorf("against a floor of slow streams' turnarounds: F %v, q_turn %v (ok %v); want the stream of F alone (none on a hint at 605 ms), and its turnarounds of 1 s past twice T_base: no q_turn",
			m.f, m.qTurn, m.qOK)
	}
	c.inF = map[uint32]bool{5: true}
	c.state, c.B, c.bPub, c.qRef = wcHold, 4*wtMiB, 4*wtMiB, 0
	if o := c.out(wcIn{now: now, streams: []wcStream{inF}}, now, nil); o.turnRef <= 0 || o.turnRef > ms(80) {
		t.Errorf("turnRef %v; want T_base's floor (~75ms), not the slow streams' (600ms)", o.turnRef)
	}
}

// Step 2: a stream that looks like a slow source does not join F on use. A
// source's turnaround is how long it takes to write what its window had
// left, so R·turnaround is its window less a step by construction, and right
// after it left F its turnarounds are timed from the limits of the larger
// window it had (4.7 s at 14 Mbit/s against a window of 2 MiB now:
// R·turnaround 8 MB, past any window). A stream whose turnaround is as long
// but whose sender waits at its window (a hit: its updates come late, not
// its data) still joins on use.
func TestWcoreS2SlowSourceJoinsNotOnUse(t *testing.T) {
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	const rate, dt = 1.75e6, 0.3
	for _, x := range []struct {
		hit  uint32
		join bool
	}{{0, false}, {1, true}} {
		c := wtCore(1)
		c.tBase = ms(72)
		c.turnMins.add(wtT0, ms(73), wcTurnWin)
		c.qSnd, c.qSndAt = 0, wtT0
		s := wcStream{id: 3, read: uint64(rate * dt), upd: 1, hit: x.hit, turnMin: ms(4700), turnLast: ms(4700), win: wtNeed}
		s.turns = wcWakeTurns(nil, s)
		if m := c.measure(wcIn{now: wtT0, streams: []wcStream{s}}, dt); (len(m.f) == 1) != x.join {
			t.Errorf("a turnaround of 4.7 s at 14 Mbit/s, %d hit(s): joined F %v, want %v", x.hit, len(m.f) == 1, x.join)
		}
	}
}

// Step 2, the model: a user's download whose source is slow -- one whose
// source drops to 8, 14 or 27 Mbit/s after 20 s at the path's pace, and one
// slow from its first byte -- on a path of 300 Mbit/s (the field's round
// trip of 72 to 85 ms, and 50 and 100 ms around it) and of 1000/72. For
// 120 s: no drain, no route change and no "fill the path" (STARTUP taking
// the source's pace for the path's), from the first record of the exit with
// no queue of the fast download left in it (a record span after the drop:
// on 1000 Mbit/s paths a drain for the rate comes with the drop itself, the
// exit's record still holding the fast download's queue). Before, the
// source down to 14 Mbit/s on 300/72 "filled the path" at 14 Mbit/s and
// drained 3.0 → 2.0 MiB on a "queue" of 599 ms 9 s after the drop, as the
// field's lines; slow from the start, 7 s in.
func TestWcoreS2SlowSourceModel(t *testing.T) {
	type run struct {
		rate float64
		rtt  int
		src  float64
		drop time.Duration // 0: slow from the start
	}
	var runs []run
	for _, rtt := range []int{50, 72, 100} {
		for _, src := range []float64{8, 14, 27} {
			runs = append(runs, run{300, rtt, src, 20 * time.Second}, run{300, rtt, src, 0})
		}
	}
	runs = append(runs, run{1000, 72, 14, 20 * time.Second})
	for _, x := range runs {
		for seed := uint64(1); seed <= 2; seed++ {
			c := wmPath(x.rate, wmMS(x.rtt), wmMS(50))
			c.seed, c.mode, c.dur = seed, wmCore, x.drop+120*time.Second
			c.streams = []wmStreamCfg{{src: x.src * wmMbit}}
			from := time.Duration(0)
			if x.drop > 0 {
				c.streams = []wmStreamCfg{{srcAt: []wmChange{{x.drop, x.src * wmMbit}}}}
				from = x.drop + wcSndSpan
			}
			var bad []string
			maxT := time.Duration(0)
			c.check = func(at time.Duration, k *wcore, in wcIn, o wcOut) {
				if at < from {
					return
				}
				maxT = max(maxT, k.tBase)
				for _, e := range o.events {
					if e.kind == wcEvDrain || e.kind == wcEvRoute || e.kind == wcEvFilled {
						bad = append(bad, fmt.Sprintf("%.1fs %v", at.Seconds(), e))
					}
				}
			}
			r := wmRun(c)
			if len(bad) > 0 || maxT > wmMS(x.rtt)*11/10 {
				t.Errorf("%v/%d, a source of %v Mbit/s (from %v), s%d: %d drain/route/filled event(s) %v, T_base up to %v (%.1f Mbit/s, drains %v)",
					x.rate, x.rtt, x.src, x.drop, seed, len(bad), bad, maxT, r.mbit, r.drains)
			}
		}
	}
}
