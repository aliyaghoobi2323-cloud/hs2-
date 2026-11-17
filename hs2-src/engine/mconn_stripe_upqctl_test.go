package engine

import (
	"testing"
	"time"
)

// hl220: under an edge cap with HS2_QUEUE_CTL_KEEP, Iran's upload gate takes
// the controller's target (qLow): a 5 ms queue (under qLow) neither cuts the
// width nor, in that band, grows it; a queue over qLow still halves it; with
// no cap (or KEEP off, or HS2_QUEUE_CTL_STRIPE=0) the 3 ms target cuts at 5 ms.
func TestStripeUpGateUnderEdgeCap(t *testing.T) {
	oldT, oldUp, oldKeep, oldOff := stripeQTarget.Load(), stripeUpQTarget.Load(), queueCtlKeep, stripeUpQCtlOff
	t.Cleanup(func() {
		stripeQTarget.Store(oldT)
		stripeUpQTarget.Store(oldUp)
		queueCtlKeep, stripeUpQCtlOff = oldKeep, oldOff
		qCtlStatEdge.capped.Store(0)
	})
	stripeQTarget.Store(int64(3 * time.Millisecond))
	stripeUpQTarget.Store(0)
	run := func(keep, capped, off bool) (before, after int) {
		queueCtlKeep, stripeUpQCtlOff = keep, off
		qCtlStatEdge.capped.Store(0)
		if capped {
			qCtlStatEdge.capped.Store(1)
		}
		fixUpPathQ(t, 0)
		m, p := gateConn(6)
		m.iran, m.stUp = true, true
		now := time.Now()
		m.stripeWidthLocked(p, now)
		for i := 0; i < 10; i++ {
			now = now.Add(stripeQGrow)
			m.stripeWidthLocked(p, now)
		}
		before = m.stW
		stripeUpPathQ.mu.Lock()
		stripeUpPathQ.q = 5 * time.Millisecond
		stripeUpPathQ.mu.Unlock()
		for i := 0; i < 3; i++ {
			now = now.Add(stripeQEvery)
			m.stripeWidthLocked(p, now)
		}
		return before, m.stW
	}
	if b, a := run(true, true, false); b != 6 || a != 6 {
		t.Fatalf("capped+keep: width %d -> %d under a 5 ms queue, want 6 -> 6", b, a)
	}
	for _, c := range []struct{ keep, capped, off bool }{{true, false, false}, {false, true, false}, {true, true, true}} {
		if b, a := run(c.keep, c.capped, c.off); b != 6 || a >= 6 {
			t.Fatalf("%+v: width %d -> %d under a 5 ms queue, want 6 -> cut", c, b, a)
		}
	}
	// over qLow the capped gate still cuts at once
	queueCtlKeep, stripeUpQCtlOff = true, false
	qCtlStatEdge.capped.Store(1)
	fixUpPathQ(t, 12*time.Millisecond)
	m, p := gateConn(6)
	m.iran, m.stUp = true, true
	m.stWInit, m.stW = true, 6
	if w := m.stripeWidthLocked(p, time.Now()); w != 3 {
		t.Fatalf("capped: width %d after a 12 ms queue, want 3", w)
	}
	if m.stripeUpRoomLocked() {
		t.Fatalf("capped: room to grow right after a cut")
	}
}

