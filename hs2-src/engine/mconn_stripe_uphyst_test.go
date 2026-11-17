package engine

import (
	"os"
	"testing"
	"time"
)

// ul197: the hysteresis helper: the first over-threshold sample is held
// unless far over; a second consecutive one cuts; a stale one is forgotten.
func TestStripeUpHystHold(t *testing.T) {
	ms := time.Millisecond
	oldF := stripeUpHystFast
	t.Cleanup(func() { stripeUpHystFast = oldF })
	stripeUpHystFast = 3 * ms
	if os.Getenv("HS2_MCONN_STRIPE_UPGATE_HYST_FAST_MS") == "" && oldF != 3*ms {
		t.Fatalf("fast default %v, want 3ms", oldF)
	}
	m := &mConn{iran: true}
	now := time.Now()
	at := 2 * ms
	step := func(q time.Duration) bool { now = now.Add(stripeQEvery); return m.stripeUpHystHoldLocked(now, q, at) }
	if step(1 * ms) {
		t.Fatal("under the threshold: held")
	}
	if !step(3 * ms) {
		t.Fatal("first sample 1 ms over: not held")
	}
	if step(3 * ms) {
		t.Fatal("second consecutive sample over: held (must cut)")
	}
	if step(2500 * time.Microsecond) {
		t.Fatal("third consecutive sample over: held")
	}
	step(0) // back under: the run ends
	if step(5*ms + time.Microsecond) {
		t.Fatal("first sample >3 ms over: held (a real queue must cut at once)")
	}
	step(0)
	if !step(5 * ms) {
		t.Fatal("exactly 3 ms over: not held")
	}
	// the previous over-sample is stale after an idle gap
	now = now.Add(stripeUpHystStale + time.Millisecond)
	if !m.stripeUpHystHoldLocked(now, 3*ms, at) {
		t.Fatal("over after an idle gap: not held")
	}
}

// ul197: through the gate (Iran, split on): key off cuts on the first noisy
// sample; key on holds it (no cut, no growth) and cuts on the second, and a
// big queue cuts at once.
func TestStripeUpHystGate(t *testing.T) {
	ms := time.Millisecond
	oldT, oldS, oldH := stripeQTarget.Load(), stripeUpSplit, stripeUpHyst.Load()
	t.Cleanup(func() { stripeQTarget.Store(oldT); stripeUpSplit = oldS; stripeUpHyst.Store(oldH) })
	stripeQTarget.Store(int64(3 * ms))
	stripeUpSplit = true
	fixPathQ(t, 0)
	fixUpPathQ(t, 0)
	setUpQ := func(q time.Duration) {
		p := stripeUpPathQ
		p.mu.Lock()
		p.q = q
		p.mu.Unlock()
	}
	gate := func() (*mConn, *msub, func(time.Duration) int) {
		m, p := gateConn(4)
		m.iran, m.stUp = true, true
		m.stWInit, m.stW = true, 4
		now := time.Now()
		m.stGrowAt = now
		return m, p, func(q time.Duration) int {
			setUpQ(q)
			now = now.Add(stripeQEvery)
			return m.stripeWidthLocked(p, now)
		}
	}
	// key off: 1 ms over the 2 ms cut threshold halves at once
	stripeUpHyst.Store(false)
	_, _, step := gate()
	if w := step(3 * ms); w != 2 {
		t.Fatalf("key off: width %d after a noisy sample, want 2", w)
	}
	// key on
	stripeUpHyst.Store(true)
	m, _, step := gate()
	if w := step(3 * ms); w != 4 {
		t.Fatalf("key on: width %d after the first noisy sample, want 4 (held)", w)
	}
	if m.stQ < time.Duration(stripeQTarget.Load())/2 {
		t.Fatalf("held sample: stQ %v allows growth", m.stQ)
	}
	if w := step(3 * ms); w != 2 {
		t.Fatalf("key on: width %d after the second, want 2 (cut)", w)
	}
	_, _, step = gate()
	if w := step(10 * ms); w != 2 {
		t.Fatalf("key on: width %d after an 8 ms excess, want 2 (fast cut)", w)
	}
}
