package engine

import (
	"fmt"
	"math"
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

	// the path holds the link's sender: the exit's writer waited on its
	// socket for most of a record's span (a path slower than the windows:
	// the data a turnaround waits for sits in the exit's socket). No
	// judgement, as long as one of the latest two records says so.
	c = wfCore(now, 0)
	c.inF[1] = true
	c.sndBlks[0] = wcRate{now, 0.9}
	pathHeld := wcStream{id: 1, win: wtNeed, upd: 2, read: uint64(17 * wmMbit * dt), turnMin: ms(492), turnLast: ms(492)}
	for i := 0; i < 3; i++ {
		c.qSndAt = next()
		if i == 1 {
			copy(c.sndBlks[1:], c.sndBlks[:])
			c.sndBlks[0] = wcRate{now, 0.45}
		}
		if m := c.measure(wfIn(now, pathHeld), dt); len(m.f) != 1 || !m.qOK {
			t.Errorf("the exit's writer waited on its socket, interval %d: F %v, q_turn ok %v; want kept, counted", i, m.f, m.qOK)
		}
	}

	// within the slack but past a quarter of it, a stream moving under half
	// of what its window allows in such a round trip is no evidence (a
	// source of 60 Mbit/s with 2 MiB: 140 ms); one moving 200 Mbit/s is
	ret := func(id uint32, mbit float64, turn time.Duration) wcStream {
		return wcStream{id: id, win: wtNeed, upd: 4, read: uint64(mbit * wmMbit * dt), turnMin: turn, turnLast: turn}
	}
	c = wfCore(now, 0)
	c.inF[1], c.inF[3] = true, true
	for i, want := range []int{2, 1} {
		c.qSndAt = next()
		in := wfIn(now, ret(1, 60, ms(140)))
		in.streams = append(in.streams, ret(3, 200, ms(120)))
		in.streams[1].turns = wcWakeTurns(nil, in.streams[1])
		in.linkBytes += in.streams[1].read
		if m := c.measure(in, dt); len(m.f) != want {
			t.Errorf("60 Mbit/s at 140 ms beside 200 at 120, interval %d: F %v, want %d stream(s)", i+1, m.f, want)
		}
	}
	// marked, it joins again only on two intervals in a row whose
	// turnaround is the floor and the queue seen within Δq (its sender
	// waited for the update): not on one within the slack
	for i, tc := range []struct {
		s    wcStream
		join bool
	}{{ret(1, 200, ms(120)), false}, {fastS, false}, {fastS, true}} {
		c.qSndAt = next()
		in := wfIn(now, tc.s)
		in.streams = append(in.streams, ret(3, 200, ms(120)))
		in.streams[1].turns = wcWakeTurns(nil, in.streams[1])
		if m := c.measure(in, dt); c.inF[1] != tc.join {
			t.Errorf("rejoining, interval %d (turnaround %v): F %v, want it joined %v", i+1, tc.s.turnMin, m.f, tc.join)
		}
	}
}

// The share of a record's span in which the exit's writer waited on its
// socket: txBlockedNs over monoNs between two records (the first has none).
func TestWcoreS2BuilderBlocked(t *testing.T) {
	var b wcBuilder
	at := wtT0
	snap := func(mono, blk uint64) wcIn {
		at = at.Add(2 * time.Second)
		return b.build(wcLinkSnap{now: at, sndAt: at, sndSrtt: 80 * time.Millisecond, sndMinRTT: 72 * time.Millisecond,
			sndSegsOut: mono / 1e6, sndMono: mono, sndBlocked: blk})
	}
	if in := snap(10e9, 1e9); !in.snd.fresh || in.snd.blockedOK {
		t.Fatalf("first record: %+v, want fresh with no share", in.snd)
	}
	if in := snap(12e9, 2e9); !in.snd.blockedOK || in.snd.blocked != 0.5 {
		t.Fatalf("second record: %+v, want a share of 0.5", in.snd)
	}
	if in := snap(14e9, 1e9); in.snd.blockedOK {
		t.Fatalf("a counter that went back (a restarted exit): %+v, want no share", in.snd)
	}
}

