package engine

import (
	"sort"
	"time"
)

// id125 F4: HS2_RETIRE_KEEP_IDLE=1 (default on, edge, via envdef since rel62k;
// =0 turns it off) — a pool shrink
// does not close live idle user connections.
//
// Without it a connection on a RETIRING link is closed once it has moved
// nothing for drain_idle (310 s), and once the link has been retiring for
// retireForce (20 min) every connection that is not flowing is closed too. A
// retiring link is only a link that takes no new users, so this resets idle
// but live sockets (SSH, push channels, long-poll, IM keepalives, xray mux)
// for no gain but an earlier link close: production saw ~8.7k such closes a
// day on one edge.
//
// With the key, a retiring link keeps its idle and trickling connections; only
// connections idle for retireKeepIdleFor (HS2_RETIRE_KEEP_IDLE_MIN minutes,
// default 60; never below drain_idle) are reclaimed, and the 20-minute
// trickle force-close is skipped. The pool cannot grow without bound:
// reconcile brings retiring links back into service before it dials or
// accepts new ones, so live links stay at most the peak target. If the pool
// is still above max_links, the old rules apply to the surplus number of
// retiring links, the ones with the fewest open connections first. Cut-path
// drains (lb_cutdrain) and lb evac are path-quality moves and are unchanged.
var (
	retireKeepIdle    bool               // loadEnvKeys: HS2_RETIRE_KEEP_IDLE
	retireKeepIdleFor = 60 * time.Minute // loadEnvKeys: HS2_RETIRE_KEEP_IDLE_MIN
)

const retireKeepLogEvery = 10 * time.Minute

// retireStrictLocked returns, for the retiring links chosen for reclaim, the set
// that still follows the old rules (nil: none) when the key is on. The pool
// above max_links (up > max) puts its up-max retiring links with the fewest
// open connections there. Caller holds m.mu.
func (m *LinkManager) retireStrictLocked(reclaim []*managedLink, up int) map[*managedLink]bool {
	if m.max <= 0 || up <= m.max || len(reclaim) == 0 {
		return nil
	}
	over := up - m.max
	order := append([]*managedLink(nil), reclaim...)
	sort.SliceStable(order, func(i, j int) bool { return order[i].open < order[j].open })
	strict := make(map[*managedLink]bool, min(over, len(order)))
	for _, ml := range order[:min(over, len(order))] {
		strict[ml] = true
	}
	return strict
}

// retireKeepThreshold is the idle time after which a kept retiring link's
// connection is reclaimed.
func retireKeepThreshold(drainIdle time.Duration) time.Duration {
	if drainIdle <= 0 {
		return 0
	}
	return max(drainIdle, retireKeepIdleFor)
}
