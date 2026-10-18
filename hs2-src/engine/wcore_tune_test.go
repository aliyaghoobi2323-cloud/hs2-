package engine

import (
	"math"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Unit tests of the tuning round after the rig's calibration and pilot
// (docs/architecture/wip/z5-core-calibration.fa.md §4): each fails without
// its change.

// A drain never raises the budget, whatever its cause (under random loss
// R·τ_hold is about a round trip: the formula took B from 3.2 to 5.4 MiB on
// the rig), and one that cannot shrink it (at the floor) makes no event and
// counts nothing.
func TestWcoreDrainNeverRaises(t *testing.T) {
	for _, why := range []wcDrainWhy{wcWhyQueue, wcWhyRate, wcWhyLoss} {
		c, in := wtHold()
		c.B = 4 * wtMiB
		c.tau = 3 * c.tBase // a wait behind holes as long as three round trips
		m := wtMeas(1, 60e6, 4*wtMiB)
		var ev []wcEvent
		c.toDrain(in, m, &ev, why, wcSigTurn, 20*time.Millisecond, why == wcWhyRate)
		if c.B > uint64(wcDrainShrink*4*wtMiB) || len(ev) != 1 || ev[0].B != c.B || ev[0].from != 4*wtMiB {
			t.Errorf("why %v: B %d from 4 MiB, events %+v", why, c.B, ev)
		}
	}
	c, in := wtHold()
	c.B = wtNeed // the floor of one stream
	m := wtMeas(1, 60e6, wtNeed)
	var ev []wcEvent
	c.toDrain(in, m, &ev, wcWhyQueue, wcSigTurn, 20*time.Millisecond, false)
	if len(ev) != 0 || c.cnt.drainQ != 0 || c.B != wtNeed {
		t.Errorf("a drain at the floor: events %+v, counted %d, B %d", ev, c.cnt.drainQ, c.B)
	}
}

// A cap where losses of ours came is never under the floor of the fast
// streams (the rig: 0.9 × 2 MiB was noted, and held B there).
func TestWcoreNoteCapFloor(t *testing.T) {
	c := wtCore(1)
	c.inF = map[uint32]bool{1: true, 3: true}
	c.noteCap(wtT0, 2*wtNeed)
	if c.bCap != 2*wtNeed {
		t.Fatalf("cap %d at the floor of two streams, want %d", c.bCap, 2*wtNeed)
	}
	c.bCap = 0
	c.noteCap(wtT0, 10*wtMiB)
	if c.bCap != uint64(wcCapBack*10*wtMiB) {
		t.Fatalf("cap %d, want 0.9 of 10 MiB", c.bCap)
	}
}

// A record of the exit whose span began before the latest drain, cut or
// start judges nothing (a record of a 50 Mbit/s spell cut the budget after
// the path was fast again), nor sets a loss level.
func TestWcoreStaleRecord(t *testing.T) {
	c, _ := wtHold()
	c.bHist = []wcRate{{wtT0, 10 * wtMiB}}
	m := wtMeas(1, 60e6, 8*wtMiB)
	m.at = wtT0
	m.snd, m.sndOK, m.sndSegs = 0.063, true, 50000
	if cong, _, _ := c.lossVerdict(m, false); !cong {
		t.Fatal("a fresh record past its level after a larger B: no verdict")
	}
	c.chgAt = wtT0.Add(-time.Second) // a drain a second ago
	if cong, jump, _ := c.lossVerdict(m, false); cong || jump {
		t.Fatal("a record from before the drain judged the budget of now")
	}
	ref := c.sndRef
	c.bHist = nil
	c.noteLoss(m)
	if c.sndRef != ref {
		t.Fatalf("a stale record set the loss level: %+v", c.sndRef)
	}
}

// The resend share past its level by its own spread: under 0.1% random loss
// a record of 50k segments is past at 0.2%; one of a few hundred segments at
// 1% is not past at 1.3% (it was, by +0.3 points).
func TestWcoreLossBySegments(t *testing.T) {
	c := wtCore(1)
	c.sndRef = wcLossRef{v: 0.001, b: wtNeed, ok: true}
	m := wcMeas{snd: 0.0021, sndOK: true, sndSegs: 50000}
	if !c.sndRef.past(m.snd, c.sndAbs(m)) {
		t.Errorf("0.21%% against 0.1%% over 50k segments: not past (abs %.4f)", c.sndAbs(m))
	}
	c.sndRef = wcLossRef{v: 0.01, b: wtNeed, ok: true}
	m = wcMeas{snd: 0.013, sndOK: true, sndSegs: 860}
	if c.sndRef.past(m.snd, c.sndAbs(m)) {
		t.Errorf("1.3%% against 1%% over 860 segments: past (abs %.4f)", c.sndAbs(m))
	}
}

// With the cap at the floor and a signal with no bias showing the queue
// empty, two window-limited intervals clear the cap and the backoff at once
// (the rig sat 12 s at the floor after a slower spell, 85 Mbit/s against 250);
// a cap above the floor waits wcCapStale as before.
func TestWcoreCapClearedAtFloor(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cap   uint64
		clear bool
	}{
		{"at the floor", wtNeed, true},
		{"above it", 6 * wtMiB, false},
	} {
		c, in := wtHold()
		c.B, c.bCap, c.bCapAt, c.backoff = tc.cap, tc.cap, wtT0.Add(-time.Second), 3
		m := wtMeas(1, 60e6, float64(tc.cap))
		m.at = wtT0
		m.qOwn, m.qOwnOK = time.Millisecond, true
		m.qTurn, m.qOK = c.qRef, true
		var ev []wcEvent
		for i := 0; i < 2 && c.state == wcHold; i++ {
			c.hold(in, m, &ev)
		}
		if cleared := c.bCap == 0 && c.backoff == 0; cleared != tc.clear {
			t.Errorf("%s: cap %d backoff %d, want cleared %v", tc.name, c.bCap, c.backoff, tc.clear)
		}
	}
}

