package engine

import (
	"fmt"
	"testing"
	"time"
)

// Step 2: the code fixes the independent judge of step 1 proposed
// (docs/architecture/wip/z5-judge.fa.md §7.3, p1 to p4), each reproduced
// here before its fix.

// wtOffRecs is a core in OFF on a path of T_base tb whose exit's latest
// records (2 s apart, the newest now) showed the queues qs (ms, oldest
// first) and no resends; m is its interval of one fast stream with 2 MiB.
func wtOffRecs(tb time.Duration, qs ...int) (*wcore, wcMeas) {
	return wtOffRecsGap(tb, 2*time.Second, qs...)
}

// wtOffRecsGap is wtOffRecs with the records gap apart.
func wtOffRecsGap(tb, gap time.Duration, qs ...int) (*wcore, wcMeas) {
	c := wtCore(1)
	c.tBase = tb
	c.sndSeen, c.sndAny, c.sndAnyAt = true, true, wtT0.Add(-time.Minute)
	c.sndRef = wcLossRef{v: 0, b: wtNeed, ok: true}
	for i, q := range qs {
		at := wtT0.Add(-time.Duration(len(qs)-1-i) * gap)
		d := time.Duration(q) * time.Millisecond
		copy(c.qSnds[1:], c.qSnds[:])
		c.qSnds[0] = wcRate{at, d.Seconds()}
		c.qSnd, c.qSndAt = d, at
	}
	m := c.measure(wtIn(wtT0, 1, wtNeed), 1)
	// (the stream is F, steady at 20 MB/s: only the queue fields are measure's)
	w := wtMeas(1, 20e6, wtNeed)
	m.f, m.sameF, m.rF, m.rLink, m.wF, m.wPrev, m.hStep, m.hit = w.f, true, w.rF, w.rLink, w.wF, w.wPrev, w.hStep, 1
	m.idleF, m.quiet = false, false
	return c, m
}

// TestWcoreS2HarmStandingQueue (judge p1): "do no harm" kept the core OFF
// on the largest q_snd of the exit's latest three records past Δq. A record
// is a snapshot of the queue, and on a path with room the sender's credit
// bursts (3.9 times the path's rate) swing it: the rig's 300/50 read 0.9 to
// 20.3 ms, 300/100 3.3 to 18.4 ms, so one burst's record kept the core OFF
// (300/100: 123, 234 and 258 Mbit/s; 300/50 OFF in both class B runs). A
// queue the windows hold past the knee stands in every record (the rig's
// full paths, 100/100, 100/50 and 4 downloads on 300/150: 6.6 to 52 ms).
func TestWcoreS2HarmStandingQueue(t *testing.T) {
	for _, tc := range []struct {
		name string
		tb   int
		gap  time.Duration // between records (0: 2 s)
		qs   []int
		harm bool
	}{
		{"300/50: bursts, one record under Δq (5 ms)", 50, 0, []int{14, 3, 18}, false},
		{"300/100: bursts, one record under Δq (10 ms)", 100, 0, []int{18, 12, 4}, false},
		{"100/50: a queue in every record", 50, 0, []int{7, 30, 12}, true},
		{"100/100: a queue in every record", 100, 0, []int{14, 30, 11}, true},
		{"one record, a queue", 50, 0, []int{12}, true},
		{"a record past the latest three is no part of it", 50, 0, []int{2, 12, 9, 14}, true},
		// (4 s apart the three slots hold records of 8, 4 and 0 s ago: the
		// oldest is past three record spans, 7.5 s, and only its age leaves it
		// out)
		{"a record older than three spans is no part of it", 50, 4 * time.Second, []int{2, 12, 14}, true},
		{"... within them it is", 50, 3 * time.Second, []int{2, 12, 14}, false},
	} {
		gap := tc.gap
		if gap == 0 {
			gap = 2 * time.Second
		}
		c, m := wtOffRecsGap(time.Duration(tc.tb)*time.Millisecond, gap, tc.qs...)
		if got := c.harm(m); got != tc.harm {
			t.Errorf("%s (records %v ms %v apart, T_base %d ms): harm %v, want %v", tc.name, tc.qs, gap, tc.tb, got, tc.harm)
		}
	}
}