// hl222: under an edge cap one queue spike halves the width once (not
// 6 -> 3 -> 1 on its 100 ms samples), a sub's start burst does not cut in
// its warm-up, a queue past twice the threshold still cuts at once, and a
// cleared queue ends the cut's hold (growth stripeQGrow after the cut).
func TestStripeUpGateCapSpike(t *testing.T) {
	oldT, oldUp, oldKeep, oldOff, oldSp := stripeQTarget.Load(), stripeUpQTarget.Load(), queueCtlKeep, stripeUpQCtlOff, stripeUpSpikeOff
	t.Cleanup(func() {
		stripeQTarget.Store(oldT)
		stripeUpQTarget.Store(oldUp)
		queueCtlKeep, stripeUpQCtlOff, stripeUpSpikeOff = oldKeep, oldOff, oldSp
		qCtlStatEdge.capped.Store(0)
	})
	stripeQTarget.Store(int64(3 * time.Millisecond))
	stripeUpQTarget.Store(0)
	queueCtlKeep, stripeUpQCtlOff = true, false
	qCtlStatEdge.capped.Store(1)
	setQ := func(q time.Duration) {
		stripeUpPathQ.mu.Lock()
		stripeUpPathQ.q = q
		stripeUpPathQ.mu.Unlock()
	}
	start := func(off bool) (*mConn, *msub, time.Time) {
		stripeUpSpikeOff = off
		fixUpPathQ(t, 0)
		stripeGlobal.mu.Lock()
		stripeGlobal.cutAt, stripeGlobal.growAt = time.Time{}, time.Time{}
		stripeGlobal.mu.Unlock()
		m, p := gateConn(6)
		m.iran, m.stUp = true, true
		now := time.Now().Add(time.Hour)
		m.stripeWidthLocked(p, now)
		for i := 0; i < 10; i++ {
			now = now.Add(stripeQGrow)
			m.stripeWidthLocked(p, now)
		}
		if m.stW != 6 {
			t.Fatalf("width %d before the spike, want 6", m.stW)
		}
		return m, p, now
	}
	step := func(m *mConn, p *msub, now *time.Time, n int) {
		for i := 0; i < n; i++ {
			*now = now.Add(stripeQEvery)
			m.stripeWidthLocked(p, *now)
		}
	}
	// one spike of 10 ms for 4 samples: one halving
	m, p, now := start(false)
	setQ(10 * time.Millisecond)
	step(m, p, &now, 4)
	if m.stW != 3 {
		t.Fatalf("spike: width %d after 4 samples of 10 ms, want 3 (one halving)", m.stW)
	}
	// it stays past stripeQGrow: a second halving
	step(m, p, &now, 2)
	if m.stW != 1 {
		t.Fatalf("persistent queue: width %d after %v, want 1", m.stW, 6*stripeQEvery)
	}
	// hl223: a queue of qHigh..qFastDelay halves once per stripeUpBigGap
	m, p, now = start(false)
	setQ(20 * time.Millisecond)
	step(m, p, &now, 1)
	if m.stW != 3 {
		t.Fatalf("20 ms: width %d after 1 sample, want 3", m.stW)
	}
	step(m, p, &now, 1)
	if m.stW != 3 {
		t.Fatalf("20 ms: width %d %v after the cut, want 3 (one cut per %v)", m.stW, stripeQEvery, stripeUpBigGap)
	}
	step(m, p, &now, int(stripeUpBigGap/stripeQEvery)-1)
	if m.stW != 1 {
		t.Fatalf("20 ms: width %d after %v, want 1 (second cut)", m.stW, stripeUpBigGap)
	}
	// 40 ms and more cuts every sample
	m, p, now = start(false)
	setQ(qFastDelay)
	step(m, p, &now, 2)
	if m.stW != 1 {
		t.Fatalf("40 ms: width %d after 2 samples, want 1", m.stW)
	}
	// warm-up: a sub that starts sending; its burst does not cut for stripeUpWarm
	m, p, now = start(false)
	m.stripeUpNotePushLocked(&msub{}, now)
	setQ(10 * time.Millisecond)
	step(m, p, &now, 2)
	if m.stW != 6 {
		t.Fatalf("warm-up: width %d, want 6", m.stW)
	}
	step(m, p, &now, 2)
	if m.stW != 3 {
		t.Fatalf("after warm-up: width %d, want 3", m.stW)
	}
	// the queue is gone: growth stripeQGrow after the cut, not stripeCutHold
	cutAt := m.stCapCutAt
	setQ(0)
	for m.stW == 3 && now.Sub(cutAt) < stripeCutHold {
		step(m, p, &now, 1)
	}
	if m.stW != 4 || now.Sub(cutAt) > stripeQGrow+stripeQEvery {
		t.Fatalf("release: width %d at %v after the cut, want 4 by %v", m.stW, now.Sub(cutAt), stripeQGrow+stripeQEvery)
	}
	// HS2_QUEUE_CTL_STRIPE_SPIKE=0: every 10 ms sample halves
	m, p, now = start(true)
	setQ(10 * time.Millisecond)
	step(m, p, &now, 3)
	if m.stW != 0 {
		t.Fatalf("spike off: width %d after 3 samples, want 0", m.stW)
	}
}

