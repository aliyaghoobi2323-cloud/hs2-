package engine

import (
	"fmt"
	"math"
	"testing"
	"time"
	"unsafe"
)

// Fixes of red team round 1's open findings (Phase Z5): one test per item,
// each failing before its change.

// Item 5a: the autopilot's freeze (apProbing) ends STARTUP where it is. No
// knee was found there: no "fill the path" line, and the path is not full
// (in stage 2 a full path takes c_min under need).
func TestWcoreFixApProbingStartup(t *testing.T) {
	for _, stage := range []int{1, 2} {
		c, in := wtStartup()
		c.cfg.stage = stage
		in.apProbing = true
		m := wtMeas(1, 36e6, 4*wtMiB)
		var ev []wcEvent
		c.startup(in, m, &ev)
		if c.state != wcHold {
			t.Errorf("stage %d: %v, want hold", stage, c.state)
		}
		for _, e := range ev {
			if e.kind == wcEvFilled {
				t.Errorf("stage %d: a %q event for a STARTUP the autopilot's probe stopped", stage, e)
			}
		}
		if c.pathFull || c.cmin() != wtNeed {
			t.Errorf("stage %d: pathFull %v, c_min %d: the freeze found no full path", stage, c.pathFull, c.cmin())
		}
	}
}

// wtWallJump is t with its wall clock moved by d and its monotonic reading
// kept (an NTP step or a VM's migration). ok is false if this Go's time.Time
// is not laid out as the test assumes (wall uint64 first, seconds since 1885
// in bits 30-62 when the monotonic bit is set).
func wtWallJump(t time.Time, d time.Duration) (time.Time, bool) {
	j := t
	w := (*uint64)(unsafe.Pointer(&j))
	if *w>>63 == 0 {
		return t, false
	}
	*w += uint64(d/time.Second) << 30
	return j, j.Sub(t) == 0 && j.Unix()-t.Unix() == int64(d/time.Second)
}

// Item 5b: the windowed minima (turnBase, the srtt fallback, q_ctrl's floor)
// count time as the rest of the core does, by the monotonic clock: a step of
// the wall clock keeps their history (on the wall clock an hour's step took
// all of turnBase's 60 s away, and its next sample, with the queue in it,
// was turnBase: a false "route change").
func TestWcoreFixMinWinMonotonic(t *testing.T) {
	base := time.Now()
	if _, ok := wtWallJump(base, time.Hour); !ok {
		t.Skip("time.Time's layout is not the one this test assumes")
	}
	var w wcMinWin
	w.add(base, 150*time.Millisecond, wcTurnWin)
	if got := w.min(base, wcTurnWin); got != 150*time.Millisecond {
		t.Fatalf("the first sample: min %v, want 150ms", got)
	}
	for i := 0; i < 50; i++ {
		v := 130 * time.Millisecond
		if i == 10 {
			v = 100 * time.Millisecond
		}
		w.add(base.Add(time.Duration(i)*time.Second), v, wcTurnWin)
	}
	now := base.Add(50 * time.Second)
	if got := w.min(now, wcTurnWin); got != 100*time.Millisecond {
		t.Fatalf("min %v before the step, want 100ms", got)
	}
	for _, d := range []time.Duration{time.Hour, -time.Hour, 24 * time.Hour} {
		j, _ := wtWallJump(now, d)
		if got := w.min(j, wcTurnWin); got != 100*time.Millisecond {
			t.Errorf("wall clock stepped by %v: min %v, want 100ms (the 60 s of history kept)", d, got)
		}
	}
	// and a sample right after the step lands in the bucket of its own time
	j, _ := wtWallJump(now.Add(time.Second), time.Hour)
	w.add(j, 90*time.Millisecond, wcTurnWin)
	if got := w.min(now.Add(time.Second), wcTurnWin); got != 90*time.Millisecond {
		t.Errorf("a sample after the step: min %v, want 90ms", got)
	}
}