// A path that slows to 20 Mbit/s under 2 MiB windows (100 ms, 100 → 20 →
// 100 Mbit/s): the exit's BBR is held by its cwnd and the data waits in the
// exit's socket, where no srtt sees it, and the turnarounds run to 490 ms.
// The path holds the sender: the exit's writer waits on its socket, or
// while the exit recovers from the loss the slowdown brought, resends (two
// records read a wait of 0.39–0.47 with 24–57% of the segments resent, the
// next one a wait of 0.91), and no interval may judge the stream without
// evidence (stage 2 shrinks exactly such windows). Judged by the wait alone,
// the stream left F in 5 of these 24 runs and the core went OFF for the
// spell (the independent judge: 10 of 24 runs with OFF against base's 5;
// base's 5 leave F when the sender stalls for three intervals, as here).
// The spell starts at 18 to 31 s, four seeds each.
func TestWcoreS2PathHeldStaysFast(t *testing.T) {
	for _, st := range []int{18, 20, 22, 25, 28, 31} {
		for seed := uint64(1); seed <= 4; seed++ {
			c := wmPath(100, wmMS(100), wmMS(50))
			from := time.Duration(st) * time.Second
			c.mode, c.seed, c.dur = wmCore, seed, from+25*time.Second
			c.changes = []wmChange{{from, 20 * wmMbit}, {from + 20*time.Second, 100 * wmMbit}}
			var neg []string
			c.check = func(at time.Duration, k *wcore, in wcIn, o wcOut) {
				if at < from || at >= from+20*time.Second || !k.lastAt.Equal(in.now) {
					return
				}
				if at0, ok := k.srcAt[1]; (ok && at0.Equal(in.now) || k.slowN[1] > 0) && len(neg) < 3 {
					neg = append(neg, fmt.Sprintf("%.1fs (%s, held share %.2f)", at.Seconds(), k.state, k.sndBlocked(in.now)))
				}
			}
			wmRun(c)
			if len(neg) > 0 {
				t.Errorf("spell from %d s, seed %d: the path-held stream judged without evidence at %v", st, seed, neg)
			}
		}
	}
}

// The independent judge's two defects of the first version, and the rules
// that answer them:
//   - (a) after the first interval that judged a stream without evidence, one
//     that cannot judge (a dip of the link's rate) counted its 600 ms
//     turnaround as a 527 ms queue: a stream so judged keeps its turnarounds
//     out of q_turn until evidence returns (FM1's rule);
//   - (b) a held stream whose updates and turnarounds fell in turns (one
//     interval an update and no turnaround, the next the turnaround and no
//     update) left F after two updates: an update without a turnaround is no
//     verdict, a turnaround without an update is one; but two intervals in a
//     row with updates and no turnaround are (none turned within an interval).
func TestWcoreS2EvidenceGaps(t *testing.T) {
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	const dt = 0.4
	now := wtT0
	next := func() time.Time { now = now.Add(400 * time.Millisecond); return now }

	slow := wcStream{id: 1, win: wtNeed, upd: 1, read: uint64(14 * wmMbit * dt), turnMin: ms(600), turnLast: ms(600)}
	c := wfCore(now, 0)
	c.inF[1] = true
	for i, rd := range []float64{14, 5, 14, 14} { // judged, a dip, after it, judged
		s := slow
		s.read = uint64(rd * wmMbit * dt)
		c.qSndAt = now
		m := c.measure(wfIn(next(), s), dt)
		if want := i < 3; (len(m.f) == 1) != want || want && (m.qOK || !m.quiet) {
			t.Errorf("(a) interval %d: F %v, quiet %v, q_turn %v (ok %v); want in F %v with its turnaround out of q_turn", i+1, m.f, m.quiet, m.qTurn, m.qOK, want)
		}
	}

	held := func(upd uint32, turn time.Duration) wcStream {
		return wcStream{id: 1, win: wtNeed, upd: upd, hit: upd, read: uint64(40 * wmMbit * dt), turnMin: turn, turnLast: turn}
	}
	c = wfCore(now, 0)
	c.inF[1] = true
	for i := 0; i < 6; i++ {
		s := held(1, 0)
		if i%2 == 1 {
			s = held(0, ms(75))
		}
		c.qSndAt = now
		if m := c.measure(wfIn(next(), s), dt); len(m.f) != 1 || c.slowN[1] > 0 {
			t.Errorf("(b) interval %d (update %d, turnaround %v): F %v, slowN %d; want kept, no verdict against it", i+1, s.upd, s.turnMin, m.f, c.slowN[1])
		}
	}
	// a slow source whose updates and turnarounds fall in turns is judged on
	// its turnarounds; one with updates and no turnaround twice in a row on
	// the second of them
	c = wfCore(now, 0)
	c.inF[1] = true
	for i, s := range []wcStream{held(1, 0), held(0, ms(2700)), held(1, 0), held(0, ms(2700))} {
		c.qSndAt = now
		if m := c.measure(wfIn(next(), s), dt); (len(m.f) == 1) != (i < 3) {
			t.Errorf("updates and 2.7 s turnarounds in turns, interval %d: F %v, want in F %v", i+1, m.f, i < 3)
		}
	}
	c = wfCore(now, 0)
	c.inF[1] = true
	for i, want := range []int{1, 1, 0} {
		c.qSndAt = now
		if m := c.measure(wfIn(next(), held(1, 0)), dt); len(m.f) != want {
			t.Errorf("updates and no turnaround, interval %d: F %v, want %d stream(s)", i+1, m.f, want)
		}
	}
}

