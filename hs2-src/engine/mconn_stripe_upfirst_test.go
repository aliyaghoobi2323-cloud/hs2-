package engine

import (
	"testing"
	"time"
)

// hl235: under an edge cap the gate's first samples of a fresh upload stripe
// (lab hl234 hS2 u1b: 6.6 ms in the same millisecond as the first stripe sub)
// do not cut; after stripeUpFirstN samples and stripeUpWarm the same queue
// cuts; a queue past qFastDelay cuts on the first sample; with no cap the
// first sample cuts as before.
func TestStripeUpGateCapFirstSample(t *testing.T) {
	oldT, oldUp, oldKeep, oldOff, oldSp := stripeQTarget.Load(), stripeUpQTarget.Load(), queueCtlKeep, stripeUpQCtlOff, stripeUpSpikeOff
	t.Cleanup(func() {
		stripeQTarget.Store(oldT)
		stripeUpQTarget.Store(oldUp)
		queueCtlKeep, stripeUpQCtlOff, stripeUpSpikeOff = oldKeep, oldOff, oldSp
		qCtlStatEdge.capped.Store(0)
	})
	stripeQTarget.Store(int64(3 * time.Millisecond))
	stripeUpQTarget.Store(0)
	queueCtlKeep, stripeUpQCtlOff, stripeUpSpikeOff = true, false, false
	cutSeen := func() bool {
		stripeGlobal.mu.Lock()
		defer stripeGlobal.mu.Unlock()
		return !stripeGlobal.cutAt.IsZero()
	}
	fresh := func(capped bool, q time.Duration) (*mConn, *msub, time.Time) {
		qCtlStatEdge.capped.Store(0)
		if capped {
			qCtlStatEdge.capped.Store(1)
		}
		fixUpPathQ(t, q) // also clears stripeGlobal
		m, p := gateConn(4)
		m.iran, m.stUp = true, true
		return m, p, time.Now().Add(2 * time.Hour)
	}
	// capped, 10 ms (over the 8 ms cut threshold, under qHigh): no cut until
	// 2 samples and stripeUpWarm since the first
	m, p, now := fresh(true, 10*time.Millisecond)
	first := stripeUpSpikeStat.first.Load()
	m.stripeWidthLocked(p, now)
	if cutSeen() || m.stCapCut || !m.stCapCutAt.IsZero() {
		t.Fatalf("capped: the first sample of a fresh stripe cut")
	}
	if !m.stGrowAt.Equal(now) {
		t.Fatalf("capped: growth held to %v after a skipped first sample", m.stGrowAt.Sub(now))
	}
	var at time.Duration
	for at = stripeQEvery; at < stripeUpWarm; at += stripeQEvery {
		m.stripeWidthLocked(p, now.Add(at))
		if cutSeen() {
			t.Fatalf("capped: cut %v into the stripe (sample %d), want none before %v", at, m.stGateN, stripeUpWarm)
		}
	}
	if d := stripeUpSpikeStat.first.Load() - first; d < 2 {
		t.Fatalf("capped: %d first-sample skips counted, want >= 2", d)
	}
	m.stripeWidthLocked(p, now.Add(at))
	if !cutSeen() || !m.stCapCut {
		t.Fatalf("capped: no cut %v into the stripe (%d samples) on a 10 ms queue", at, m.stGateN)
	}
	// a queue the controller cuts fast for: first sample cuts
	m, p, now = fresh(true, qFastDelay+5*time.Millisecond)
	m.stripeWidthLocked(p, now)
	if !cutSeen() {
		t.Fatalf("capped: a %v first sample did not cut", qFastDelay+5*time.Millisecond)
	}
	// no cap: the first sample cuts as before
	m, p, now = fresh(false, 10*time.Millisecond)
	m.stripeWidthLocked(p, now)
	if !cutSeen() {
		t.Fatalf("uncapped: the first 10 ms sample did not cut")
	}
	// a gate not started by the gate itself (stGate0 zero) is not affected
	m, p, now = fresh(true, 20*time.Millisecond)
	m.stWInit, m.stW = true, 4
	if w := m.stripeWidthLocked(p, now); w != 2 {
		t.Fatalf("capped, pre-set gate: width %d after a 20 ms sample, want 2", w)
	}
}