// Item 5c: a snapshot whose TCP_INFO could not be read has no counters of
// the socket; the next one's deltas are taken from the last one that had
// them, not from 0 (which made one wake's delta the link's whole lifetime).
func TestWcoreFixBuilderTCPInfoGap(t *testing.T) {
	var b wcBuilder
	b.build(wcLinkSnap{now: wtT0, segsIn: 100000, ooo: 900, oooOK: true, poolFree: -1})
	in := b.build(wcLinkSnap{now: wtT0.Add(100 * time.Millisecond), tcpNone: true, poolFree: -1})
	if in.segsIn != 0 || in.ooo != 0 || in.oooOK {
		t.Errorf("a wake without TCP_INFO: segsIn %d ooo %d oooOK %v, want nothing", in.segsIn, in.ooo, in.oooOK)
	}
	in = b.build(wcLinkSnap{now: wtT0.Add(200 * time.Millisecond), segsIn: 101000, ooo: 910, oooOK: true, poolFree: -1})
	if in.segsIn != 1000 || in.ooo != 10 {
		t.Errorf("after a wake without TCP_INFO: segsIn %d ooo %d, want 1000 and 10 (not the lifetime counters)", in.segsIn, in.ooo)
	}
}

// wtWake is one runner wake of the HOLD core of wtHold, 100 ms after the
// previous one, between evaluations: n segments in, ooo of them behind a
// hole.
func wtWake(c *wcore, now time.Time, n, ooo uint64) wcIn {
	in := wtRedIn(now, 100*time.Millisecond, wcSnd{})
	in.segsIn, in.ooo = n, ooo
	c.evalAt = now.Add(time.Second) // between evaluations: fastStop
	return in
}

// Item 3: between evaluations a wake whose packets arrived behind holes past
// their level halved B at once (a loss on a clean path: fastStop). Packets
// a path reorders by a few ms arrive behind a hole too: with 0.05% of them
// held 2 ms, 1000/100 fell from 736 to 97 Mbit/s with 15-28 cuts. A hole of a
// loss stays open a round trip and more; a cut now needs the holes of this
// wake and the one before to have kept packets waiting a quarter of T_base
// or more (Linux's RACK calls a packet lost only once it is reordered by
// more than min_rtt/4) with wcOooMinSegs segments or more behind them.
func TestWcoreFixFastStopReorder(t *testing.T) {
	for _, tc := range []struct {
		name  string
		wakes [][2]uint64 // segs in, ooo, a wake each
		cut   bool
	}{
		{"one reordering of 2 ms (2% of a wake)", [][2]uint64{{1000, 0}, {1000, 20}}, false},
		{"reorderings in two wakes, 3 ms of holes in all", [][2]uint64{{1000, 15}, {1000, 15}}, false},
		{"a few packets only, past the level", [][2]uint64{{40, 0}, {40, 10}}, false},
		{"a loss: half a wake behind its hole", [][2]uint64{{1000, 0}, {1000, 500}}, true},
		{"a loss that opened at the end of a wake", [][2]uint64{{1000, 100}, {1000, 900}}, true},
	} {
		c, _ := wtHold()
		c.oooRef = wcLossRef{v: 0.0005, b: 8 * wtMiB, ok: true}
		now := wtT0
		for _, w := range tc.wakes {
			now = now.Add(100 * time.Millisecond)
			c.step(wtWake(c, now, w[0], w[1]))
		}
		if cut := c.cnt.drainLoss > 0; cut != tc.cut {
			t.Errorf("%s: cut %v (B %.1f MiB), want %v", tc.name, cut, float64(c.B)/wtMiB, tc.cut)
		}
	}
}

// Item 3: a cap from a cut (bCap) clears after a clean period on a path
// whose packets arrive behind holes at a steady level under wcOooClean
// (reordering): "clean" was ooo under wcOooCongAbs (1%), which such a path
// never shows, and the cap held B for good.
func TestWcoreFixCapClearsAtOooLevel(t *testing.T) {
	c, in := wtHold()
	c.oooRef = wcLossRef{v: 0.03, b: 8 * wtMiB, ok: true}
	c.bCap, c.bCapAt = 8*wtMiB, wtT0.Add(-time.Minute)
	m := wtMeas(1, 60e6, 8*wtMiB)
	m.at = wtT0
	m.ooo, m.oooOK, m.oooLoss = 0.03, true, true
	m.qOwn, m.qOwnOK = time.Millisecond, true
	m.qTurn, m.qOK = c.qRef, true
	var ev []wcEvent
	for i := 0; i < 4 && c.bCap != 0; i++ {
		c.hold(in, m, &ev)
	}
	if c.bCap != 0 {
		t.Errorf("a cap a minute old on a window-limited path with no queue and ooo at its level (3%%): still %d", c.bCap)
	}
	// ooo past its level is no clean period
	c, in = wtHold()
	c.oooRef = wcLossRef{v: 0.03, b: 8 * wtMiB, ok: true}
	c.bCap, c.bCapAt = 8*wtMiB, wtT0.Add(-time.Minute)
	m.ooo = 0.09
	for i := 0; i < 4; i++ {
		c.hold(in, m, &ev)
	}
	if c.bCap == 0 {
		t.Error("a cap cleared with ooo past its level")
	}
}