// TestWcoreS2HarmLossLevel (judge p1): "do no harm" kept the core OFF on
// the exit's resend share past 1%, its OFF level an average anchored on the
// first record. The rig's 1000/100 with 1% random loss resent 1.02 to 1.10%
// in its first records and 1.0 to 1.03% after: the core stayed OFF for 8.8
// to 16.6 s (213 → 79 Mbit/s against ce19dee, whose first records came
// under 1%). Random loss steady at its level is what τ_hold covers: up to
// 1.5% the core starts once two records in a row found the level steady;
// past 1%, loss that comes in bursts (a record past 1.5% or past the level
// by its spread: a policer's, the windows overflowing a queue, a slow
// start's overshoot), or a policer the core met, keeps it OFF.

func TestWcoreS2HarmLossLevel(t *testing.T) {
	rec := func(c *wcore, m *wcMeas, share float64) {
		m.snd, m.sndOK, m.sndSegs, m.sndAt = share, true, 50000, m.at
	}
	for _, tc := range []struct {
		name   string
		shares []float64
		police bool
		harm   bool
	}{
		{"1% random loss, the first record past it", []float64{0.0110, 0.0102, 0.0103, 0.0101}, false, false},
		{"... not yet steady for two records", []float64{0.0110, 0.0102}, false, true},
		{"1.4% steady", []float64{0.014, 0.0138, 0.0141, 0.0139}, false, false},
		{"a slow start's 1.26%, then 1.92%", []float64{0.0126, 0.0192, 0.0110, 0.0105, 0.0102}, false, true},
		{"past 1.5%", []float64{0.016, 0.0158, 0.0162}, false, true},
		{"1.2% in bursts", []float64{0.012, 0.025, 0.004, 0.011}, false, true},
		{"1.3% left by a slow start's 11% (its first record)", []float64{0.11, 0, 0, 0, 0, 0, 0}, false, true},

		{"1.1% behind a policer the core met", []float64{0.0110, 0.0108, 0.0109}, true, true},
		{"0.5%", []float64{0.005, 0.005}, false, false},
		{"a loss burst in the latest records (2.3%), the level under 1%", []float64{0.004, 0.023}, false, true},
		{"... older than three record spans", []float64{0.004, 0.023, 0.002, 0.002, 0.002, 0.002}, false, false},
	} {
		c, m := wtOffRecs(100*time.Millisecond, 1, 1, 1)
		c.sndRef = wcLossRef{}
		if tc.police {
			c.policedAt = wtT0.Add(-time.Minute)
		}
		for i, s := range tc.shares {
			m.at = wtT0.Add(time.Duration(i) * 2 * time.Second)
			rec(c, &m, s)
			c.off(wtIn(m.at, 1, wtNeed), m, nil)
			c.state, c.offN = wcOff, 0 // (only the level and its signature are under test)
		}
		if got := c.harm(m); got != tc.harm {
			t.Errorf("%s (records %v, level %.2f%%): harm %v, want %v", tc.name, tc.shares, 100*c.sndRef.v, got, tc.harm)
		}
	}
}

// TestWcoreS2HarmModel (judge p1): the link model's 1000/100 behind a 50 ms
// queue with 1% random loss: the exit resends 0.85 to 1.18% a record, and the
// core stayed OFF until 16.4 s in seed 2 (8 s in seed 1). Each seed leaves
// OFF within the first 10 s now (two records after the first that find the
// level steady), and the core resends within its level.
func TestWcoreS2HarmModel(t *testing.T) {
	for _, x := range []struct {
		rate   float64
		rtt, q int
		loss   float64
		seeds  int
		within time.Duration
	}{{1000, 100, 50, 0.01, 3, 10 * time.Second}} {
		for seed := 1; seed <= x.seeds; seed++ {
			c := wmPath(x.rate, wmMS(x.rtt), wmMS(x.q))
			c.loss, c.seed, c.mode = x.loss, uint64(seed), wmCore
			on := time.Duration(-1)
			c.check = func(at time.Duration, k *wcore, in wcIn, o wcOut) {
				if on < 0 && k.state != wcOff {
					on = at
				}
			}
			r := wmRun(c)
			name := fmt.Sprintf("%s l%g s%d", c.name, 100*x.loss, seed)
			if on < 0 || on > x.within {
				t.Errorf("%s: the core left OFF at %v, want within %v (%.1f Mbit, resent %.2f%%)", name, on, x.within, r.mbit, 100*r.resend)
			} else {
				t.Logf("%s: on at %v, %.1f Mbit, resent %.2f%%", name, on, r.mbit, 100*r.resend)
			}
			if r.resend > x.loss+0.005 {
				t.Errorf("%s: resent %.2f%% on %g%% random loss", name, 100*r.resend, 100*x.loss)
			}
		}
	}
}

