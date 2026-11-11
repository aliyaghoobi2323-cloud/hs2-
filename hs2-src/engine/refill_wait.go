package engine

import (
	"sync/atomic"
	"time"
)

// Start-window wait bound (HS2_REFILL_WAIT_MS, edge, default off).
//
// The refill hold (refill.go) queues a new connection while the pool is still
// opening after a start, until a link with room under the fair-share cap comes
// up — up to refillHoldMax. At a start with thousands of connections opening
// in two seconds while the exit dials its links in batches (lab: 1, 20, 33 per
// path at 0, 4, 6 s) a light connection waited up to 2.2 s, and every byte its
// user sent meanwhile waited with it: the crowd's p99 of the first 15 s was
// 0.4-1.9 s before any path was bad.
//
// With the key at N > 0 a connection waits in the hold at most N ms; then it
// leaves the queue and takes the least-loaded link now (Pick: a link not
// pressed, light before heavy, fewest flowing / picks / users). The hold
// stays for everyone else, so the bulk still spreads over the links as they
// come; only the tail of the wait is cut. Links are never refused and nothing
// is closed.
var refillWait time.Duration // loadEnvKeys: HS2_REFILL_WAIT_MS (0: off)

// refillWaitCut counts connections that left the hold at the bound.
var refillWaitCut atomic.Int64

// refillWaitTimer is the per-connection bound's channel (nil: off, never
// fires).
func refillWaitTimer() (<-chan time.Time, func()) {
	if refillWait <= 0 {
		return nil, func() {}
	}
	t := time.NewTimer(refillWait)
	return t.C, func() { t.Stop() }
}

// leaveHeld takes a waiter out of the hold's queue. It returns the link the
// hold already gave it, if any (that placement is kept, not released).
func (m *LinkManager) leaveHeld(w *holdWaiter) *managedLink {
	m.mu.Lock()
	for i, q := range m.hold.queue {
		if q == w {
			m.hold.queue = append(m.hold.queue[:i], m.hold.queue[i+1:]...)
			m.mu.Unlock()
			return nil
		}
	}
	m.mu.Unlock()
	return <-w.ch // answered under the lock before we looked
}