// Item 3, in the model: a path that reorders 0.05% of the link's packets by
// 2 ms (and the model's sender resends them, as Linux does until it has
// seen the reordering) keeps most of the gain the core has without it.
func TestWcoreFixReorderModel(t *testing.T) {
	// (the model's sender takes each reordered packet for lost, as Linux
	// does until it has seen reordering: a burst after its PROBE_RTT then
	// sets off a run of resends and its rate collapses for a while, with the
	// core's windows as without them; what is asked of the core is to cut
	// for none of it and keep most of its gain)
	for _, x := range []struct {
		rate     float64
		rtt      int
		seed     uint64
		gain     float64 // core at least this many times base
		maxDrain int
	}{{1000, 100, 1, 2.5, 3}, {1000, 100, 2, 2.5, 3}, {1000, 150, 1, 2.5, 3}} {
		c := wmPath(x.rate, wmMS(x.rtt), 50*time.Millisecond)
		c.reorder, c.reorderMs, c.seed, c.dur = 0.0005, 2, x.seed, 40*time.Second
		b, k := wmRun(c.with(wmBase)), wmRun(c.with(wmCore))
		msg := fmt.Sprintf("%s reorder 0.05%%/2ms s%d: base %.0f Mbit | core %.0f Mbit resent %.2f%% drains %v", c.name, x.seed, b.mbit, k.mbit, 100*k.resend, k.drains)
		if k.mbit < x.gain*b.mbit || k.drains[1] > x.maxDrain || k.resend > b.resend+0.005 {
			t.Errorf("%s: want ≥ %.1f× base, ≤ %d drains for loss, resent ≤ base + 0.5", msg, x.gain, x.maxDrain)
		} else {
			t.Log(msg)
		}
	}
}

