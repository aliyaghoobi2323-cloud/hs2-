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

// Step 2, the field's slow downloads counted fast (build a372b1301121 on a
// real Iran edge, a 72–85 ms path, an hour with only users' traffic on it):
// twelve times "downloads fill the path at ~12–27 Mbit/s (round trip 73 ms):
// window held at 2.0 MiB for 1 fast connection(s)" and then "path queue grew
// by 99–1630 ms — download windows 3.0 MiB → 2.0 MiB". A download whose
// server sends 14 Mbit/s turns its 2 MiB window in (window − step) / rate,
// 600 ms, nothing of the path: such a stream stayed in F (an interval saw
// one window update of it at most, and the slow-source rule asked for two),
// the core left OFF on it, grew its window to 3 MiB to no avail, and read
// its turnaround, now 1.2 s, as queue. Harmless in step 1 (no ceiling under
// 2 MiB); stage 2's ceilings go down to 256 KiB, and a user whose server
// speeds up again would be held at what the core made of his slow spell.
// Here the source drops to 12–27 Mbit/s 15 s into the download (or is slow
// from the start, or hands its data over in 64 KiB chunks), on paths where
// the core was OFF (base windows past the knee) and where it was in HOLD
// (1000 Mbit/s): for the 120 s after, the core drains nothing, takes no
// route change, finds no knee, and counts the stream fast for no evaluation
// from 5 s after the drop on.
func TestWcoreS2SlowSourceIsNotFast(t *testing.T) {
	for _, x := range []struct {
		rate  float64 // the path, Mbit/s
		rtt   int     // ms
		src   float64 // the source's pace from drop on, Mbit/s
		drop  int     // s (0: slow from the start)
		burst int     // the source's chunks, bytes (0: as it comes)
	}{
		{300, 72, 14, 15, 0}, {300, 72, 27, 15, 0}, {300, 72, 12, 0, 0}, {300, 85, 20, 15, 64 << 10},
		{100, 85, 14, 15, 0}, {100, 72, 20, 15, 0}, {1000, 72, 14, 15, 0}, {1000, 85, 27, 15, 0},
	} {
		c := wmPath(x.rate, wmMS(x.rtt), wmMS(50))
		c.mode, c.seed = wmCore, 1
		drop := time.Duration(x.drop) * time.Second
		c.dur = drop + 120*time.Second
		c.streams = []wmStreamCfg{{srcAt: []wmChange{{drop, x.src * wmMbit}}, srcBurst: x.burst}}
		name := fmt.Sprintf("%.0f/%d, source %.0f Mbit/s from %v (chunks %d)", x.rate, x.rtt, x.src, drop, x.burst)
		var ev []string
		fast, evals := 0, 0
		var cnt0, cnt1 wcCounters // (a cut between evaluations makes no event of the core's own)
		c.check = func(at time.Duration, k *wcore, in wcIn, o wcOut) {
			if at <= drop {
				cnt0 = k.cnt
				return
			}
			cnt1 = k.cnt
			for _, e := range o.events {
				if e.kind == wcEvDrain || e.kind == wcEvRoute || e.kind == wcEvFilled {
					ev = append(ev, fmt.Sprintf("%.1fs %s", at.Seconds(), e))
				}
			}
			if at > drop+5*time.Second && k.lastAt.Equal(in.now) {
				evals++
				if k.inF[1] {
					fast++
				}
			}
		}
		r := wmRun(c)
		drains := cnt1.drainQ + cnt1.drainLoss + cnt1.drainRate - cnt0.drainQ - cnt0.drainLoss - cnt0.drainRate
		if len(ev) > 0 || fast > 0 || drains > 0 {
			t.Errorf("%s: counted fast in %d of %d evaluations from 5 s after the drop, %d drain(s) and cut(s) after it, events %q (%.1f Mbit, drains %v over the run)",
				name, fast, evals, drains, ev, r.mbit, r.drains)
		}
	}
}

// wfCore is an edge's core of stage 2 on a 72 ms path whose turnaround
// floor (turnBase) is 73 ms, with a record of the exit that showed q of
// queue at now.
func wfCore(now time.Time, q time.Duration) *wcore {
	c := newWcore(wcConfig{side: wcEdge, stage: 2, need: wtNeed, maxWin: wtMax, minWin: wtMin}, func() float64 { return 0.5 })
	c.tBase = 72 * time.Millisecond
	c.turnMins.add(now, 73*time.Millisecond, wcTurnWin)
	c.tbOff, c.tbOK = time.Millisecond, true
	c.qSnd, c.qSndAt = q, now
	return c
}

// wfIn is an interval of dt at now of one stream (its read over dt is the
// link's), its turnaround samples as the merge of the wakes makes them.
func wfIn(now time.Time, s wcStream) wcIn {
	s.turns = wcWakeTurns(nil, s)
	return wcIn{now: now, poolFree: -1, linkBytes: s.read, streams: []wcStream{s}}
}