// TestWcoreS2FloorQueueNoProbe (judge p2): at the floor (B = n_F·need: in
// stage 1 the windows without the core) behind a queue that stands past
// qTarget + Δq, the base windows already fill the path and a probe only adds
// queue: the rig's 8 downloads on 1000/100 held 16 MiB with q_snd at 24 to
// 43 ms and probed to 18.2 MiB, 3.5% more rate for a p99 2.27 times base's.
// Once two evaluations in a row showed such a queue (the exit's record, or,
// with no fresh record, the link socket's own srtt past qTarget + wcDqOwn)
// no probe starts; off the floor, or under that queue, a due probe does.
// The link socket's srtt holds the edge's own sending too (the user's
// uploads, window updates): with a fresh record of the exit it judges
// nothing, as everywhere else (measure's noQ, slowed). Taken with the
// record, an upload on the same link lifting q_own past 25 ms with q_snd
// at 1 ms held the floor's probes off for good (review, M2).
func TestWcoreS2FloorQueueNoProbe(t *testing.T) {
	ms := time.Millisecond
	for _, tc := range []struct {
		name       string
		b          uint64        // B, one stream (the floor: need)
		r          float64       // the link's rate (10 MB/s: HOLD's budget at the floor)
		qSnd, qOwn time.Duration // qSnd 0: no record of the exit
		probe      bool
	}{
		{"at the floor, the exit's record past qTarget+Δq", wtNeed, 10e6, 32 * ms, 1 * ms, false},
		{"... the link's own srtt past qTarget+wcDqOwn, no record", wtNeed, 10e6, 0, 32 * ms, false},
		{"at the floor, a queue under qTarget+Δq", wtNeed, 10e6, 18 * ms, 1 * ms, true},
		{"off the floor (a queue there is HOLD's to drain)", 4 * wtMiB, 60e6, 32 * ms, 1 * ms, true},
		{"at the floor, the exit's record 1 ms, the link's own srtt 30 ms (an upload on it)", wtNeed, 10e6, 1 * ms, 30 * ms, true},
		{"... the link's own srtt 1 ms", wtNeed, 10e6, 1 * ms, 1 * ms, true},
	} {
		c, _ := wtHold()
		c.rhat = c.rhat[:0]
		c.noteRate(wtT0, tc.r)
		in := wtIn(wtT0, 1, uint32(tc.b))
		c.B, c.bPub, c.bGood = tc.b, tc.b, tc.b
		// (the references hold the queue as HOLD found it: no growth)
		c.qSndRef, c.qSndRefOK, c.qOwnRef = tc.qSnd, tc.qSnd > 0, tc.qOwn
		probed := false
		for i := 0; i < 3; i++ {
			now := wtT0.Add(time.Duration(i) * time.Second)
			in.now = now
			if i == 2 {
				c.probeAt = now // due at the third
			}
			m := wtMeas(1, tc.r, float64(tc.b))
			m.at, m.qOK, m.qTurn = now, true, c.qRef
			if tc.qSnd > 0 {
				m.qSnd, m.qSndOK = tc.qSnd, true
			}
			m.qOwn, m.qOwnOK = tc.qOwn, true
			var ev []wcEvent
			c.hold(in, m, &ev)
			if c.state == wcProbe {
				probed = true
				break
			}
			if c.state != wcHold {
				t.Fatalf("%s: %v at evaluation %d, want HOLD", tc.name, c.state, i)
			}
		}
		if probed != tc.probe {
			t.Errorf("%s: probed %v, want %v (B %.2f MiB)", tc.name, probed, tc.probe, float64(c.B)/wtMiB)
		}
	}
}