// Item 2: a loss of ours whose record from the exit shows no queue at its
// socket (q_snd under wcPoliceQ) is a policer's signature: the loss point
// (bCap) holds for the Q_cap TTL (10 min), not until three window-limited
// intervals without a queue (that is how a policed path always looks below
// the policer's rate), probes back off at once, the drain goes to the floor,
// and a loss that goes on at the budget a drain left is a loss point too. A
// loss with a queue at the exit keeps the rules it had.
func TestWcoreFixPolicerCap(t *testing.T) {
	for _, tc := range []struct {
		name string
		q    time.Duration // the exit's queue (q_snd) as the loss came
		keep bool
	}{{"no queue (a policer)", time.Millisecond, true}, {"a queue of 12 ms (a tail drop; under its growth past q_ref)", 12 * time.Millisecond, false}} {
		c, in := wtHold()
		c.bHist = []wcRate{{wtT0, 10 * wtMiB}} // a budget past the level's earlier in the record: the loss came with B
		m := wtMeas(1, 60e6, 8*wtMiB)
		m.at = wtT0
		m.snd, m.sndOK, m.sndSegs, m.sndAt = 0.05, true, 100000, wtT0
		m.qSnd, m.qSndOK, m.qOwn, m.qOwnOK = tc.q, true, time.Millisecond, true
		m.qTurn, m.qOK = c.qRef, true
		var ev []wcEvent
		c.hold(in, m, &ev)
		if c.state != wcDrain || c.bCap == 0 {
			t.Fatalf("%s: %v cap %d, want a drain for loss and a cap", tc.name, c.state, c.bCap)
		}
		if tc.keep && (c.backoff < wcPoliceBackoff || c.B > wtNeed) {
			t.Errorf("%s: backoff %d B %.1f MiB, want backoff %d or more and B at the floor", tc.name, c.backoff, float64(c.B)/wtMiB, wcPoliceBackoff)
		}
		cp := c.bCap
		// 20 s later: HOLD at the cap, window-limited, no queue, clean
		c.state, c.lossDrain, c.B, c.bPub, c.bHist = wcHold, false, cp, cp, nil
		m = wtMeas(1, 60e6, float64(cp))
		m.at = wtT0.Add(20 * time.Second)
		m.qOwn, m.qOwnOK = time.Millisecond, true
		m.qTurn, m.qOK = c.qRef, true
		hin := wtIn(m.at, 1, uint32(cp))
		for i := 0; i < 4; i++ {
			c.hold(hin, m, &ev)
			c.state = wcHold
		}
		if kept := c.bCap != 0; kept != tc.keep {
			t.Errorf("%s: 20 s on, cap %d (was %d), want kept %v", tc.name, c.bCap, cp, tc.keep)
		}
	}

	// a loss that goes on, with no queue at the exit, at the budget the drain left
	c, in := wtHold()
	c.B, c.bPub, c.bCap, c.bCapAt = 8*wtMiB, 8*wtMiB, 14*wtMiB, wtT0
	c.lossDrain, c.lossDrainN = true, 0
	m := wtMeas(1, 60e6, 8*wtMiB)
	m.at = wtT0.Add(time.Second)
	m.snd, m.sndOK, m.sndSegs, m.sndAt = 0.05, true, 100000, m.at
	m.qSnd, m.qSndOK, m.qOwn, m.qOwnOK = 0, true, 0, true
	var ev []wcEvent
	c.hold(in, m, &ev)
	if want := uint64(wcCapBack * 8 * wtMiB); c.bCap > want || c.B > wtNeed || !c.policed(m.at) {
		t.Errorf("a policer's loss going on at 8 MiB: cap %.1f MiB B %.1f MiB, want cap at most %.1f, B at the floor",
			float64(c.bCap)/wtMiB, float64(c.B)/wtMiB, float64(want)/wtMiB)
	}
	// ... and no policer from packets behind holes alone (a path that
	// reorders them, the link socket's srtt at its floor)
	c, in = wtHold()
	c.B, c.bPub, c.lossDrain = 8*wtMiB, 8*wtMiB, true
	c.oooRef = wcLossRef{v: 0.0005, b: 8 * wtMiB, ok: true}
	m = wtMeas(1, 60e6, 8*wtMiB)
	m.at = wtT0.Add(time.Second)
	m.ooo, m.oooOK, m.oooLoss = 0.3, true, true
	m.qOwn, m.qOwnOK = 0, true
	c.hold(in, m, &ev)
	if c.policed(m.at) || c.B < 8*wtMiB {
		t.Errorf("holes and no queue at the link socket: policed %v B %.1f MiB, want no policer's verdict", c.policed(m.at), float64(c.B)/wtMiB)
	}
}

// Item 2, in the model: behind a policer at 60% of a 1 Gbit/s path with a
// 300 ms round trip, the run the red team's grid saw drained (a streak of
// windows past lossFrac where the windows without the core resend nothing).
func TestWcoreFixPolicerModel(t *testing.T) {
	for _, x := range []struct {
		rate, rtt, q, pol float64
		seed              uint64
	}{{1000, 300, 50, 600, 2}} {
		c := wmPath(x.rate, wmMS(int(x.rtt)), wmMS(int(x.q)))
		c.policer, c.seed, c.dur = x.pol*wmMbit, x.seed, 40*time.Second
		b, k := wmRun(c.with(wmBase)), wmRun(c.with(wmCore))
		msg := fmt.Sprintf("%s pol%.0f s%d: base %.0f Mbit resent %.2f%% [%s] | core %.0f Mbit resent %.2f%% [%s] drains %v",
			c.name, x.pol, x.seed, b.mbit, 100*b.resend, wmLossNote(b), k.mbit, 100*k.resend, wmLossNote(k), k.drains)
		if wmDrainedByCore(b, k) {
			t.Errorf("the loss rule would drain the link under the core: %s", msg)
		} else {
			t.Log(msg)
		}
	}
}

