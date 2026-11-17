package smux

import (
	"sync/atomic"
	"time"
)

// hs2 dl76: a heavy stream's window floor (SetHeavyFloor). A marked stream
// starts like any other (the base window, MaxStreamBuffer) and, from the
// first receive-rate epoch (rateEpoch) in which it took at least
// heavyFloorEpochBytes (1 MiB in 250 ms, ~34 Mbit/s: far above a light
// user), its base — the window the adaptive rules never cut below — becomes
// the floor. hs2 marks Iran's mconn subs (the download's receiving side): the
// lab (10 lossy 90 ms paths, 3000 light users) showed a download's primary
// held at the 2 MiB base for seconds by the queue / neighbour rules,
// window-bound at ~100 Mbit/s while its link sat app-limited with no queue.
//
// The floor is like SetOffWindow's fixed window: credit, not memory, and not
// drawn from the WindowBudget; growth above it still is. Growth the stream
// already had below the floor goes back to the budget.

var heavyFloorEpochBytes uint32 = 1 << 20 // a var for the tests

// SetHeavyFloor marks s: once it receives at a heavy rate its base window
// becomes n (no-op without AutoWindow or for n not above the base).
func (s *Stream) SetHeavyFloor(n int) {
	if s.aw == nil || n <= 0 {
		return
	}
	s.bufferLock.Lock()
	if uint32(n) > s.aw.base {
		s.aw.floorWant = uint32(n)
	}
	s.bufferLock.Unlock()
}

// SetHeavyFloorGated (hs2 dl77) is SetHeavyFloor with a queue gate: while
// the floor is raised, a probe of the stream's loop (UPD out, data back:
// the exit's socket and the path included) that comes back more than qLow
// (~4 ms at 90 ms) over the path's minimum drops the base back to where it
// was at once — the floor's extra window was only queueing in front of
// light users then — and the floor comes back after a hold (0.5 s, doubled
// while the floor keeps getting dropped within 2 s, up to 8 s).
//
// need: queued probes in a row that drop it (1: the first; 2 ignores a lone
// late probe — jitter, a loss recovery the Rel filter missed).
func (s *Stream) SetHeavyFloorGated(n, need int) {
	if s.aw == nil || n <= 0 {
		return
	}
	s.bufferLock.Lock()
	if uint32(n) > s.aw.base {
		s.aw.floorWant = uint32(n)
		s.aw.floorGate = true
		s.aw.floorNeed = uint8(min(max(need, 1), 8))
	}
	s.bufferLock.Unlock()
}

// SetHeavyFloorSoft (dl77, with SetHeavyFloorGated): the gate cuts the
// raised base by a quarter (not back to the old base) and without a hold;
// an epoch at a heavy rate with no queued probe takes it up a quarter again,
// up to the floor — the floor settles where this stream's loop shows no
// queue.
func (s *Stream) SetHeavyFloorSoft() {
	if s.aw == nil {
		return
	}
	s.bufferLock.Lock()
	s.aw.floorSoft = true
	s.bufferLock.Unlock()
}

// HeavyFloorQueue (dl77, 0: qLow): the queueing delay over the path's
// minimum at which the gate counts a probe as queued; set at start.
var HeavyFloorQueue time.Duration

// awFloorQueued: a probe of a gated floor that counts as queued.
func (a *streamAW) awFloorQueued(sample, min, rawMin time.Duration) bool {
	if q := HeavyFloorQueue; q > 0 {
		return sample > min+q
	}
	return sample > min+a.cfg.qLow(rawMin)
}

// awFloorTargetLocked: the base the epoch raise goes to (soft: a step).
func (a *streamAW) awFloorTargetLocked() uint32 {
	if !a.floorSoft || a.floorBase == 0 {
		return a.floorWant
	}
	if a.floorQE {
		return 0 // queued this epoch: no step up
	}
	return min(a.floorWant, a.base+a.base/4)
}

const (
	floorHoldMin = 500 * time.Millisecond
	floorHoldMax = 8 * time.Second
	floorStable  = 2 * time.Second // a floor that held this long resets the hold
)

// heavyFloorDrops counts SetHeavyFloorGated drops (HeavyFloorDrops).
var heavyFloorDrops atomic.Int64

// HeavyFloorDrops: floors the queue gate dropped so far.
func HeavyFloorDrops() int64 { return heavyFloorDrops.Load() }

// awFloorMayRaiseLocked: the gate's hold is over (always true ungated).
func (a *streamAW) awFloorMayRaiseLocked(ns int64) bool {
	return !a.floorGate || ns >= a.floorHoldUntil
}

// awDropFloorLocked (bufferLock held): the base goes back to the one before
// the floor; growth the budget granted above it stays.
func (s *Stream) awDropFloorLocked(now time.Time) {
	a := s.aw
	if a.floorBase == 0 || a.base <= a.floorBase {
		return
	}
	ns := now.UnixNano()
	a.floorQ = 0
	nb := a.floorBase
	if a.floorSoft {
		nb = max(a.floorBase, a.base-a.base/4)
		a.floorHoldUntil = ns + int64(rateEpoch)
	} else if ns-a.floorAt >= int64(floorStable) || a.floorHold == 0 {
		a.floorHold = floorHoldMin
	} else {
		a.floorHold = min(2*a.floorHold, floorHoldMax)
	}
	if !a.floorSoft {
		a.floorHoldUntil = ns + int64(a.floorHold)
	}
	a.base = nb
	nw := a.base + a.extra
	a.win, a.limitedAt, a.usedAt = nw, now, now
	a.probeOn, a.pendingUPD, a.nRecent = false, false, 0
	consumed := s.numRead
	a.limit, a.limitWin = consumed+nw, nw
	s.incr = 0
	heavyFloorDrops.Add(1)
	if consumed > 0 {
		go s.sendWindowUpdateSeq(consumed, nw, s.nextUPDSeqLocked()) // the peer learns the smaller window now (aw163: in order)
	}
}

// heavyFloorN counts streams whose base was raised (HeavyFloorN).
var heavyFloorN atomic.Int64

// HeavyFloorN: streams that reached SetHeavyFloor's floor so far.
func HeavyFloorN() int64 { return heavyFloorN.Load() }

// awRaiseBaseLocked (bufferLock held): the base becomes f > base.
func (s *Stream) awRaiseBaseLocked(f uint32) {
	a := s.aw
	if a.closed || a.off || f <= a.base {
		return
	}
	if a.extra > 0 {
		// growth below f is the floor's now: back to the budget and out of
		// the session's bucket (where the growth had put it)
		give := a.extra
		if a.win > f {
			give = f - a.base
		}
		a.extra -= give
		a.cfg.Budget.give(int64(give))
		atomic.AddInt32(&s.sess.bucket, -int32(give))
	}
	if a.floorBase == 0 {
		a.floorBase = a.base
	}
	a.floorAt = time.Now().UnixNano()
	a.base = f
	if a.win < f {
		a.win = f
	}
	a.grown.Store(a.extra > 0)
	a.pendingUPD = true // the next read tells the sender
	heavyFloorN.Add(1)
}