// STARTUP stays within a fresh cap where losses of ours came (bCap), as
// HOLD does, and ends there (the rig's 300/150: after two probe wins it went
// 4.4 → 5.5 → 8.2 MiB past a cap of 5.3 and overflowed again); a stale cap
// bounds nothing.
func TestWcoreStartupCap(t *testing.T) {
	for _, tc := range []struct {
		name  string
		age   time.Duration
		bound bool
	}{
		{"fresh", time.Second, true},
		{"stale", wcQcapTTL + time.Second, false},
	} {
		c, in := wtStartup()
		c.bCap, c.bCapAt = 5*wtMiB, wtT0.Add(-tc.age)
		m := wtMeas(1, 60e6, 4*wtMiB)
		m.at = wtT0
		var ev []wcEvent
		c.startup(in, m, &ev)
		if c.state != wcStartup || (c.B == 5*wtMiB) != tc.bound || c.B < 5*wtMiB {
			t.Errorf("%s cap: %v B %d, want STARTUP at 5 MiB: %v", tc.name, c.state, c.B, tc.bound)
			continue
		}
		if !tc.bound {
			continue
		}
		in.now = wtT0.Add(time.Second)
		m.at = in.now
		c.startup(in, m, &ev) // the same rate: flat, at the cap
		if c.state == wcStartup || c.B > 5*wtMiB {
			t.Errorf("at the cap with the rate flat: %v B %d, want STARTUP over", c.state, c.B)
		}
	}
}

// A probe that pays past a cap at the floor clears the cap (it came from a
// slower spell of the path); past a cap above the floor it raises the cap
// to its budget.
func TestWcoreProbeFloorCap(t *testing.T) {
	for _, tc := range []struct {
		name string
		cap  uint64
		want uint64
	}{
		{"a cap at the floor: cleared", wtNeed, 0},
		{"a cap above it: raised to the probe's budget", 8 * wtMiB, 9 * wtMiB},
	} {
		c, in := wtProbe()
		c.bCap, c.bCapAt, c.backoff = tc.cap, wtT0.Add(-time.Second), 2
		m := wtMeas(1, 60.5e6, 9*wtMiB)
		m.at = wtT0
		m.qTurn, m.qOK = 5*time.Millisecond, true // nothing queued
		var ev []wcEvent
		c.probe(in, m, &ev)
		if c.cnt.probeOK != 1 || c.bCap != tc.want || c.backoff != 0 {
			t.Errorf("%s: probe ok %d, cap %d backoff %d, want cap %d", tc.name, c.cnt.probeOK, c.bCap, c.backoff, tc.want)
		}
	}
}

