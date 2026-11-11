package engine

import (
	"context"
	"fmt"
	"slices"
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
// user sent meanwhile waited with it: the crowd's p99 of the first 5 s was
// 0.6-1.5 s before any path was bad.
//
// With the key at N > 0 a connection that has waited N ms in the hold takes
// the least-loaded link that holds fewer than refillWaitCapX times the
// hold's fair-share cap (Pick's order: not pressed, light before heavy,
// fewest flowing / picks / users); when every link is that full it keeps
// waiting and looks again N ms later. The rest of the queue is unchanged, so
// the bulk still spreads over the links as they come; only the tail of the
// wait is cut. Nothing is refused or closed.
//
// The first version took the least-loaded link with no limit: start p99
// 1093 -> 170 ms but the busiest link held 93 connections instead of 72
// and the crowd's p95 stayed ~9 ms higher for the rest of the run
// (docs/reality-architecture.md 8.7).
var refillWait time.Duration // loadEnvKeys: HS2_REFILL_WAIT_MS (0: off)

// refillWaitCapX: a connection leaving the hold at the bound goes only to a
// link under this multiple of the fair-share cap.
const refillWaitCapX = 2

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

// leaveHeldBounded is the bound's placement for waiter w: a link under
// refillWaitCapX times the cap (w leaves the queue, counted), or the answer
// the hold already sent w (done: ml may be nil when the hold ended); !done:
// every link is that full, w stays queued.
func (m *LinkManager) leaveHeldBounded(w *holdWaiter) (ml *managedLink, done bool) {
	m.mu.Lock()
	for i, q := range m.hold.queue {
		if q != w {
			continue
		}
		if refillWaitBytes {
			ml = m.pickByBytesLocked(refillWaitCapX * m.refillCapLocked()) // v3
		} else {
			ml = m.pickLocked(refillWaitCapX * m.refillCapLocked())
		}
		if ml == nil {
			m.mu.Unlock()
			return nil, false
		}
		m.hold.queue = append(m.hold.queue[:i], m.hold.queue[i+1:]...)
		m.placeLocked(ml)
		m.mu.Unlock()
		refillWaitCut.Add(1)
		return ml, true
	}
	m.mu.Unlock()
	return <-w.ch, true // answered under the lock before we looked
}

// waitHeld waits for the hold's answer to w, leaving at the bound
// (HS2_REFILL_WAIT_MS) for a link with room under refillWaitCapX times the
// cap. ok false: ctx ended (w withdrawn). ml nil with ok: the hold ended with
// no link up.
func (m *LinkManager) waitHeld(ctx context.Context, w *holdWaiter) (ml *managedLink, ok bool) {
	for {
		bound, stop := refillWaitTimer()
		select {
		case ml := <-w.ch:
			stop()
			return ml, true
		case <-bound:
			if ml, done := m.leaveHeldBounded(w); done {
				return ml, true
			}
		case <-ctx.Done():
			stop()
			m.cancelHeld(w)
			return nil, false
		}
	}
}

// refillWaitBytes (HS2_REFILL_WAIT_BYTES=1, edge, default off; v3 of the
// bound): a connection leaving the hold at the bound goes to the serving link
// under the limit that moved the fewest bytes in the last sample (rate, both
// directions; ties: fewer users), not by Pick's connection counts. v2 put the
// leavers by counts and still sent them to links whose queues were the
// slowest at the start (docs/reality-architecture.md 8.7, 8.9).
var refillWaitBytes bool // loadEnvKeys

// pickByBytesLocked: the serving link with users < limit and the lowest byte
// rate; nil when none. Caller holds m.mu.
func (m *LinkManager) pickByBytesLocked(limit int) *managedLink {
	var best *managedLink
	for _, ml := range m.links {
		if !ml.serving() || !m.infoReadyLocked(ml) || stripeOn(ml) || int(ml.users.Load()) >= limit {
			continue
		}
		if best == nil || ml.rate < best.rate || (ml.rate == best.rate && ml.users.Load() < best.users.Load()) {
			best = ml
		}
	}
	return best
}

// linkSpreadLog (HS2_LINK_SPREAD_LOG=1, lab diagnostic, default off): every
// 10 s the reverse edge logs how its open user connections spread over the
// serving links, and the count of connections that left the start hold at the
// bound so far — so a lab run shows whether the start window's placement
// stays unbalanced for the rest of the run (docs 8.13).
var linkSpreadLog bool // loadEnvKeys

// spreadLine is that log line, "" with no serving link.
func (m *LinkManager) spreadLine() string {
	m.mu.Lock()
	var u []int
	for _, ml := range m.links {
		if ml.serving() {
			u = append(u, int(ml.users.Load()))
		}
	}
	m.mu.Unlock()
	if len(u) == 0 {
		return ""
	}
	slices.Sort(u)
	sum := 0
	for _, x := range u {
		sum += x
	}
	mean := float64(sum) / float64(len(u))
	over := 0
	for _, x := range u {
		if float64(x) > 1.5*mean {
			over++
		}
	}
	return fmt.Sprintf("link spread: %d serving links, %d users, mean %.1f, p90 %d, max %d, %d links above 1.5x mean; %d left the start hold at the bound so far",
		len(u), sum, mean, u[len(u)*9/10], u[len(u)-1], over, refillWaitCut.Load())
}

// refillCompleteMin: with the start-window bound on (HS2_REFILL_WAIT_MS > 0)
// a start episode whose pool is complete stays on at least this long after it
// began. With the exit's start dial (HS2_START_DIAL) the pool can be complete
// before most users have connected; the episode then ended and the crowd
// arriving a second later was placed by plain Pick — flowing streams first,
// open connections last — which put 227 connections on one link (the fair
// share was 40) for the rest of a lab run (docs 8.13). While the episode is
// on, every link holds at most the fair share; with the pool complete nobody
// waits for that. A partial-loss episode ends as before.
var refillCompleteMin = 5 * time.Second
