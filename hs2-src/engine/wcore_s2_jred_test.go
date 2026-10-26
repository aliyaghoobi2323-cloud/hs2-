package engine

import (
	"fmt"
	"testing"
	"time"
)

// Red team of step 2's judge fixes (p1 to p4, z5-judge §7.3): each finding
// reproduced here before its fix.

// TestWcoreJRedBackDip (red team, p4): the rate a loss point arms recovery
// from (backR) is its interval's, and that interval is often a dip of the
// core's own making: the recovery of the cut just before it (the windows
// full of data behind its holes), a drain, the sender's PROBE_RTT, or a
// slower-path drain that read a dip. On a steady path the next intervals
// carry the path's rate at once, past wcBackGrowth of the dip, and the rule
// cleared a cap the losses had just confirmed and probed past it: 4
// downloads on 1000/100 behind 10 ms armed at 338 Mbit/s with the path at
// ~550, probed from a 9.5 MiB cap to 12.2 and cut four times after
// (TestWcoreJRedBackSteadyModel). What the link carried within wcBackDwell
// of the loss point is its rate too; a spell that slow is one that stays.
func TestWcoreJRedBackDip(t *testing.T) {
	ms := time.Millisecond
	for _, tc := range []struct {
		name  string
		since time.Duration // the loss point came this long before the first evaluation
		r     []float64     // the link's rate at each evaluation, 500 ms apart (the loss point at 6 MB/s)
		back  bool
	}{
		{"a dip: the path's rate at once after the loss point", 300 * ms, []float64{10e6, 10e6, 10e6, 10e6, 10e6, 10e6, 10e6}, false},
		{"a spell that stayed slow, then the path faster", 300 * ms, []float64{6e6, 6.2e6, 6e6, 6.1e6, 6e6, 10e6, 10e6}, true},
		{"the loss point long before (the rule as it was)", 5 * time.Second, []float64{10e6, 10e6}, true},
	} {
		c, _ := wtHold()
		c.rhat = c.rhat[:0]
		c.noteRate(wtT0, tc.r[0])
		c.B, c.bPub, c.bGood = 3*wtMiB, 3*wtMiB, 3*wtMiB
		c.bCap, c.bCapAt, c.backoff = 3*wtMiB, wtT0.Add(-tc.since), 2
		c.backR, c.backAt = 6e6, wtT0.Add(-tc.since)
		c.qSndRef, c.qSndRefOK = time.Millisecond, true
		in := wtIn(wtT0, 1, 3*wtMiB)
		back := false
		for i, r := range tc.r {
			now := wtT0.Add(time.Duration(i) * 500 * ms)
			in.now = now
			m := wtMeas(1, r, 3*wtMiB)
			m.at, m.qOK, m.qTurn = now, true, c.qRef
			m.snd, m.sndOK, m.sndSegs, m.sndAt = 0, true, 20000, now
			m.qSnd, m.qSndOK, m.qOwn, m.qOwnOK = ms, true, ms, true
			var ev []wcEvent
			c.state = wcHold
			c.backDwell(now, m.rLink) // (as step does for every evaluation)
			c.hold(in, m, &ev)
			if c.bCap == 0 && c.backoff == 0 && (c.state == wcProbe || !c.probeAt.After(now)) {
				back = true
				break
			}
		}
		if back != tc.back {
			t.Errorf("%s: back %v, want %v (backR %.1f MB/s, cap %.2f MiB, backoff %d)", tc.name, back, tc.back, c.backR/1e6, float64(c.bCap)/wtMiB, c.backoff)
		}
	}
}

// TestWcoreJRedBackSteadyModel (red team, p4): on steady paths (no change
// of rate, no policer) the recovery rule fired in 44 of 432 model runs, on
// loss points and slower-path drains whose interval read a dip of the
// core's own, and cleared the cap the losses had found: 4 downloads on
// 1000/100 behind 10 ms 551 → 517 Mbit/s with twice the cuts (27 → 52 in
// 6 seeds), 1000/100 behind 20 ms 744 → 721. It is for a path that got
// faster than a slow spell's rate: none fires where the path never changed.
func TestWcoreJRedBackSteadyModel(t *testing.T) {
	fires, runs := 0, 0
	var where []string
	for _, cell := range []struct {
		rate   float64
		rtt, q int
		n      int
	}{
		{1000, 100, 10, 4}, {1000, 100, 20, 1}, {300, 100, 20, 1}, {300, 100, 50, 1},
		{300, 150, 30, 1}, {1000, 150, 50, 1}, {100, 100, 50, 1},
	} {
		for seed := uint64(1); seed <= 6; seed++ {
			c := wmPath(cell.rate, wmMS(cell.rtt), wmMS(cell.q))
			c.seed, c.mode = seed, wmCore
			if cell.n > 1 {
				c.streams = make([]wmStreamCfg, cell.n)
				c.name += fmt.Sprintf(" x%d", cell.n)
			}
			var k0 *wcore
			c.check = func(at time.Duration, k *wcore, in wcIn, o wcOut) { k0 = k }
			wmRun(c)
			runs++
			if k0 != nil && k0.cnt.back > 0 {
				fires += k0.cnt.back
				where = append(where, fmt.Sprintf("%s s%d: %d", c.name, seed, k0.cnt.back))
			}
		}
	}
	if fires > 0 {
		t.Errorf("the recovery rule fired %d times in %d runs on steady paths: %v", fires, runs, where)
	}
}