// hl222: with the split gate's lower cut threshold (5.36 ms) a 12 ms sample
// of the same spike does not cut again; 16 ms (past qHigh) does.
func TestStripeUpGateCapSpikeSplit(t *testing.T) {
	oldKeep, oldOff, oldSp := queueCtlKeep, stripeUpQCtlOff, stripeUpSpikeOff
	t.Cleanup(func() {
		queueCtlKeep, stripeUpQCtlOff, stripeUpSpikeOff = oldKeep, oldOff, oldSp
		qCtlStatEdge.capped.Store(0)
	})
	queueCtlKeep, stripeUpQCtlOff, stripeUpSpikeOff = true, false, false
	qCtlStatEdge.capped.Store(1)
	m, _ := gateConn(6)
	m.iran = true
	now := time.Now()
	m.stripeUpCapCutLocked(now)
	cutAt := 5360 * time.Microsecond
	if !m.stripeUpCapSkipLocked(now.Add(stripeQEvery), 12*time.Millisecond, cutAt) {
		t.Fatalf("12 ms of the same spike cut again")
	}
	if !m.stripeUpCapSkipLocked(now.Add(stripeQEvery), 16*time.Millisecond, cutAt) {
		t.Fatalf("16 ms (past qHigh) cut again within %v (hl223)", stripeUpBigGap)
	}
	if m.stripeUpCapSkipLocked(now.Add(stripeUpBigGap), 16*time.Millisecond, cutAt) {
		t.Fatalf("16 ms (past qHigh) did not cut %v after the cut", stripeUpBigGap)
	}
	if m.stripeUpCapSkipLocked(now.Add(stripeQGrow), 12*time.Millisecond, cutAt) {
		t.Fatalf("a queue that stayed stripeQGrow did not cut")
	}
}

// hl228: under an edge cap a queue under qHigh while the controller's probe
// step is in force (that tick and the next) does not cut; qHigh and more
// still does; after the probe window it cuts again.
func TestStripeUpGateCapProbe(t *testing.T) {
	oldKeep, oldOff, oldSp := queueCtlKeep, stripeUpQCtlOff, stripeUpSpikeOff
	t.Cleanup(func() {
		queueCtlKeep, stripeUpQCtlOff, stripeUpSpikeOff = oldKeep, oldOff, oldSp
		qCtlStatEdge.capped.Store(0)
		qCtlStatEdge.probeUntil.Store(0)
	})
	queueCtlKeep, stripeUpQCtlOff, stripeUpSpikeOff = true, false, false
	qCtlStatEdge.capped.Store(1)
	m, _ := gateConn(6)
	m.iran = true
	now := time.Now()
	cutAt := 5360 * time.Microsecond
	if m.stripeUpCapSkipLocked(now, 12*time.Millisecond, cutAt) {
		t.Fatalf("12 ms with no probe skipped")
	}
	qCtlStatEdge.probeNote(now)
	if !m.stripeUpCapSkipLocked(now.Add(healthTick), 12*time.Millisecond, cutAt) {
		t.Fatalf("12 ms under the controller's probe cut")
	}
	if m.stripeUpCapSkipLocked(now.Add(healthTick), 16*time.Millisecond, cutAt) {
		t.Fatalf("16 ms (qHigh) under a probe skipped")
	}
	if m.stripeUpCapSkipLocked(now.Add(qProbeQuiet), 12*time.Millisecond, cutAt) {
		t.Fatalf("12 ms after the probe window skipped")
	}
}