// TestWcoreS2FloorQueueModel (judge p2): on the link model, one download on
// 1000/100 and 7 more that join it at 10 s: 16 MiB of windows without the
// core hold a standing queue of 30 to 40 ms there, and the core probed from
// that floor to 16–18.1 MiB 8 to 12 times in 6 seeds. None starts at the
// floor behind such a queue now.
func TestWcoreS2FloorQueueModel(t *testing.T) {
	for _, q := range []int{50, 100, 200} {
		for seed := uint64(1); seed <= 3; seed++ {
			c := wmPath(1000, wmMS(100), wmMS(q))
			c.seed, c.mode = seed, wmCore
			c.streams = make([]wmStreamCfg, 8)
			for i := 1; i < len(c.streams); i++ {
				c.streams[i].start = 10 * time.Second
			}
			// each HOLD evaluation: at the floor behind a queue past
			// qTarget + Δq (the record's; with none fresh, q_own's past
			// qTarget + wcDqOwn)
			floorQ := func(k *wcore) bool {
				m, now, b := k.last, k.lastAt, k.B
				if k.state == wcProbe {
					b = k.probeFrom // (the evaluation that started it)
				}
				if floor := uint64(max(len(m.f), 1)) * uint64(k.cmin()); b > floor*21/20 {
					return false
				}
				if q, ok := m.pathQueue(); ok {
					return q > k.qTarget(now)+k.dq()
				}
				return m.qOwnOK && m.qOwn > k.qTarget(now)+wcDqOwn
			}
			var queued []bool
			var bad []string
			prev, lastEval := wcOff, time.Time{}
			c.check = func(at time.Duration, k *wcore, in wcIn, o wcOut) {
				if k.lastAt != lastEval && (k.state == wcHold || prev == wcHold) {
					lastEval = k.lastAt
					queued = append(queued, floorQ(k))
				}
				if o.state == wcProbe && prev != wcProbe {
					n := len(queued)
					floor := uint64(max(len(k.last.f), 1)) * uint64(k.cmin())
					if n >= 2 && queued[n-1] && queued[n-2] && k.probeFrom <= floor*21/20 {
						bad = append(bad, fmt.Sprintf("%.1fs → %.1f MiB", at.Seconds(), float64(k.B)/wtMiB))
					}
				}
				prev = o.state
			}
			r := wmRun(c)
			name := fmt.Sprintf("1000/100 q%d 1+7 at 10 s s%d", q, seed)
			if len(bad) > 0 {
				t.Errorf("%s: probes from the floor behind a standing queue at %v (%.0f Mbit, interactive p99 %.0f ms)", name, bad, r.mbit, r.iP99)
			} else {
				t.Logf("%s: %.0f Mbit, resent %.2f%%, interactive %.0f/%.0f ms, probes %v", name, r.mbit, 100*r.resend, r.iP50, r.iP99, r.probes)
			}
		}
	}
}

// TestWcoreS2LongStartupModel (judge p3): past wcLongRTT one record of the
// exit with a queue past wcDqMin (5 ms) ended STARTUP, and a record is a
// snapshot: the rig's 1000/500 ended at 2 MiB on one of 6.4 ms. On the
// model's 300/500 behind 200 ms the core reached 53 to 61% of N4's gain in
// seeds 1 to 4; with STARTUP ending only on a queue past what the windows
// without the core left by Δq (or on resends), 78 to 86%.
func TestWcoreS2LongStartupModel(t *testing.T) {
	for seed := uint64(1); seed <= 4; seed++ {
		c := wmPath(300, wmMS(500), wmMS(200))
		c.seed = seed
		b, n, k := wmRun(c.with(wmBase)), wmRun(c.with(wmN4)), wmRun(c.with(wmCore))
		share := (k.mbit - b.mbit) / (n.mbit - b.mbit)
		msg := fmt.Sprintf("300/500 q200 s%d: base %.1f, N4 %.1f, core %.1f Mbit (%.0f%% of N4's gain), resent %.2f%%", seed, b.mbit, n.mbit, k.mbit, 100*share, 100*k.resend)
		if share < 0.7 || k.resend > b.resend+0.005 {
			t.Error(msg)
		} else {
			t.Log(msg)
		}
	}
}

// Post-merge review of step 2's p1 to p3: what the three commits left open,
// each reproduced here before its fix.

