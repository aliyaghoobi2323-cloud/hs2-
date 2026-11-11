package engine

import (
	"testing"
	"time"
)

// withStartDial turns HS2_START_DIAL on for one test with a fast start gate
// and the given window.
func withStartDial(t *testing.T, on bool, window time.Duration) {
	t.Helper()
	oOn, oGate, oFor := startDial, startGate, startDialFor
	startDial, startDialFor = on, window
	startGate = newDialGate(startInflight, func() time.Duration { return time.Millisecond })
	t.Cleanup(func() { startDial, startGate, startDialFor = oOn, oGate, oFor })
}

// The first pool after a start dials through the start gate: 40 links come up
// far sooner than the ordinary gate's pace allows, every turn counted.
func TestStartDialFirstPoolFast(t *testing.T) {
	withStartDial(t, true, 3*time.Second)
	sp := newScaleTestPool(t, 1, 300, 100*time.Millisecond) // ordinary: 4 s for 40
	t0 := time.Now()
	before := startDialTurns.Load()
	sp.setTarget(40)
	eventually(t, "40 live links", func() bool { return sp.liveCount() == 40 })
	if took := time.Since(t0); took > 1500*time.Millisecond {
		t.Fatalf("40 links took %s with the start gate", took)
	}
	if n := startDialTurns.Load() - before; n != 40 {
		t.Fatalf("%d start-gate turns for 40 dials", n)
	}
}

// Once the window is over, a redial (a link that ended) and a raised target
// take the ordinary gate: steady state is unchanged.
func TestStartDialSteadyStateOrdinary(t *testing.T) {
	withStartDial(t, true, 200*time.Millisecond)
	sp := newScaleTestPool(t, 1, 300, time.Millisecond)
	sp.setTarget(10)
	eventually(t, "10 live links", func() bool { return sp.liveCount() == 10 })
	time.Sleep(300 * time.Millisecond)
	before := startDialTurns.Load()
	sp.endOneWith("read: reset by the network or the other server")
	sp.setTarget(20)
	eventually(t, "20 live links", func() bool { return sp.liveCount() == 20 })
	time.Sleep(100 * time.Millisecond)
	if n := startDialTurns.Load() - before; n != 0 {
		t.Fatalf("%d start-gate turns after the window", n)
	}
}

// Key off: no turn ever comes from the start gate.
func TestStartDialOff(t *testing.T) {
	withStartDial(t, false, 3*time.Second)
	sp := newScaleTestPool(t, 1, 300, time.Millisecond)
	before := startDialTurns.Load()
	sp.setTarget(20)
	eventually(t, "20 live links", func() bool { return sp.liveCount() == 20 })
	if n := startDialTurns.Load() - before; n != 0 {
		t.Fatalf("%d start-gate turns with the key off", n)
	}
}

// The window re-opens when a link is back after an outage (the edge
// restarted) — at once for the pool's very first link, otherwise at most once
// per startDialRearm.
func TestStartDialRearm(t *testing.T) {
	withStartDial(t, true, time.Second)
	p := newExitPool(t.Context(), 1, 100, nil, nil)
	p.mu.Lock()
	first := p.startArmed
	p.mu.Unlock()
	if first.IsZero() || p.startArms != 1 {
		t.Fatalf("not armed at the pool's start (arms %d)", p.startArms)
	}
	// The exit started before its edge: the first link back re-arms at once.
	p.mu.Lock()
	p.outage = true
	p.upCh = make(chan struct{})
	p.mu.Unlock()
	p.dialed()
	if p.startArms != 2 {
		t.Fatalf("first link after a refused start: arms %d, want 2", p.startArms)
	}
	// A later outage within the rearm time does not.
	p.mu.Lock()
	p.outage = true
	p.mu.Unlock()
	p.dialed()
	if p.startArms != 2 {
		t.Fatalf("outage within %s re-armed (arms %d)", startDialRearm, p.startArms)
	}
	// After it, it does.
	p.mu.Lock()
	p.startArmed = time.Now().Add(-startDialRearm - time.Second)
	p.outage = true
	p.mu.Unlock()
	p.dialed()
	if p.startArms != 3 {
		t.Fatalf("outage after %s: arms %d, want 3", startDialRearm, p.startArms)
	}
}