// hl229: the stripe gate leaves an onset queue up to qOnsetGate to the
// controller while a lane's onset window holds; not past it, not after it.
func TestStripeUpGateCapOnset(t *testing.T) {
	oldKeep, oldOff, oldSp := queueCtlKeep, stripeUpQCtlOff, stripeUpSpikeOff
	t.Cleanup(func() {
		queueCtlKeep, stripeUpQCtlOff, stripeUpSpikeOff = oldKeep, oldOff, oldSp
		qCtlStatEdge.capped.Store(0)
		qCtlStatEdge.onsetUntil.Store(0)
	})
	queueCtlKeep, stripeUpQCtlOff, stripeUpSpikeOff = true, false, false
	qCtlStatEdge.capped.Store(1)
	m, _ := gateConn(6)
	m.iran = true
	now := time.Now()
	cutAt := 5360 * time.Microsecond
	if m.stripeUpCapSkipLocked(now, 23*time.Millisecond, cutAt) {
		t.Fatalf("23 ms with no onset window skipped")
	}
	qCtlStatEdge.onsetNote(now.Add(qProbeQuiet), 0)
	if !m.stripeUpCapSkipLocked(now.Add(healthTick), 23*time.Millisecond, cutAt) {
		t.Fatalf("23 ms in the onset window cut")
	}
	if m.stripeUpCapSkipLocked(now.Add(healthTick), 30*time.Millisecond, cutAt) {
		t.Fatalf("30 ms (> qOnsetGate) in the onset window skipped")
	}
	if m.stripeUpCapSkipLocked(now.Add(qProbeQuiet), 23*time.Millisecond, cutAt) {
		t.Fatalf("23 ms after the onset window skipped")
	}
	// hl231: after the controller's no-hold drop on a 77 ms onset tick,
	// samples up to 77 ms are left to it; past it (a queue that grows) cut
	now = now.Add(time.Minute)
	if m.stripeUpCapSkipLocked(now, 70*time.Millisecond, cutAt) {
		t.Fatalf("70 ms with no onset window skipped")
	}
	qCtlStatEdge.onsetNote(now.Add(qProbeQuiet), 77*time.Millisecond)
	if !m.stripeUpCapSkipLocked(now.Add(healthTick), 70*time.Millisecond, cutAt) {
		t.Fatalf("70 ms under a 77 ms onset queue cut")
	}
	if m.stripeUpCapSkipLocked(now.Add(healthTick), 90*time.Millisecond, cutAt) {
		t.Fatalf("90 ms past a 77 ms onset queue skipped")
	}
	if m.stripeUpCapSkipLocked(now.Add(qProbeQuiet), 70*time.Millisecond, cutAt) {
		t.Fatalf("70 ms after the onset window skipped")
	}
	qCtlStatEdge.onsetQ.Store(0)
}

// hl232: under a probe step, after one cut the stripe gate leaves a queue
// under qFastDelay to the controller (no 10 -> 5 -> 2 -> 1 collapse); not at
// qFastDelay, not outside the probe window.
func TestStripeUpGateCapProbeOneCut(t *testing.T) {
	oldKeep, oldOff, oldSp := queueCtlKeep, stripeUpQCtlOff, stripeUpSpikeOff
	t.Cleanup(func() {
		queueCtlKeep, stripeUpQCtlOff, stripeUpSpikeOff = oldKeep, oldOff, oldSp
		qCtlStatEdge.capped.Store(0)
		qCtlStatEdge.probeUntil.Store(0)
	})
	queueCtlKeep, stripeUpQCtlOff, stripeUpSpikeOff = true, false, false
	qCtlStatEdge.capped.Store(1)
	m, _ := gateConn(6)
	m.iran = true
	now := time.Now()
	cutAt := 5360 * time.Microsecond
	qCtlStatEdge.probeNote(now)
	if m.stripeUpCapSkipLocked(now.Add(400*time.Millisecond), 30*time.Millisecond, cutAt) {
		t.Fatalf("30 ms under a probe with no cut yet skipped")
	}
	m.stripeUpCapCutLocked(now.Add(400 * time.Millisecond))
	if !m.stripeUpCapSkipLocked(now.Add(700*time.Millisecond), 30*time.Millisecond, cutAt) {
		t.Fatalf("30 ms under a probe after one cut not left to the controller")
	}
	if m.stripeUpCapSkipLocked(now.Add(700*time.Millisecond), qFastDelay, cutAt) {
		t.Fatalf("qFastDelay under a probe skipped")
	}
	qCtlStatEdge.probeUntil.Store(0)
	if m.stripeUpCapSkipLocked(now.Add(700*time.Millisecond), 30*time.Millisecond, cutAt) {
		t.Fatalf("30 ms with no probe skipped")
	}
}