// HOLD's references follow the queue within Δq only: an interval of the
// sender's PROBE_RTT (turnarounds of hundreds of ms) that the exit's record
// calls bare does not move q_ref.
func TestWcoreHoldRefOutlier(t *testing.T) {
	c, in := wtHold()
	ref := c.qRef
	m := wtMeas(1, 20e6, 8*wtMiB)
	m.at = wtT0
	m.qTurn, m.qOK = 468*time.Millisecond, true
	m.sndOK, m.qSnd, m.qSndOK = true, 2*time.Millisecond, true // the record shows none
	c.qSndRef, c.qSndRefOK = 2*time.Millisecond, true
	var ev []wcEvent
	c.hold(in, m, &ev)
	if c.qRef != ref {
		t.Fatalf("q_ref %v after a 468 ms turnaround the record called bare, want %v", c.qRef, ref)
	}
	m.qTurn = ref + c.dq()/2 // within Δq: followed
	c.hold(in, m, &ev)
	if c.qRef <= ref {
		t.Fatalf("q_ref %v: did not follow a turnaround within Δq (%v)", c.qRef, m.qTurn)
	}
}

// setB takes the larger n_F of this interval and the previous one (a stream
// of F sent nothing for an interval: B was cut to the other's ceiling), and
// blames the pool only when it has less than a step of a window left.
func TestWcoreSetB(t *testing.T) {
	c := wtCore(1)
	c.nfPrev = 2
	in := wtIn(wtT0, 1, 8*wtMiB)
	m := wtMeas(1, 60e6, 8*wtMiB)
	c.setB(in, m, 20*wtMiB)
	if c.B != 20*wtMiB || c.capped {
		t.Fatalf("B %d capped %v, want 20 MiB under 2 × 16 MiB", c.B, c.capped)
	}
	c.nfPrev = 1
	in.poolFree = 64 << 20 // plenty of pool: capped by MaxStreamBuffer
	c.setB(in, m, 20*wtMiB)
	if c.B != wtMax || !c.capped || c.capPool {
		t.Fatalf("B %d capped %v pool %v, want 16 MiB by the ceiling", c.B, c.capped, c.capPool)
	}
	in.poolFree = 2 * wtMiB // the pool is what holds it, and it still has a step
	c.setB(in, m, 20*wtMiB)
	if !c.capped || c.capPool {
		t.Fatalf("capped %v pool %v with 2 MiB of pool left", c.capped, c.capPool)
	}
	in.poolFree = 256 << 10 // under a step (1 MiB at 10 MiB)
	c.setB(in, m, 20*wtMiB)
	if !c.capped || !c.capPool {
		t.Fatalf("capped %v pool %v with 256 KiB of pool left", c.capped, c.capPool)
	}
}

