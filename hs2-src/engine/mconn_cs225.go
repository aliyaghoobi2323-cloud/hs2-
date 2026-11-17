package engine

import "sync/atomic"

// cs225: the call's copy sub went on the primary's path (lab2 cs224 N2).
//
// Call 333dd5173fc99e8d lost sub 0 (path 56.1) to rst_a at 23:42:28; the lite
// upgrade (mconn.go, len(live) == 1) then opened its copy sub 12 with
// HS2_MCONN_CALL_AVOID's placement (pickPlaneCall: the least loaded light link
// with no download and no other copy). The avoid list names only the live
// sub's LINK, so the pick took another link on the SAME path (54.1) as primary
// sub 1, and the call rode one path, both copies, for 5 minutes. bh_e120
// blackholed 54.1 at 23:47:28: both subs went dark together, Kharej's darkcall
// needed 1.4 s to call the primary dark and resent 204 KB onto a fresh sub,
// call stall 2.4 s. A copy on any other path would have carried it through.
// HS2_DUP_PATHS (dup_paths.go) has the path rule, but it is off by default
// and pickPlaneCall's own branch never reaches pickPlaneLockedFP anyway.
//
// HS2_CS225_CALLPATH (default on; =0 gives back cs224): pickPlaneCall's
// call placement first takes, among the links its filter accepts, one on a
// healthy path (dupPathsCoreLocked: at least half its live links are not
// degraded / suspect / draining, the balancer has not cooled it) that none of
// the avoided links (the live subs' and the dead subs' links) ride; none: the
// old pick. The same branch serves the upgrade (second sub), the pair subs
// (callMoreSubsLocked) and the raw upgrade (mconn_raw.go).
var cs225CallPath atomic.Bool

func init() { cs225CallPath.Store(envBoolDefault("HS2_CS225_CALLPATH", true)) }

var cs225Stat struct {
	off  atomic.Uint64 // call subs placed on another path than the others
	same atomic.Uint64 // call subs left on one of their paths (no other path)
}

// cs225OffPathLocked (m.mu held): the filter ok narrowed to links on a
// healthy path none of avoid's links ride; nil when there is no such path
// (or the key is off).
func (m *LinkManager) cs225OffPathLocked(avoid []any, ok func(*managedLink) bool) func(*managedLink) bool {
	if !cs225CallPath.Load() {
		return nil
	}
	_, healthy := m.dupPathsCoreLocked(avoid)
	if healthy == nil {
		return nil
	}
	return func(ml *managedLink) bool {
		return healthy[mlPath(ml)] && !m.pathCooledLocked(ml) && !cs226LowLink(ml.link) && (ok == nil || ok(ml))
	}
}