// TestWcoreS2LongStandingQueue (review, M1): past wcLongRTT a record of the
// exit whose queue is past wcDqMin but within Δq of what OFF left, with no
// resends, holds B where it is and counts for nothing (judge p3: one such
// record is a snapshot). A run of them is no snapshot: a ×1.25 step past the
// knee by 5 to 10% leaves a queue that stands under Δq (60 ms at 600 ms),
// and no rule of STARTUP saw it (flat, the autopilot's freeze, a settle
// part: none came). 60 records of 45 ms, 2 s apart, with the autopilot
// probing the pool in every other interval, left STARTUP at 4 MiB with flat
// 0 after 120 s. Once the exit's latest three records all show the queue,
// STARTUP ends there.
func TestWcoreS2LongStandingQueue(t *testing.T) {
	c, in := wtStartup()
	c.tBase, c.sndSeen, c.sndAny = 600*time.Millisecond, true, true
	c.qOffSnd = 3 * time.Millisecond
	push := func(at time.Time, q time.Duration) {
		copy(c.qSnds[1:], c.qSnds[:])
		c.qSnds[0] = wcRate{at, q.Seconds()}
		c.qSnd, c.qSndAt = q, at
	}
	for i := 3; i >= 1; i-- { // the clean records OFF left on
		push(wtT0.Add(-time.Duration(i)*2*time.Second), 2*time.Millisecond)
	}
	end := -1
	var ev []wcEvent
	for i := 0; i < 60; i++ {
		now := wtT0.Add(time.Duration(i) * 2 * time.Second)
		in.now, in.apProbing = now, i%2 == 1
		m := wtMeas(1, 6e6, 4*wtMiB)
		m.at = now
		m.qTurn, m.qOK = c.qRef, true
		push(now, 45*time.Millisecond)
		m.sndOK, m.snd, m.sndSegs, m.sndAt = true, 0, 20000, now
		m.qSnd, m.qSndOK = 45*time.Millisecond, true
		c.startup(in, m, &ev)
		if c.state != wcStartup {
			end = i
			break
		}
	}
	switch {
	case end < 0:
		t.Errorf("60 records of 45 ms (Δq 60 ms): still STARTUP at %.2f MiB, flat %d", float64(c.B)/wtMiB, c.flat)
	case end != wcSndHist-1:
		t.Errorf("STARTUP ended at record %d, want the %dth (a queue in each of the latest %d)", end+1, wcSndHist, wcSndHist)
	case len(ev) == 0 || ev[0].kind != wcEvFilled:
		t.Errorf("STARTUP ended with events %+v, want the knee", ev)
	default:
		t.Logf("the knee at record %d: %v, B %.2f MiB", end+1, c.state, float64(c.B)/wtMiB)
	}
}

// wtOffMeas is an OFF interval at at of the core c with n fast streams of
// need each, steady at 20 MB/s, after three records of the exit 2 s apart
// that showed 1 ms and no resends.
func wtOffMeas(c *wcore, at time.Time, n int) (wcIn, wcMeas) {
	for i := wcSndHist - 1; i >= 0; i-- {
		rat := at.Add(-time.Duration(i) * 2 * time.Second)
		copy(c.qSnds[1:], c.qSnds[:])
		c.qSnds[0] = wcRate{rat, 0.001}
		c.qSnd, c.qSndAt = time.Millisecond, rat
	}
	in := wtIn(at, n, wtNeed)
	m := c.measure(in, 1)
	w := wtMeas(n, 20e6, float64(n)*wtNeed)
	m.f, m.sameF, m.rF, m.rLink, m.wF, m.wPrev, m.hStep, m.hit = w.f, true, w.rF, w.rLink, w.wF, w.wPrev, w.hStep, 1
	m.idleF, m.quiet = false, false
	return in, m
}

// wtFirstStep takes c out of OFF at at (n fast streams; the window pool
// has poolFree left, < 0: none): STARTUP's first step.
func wtFirstStep(c *wcore, at time.Time, n int, poolFree int64) {
	in, m := wtOffMeas(c, at, n)
	in.poolFree = poolFree
	c.state, c.offN = wcOff, 1
	c.off(in, m, nil)
}

// wtStep1 is a STARTUP interval at at: the n streams read rF together, the
// window turnaround is q past q_ref.
func wtStep1(c *wcore, at time.Time, n int, rF float64, q time.Duration, sameF bool) []wcEvent {
	in := wtIn(at, n, uint32(c.B/uint64(n)))
	m := wtMeas(n, rF, float64(c.B))
	m.at, m.sameF = at, sameF
	m.qTurn, m.qOK = c.qRef+q, true
	var ev []wcEvent
	c.startup(in, m, &ev)
	return ev
}