// turnBase made of slow streams' turnarounds (595 ms on a 72 ms path, a
// rise notePaths refused as a route change) is capped at T_base and the
// offset they had for the growth brake (turnRef) and the ramp's stop
// (fastStop) as for the evidence, but not where packets wait behind holes
// (every turnaround holds that wait there, and turnBase with it).
func TestWcoreS2CapTurn(t *testing.T) {
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	mk := func(holey bool) *wcore {
		c := wfCore(wtT0, 0)
		c.turnMins = wcMinWin{}
		c.turnMins.add(wtT0, ms(595), wcTurnWin)
		c.state, c.B, c.bPub = wcHold, 4*wtMiB, 4*wtMiB
		c.inF[1] = true
		c.last.holey = holey
		return c
	}
	in := wcIn{now: wtT0, poolFree: -1, streams: []wcStream{{id: 1, win: 4 * wtMiB, upd: 4, turnLast: ms(120)}}}
	for _, tc := range []struct {
		holey bool
		ref   time.Duration
	}{{false, ms(73)}, {true, ms(595)}} {
		c := mk(tc.holey)
		if o := c.out(in, wtT0, nil); o.turnRef != tc.ref {
			t.Errorf("holey %v: turnRef %v, want %v", tc.holey, o.turnRef, tc.ref)
		}
		c.state, c.B, c.bPub = wcStartup, 8*wtMiB, 4*wtMiB
		c.fastStop(in, false)
		if stopped := c.B == c.bPub && c.stopQ; stopped != !tc.holey {
			t.Errorf("holey %v: a ramp past a 120 ms turnaround stopped %v, want %v", tc.holey, stopped, !tc.holey)
		}
	}
}

// What of a record's span the path held the exit's sender: its writer's
// wait on the socket and the share of its segments resent.
func TestWcoreS2SndHeld(t *testing.T) {
	for _, tc := range []struct {
		snd  wcSnd
		want float64
	}{
		{wcSnd{fresh: true}, -1},
		{wcSnd{fresh: true, blocked: 0.45, blockedOK: true}, 0.45},
		{wcSnd{fresh: true, retrans: 30, segsOut: 100}, 0.3},
		{wcSnd{fresh: true, blocked: 0.45, blockedOK: true, retrans: 30, segsOut: 100}, 0.75},
	} {
		if got := sndHeld(tc.snd); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%+v: %v, want %v", tc.snd, got, tc.want)
		}
	}
}

