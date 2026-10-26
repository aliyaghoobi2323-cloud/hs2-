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
	c := wtCore(1)
	c.tBase = tb
	c.sndSeen, c.sndAny, c.sndAnyAt = true, true, wtT0.Add(-time.Minute)
	c.sndRef = wcLossRef{v: 0, b: wtNeed, ok: true}
	for i, q := range qs {
		at := wtT0.Add(-time.Duration(len(qs)-1-i) * 2 * time.Second)
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
		qs   []int
		harm bool
	}{
		{"300/50: bursts, one record under Δq (5 ms)", 50, []int{14, 3, 18}, false},
		{"300/100: bursts, one record under Δq (10 ms)", 100, []int{18, 12, 4}, false},
		{"100/50: a queue in every record", 50, []int{7, 30, 12}, true},
		{"100/100: a queue in every record", 100, []int{14, 30, 11}, true},
		{"one record, a queue", 50, []int{12}, true},
		{"a record older than three spans is no part of it", 50, []int{2, 12, 9, 14}, true},
	} {
		c, m := wtOffRecs(time.Duration(tc.tb)*time.Millisecond, tc.qs...)
		if got := c.harm(m); got != tc.harm {
			t.Errorf("%s (records %v ms, T_base %d ms): harm %v, want %v", tc.name, tc.qs, tc.tb, got, tc.harm)
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
// Once two evaluations in a row showed such a queue (the exit's record, or
// the link socket's own srtt past qTarget + wcDqOwn) no probe starts; off
// the floor, or under that queue, a due probe does.
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
			// qTarget + Δq (the record's, or q_own's past qTarget + wcDqOwn)
			floorQ := func(k *wcore) bool {
				m, now, b := k.last, k.lastAt, k.B
				if k.state == wcProbe {
					b = k.probeFrom // (the evaluation that started it)
				}
				if floor := uint64(max(len(m.f), 1)) * uint64(k.cmin()); b > floor*21/20 {
					return false
				}
				if q, ok := m.pathQueue(); ok && q > k.qTarget(now)+k.dq() {
					return true
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

// TestWcoreS2BackAfterSlower (judge p4): what a slower spell of the path
// taught (its loss point, the backoff of probes that failed in it or right
// after it) held the core under the path once it was fast again. HOLD
// evaluations in a row (wcBackN) in which the link carries more than
// wcBackGrowth past its rate when that loss point came, with the queue
// empty, clear both and probe at once; not behind a policer, not with a
// queue standing, not for a rate within wcBackGrowth of it.
func TestWcoreS2BackAfterSlower(t *testing.T) {
	ms := time.Millisecond
	for _, tc := range []struct {
		name    string
		r       float64       // the link's rate (the loss point came at 6 MB/s)
		q       time.Duration // the exit's record
		policed bool
		back    bool
	}{
		{"faster with the queue empty", 10e6, 1 * ms, false, true},
		{"... behind a policer", 10e6, 1 * ms, true, false},
		{"faster with a queue standing", 10e6, 30 * ms, false, false},
		{"within wcBackGrowth of the rate then", 7e6, 1 * ms, false, false},
	} {
		c, _ := wtHold()
		c.rhat = c.rhat[:0]
		c.noteRate(wtT0, tc.r)
		c.B, c.bPub, c.bGood = 3*wtMiB, 3*wtMiB, 3*wtMiB
		c.bCap, c.bCapAt, c.backoff = 3*wtMiB, wtT0.Add(-time.Second), 2
		c.backR, c.backAt = 6e6, wtT0.Add(-5*time.Second)
		c.qSndRef, c.qSndRefOK = tc.q, true
		if tc.policed {
			c.policedAt = wtT0.Add(-time.Minute)
		}
		in := wtIn(wtT0, 1, 3*wtMiB)
		for i := 0; i < wcBackN; i++ {
			now := wtT0.Add(time.Duration(i) * time.Second)
			in.now = now
			m := wtMeas(1, tc.r, 3*wtMiB)
			m.at, m.qOK, m.qTurn = now, true, c.qRef
			m.snd, m.sndOK, m.sndSegs, m.sndAt = 0, true, 20000, now
			m.qSnd, m.qSndOK, m.qOwn, m.qOwnOK = tc.q, true, tc.q, true
			var ev []wcEvent
			c.hold(in, m, &ev)
		}
		back := c.bCap == 0 && c.backoff == 0 && (c.state == wcProbe || !c.probeAt.After(in.now))
		if back != tc.back {
			t.Errorf("%s: loss point %.2f MiB, backoff %d, %v (probe at %v): back %v, want %v",
				tc.name, float64(c.bCap)/wtMiB, c.backoff, c.state, c.probeAt.Sub(in.now), back, tc.back)
		}
	}
}

// TestWcoreS2BackAfterSlowerModel (judge p4): the model's 300 → 50 → 300
// behind 50 ms (the rig's dlchg) got back to 90% of the rate before the
// drop within 10 s in 4 of 6 seeds, 1000 → 300 → 1000 behind 20 ms (the
// rig's FP chg) in none; now 5 of 6 and 1 of 6 (the loss points of that
// spell on a 20 ms queue come with under wcPoliceQ of it, as a policer's,
// and set no backR), with resends within base's + 3 points.
func TestWcoreS2BackAfterSlowerModel(t *testing.T) {
	for _, x := range []struct {
		rate, to    float64
		rtt, q      int
		down, up    time.Duration
		dur         time.Duration
		want, seeds int
	}{
		{300, 50, 150, 50, 18 * time.Second, 30 * time.Second, 42 * time.Second, 5, 6},
		{1000, 300, 100, 20, 15 * time.Second, 30 * time.Second, 50 * time.Second, 1, 6},
	} {
		ok := 0
		var b90 []string
		for seed := 1; seed <= x.seeds; seed++ {
			c := wmPath(x.rate, wmMS(x.rtt), wmMS(x.q))
			c.seed, c.dur = uint64(seed), x.dur
			c.changes = []wmChange{{x.down, x.to * wmMbit}, {x.up, x.rate * wmMbit}}
			b, k := wmRun(c.with(wmBase)), wmRun(c.with(wmCore))
			if k.back90 >= 0 && k.back90 <= 10*time.Second {
				ok++
			}
			if k.back90 < 0 {
				b90 = append(b90, "never")
			} else {
				b90 = append(b90, fmt.Sprintf("%.1f", k.back90.Seconds()))
			}
			if k.resend > b.resend+0.03 {
				t.Errorf("%s s%d: resent %.2f%% (base %.2f%%)", c.name, seed, 100*k.resend, 100*b.resend)
			}
		}
		name := fmt.Sprintf("%.0f>%.0f>%.0f/%d q%d", x.rate, x.to, x.rate, x.rtt, x.q)
		if ok < x.want {
			t.Errorf("%s: back to 90%% within 10 s in %d of %d seeds, want %d (%v s)", name, ok, x.seeds, x.want, b90)
		} else {
			t.Logf("%s: back to 90%% within 10 s in %d of %d seeds (%v s)", name, ok, x.seeds, b90)
		}
	}
}