// A stream is fast (F) only on evidence that its window, not its source,
// holds its sender: a turnaround the path explains (its floor, the queue a
// signal with no bias shows, and slack). The field's slow download (14
// Mbit/s with a 2 MiB window on a 72 ms path: a 600 ms turnaround, a pause
// at the limit in every cycle as its source's own pauses land there) leaves
// F on the second interval that can judge it, and its turnarounds and hits
// stop counting toward q_turn and the hit share on the first. An interval
// judges only on a clean path, outside a dip of the link's rate and the
// interval after it, and with a record of the exit that came wcEvidSettle
// round trips after the published budget last rose. A stream the window
// holds above the knee (no hits, a turnaround of T_base and the standing
// queue) stays, and joins; a slow stream does not join on its hits or on
// the "use" of its window, even with turnBase made of slow streams'
// turnarounds alone.
func TestWcoreS2Evidence(t *testing.T) {
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	const dt = 0.4
	slow := wcStream{id: 1, win: wtNeed, upd: 1, hit: 1, read: uint64(14 * wmMbit * dt), turnMin: ms(600), turnLast: ms(600)}
	now := wtT0
	next := func() time.Time { now = now.Add(400 * time.Millisecond); return now }

	// the field's: a fresh record of a bare path
	c := wfCore(now, 0)
	c.inF[1] = true
	m := c.measure(wfIn(next(), slow), dt)
	c.qSndAt = now
	if len(m.f) != 1 || m.qOK || m.hit != 0 {
		t.Errorf("first judged interval: F %v, q_turn %v (ok %v), hit share %.2f; want still in F with its turnarounds and hits out of q_turn and the share",
			m.f, m.qTurn, m.qOK, m.hit)
	}
	if m = c.measure(wfIn(next(), slow), dt); len(m.f) != 0 {
		t.Errorf("second judged interval: still in F (%v)", m.f)
	}

	// no judgement: a record from before the budget's latest rise has
	// had wcEvidSettle round trips, under holes, in a dip and after it
	for _, tc := range []struct {
		name string
		prep func(c *wcore, in *wcIn)
	}{
		{"the budget rose 100 ms before the record", func(c *wcore, in *wcIn) {
			// (the ramp moves the published budget up at that wake)
			c.B, c.bPub, c.acc.now = 3*wtMiB, 2*wtMiB, c.qSndAt.Add(-ms(100))
			c.ramp()
		}},
		{"data waits behind holes", func(c *wcore, in *wcIn) { in.oooOK, in.segsIn, in.ooo = true, 1000, 300 }},
	} {
		c := wfCore(now, 0)
		c.inF[1] = true
		for i := 0; i < 3; i++ {
			in := wfIn(next(), slow)
			c.qSndAt = now
			tc.prep(c, &in)
			if m := c.measure(in, dt); len(m.f) != 1 || !m.qOK {
				t.Errorf("%s, interval %d: F %v, q_turn ok %v; want kept, its turnarounds counted", tc.name, i, m.f, m.qOK)
			}
		}
	}
	c = wfCore(now, 0)
	c.inF[1] = true
	fastS := wcStream{id: 1, win: wtNeed, upd: 8, hit: 8, read: uint64(200 * wmMbit * dt), turnMin: ms(74), turnLast: ms(80)}
	c.measure(wfIn(next(), fastS), dt)
	for i, want := range []int{1, 1, 1, 0} { // the dip, after it, judged once, judged twice
		c.qSndAt = now
		if m := c.measure(wfIn(next(), slow), dt); len(m.f) != want {
			t.Errorf("a source that drops from 200 to 14 Mbit/s, interval %d after: F %v, want %d stream(s)", i+1, m.f, want)
		}
	}

	// held by its window above the knee: no hits, T_base and a standing
	// queue the record shows (40 ms), or one within the slack it does not
	held := func(turn time.Duration) wcStream {
		return wcStream{id: 1, win: 4 * wtMiB, upd: 8, read: uint64(280 * wmMbit * dt), turnMin: turn, turnLast: turn + ms(5)}
	}
	for _, tc := range []struct {
		q, turn time.Duration
	}{{ms(40), ms(115)}, {0, ms(140)}} {
		c := wfCore(now, tc.q)
		c.inF[1] = true
		for i := 0; i < 4; i++ {
			c.qSndAt = next()
			if m := c.measure(wfIn(now, held(tc.turn)), dt); len(m.f) != 1 || !m.qOK {
				t.Errorf("held above the knee (turnaround %v, q_snd %v), interval %d: F %v, q_turn ok %v", tc.turn, tc.q, i, m.f, m.qOK)
			}
		}
	}

	// joining
	inflated := func(c *wcore) {
		// the link's streams were all slow for 60 s: turnBase is theirs
		c.turnMins = wcMinWin{}
		c.turnMins.add(now, ms(595), wcTurnWin)
	}
	burst := slow
	burst.hit, burst.read = 0, uint64(19*wmMbit*dt) // r·turnaround ≥ 0.9 of its window less half a step
	for _, tc := range []struct {
		name string
		s    wcStream
		q    time.Duration
		prep func(c *wcore)
		join bool
	}{
		{"a slow source with hits", slow, 0, nil, false},
		{"a slow source with hits, turnBase of slow streams", slow, 0, inflated, false},
		{"a source using its window at 600 ms", burst, 0, nil, false},
		{"held below the knee: hits, a bare turnaround", fastS, 0, nil, true},
		{"held below the knee, turnBase of slow streams", fastS, 0, inflated, true},
		{"held above the knee: in use, T_base and the record's queue", held(ms(100)), ms(15), nil, true},
	} {
		c := wfCore(now, tc.q)
		if tc.prep != nil {
			tc.prep(c)
		}
		if m := c.measure(wfIn(next(), tc.s), dt); (len(m.f) == 1) != tc.join {
			t.Errorf("%s: F %v, want joined %v", tc.name, m.f, tc.join)
		}
	}

	// the exit's side (no record of the sender): judged on its own srtt
	c = wfCore(now, 0)
	c.cfg.side, c.qSndAt = wcExit, time.Time{}
	c.inF[1] = true
	for i, want := range []int{1, 0} {
		if m := c.measure(wfIn(next(), slow), dt); len(m.f) != want {
			t.Errorf("the exit's side, interval %d: F %v, want %d stream(s)", i+1, m.f, want)
		}
	}
}