// TestWcoreS2FirstStepTrial (judge p1; review, J1): STARTUP's first step
// from OFF is the test of whether the windows without the core were past
// the knee already. When the rate did not follow it and the queue did, the
// core goes back OFF (startOff, fullAt, fullN) and harm holds it there for
// wcFullOff while F keeps its size; a step the rate followed is no such
// finding. The step has to be one: in an interval where a stream joined or
// left F (sameF false) STARTUP starts its comparison over, and a step the
// window pool left no room for is none. Judged in the interval after,
// either took the core OFF for 32 s on a queue the step had no part in: a
// multi-connection download whose second connection joins F in STARTUP's
// first interval (steady rate after, q_turn 18 ms past q_ref).
func TestWcoreS2FirstStepTrial(t *testing.T) {
	ms := time.Millisecond
	t1 := wtT0.Add(2 * time.Second)

	// the rule
	c, _ := wtOffRecs(100*ms, 1, 1, 1)
	wtFirstStep(c, wtT0, 1, -1)
	if c.state != wcStartup || !c.startOff || c.B != 3*wtMiB {
		t.Fatalf("from OFF: %v startOff %v B %.2f MiB, want STARTUP's first step to 3 MiB", c.state, c.startOff, float64(c.B)/wtMiB)
	}
	ev := wtStep1(c, t1, 1, 20e6, 18*ms, true)
	if c.state != wcOff || !c.fullAt.Equal(t1) || c.fullN != 1 || c.startOff || len(ev) != 1 || ev[0].kind != wcEvFilled {
		t.Errorf("the first step, the rate flat and the queue up 18 ms: %v fullAt %v fullN %d events %+v, want OFF, the finding at %v with 1 stream",
			c.state, c.fullAt.Sub(wtT0), c.fullN, ev, t1.Sub(wtT0))
	}
	for _, x := range []struct {
		name  string
		after time.Duration
		n     int
		harm  bool
	}{
		{"within wcFullOff, as many fast streams", wcFullOff - time.Second, 1, true},
		{"past wcFullOff", wcFullOff + time.Second, 1, false},
		{"more fast streams (their windows without the core only add to it)", time.Second, 2, true},
	} {
		c := *c
		_, m := wtOffMeas(&c, t1.Add(x.after), x.n)
		if got := c.harm(m); got != x.harm {
			t.Errorf("after the finding, %s: harm %v, want %v", x.name, got, x.harm)
		}
	}

	// the rate followed the step: no finding, STARTUP goes on
	c, _ = wtOffRecs(100*ms, 1, 1, 1)
	wtFirstStep(c, wtT0, 1, -1)
	wtStep1(c, t1, 1, 30e6, 0, true)
	if c.state != wcStartup || c.startOff || !c.fullAt.IsZero() {
		t.Errorf("the first step, the rate up 50%% with B: %v startOff %v fullAt %v, want STARTUP on, no finding", c.state, c.startOff, c.fullAt)
	}

	// a stream joined or left F in the first interval: its step was never
	// judged
	c, _ = wtOffRecs(100*ms, 1, 1, 1)
	wtFirstStep(c, wtT0, 1, -1)
	wtStep1(c, t1, 1, 20e6, 0, false)
	ev = wtStep1(c, t1.Add(2*time.Second), 1, 20e6, 18*ms, true)
	if c.state == wcOff || !c.fullAt.IsZero() {
		t.Errorf("F changed in the first interval, then the queue up 18 ms at a steady rate: %v fullAt %v events %+v, want no finding",
			c.state, c.fullAt.Sub(wtT0), ev)
	}

	// the window pool left no room for the step: no step, no finding
	c, _ = wtOffRecs(100*ms, 1, 1, 1)
	wtFirstStep(c, wtT0, 1, 0)
	if c.state != wcStartup || c.B != 2*wtMiB {
		t.Fatalf("from OFF with the pool empty: %v B %.2f MiB, want STARTUP at 2 MiB", c.state, float64(c.B)/wtMiB)
	}
	ev = wtStep1(c, t1, 1, 20e6, 18*ms, true)
	if c.state == wcOff || !c.fullAt.IsZero() {
		t.Errorf("no step (the pool), the queue up 18 ms: %v fullAt %v events %+v, want no finding", c.state, c.fullAt.Sub(wtT0), ev)
	}
}