// An exit whose CPU, not the path, holds the link's sender (the rig's
// 1000 Mbit/s path carries ~750: TLS and the smux writer). The data waits in
// the exit's smux, where no srtt sees it; its writer is busy, not waiting on
// its socket (txBlockedNs), and a pong goes ahead of it. τ_hold learns that
// wait, and HOLD's windows hold it: twice what the path takes in a round trip,
// turnarounds 50-80 ms past the path's floor with every queue signal at 0.
// The download is held by its window all the same (base's 2 MiB is far under
// what it moves in a round trip), and it stays fast. Judged against its own
// window (the share of it moved in floor + queue), FM2 found no evidence on
// the intervals where the rate dipped a little or a probe added window, the
// stream left F twice in a row, the core went OFF and STARTUP came again (the
// rig, 1000/72 q50, one download: 720 → 654 Mbit/s over three runs; the
// model: 728 → 662-694).
func TestWcoreS2CPUHeldStaysFast(t *testing.T) {
	for _, x := range []struct {
		rate     float64
		rtt, cpu int // cpu: the exit's writer, % of the path's nominal rate
	}{{1000, 72, 75}, {1000, 50, 75}, {1000, 72, 60}, {1000, 50, 60}, {300, 150, 75}} {
		for seed := uint64(1); seed <= 3; seed++ {
			c := wmPath(x.rate, wmMS(x.rtt), wmMS(50))
			c.mode, c.seed, c.dur, c.cpu = wmCore, seed, 45*time.Second, float64(x.cpu)/100*c.rate
			held := false
			var neg []string
			c.check = func(at time.Duration, k *wcore, in wcIn, o wcOut) {
				if !k.lastAt.Equal(in.now) {
					return
				}
				held = held || k.state == wcHold
				at0, ok := k.srcAt[1]
				if held && (ok && at0.Equal(in.now) || !k.inF[1] || k.state == wcOff) && len(neg) < 3 {
					neg = append(neg, fmt.Sprintf("%.1fs (%s, in F %v)", at.Seconds(), k.state, k.inF[1]))
				}
			}
			r := wmRun(c)
			if !held || len(neg) > 0 {
				t.Errorf("%.0f/%d, the exit's CPU at %d%%, seed %d: HOLD reached %v; judged without evidence, out of F or OFF after it at %v (%.1f Mbit)",
					x.rate, x.rtt, x.cpu, seed, held, neg, r.mbit)
			}
		}
	}
}

// The evidence of a stream whose window is past need (wcEvidUse): the share
// of a window of need it moves in the path's round trip, however long it
// turns. A download the exit's CPU holds (750 Mbit/s on a 72 ms path, its
// window from HOLD's τ_hold past twice what the path takes, no queue any
// signal shows) stays in F with its rate dipped to 576 Mbit/s and with a
// probe's window turning past the slack; a source of 40 Mbit/s whose window
// STARTUP took to 3 MiB leaves, as one under need does.
func TestWcoreS2EvidencePastNeed(t *testing.T) {
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	const dt = 0.4
	now := wtT0
	next := func() time.Time { now = now.Add(400 * time.Millisecond); return now }
	mib := func(x float64) uint32 { return uint32(x * wtMiB) }
	for _, tc := range []struct {
		name string
		s    wcStream
		stay bool
	}{
		{"held by the exit's CPU, 13 MiB, 125 ms", wcStream{id: 1, win: mib(13), upd: 20, read: uint64(750 * wmMbit * dt), turnMin: ms(125), turnLast: ms(130)}, true},
		{"held by the exit's CPU, its rate dipped to 576 Mbit/s", wcStream{id: 1, win: mib(12.94), upd: 19, read: uint64(576 * wmMbit * dt), turnMin: ms(128), turnLast: ms(128)}, true},
		{"held by the exit's CPU, a probe's 15 MiB past the slack", wcStream{id: 1, win: mib(15), upd: 18, read: uint64(740 * wmMbit * dt), turnMin: ms(160), turnLast: ms(165)}, true},
		{"a source of 40 Mbit/s with 3 MiB", wcStream{id: 1, win: mib(3), upd: 2, read: uint64(40 * wmMbit * dt), turnMin: ms(700), turnLast: ms(700)}, false},
		{"a source of 40 Mbit/s with 2 MiB, within the slack", wcStream{id: 1, win: mib(2), upd: 2, read: uint64(40 * wmMbit * dt), turnMin: ms(140), turnLast: ms(140)}, false},
	} {
		c := wfCore(now, 0)
		c.inF[1] = true
		for i := 0; i < 2; i++ {
			c.qSndAt = next()
			c.measure(wfIn(now, tc.s), dt)
		}
		if c.inF[1] != tc.stay {
			t.Errorf("%s: in F after two judged intervals %v, want %v", tc.name, c.inF[1], tc.stay)
		}
	}
}