// Where data waits behind holes the turnaround holds that wait: HOLD judges
// the queue by q_own above its reference, and a drain ends on q_own back.
func TestWcoreHoldHoles(t *testing.T) {
	holes := func(c *wcore, m *wcMeas) {
		c.oooRef = wcLossRef{v: 0.5, b: 8 * wtMiB, ok: true}
		c.qOwnRef = 4 * time.Millisecond
		m.ooo, m.oooOK, m.holey = 0.5, true, true
		m.qOwnOK = true
	}
	c, in := wtHold()
	m := wtMeas(1, 60e6, 8*wtMiB)
	m.at, m.hit = wtT0, 0
	holes(c, &m)
	m.qTurn, m.qOK = 150*time.Millisecond, true // the wait behind holes
	m.qOwn = 6 * time.Millisecond
	var ev []wcEvent
	for i := 0; i < 3; i++ {
		c.hold(in, m, &ev)
	}
	if c.state != wcHold || len(ev) != 0 {
		t.Fatalf("a long turnaround under holes: %v %+v", c.state, ev)
	}
	m.qOwn = 16 * time.Millisecond // q_own past its reference by wcDqOwn
	for i := 0; i < 2 && c.state == wcHold; i++ {
		c.hold(in, m, &ev)
	}
	if c.state != wcDrain || len(ev) != 1 || ev[0].sig != wcSigOwn {
		t.Fatalf("q_own grew twice: %v %+v", c.state, ev)
	}
	c.settling = true
	m.qOwn = 12 * time.Millisecond
	c.settled(in, m, &ev)
	if c.state != wcDrain {
		t.Fatal("left the drain with q_own still up")
	}
	m.qOwn = 5 * time.Millisecond
	c.settled(in, m, &ev)
	if c.state != wcHold {
		t.Fatal("did not leave the drain with q_own back")
	}
	// a clean interval after a holey one is judged as holey too (a tail
	// loss's timeout: few packets, none behind a hole, turnarounds of the
	// wait)
	d := wtCore(1)
	d.measure(wcIn{now: wtT0, oooOK: true, segsIn: 1000, ooo: 500}, 1)
	if m := d.measure(wcIn{now: wtT0.Add(time.Second), oooOK: true, segsIn: 10}, 1); !m.holey {
		t.Fatal("the interval after one behind holes is clean")
	}
}

// A stream of F whose sender sent nothing in an interval (its PROBE_RTT)
// and whose reader is not behind stays in F, for up to wcIdleKeep intervals;
// nothing is judged meanwhile.
func TestWcoreIdleStaysInF(t *testing.T) {
	c := wtCore(1)
	c.inF[1] = true
	in := wcIn{now: wtT0, streams: []wcStream{{id: 1, win: 8 * wtMiB}}}
	for i := 0; i < wcIdleKeep; i++ {
		m := c.measure(in, 0.5)
		if len(m.f) != 1 || !m.idleF || m.wF != 8*wtMiB {
			t.Fatalf("interval %d: F %v idle %v wF %.0f", i, m.f, m.idleF, m.wF)
		}
	}
	if m := c.measure(in, 0.5); len(m.f) != 0 {
		t.Fatal("still in F after wcIdleKeep intervals with nothing sent")
	}
	in.streams[0].lag = 1 // a reader behind: out at once
	c.inF[1] = true
	if m := c.measure(in, 0.5); len(m.f) != 0 {
		t.Fatal("kept in F with its reader behind")
	}
}

// A stream joins F when its window is in use (R·turnaround ≥ 0.9 of it less
// half a step) and no signal with no bias shows a standing queue past the
// target; the hit share is a hint only with a bare turnaround.
func TestWcoreFMembership(t *testing.T) {
	const w = 8 * wtMiB
	step := float64(wcStep(wtNeed, w))
	used := (float64(w) - step/2) / 0.1 // bytes/s that use it in a 100 ms turnaround
	for _, tc := range []struct {
		name string
		s    wcStream
		qSnd time.Duration // 0: no record
		join bool
	}{
		{"in use, no queue", wcStream{id: 1, win: w, upd: 4, read: uint64(used), turnMin: 100 * time.Millisecond, turnLast: 100 * time.Millisecond}, 0, true},
		{"half in use", wcStream{id: 1, win: w, upd: 4, read: uint64(used / 2), turnMin: 100 * time.Millisecond, turnLast: 100 * time.Millisecond}, 0, false},
		{"in use, the exit's record shows a standing queue", wcStream{id: 1, win: w, upd: 4, read: uint64(used), turnMin: 100 * time.Millisecond, turnLast: 100 * time.Millisecond}, 40 * time.Millisecond, false},
		{"hits with a bare turnaround", wcStream{id: 1, win: w, upd: 4, hit: 3, read: uint64(used / 2), turnMin: 101 * time.Millisecond, turnLast: 101 * time.Millisecond}, 0, true},
		{"hits with a long turnaround (a slow stream's own gaps)", wcStream{id: 1, win: w, upd: 4, hit: 4, read: uint64(used / 2), turnMin: 150 * time.Millisecond, turnLast: 150 * time.Millisecond}, 0, false},
	} {
		c := wtCore(1)
		c.turnMins.add(wtT0, 100*time.Millisecond, wcTurnWin)
		if tc.qSnd > 0 {
			c.qSnd, c.qSndAt = tc.qSnd, wtT0
		}
		in := wcIn{now: wtT0, streams: []wcStream{tc.s}}
		in.streams[0].turns = wcWakeTurns(nil, tc.s)
		if m := c.measure(in, 1); (len(m.f) == 1) != tc.join {
			t.Errorf("%s: F %v, want joined %v", tc.name, m.f, tc.join)
		}
	}
}