// Item 1: past wcLongRTT (paths a DPI box holds back, satellite links) the
// core is outside what it was calibrated on: on the exit's side, and on the
// edge with an exit that sends no TCP_INFO in its record, it has no signal
// of its own losses at all there, and it stays OFF.
func TestWcoreFixLongRTTBlindOff(t *testing.T) {
	for _, tc := range []struct {
		name string
		side wcSide
		seen bool // the exit's record has TCP_INFO in it
		off  bool
	}{{"the exit's side", wcExit, false, true}, {"the edge, an old exit", wcEdge, false, true}, {"the edge, a new exit", wcEdge, true, false}} {
		c := newWcore(wcConfig{side: tc.side, stage: 1, need: wtNeed, maxWin: wtMax, minWin: wtMin}, func() float64 { return 0.5 })
		c.tBase = 600 * time.Millisecond
		c.sndAny, c.sndSeen = tc.side == wcEdge, tc.seen
		m := wtMeas(1, 6e6, 2*wtMiB)
		m.qSnd3, m.qSnd3OK = time.Millisecond, tc.seen
		if got := c.harm(m); got != tc.off {
			t.Errorf("%s, T_base 600 ms, OFF: harm %v, want %v", tc.name, got, tc.off)
		}
		// in HOLD: OFF at the next evaluation
		c.state, c.B, c.bPub, c.bGood = wcHold, 6*wtMiB, 6*wtMiB, 6*wtMiB
		in := wtIn(wtT0, 1, 6*wtMiB)
		in.streams[0].read = 6e6
		in.streams[0].turnMin, in.streams[0].turnLast = 610*time.Millisecond, 610*time.Millisecond
		in.minRTT, in.srtt, in.segsIn, in.oooOK = 600*time.Millisecond, 601*time.Millisecond, 4000, true
		wtInF(c, in)
		c.lastAt = wtT0.Add(-2 * time.Second)
		c.step(in)
		if (c.state == wcOff) != tc.off {
			t.Errorf("%s, T_base 600 ms, HOLD: %v after an evaluation, want off %v", tc.name, c.state, tc.off)
		}
	}
}

// Item 1: past wcLongRTT the edge grows B (a STARTUP step, a probe) only on
// a fresh record of the exit that shows no queue (q_snd up to wcDqMin) and
// no new resends; an interval with no record waits, one whose record shows
// resends ends STARTUP, and one whose record shows a queue ends it only past
// the queue OFF left by Δq (judge, p3): under that, B stays where it is.
func TestWcoreFixLongRTTGrowsOnRecord(t *testing.T) {
	rec := func(m *wcMeas, q time.Duration, snd float64) {
		m.sndOK, m.snd, m.sndSegs, m.sndAt = true, snd, 20000, wtT0
		m.qSnd, m.qSndOK = q, true
	}
	for _, tc := range []struct {
		name  string
		setup func(m *wcMeas)
		state wcState
		grew  bool
	}{
		{"no record in the interval: wait", func(m *wcMeas) {}, wcStartup, false},
		{"a clean record: a step", func(m *wcMeas) { rec(m, 2*time.Millisecond, 0) }, wcStartup, true},
		{"a record with a queue: B stays", func(m *wcMeas) { rec(m, 20*time.Millisecond, 0) }, wcStartup, false},
		{"... past the queue OFF left by Δq (60 ms): the knee", func(m *wcMeas) { rec(m, 70*time.Millisecond, 0) }, wcHold, false},
		{"a record with resends: the knee", func(m *wcMeas) { rec(m, 2*time.Millisecond, 0.0002) }, wcHold, false},
	} {
		c, in := wtStartup()
		c.tBase, c.sndSeen, c.sndAny = 600*time.Millisecond, true, true
		m := wtMeas(1, 4e6, 4*wtMiB) // the rate doubled with B (from 2 MB/s)
		c.prevR, c.fullR = 2e6, 2e6
		m.at = wtT0
		tc.setup(&m)
		flat := c.flat
		var ev []wcEvent
		c.startup(in, m, &ev)
		if c.state != tc.state || (c.B > 4*wtMiB) != tc.grew {
			t.Errorf("%s: %v B %.1f MiB, want %v grown %v", tc.name, c.state, float64(c.B)/wtMiB, tc.state, tc.grew)
		}
		if tc.name == "no record in the interval: wait" && c.flat != flat {
			t.Errorf("%s: flat %d → %d, want the interval not counted", tc.name, flat, c.flat)
		}
	}
	// HOLD: a probe that is due starts only on a clean record
	for _, clean := range []bool{false, true} {
		c, in := wtHold()
		c.tBase, c.sndSeen, c.sndAny = 600*time.Millisecond, true, true
		c.probeAt = wtT0
		m := wtMeas(1, 60e6, 8*wtMiB)
		m.at = wtT0
		m.qTurn, m.qOK, m.qOwn, m.qOwnOK = c.qRef, true, time.Millisecond, true
		if clean {
			rec(&m, 2*time.Millisecond, 0)
		}
		var ev []wcEvent
		c.hold(in, m, &ev)
		if (c.state == wcProbe) != clean {
			t.Errorf("HOLD at 600 ms, a probe due, clean record %v: %v", clean, c.state)
		}
	}
}