// A slow source beside a fast download on a lossy path (0.1% random loss):
// data waits behind holes, no interval judges the lack of evidence, and a
// slow source that joined F (on its "use" of the window) stayed in it in 52
// to 98% of the evaluations. B, the knee and q_turn were made with it: the
// download beside it got 0.30 to 0.67 of what it gets alone on 1000/50
// (sources of 8, 14 and 27 Mbit/s, three seeds each), and 0.30 to 0.70 on
// 1000/100. A stream that moves under wcPeerShare of the link's fastest
// stream could not join F; it leaves it on two evaluations in a row too,
// holes or not.
func TestWcoreS2SlowBesideFastLossy(t *testing.T) {
	for _, x := range []struct {
		rtt  int
		gain bool // the download gets 3/4 of its rate alone
	}{{50, true}, {100, false}} {
		for _, src := range []float64{8, 14, 27} {
			for seed := uint64(1); seed <= 3; seed++ {
				c := wmPath(1000, wmMS(x.rtt), wmMS(50))
				c.mode, c.seed, c.loss, c.dur = wmCore, seed, 0.001, 60*time.Second
				alone := wmRun(c)
				c.streams = []wmStreamCfg{{}, {srcAt: []wmChange{{0, src * wmMbit}}}}
				evals, slowF := 0, 0
				c.check = func(at time.Duration, k *wcore, in wcIn, o wcOut) {
					if at >= 10*time.Second && k.lastAt.Equal(in.now) {
						evals++
						if k.inF[3] {
							slowF++
						}
					}
				}
				both := wmRun(c)
				// (the source's own pace is all it gets either way)
				if 2*slowF > evals || x.gain && both.mbit-src < 0.75*alone.mbit {
					t.Errorf("1000/%d l0.1, a source of %.0f Mbit/s beside the download, seed %d: the source in F in %d of %d evaluations, the download %.1f Mbit/s against %.1f alone",
						x.rtt, src, seed, slowF, evals, both.mbit-src, alone.mbit)
				}
			}
		}
	}
}

// The slow source on the field's reordering (wmS2Field: the Iran path's
// bursts of reordered packets, the exit's RACK sender): holes keep an
// interval from judging the lack of evidence (they lengthen the turnaround
// and hide the pauses), and the provisional reactions to holes take back
// what reordering alone did; neither keeps a slow source fast. A download
// whose source drops to 14–27 Mbit/s 15 s in, on 1000/72 and 300/85: no
// drain or cut, no "fill the path" and not fast from 5 s after the drop
// (the core before FM2, on a sweep of 48 such runs: fast in 9129 of 9166
// evaluations, 30 "fill the path", 39 drains; with it none; holes kept 37
// of 6581 evaluations from judging).
func TestWcoreS2SlowSourceReordered(t *testing.T) {
	for _, x := range []struct {
		rate float64
		rtt  int
		src  float64
	}{{1000, 72, 14}, {1000, 72, 27}, {300, 85, 20}} {
		for seed := uint64(1); seed <= 2; seed++ {
			c := wmS2Field(wmPath(x.rate, wmMS(x.rtt), wmMS(50)))
			c.mode, c.seed = wmCore, seed
			drop := 15 * time.Second
			c.dur = drop + 60*time.Second
			c.streams = []wmStreamCfg{{srcAt: []wmChange{{drop, x.src * wmMbit}}}}
			var ev []string
			fast := 0
			var cnt0, cnt1 wcCounters
			c.check = func(at time.Duration, k *wcore, in wcIn, o wcOut) {
				if at <= drop+time.Second {
					cnt0 = k.cnt
					return
				}
				cnt1 = k.cnt
				for _, e := range o.events {
					if e.kind == wcEvDrain || e.kind == wcEvFilled {
						ev = append(ev, fmt.Sprintf("%.1fs %s", at.Seconds(), e))
					}
				}
				if at > drop+5*time.Second && k.lastAt.Equal(in.now) && k.inF[1] {
					fast++
				}
			}
			r := wmRun(c)
			drains := cnt1.drainQ + cnt1.drainLoss + cnt1.drainRate - cnt0.drainQ - cnt0.drainLoss - cnt0.drainRate
			if len(ev) > 0 || fast > 0 || drains > 0 {
				t.Errorf("%s, source %.0f Mbit/s from 15 s, seed %d: fast in %d evaluations, %d drain(s) or cut(s), events %q (%.1f Mbit)",
					c.name, x.src, seed, fast, drains, ev, r.mbit)
			}
		}
	}
}