// A slow source leaves F only on the second interval in a row, and never
// where data waits behind holes.
func TestWcoreSlowSourceTwice(t *testing.T) {
	slow := wcStream{id: 1, win: 8 * wtMiB, upd: 3, read: 1e6, turnMin: 400 * time.Millisecond, turnLast: 400 * time.Millisecond}
	c := wtCore(1)
	c.cfg.side = wcExit // no record to wait for
	c.turnMins.add(wtT0, 100*time.Millisecond, wcTurnWin)
	c.inF[1] = true
	in := wcIn{now: wtT0, streams: []wcStream{slow}}
	if m := c.measure(in, 1); len(m.f) != 1 {
		t.Fatal("out of F on the first slow interval")
	}
	if m := c.measure(in, 1); len(m.f) != 0 {
		t.Fatal("still in F on the second")
	}
	c.inF[1] = true
	delete(c.slowN, 1)
	in.oooOK, in.segsIn, in.ooo = true, 1000, 300
	for i := 0; i < 3; i++ {
		if m := c.measure(in, 1); len(m.f) != 1 {
			t.Fatalf("interval %d under holes: out of F", i)
		}
	}
}

// q_turn is the 25th percentile of every wake's turnaround samples
// (wcMerge), not of each stream's least in the interval.
func TestWcoreQTurnP25(t *testing.T) {
	c := wtCore(1)
	c.turnMins.add(wtT0, 100*time.Millisecond, wcTurnWin)
	c.inF[1] = true
	w := func(min, last time.Duration) wcIn {
		return wcIn{now: wtT0, streams: []wcStream{{id: 1, win: 8 * wtMiB, upd: 1, read: 1e6, turnMin: min, turnLast: last}}}
	}
	acc := wcCopy(w(101*time.Millisecond, 120*time.Millisecond)) // one lucky sample
	for i := 0; i < 4; i++ {
		wcMerge(&acc, w(115*time.Millisecond, 125*time.Millisecond))
	}
	m := c.measure(acc, 0.5)
	if !m.qOK || m.qTurn < 14*time.Millisecond || m.qTurn > 16*time.Millisecond {
		t.Fatalf("q_turn %v of samples 101, 120, 115×4, 125×4, want ~15 ms (the least: 1 ms)", m.qTurn)
	}
}

// q_ctrl's floor is T_base plus the control channel's own offset, measured
// while the link's queue was empty, not the channel's rttMin of eight pongs
// (a standing queue of 24 s became that floor on the rig).
func TestWcoreCtrlFloor(t *testing.T) {
	c := wtCore(1)
	calm := wcIn{now: wtT0, srtt: c.tBase + time.Millisecond, pongs: []time.Duration{103 * time.Millisecond}, ctrlMin: 103 * time.Millisecond}
	c.measure(calm, 1)
	// a standing queue of 100 ms: rttMin of the last pongs is up at 200 ms
	q := wcIn{now: wtT0.Add(30 * time.Second), srtt: c.tBase + 100*time.Millisecond, ctrlMin: 200 * time.Millisecond,
		pongs: []time.Duration{203 * time.Millisecond, 204 * time.Millisecond}}
	m := c.measure(q, 1)
	if !m.ctrlUp || m.ctrlQ < 99*time.Millisecond {
		t.Fatalf("q_ctrl %v up %v under a standing queue of 100 ms, want ~100 ms over T_base + 3 ms", m.ctrlQ, m.ctrlUp)
	}
	// an interval with no pong after the run shows nothing (no new sample)
	q2 := wcIn{now: wtT0.Add(31 * time.Second), srtt: c.tBase + 100*time.Millisecond, ctrlMin: 200 * time.Millisecond}
	if m := c.measure(q2, 1); m.ctrlUp {
		t.Fatal("q_ctrl up again in an interval with no pong")
	}
	// the next high pong of the run does
	q2.now, q2.pongs = wtT0.Add(32*time.Second), []time.Duration{205 * time.Millisecond}
	if m := c.measure(q2, 1); !m.ctrlUp {
		t.Fatal("q_ctrl not up on the run's next high pong")
	}
	// with no offset measured yet the channel's rttMin is the floor
	d := wtCore(1)
	if m := d.measure(q, 1); m.ctrlUp || m.ctrlQ > 5*time.Millisecond {
		t.Fatalf("q_ctrl %v up %v with no offset: want the old floor", m.ctrlQ, m.ctrlUp)
	}
}

