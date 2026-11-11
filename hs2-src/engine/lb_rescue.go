package engine

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Light-user rescue off a lowered path (HS2_LB_RESCUE=1, edge, with auto2's
// steering and HS2_MCONN=lite).
//
// Phase 4c (docs/reality-architecture.md 8.4): with path B going bad the
// crowd's whole-run p99 was 1.2-1.5 s, and the damage is the window BEFORE
// the cut, when a third of the crowd sits on B. Closing a light user's
// stream to move it (HS2_LB_EVAC) cannot fix that window without resets: a
// connection whose only sub is on B dies with it (the 4 crowd resets of one
// EVAC run were exactly such single-sub connections; a closed sub that had a
// live copy elsewhere survived). Nothing on the wire tells a request boundary
// from a pause inside one, so no idle signal makes that close safe.
//
// Rescue moves nothing and closes nothing: a lite connection with ONE live
// sub on a link of a path the dialing side reports lowered to rescueFactor
// or less (or cut) gets lite's own upgrade to dual (one light second sub,
// after mLiteAfter, by lite's own light test) even when the duplication
// budget (liteBud: 5% of the bytes) refuses it — the budget is what leaves
// part of a big crowd single — with its copy placed away from that link by
// the usual steering (the lowered path has a small weight). From then on every byte goes both ways and each side reads the
// first copy: the user is served by the good path at once, with no reconnect
// and no reset. Bounds:
//   - light only: lite's own test (under mDupMaxRate both ways), and not
//     heavy, not copying off (dupOff), not a call (callUp / callOn) —
//     downloads, uploads and calls are untouched;
//   - rescuePerSec upgrades a second over the process (a crowd of thousands
//     gets its copies over seconds, a few tens of KB/s each, never a burst);
//   - nothing is marked unless some live path is NOT lowered (every path
//     bad: there is nowhere better to put a copy);
//   - the lowered set is recomputed every steering tick from the dialing
//     side's fresh report: a path that recovers leaves it; the copies then
//     follow lite's own rules (an idle second sub closes after mLiteIdle).
//
// Default on (edge, envdef). Lab (reality phase 4d, docs/reality-architecture.md
// 8.5; B +80 ms 3% corrupt from t=15 s): crowd p99 over the run 556 -> 385 ms
// (all-tls 498 -> 320), p95 312 -> 180, after the cut 526 -> 283 ms; ~250-300
// copy subs moved off B a run (the budget part did not bind there); 0 crowd
// failures in 12 runs, heavy unchanged; equal paths unchanged by median.
var lbRescue bool // loadEnvKeys: HS2_LB_RESCUE

// lbRescueRaw (HS2_LB_RESCUE_RAW=1, edge, default off, with HS2_LB_RESCUE):
// the single-sub rescue also for a connection whose first sub is still raw
// (capMConnRaw, mconn_raw.go) — nearly every single-sub connection of a
// crowd. Without it the single-sub part reaches only connections that were
// dual once (lab, budget forced binding: 0-4 copies past the budget a run;
// with it 1130-1215 and crowd p95 586 -> 225 ms). Off by default: the raw
// upgrade's handshake then has to finish over the lowered path (each side
// reads the other's raw bytes up to the boundary there), it can take
// seconds, and a sub dying during it ends the connection by design — 14-30
// crowd failures a run of 3000 (docs/reality-architecture.md 8.8).
var lbRescueRaw bool // loadEnvKeys: HS2_LB_RESCUE_RAW

var (
	rescueFactor = 0.25
	rescuePerSec = int64(100) // HS2_LB_RESCUE_PER_SEC (lb_rescue_callfree.go); was 400
	rescueSec    atomic.Int64 // the current second (unix)
	rescueN      atomic.Int64 // upgrades in it
	// rescueTotal / rescueRefused: upgrades done / refused by the rate bound.
	rescueTotal, rescueRefused atomic.Int64
	// rescueDual: copy subs given up by dual connections all on lowered paths.
	rescueDual atomic.Int64
	// rescueLow: links (Link: a sub's key) on a lowered or cut path, set by each
	// LinkManager's steering tick for its own links (rescueMark).
	rescueLow sync.Map
)

// rescueMark records which of a manager's links are on a lowered path: low
// the links to mark this tick, prev those it marked last time (returned
// updated). Links that left (dead, recovered path) are unmarked.
func rescueMark(prev map[any]bool, low []any) map[any]bool {
	next := make(map[any]bool, len(low))
	for _, l := range low {
		next[l] = true
		rescueLow.Store(l, true)
	}
	for l := range prev {
		if !next[l] {
			rescueLow.Delete(l)
		}
	}
	return next
}

// rescueLowKey reports whether a sub's key (its Link) is on a lowered path.
func rescueLowKey(key any) bool {
	if key == nil {
		return false
	}
	_, low := rescueLow.Load(key)
	return low
}

// rescueAllow takes one upgrade from the per-second bound.
func rescueAllow(now time.Time) bool {
	sec := now.Unix()
	if old := rescueSec.Load(); old != sec && rescueSec.CompareAndSwap(old, sec) {
		rescueN.Store(0)
	}
	if rescueN.Add(1) > rescuePerSec {
		rescueRefused.Add(1)
		return false
	}
	rescueTotal.Add(1)
	return true
}

// rescueWantLocked: this single-sub lite connection, which lite's own rule
// already found light (liteWantDualLocked: under mDupMaxRate both ways over
// its window, not a call), has its one sub on a lowered path. Caller holds
// m.mu, on the Iran side in lite.
func (m *mConn) rescueWantLocked(live *msub) bool {
	if !lbRescue || m.heavy || m.dupOff || m.callUp || m.callOn || m.wantBulk || m.mode != planeLight {
		return false
	}
	return rescueLowKey(live.key)
}

// rescueDualLocked: a light dual connection with EVERY live sub on a
// lowered path (both placed there before the path went bad: about one in
// nine of a crowd with a third of the links on that path) gives up its copy
// sub through the close handshake (HS2_MCONN_CLOSE_HS: the exit stops
// sending on it and names its end offset, so no byte rides only there); the
// connection, single on the primary, then gets its copy again by lite's
// upgrade (rescueWantLocked), away from the lowered path. Returns the sub to
// close, or nil. Caller holds m.mu, Iran side, lite.
func (m *mConn) rescueDualLocked(live []*msub, p *msub, now time.Time) *msub {
	if !lbRescue || !mconnCloseHS.Load() || m.heavy || m.dupOff || m.callUp || m.callOn || m.wantBulk || m.mode != planeLight || p == nil || m.handshaking {
		return nil
	}
	var c *msub
	for _, s := range live {
		if !rescueLowKey(s.key) {
			return nil
		}
		if s != p && c == nil && !s.closeHS && subCloseHS(s.rw) {
			c = s
		}
	}
	if c == nil || !rescueAllow(now) {
		return nil
	}
	rescueDual.Add(1)
	m.tr.ev("lite: rescue — every sub on a lowered path: CLOSE-REQ on copy sub %d", c.idx)
	return c
}

// steerRescueLocked marks the links of paths lowered to rescueFactor or
// less (or cut) per the dialing side's fresh report. Caller holds m.mu.
func (m *LinkManager) steerRescueLocked(byPath map[string][]*managedLink) {
	st := m.steer
	if !lbRescue {
		return
	}
	var low []any
	good := false // some live path is not lowered: there is somewhere to go
	if st.active {
		for p, ls := range byPath {
			f, ok := st.repFactor[p]
			if !ok {
				continue
			}
			if !st.repCut[p] && f > rescueFactor {
				good = true
				continue
			}
			for _, ml := range ls {
				low = append(low, ml.link)
			}
		}
	}
	if !good {
		low = nil
	}
	if t := rescueTotal.Load(); len(low) > 0 && (len(st.rescued) == 0 || t != st.rescueLogged) {
		st.rescueLogged = t
		st.logs = append(st.logs, fmt.Sprintf("lb rescue: %d link(s) on lowered paths — copies given past the budget so far %d, copy subs moved off %d (refused by the rate bound %d, placed off call links %d)", len(low), t-rescueDual.Load(), rescueDual.Load(), rescueRefused.Load(), rescueCallFreeN.Load()))
	}
	st.rescued = rescueMark(st.rescued, low)
}