// Item 1, in the model: runs of the red team's long round trip sweep that
// drained or resent past base + 3 points. (The exit's case runs its core,
// which only the lab knob WcoreUpload gives it since step 1's decision.)
func TestWcoreFixLongRTTModel(t *testing.T) {
	up := WcoreUpload
	t.Cleanup(func() { WcoreUpload = up })
	for _, x := range []struct {
		rate   float64
		rtt, q int
		seed   uint64
		exit   bool
	}{{100, 600, 50, 2, false}, {50, 600, 50, 2, false}, {300, 800, 50, 1, false}, {50, 500, 100, 1, false}, {100, 600, 50, 2, true}} {
		c := wmPath(x.rate, wmMS(x.rtt), wmMS(x.q))
		c.seed, c.dur, c.warm, c.exit = x.seed, 90*time.Second, 20*time.Second, x.exit
		WcoreUpload = 0
		if x.exit {
			WcoreUpload = 1
		}
		b, k := wmRun(c.with(wmBase)), wmRun(c.with(wmCore))
		msg := fmt.Sprintf("%s s%d exit %v: base %.1f Mbit resent %.2f%% | core %.1f Mbit resent %.2f%% [%s] drains %v", c.name, x.seed, x.exit,
			b.mbit, 100*b.resend, k.mbit, 100*k.resend, wmLossNote(k), k.drains)
		if wmDrainedByCore(b, k) || k.resend > b.resend+0.03 || x.exit && math.Abs(k.mbit-b.mbit) > 0.01*b.mbit {
			t.Errorf("%s", msg)
		} else {
			t.Log(msg)
		}
	}
}