// With the lab's burst share on and Q_cap known, B is held where the
// sender's burst after its PROBE_RTT fits the queue; off by default.
func TestWcoreBurstCap(t *testing.T) {
	share := wcBurstShare
	t.Cleanup(func() { wcBurstShare = share })
	c, in := wtHold()
	c.qcap, c.qcapAt, c.qcapOK = 50*time.Millisecond, wtT0, true
	m := wtMeas(1, 60e6, 8*wtMiB)
	wcBurstShare = 0
	c.setB(in, m, 12*wtMiB)
	if c.B != 12*wtMiB || c.burstHeld {
		t.Fatalf("off: B %d held %v", c.B, c.burstHeld)
	}
	wcBurstShare = 0.55
	c.setB(in, m, 12*wtMiB)
	want := uint64(0.05*60e6/0.55 + m.hStep)
	if c.B < want-2 || c.B > want+2 || !c.burstHeld {
		t.Fatalf("on: B %d held %v, want %d", c.B, c.burstHeld, want)
	}
}

// The runner's hit gap: wcHitFrames of a stream's own frames' time at its
// rate (smoothed over wakes), at most wcHitGapMax.
func TestWcoreRunnerHitGap(t *testing.T) {
	l := wcLogLink("link", wcEdge)
	l.frame = 16 << 10
	now := wtT0
	l.noteRates(now, wcIn{})
	l.noteRates(now.Add(100*time.Millisecond), wcIn{streams: []wcStream{{id: 1, read: 1e6}, {id: 3}}}) // 10 MB/s, and none
	if g, want := l.gaps[1], time.Duration(3*16384/10e6*float64(time.Second)); g < want-time.Microsecond || g > want+time.Microsecond {
		t.Errorf("gap at 10 MB/s: %v, want %v", g, want)
	}
	if l.gaps[3] != wcHitGapMax {
		t.Errorf("gap of a stream that read nothing: %v", l.gaps[3])
	}
	l.noteRates(now.Add(200*time.Millisecond), wcIn{streams: []wcStream{{id: 1, read: 3e6}}})
	if _, ok := l.gaps[3]; ok || l.rates[1] <= 10e6 || l.rates[1] >= 30e6 {
		t.Errorf("rates %v gaps %v: a gone stream kept, or no smoothing", l.rates, l.gaps)
	}
}

// A drain's line says what set it off and by how much; a cut the runner
// sees that left B where it was is not logged.
func TestWcoreRunnerDrainCause(t *testing.T) {
	for _, tc := range []struct {
		e    wcEvent
		want string
	}{
		{wcEvent{why: wcWhyQueue, sig: wcSigTurn, q: 22 * time.Millisecond}, "path queue grew by 22 ms"},
		{wcEvent{why: wcWhyQueue, sig: wcSigOwn, q: 14 * time.Millisecond}, "path queue grew by 14 ms (the link's own round trip)"},
		{wcEvent{why: wcWhyQueue, sig: wcSigSnd, q: 31 * time.Millisecond}, "the exit's queue grew by 31 ms"},
		{wcEvent{why: wcWhyQueue, sig: wcSigCtrl, q: 80 * time.Millisecond}, "control pings came back 80 ms late"},
		{wcEvent{why: wcWhyLoss, sig: wcSigLoss, loss: 0.021}, "packets were lost as the windows grew (resent 2.1%)"},
		{wcEvent{why: wcWhyLoss, sig: wcSigOoo}, "packets arrived behind holes as the windows grew"},
		{wcEvent{why: wcWhyLoss, q: -1}, "packets arrived behind holes on a clean path"},
		{wcEvent{why: wcWhyRate, R: 50e6 / 8}, "path slowed to ~50 Mbit/s with its queue growing"},
	} {
		if got := wcDrainCause(tc.e); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.e, got, tc.want)
		}
	}
}