// Two records of the exit in one interval (an evaluation interval of up to
// 2 s and a record every 2 s): the merge keeps the larger share of the two
// spans in which the exit's writer waited on its socket, not only the later
// record's (post-merge review of FM2, L2). The path-held guard looks at the
// latest two records (sndBlocked), and with only the later one's share it
// never saw the earlier one's.
func TestWcoreS2MergeKeepsBlocked(t *testing.T) {
	rec := func(blk float64, ok bool) wcIn {
		return wcIn{now: wtT0, snd: wcSnd{fresh: true, srtt: 80 * time.Millisecond, minRTT: 72 * time.Millisecond,
			retrans: 1, segsOut: 1000, blocked: blk, blockedOK: ok}}
	}
	for _, tc := range []struct {
		a, b   wcIn
		want   float64
		wantOK bool
	}{
		{rec(0.8, true), rec(0.1, true), 0.8, true},
		{rec(0.1, true), rec(0.8, true), 0.8, true},
		{rec(0.8, true), rec(0, false), 0.8, true},
		{rec(0, false), rec(0.3, true), 0.3, true},
		{rec(0, false), rec(0, false), 0, false},
	} {
		acc := wcCopy(tc.a)
		wcMerge(&acc, tc.b)
		if acc.snd.blocked != tc.want || acc.snd.blockedOK != tc.wantOK || acc.snd.retrans != 2 || acc.snd.segsOut != 2000 {
			t.Errorf("records blocked %v (%v) then %v (%v): merged %+v, want blocked %v (%v) and the resends summed",
				tc.a.snd.blocked, tc.a.snd.blockedOK, tc.b.snd.blocked, tc.b.snd.blockedOK, acc.snd, tc.want, tc.wantOK)
		}
	}
}

// Before the exit's resend level is learned (sndRef), any resent segment
// reads as a loss worth a verdict (sndSmall needs the level): a record with
// one segment resent of 10000 set lossRecAt, and the next reaction to holes
// stood unjudged (lossUnjudged). It is set only once the level is known
// (post-merge review of FM2, L4).
func TestWcoreS2LossRecNeedsLevel(t *testing.T) {
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	now := wtT0.Add(10 * time.Second)
	mk := func() *wcore {
		c := wfCore(now, 0)
		c.sndSeen, c.sndLastAt = true, now
		return c
	}
	in := func(retrans uint64) wcIn {
		return wcIn{now: now, poolFree: -1, snd: wcSnd{fresh: true, srtt: ms(80), minRTT: ms(72), retrans: retrans, segsOut: 10000}}
	}
	c := mk()
	c.judgeCut(in(1))
	if !c.lossRecAt.IsZero() {
		t.Errorf("no resend level yet, one segment resent of 10000: lossRecAt %v, want none", c.lossRecAt)
	}
	c = mk()
	c.sndRef = wcLossRef{v: 0.0001, b: 4 * wtMiB, ok: true}
	c.judgeCut(in(100))
	if !c.lossRecAt.Equal(now) {
		t.Errorf("a level of 0.01%%, 1%% resent: lossRecAt %v, want %v", c.lossRecAt, now)
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