// TestWcoreS2FirstStepTrialBackoff (review, J2): on a path the windows
// without the core already fill, with records that swing, the least of the
// latest three dips under Δq now and then, and the first step's trial came
// again each wcFullOff (32 s) for as long as the downloads ran: each time a
// ×1.25 to ×1.5 step for an interval or two (a wave of queue every 32 s), a
// "fill the path … window held at …" line though the core went OFF, and a
// count of growth; and with the hold kept only for as many fast streams,
// F's count swinging by one let the trial come again within seconds. The
// hold doubles with each trial in a row on the link that found the path
// full, up to wcFullOffMax, and keeps OFF with as many fast streams or
// more; a first step the rate followed starts it over.
func TestWcoreS2FirstStepTrialBackoff(t *testing.T) {
	ms, sec := time.Millisecond, time.Second
	// trial runs a first step at at with n fast streams, the rate flat and
	// the queue up 18 ms: the finding; then the hold harm keeps, to the
	// second (and the next trial may come at its end)
	trial := func(c *wcore, at time.Time, n int) (time.Duration, time.Time, []wcEvent) {
		t.Helper()
		wtFirstStep(c, at, n, -1)
		if c.state != wcStartup {
			t.Fatalf("at %v: %v, want STARTUP (no hold left)", at.Sub(wtT0), c.state)
		}
		t1 := at.Add(2 * sec)
		ev := wtStep1(c, t1, n, 20e6, 18*ms, true)
		if c.state != wcOff || !c.fullAt.Equal(t1) {
			t.Fatalf("at %v: %v fullAt %v, want the finding", t1.Sub(wtT0), c.state, c.fullAt.Sub(wtT0))
		}
		for s := sec; s <= 10*time.Minute; s += sec {
			if _, m := wtOffMeas(c, t1.Add(s), n); !c.harm(m) {
				return s, t1.Add(s), ev
			}
		}
		t.Fatalf("at %v: harm past 10 minutes", t1.Sub(wtT0))
		return 0, t1, ev
	}
	c, _ := wtOffRecs(100*ms, 1, 1, 1)
	at := wtT0
	var got []time.Duration
	for i := 0; i < 6; i++ {
		h, end, ev := trial(c, at, 1)
		got = append(got, h)
		at = end
		// (its line: the core is off for the hold, and holds no window)
		if len(ev) != 1 || ev[0].kind != wcEvFilled || ev[0].off != h {
			t.Errorf("trial %d: events %+v, want a finding that the core is off for %v", i+1, ev, h)
		}
	}
	want := []time.Duration{32 * sec, 64 * sec, 128 * sec, 256 * sec, 256 * sec, 256 * sec}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("six trials in a row that found the path full: holds %v, want %v", got, want)
	}
	// a first step the rate followed starts the backoff over
	wtFirstStep(c, at, 1, -1)
	wtStep1(c, at.Add(2*sec), 1, 30e6, 0, true)
	if c.state != wcStartup {
		t.Fatalf("a first step the rate followed: %v, want STARTUP on", c.state)
	}
	c.toOff()
	if h, _, _ := trial(c, at.Add(10*sec), 1); h != wcFullOff {
		t.Errorf("after a first step the rate followed: hold %v, want %v", h, wcFullOff)
	}
	// the hold keeps OFF with as many fast streams or more (each adds its
	// own windows without the core to what filled the path), not with
	// fewer: a trial may come at once, and the backoff goes on
	c, _ = wtOffRecs(100*ms, 1, 1, 1)
	trial(c, wtT0, 2)
	t1 := c.fullAt
	for _, x := range []struct {
		n    int
		harm bool
	}{{2, true}, {3, true}, {1, false}} {
		c := *c
		_, m := wtOffMeas(&c, t1.Add(sec), x.n)
		if got := c.harm(m); got != x.harm {
			t.Errorf("a second after a finding with 2 fast streams, %d: harm %v, want %v", x.n, got, x.harm)
		}
	}
	if h, _, _ := trial(c, t1.Add(2*sec), 1); h != 2*wcFullOff {
		t.Errorf("a finding with 2 fast streams, then at once one with 1: hold %v, want %v", h, 2*wcFullOff)
	}
}