// The pacing-cap experiment: each end caps its socket's pacing at
// WcorePaceCap × its own delivery rate while it sends, lifts it while it
// sends nothing; off by default.
func TestWcorePaceCap(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("SO_MAX_PACING_RATE is Linux's")
	}
	old := WcorePaceCap
	t.Cleanup(func() { WcorePaceCap = old })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		buf := make([]byte, 64<<10)
		for {
			if _, err := c.Read(buf); err != nil {
				return
			}
		}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tc := c.(*net.TCPConn)
	pacing := func() uint64 {
		rc, _ := tc.SyscallConn()
		var v int
		rc.Control(func(fd uintptr) { v, _ = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MAX_PACING_RATE) })
		return uint64(uint32(v))
	}
	l := &wcLink{tcp: tc}
	if WcorePaceCap != 0 {
		t.Fatalf("pacing cap on by default: %g", WcorePaceCap)
	}
	WcorePaceCap = 1.25
	buf := make([]byte, 1<<20)
	now := wtT0
	l.paceCap(now)
	acked := func(n uint64) { // waits for the loopback to ack n bytes in all
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
			if ts, _ := tcpStats(tc); ts.bytesAcked >= n {
				return
			}
		}
		t.Fatalf("%d bytes not acked in 5 s", n)
	}
	for i := 0; i < 3; i++ {
		tc.Write(buf)
		acked(uint64(i+1) << 20)
		now = now.Add(100 * time.Millisecond)
		l.paceCap(now)
	}
	ts, _ := tcpStats(tc)
	got := pacing()
	if l.pcSet == 0 || got != uint32Cap(l.pcSet) || float64(l.pcSet) < 1.25*float64(ts.deliveryRate)*0.5 && l.pcSet > wcPaceFloor {
		t.Fatalf("sending: cap %d (socket %d), delivery rate %d", l.pcSet, got, ts.deliveryRate)
	}
	// nothing sent (or acked) since
	now = now.Add(100 * time.Millisecond)
	l.paceCap(now)
	if l.pcSet != 0 || pacing() != math.MaxUint32 {
		t.Fatalf("idle: cap %d (socket %d), want lifted", l.pcSet, pacing())
	}
}

// The cap is WcorePaceCap times the delivery rate, never under the floor,
// and where the socket's round trip shows no queue never under what the
// peer's windows allow per min_rtt.
func TestWcorePaceRate(t *testing.T) {
	old := WcorePaceCap
	t.Cleanup(func() { WcorePaceCap = old })
	WcorePaceCap = 1.25
	const T = 125 * time.Millisecond
	for _, tc := range []struct {
		name         string
		r            float64
		win          uint64
		minRTT, srtt time.Duration
		want         uint64
	}{
		{"the delivery rate rules", 40e6, 2 << 20, T, T, 50e6},
		{"no queue, the windows allow more (a sender in a slow turnaround)", 6.9e6, 2 << 20, T, T + 5*time.Millisecond, 8 * (2 << 20)},
		{"a standing queue: the delivery rate (the windows fill the path)", 6.9e6, 2 << 20, T, T + 30*time.Millisecond, 8625000},
		{"no min_rtt: the delivery rate", 6.9e6, 2 << 20, 0, T, 8625000},
		{"the floor", 1e3, 0, T, T, wcPaceFloor},
	} {
		if got := wcPaceRate(tc.r, tc.win, tc.minRTT, tc.srtt); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
}

func uint32Cap(v uint64) uint64 { return min(v, 1<<31-1) }

var _ = strings.Contains
