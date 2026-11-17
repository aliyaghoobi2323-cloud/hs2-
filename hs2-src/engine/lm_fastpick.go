package engine

import "sync/atomic"

// HS2_LM_FASTPICK=1 (lk162, Iran = the edge; on there by default since fp185
// through envdef, =0 turns it off): a new user
// connection (pickHeld) and a new mconn sub (pickPlane) are placed under the
// pool's READ lock instead of its write lock.
//
// Why: at ~1000 new connections a second (sc161 storm, lab) 98.6% of all
// mutex wait on the edge was LinkManager.mu taken for writing by these two
// picks — a scan of up to 300 links (×4 passes ×3 tiers for a light sub) run
// one at a time, each holder preempted while every other open queued behind
// it: opens took 5–15 s while requests on open connections stayed ~100 ms.
// The scans only read the pool; the one thing the write lock bought was that
// choosing and counting a placement were one step, so N connections arriving
// together landed on N different links (Pick).
//
// How: the scan runs as before (the very same code: pickLockedFP,
// pickPlaneLockedFP), under RLock, so many run at once. The placement is
// counted in the link's own atomic fpPicks, and only if that count is still
// what the scan ranked the link with (compare-and-swap): a concurrent
// placement on the same link moved its key (load, burst pressure), so the
// scan runs again with the new count — the same order a serial pick would
// have seen. Placements elsewhere only make other links worse (more load),
// and releases are lock-free on both paths, so a successful CAS is a pick a
// serial Pick could have made. After fastPickTries lost races the old
// write-locked path decides (never worse than before). fpPicks is part of
// the link's picks everywhere they are read (curPicks) and is folded into
// picks/picksAll at the next sample, under the write lock, where no fast
// picker can run.
//
// Not on the fast path (the old write-locked one decides): a refill hold in
// progress (the hold's queue is shared state), and the rarer call / rescue
// placements (pickPlaneCall's marks).
var lmFastPick atomic.Bool // loadEnvKeys

// fastPickTries is how many lost races a fast pick takes before the write
// lock decides.
const fastPickTries = 4

// fastPickStat: diagnostics (status / tests).
var fastPickStat struct {
	held, plane atomic.Uint64 // placements made on the fast path
	retry       atomic.Uint64 // scans repeated after a lost race
	fallback    atomic.Uint64 // picks the write lock decided (races lost, hold on)
}

// curPicks is the link's placements since the last sample, fast ones
// included. Caller holds m.mu (read or write).
func (ml *managedLink) curPicks() int {
	return ml.picks + int(ml.fpPicks.Load())
}

// histPicks is the placements of the samples before the current one.
func (ml *managedLink) histPicks() int {
	n := 0
	for _, p := range ml.pickHist {
		n += p
	}
	return n
}

// foldFastPicksLocked moves the fast path's placements into picks and the
// steer's running total. Caller holds m.mu for WRITING.
func (ml *managedLink) foldFastPicksLocked() {
	if n := ml.fpPicks.Swap(0); n != 0 {
		ml.picks += int(n)
		ml.steer.picksAll += uint64(n)
	}
}

// placeFast counts a placement on ml if its fast-path count is still fp (the
// value the scan ranked it with). Caller holds m.mu for reading.
func (m *LinkManager) placeFast(ml *managedLink, fp int32) bool {
	if !ml.fpPicks.CompareAndSwap(fp, fp+1) {
		return false
	}
	ml.users.Add(1)
	m.users.Add(1)
	return true
}

// pickHeldFast is pickHeld under the read lock. done=false: the caller takes
// the write-locked path (a refill hold runs, or the races were lost).
func (m *LinkManager) pickHeldFast() (l Link, release func(), ok, done bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.hold.on {
		fastPickStat.fallback.Add(1)
		return nil, nil, false, false
	}
	for try := 0; try < fastPickTries; try++ {
		ml, fp := m.pickLockedFP(0)
		if ml == nil {
			return nil, nil, false, true // no link at all, as pickHeld
		}
		if m.placeFast(ml, fp) {
			fastPickStat.held.Add(1)
			return ml.link, m.releaseFor(ml), true, true
		}
		fastPickStat.retry.Add(1)
	}
	fastPickStat.fallback.Add(1)
	return nil, nil, false, false
}

// pickPlaneFast is pickPlane under the read lock (done=false: the caller
// takes the write-locked path).
func (m *LinkManager) pickPlaneFast(p mPlane, avoid []any, pool *peerInfo) (l Link, release func(), ok, done bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for try := 0; try < fastPickTries; try++ {
		ml, fp, dup := m.pickPlaneLockedFP(p, avoid, pool)
		if ml == nil {
			return nil, nil, false, true
		}
		if m.placeFast(ml, fp) {
			dup.count()
			if stripeLog && p == planeLight {
				stripeNoteLight(ml)
			}
			fastPickStat.plane.Add(1)
			return ml.link, m.releaseFor(ml), true, true
		}
		fastPickStat.retry.Add(1)
	}
	fastPickStat.fallback.Add(1)
	return nil, nil, false, false
}