// Item 1: past wcLongRTT the core leaves OFF only after wcLongClean of the
// exit's records in a row showed the path with no queue and no resends (one
// with a queue or resends starts the count over; one with no data in it
// leaves it); past wcLongGear its STARTUP takes the smallest gear (×1.25)
// only.
func TestWcoreFixLongRTTLeavesOffOnCleanRecords(t *testing.T) {
	c := wtCore(1)
	c.tBase = 600 * time.Millisecond
	now := wtT0
	recs := []struct {
		snd  wcSnd
		want int
	}{
		{wcSnd{fresh: true, srtt: 601 * time.Millisecond, minRTT: 600 * time.Millisecond, segsOut: 5000}, 1},
		{wcSnd{fresh: true, srtt: 602 * time.Millisecond, minRTT: 600 * time.Millisecond, segsOut: 5000}, 2},
		{wcSnd{fresh: true, srtt: 640 * time.Millisecond, minRTT: 600 * time.Millisecond, segsOut: 5000}, 0}, // a queue
		{wcSnd{fresh: true, srtt: 601 * time.Millisecond, minRTT: 600 * time.Millisecond, segsOut: 5000}, 1},
		{wcSnd{fresh: true, srtt: 601 * time.Millisecond, minRTT: 600 * time.Millisecond, segsOut: 5000, retrans: 3}, 0}, // resends
		{wcSnd{fresh: true}, 0}, // no data, no TCP_INFO: no verdict
		{wcSnd{fresh: true, srtt: 601 * time.Millisecond, minRTT: 600 * time.Millisecond, segsOut: 5000}, 1},
		{wcSnd{}, 1}, // no record in the interval
	}
	for i, r := range recs {
		now = now.Add(2500 * time.Millisecond)
		in := wtIn(now, 1, 2*wtMiB)
		in.snd = r.snd
		c.step(in)
		if c.cleanRecs != r.want {
			t.Fatalf("record %d %+v: %d clean records in a row, want %d", i, r.snd, c.cleanRecs, r.want)
		}
	}
	for _, tc := range []struct {
		tBase time.Duration
		n     int
		gear  float64 // STARTUP's first step; 0: stays OFF
		top   float64 // the most a step takes in its top gear
	}{{600 * time.Millisecond, wcLongClean - 1, 0, 0}, {600 * time.Millisecond, wcLongClean, 1.25, 1.25}, {500 * time.Millisecond, wcLongClean, 1.5, 2}} {
		c := wtCore(1)
		c.tBase = tc.tBase
		c.sndAny, c.sndSeen, c.sndAnyAt = true, true, wtT0.Add(-time.Minute)
		c.offN, c.cleanRecs = 1, tc.n
		in := wtIn(wtT0, 1, 2*wtMiB)
		m := wtMeas(1, 3e6, 2*wtMiB)
		m.at = wtT0
		m.sndOK, m.snd, m.sndSegs, m.sndAt = true, 0, 20000, wtT0
		m.qSnd, m.qSndOK, m.qSnd3, m.qSnd3OK = 2*time.Millisecond, true, 2*time.Millisecond, true
		var ev []wcEvent
		c.off(in, m, &ev)
		if tc.gear == 0 {
			if c.state != wcOff {
				t.Errorf("T_base %v, %d clean records: %v, want OFF", tc.tBase, tc.n, c.state)
			}
			continue
		}
		if c.state != wcStartup || c.B != uint64(tc.gear*2*wtMiB) {
			t.Errorf("T_base %v, %d clean records: %v B %.2f MiB, want STARTUP at ×%g", tc.tBase, tc.n, c.state, float64(c.B)/wtMiB, tc.gear)
		}
		c.gear = 2
		in.now, m.at, m.sndAt = wtT0.Add(3*time.Second), wtT0.Add(3*time.Second), wtT0.Add(3*time.Second)
		m.rF, m.rLink, m.wF, m.wPrev = 4e6, 4e6, float64(c.B), float64(c.B)
		b := c.B
		c.startup(in, m, &ev)
		if got := float64(c.B) / float64(b); c.state != wcStartup || got > tc.top+0.01 || got < tc.top-0.3 {
			t.Errorf("T_base %v, STARTUP in its top gear: %v B %.2f → %.2f MiB (×%.2f), want ×%g", tc.tBase, c.state, float64(b)/wtMiB, float64(c.B)/wtMiB, got, tc.top)
		}
	}
}

// TestRedWcoreHostileInputs (seed 9, after the changes of round 1 moved its
// trajectory): a T_base from a min_rtt of ~0U µs (71 minutes: what Linux
// reports before the first sample; the runner takes it for none, the core
// can still be handed it) and turnarounds of an hour gave a growth brake of
// 1 h 5 min. The brake is a queue the fast arm lets pass over the least
// turnaround of the last wcTurnWin: never past that window.
func TestWcoreFixBrakeBounded(t *testing.T) {
	c := wtCore(1)
	in := wtIn(wtT0, 3, 4*wtMiB)
	c.inF[1], c.inF[3] = true, true
	c.state, c.B, c.bPub = wcHold, 12*wtMiB, 12*wtMiB
	c.turnMins.add(wtT0, 100*time.Millisecond, wcTurnWin)
	c.tBase = time.Duration(^uint32(0)) * time.Microsecond
	c.spread = 29 * time.Minute
	o := c.out(in, wtT0, nil)
	if o.qBrake <= 0 || o.qBrake > wcTurnWin {
		t.Fatalf("T_base %v, spread %v: brake %v, want (0, %v]", c.tBase, c.spread, o.qBrake, wcTurnWin)
	}
	// an ordinary path keeps its brake as it was
	c.tBase, c.spread = 100*time.Millisecond, 3*time.Millisecond
	if o := c.out(in, wtT0, nil); o.qBrake != max(c.probeQ(wtT0), c.dq())+2*c.spread {
		t.Fatalf("T_base 100 ms: brake %v", o.qBrake)
	}
}
