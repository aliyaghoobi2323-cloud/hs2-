package engine

import (
	"fmt"
	"sync/atomic"
)

// cs286: a call's copy sub never shares the primary's path while another
// path exists (HS2_CS286_CALLAPART, default on; =0 gives back b280).
//
// lab1 sk280 SN4 (call a64d7c2d, 11 s stall): lb auto2's late "cut" verdict
// on path 52.1 retired 47 links, among them the call's copy sub; the call
// upgrade then put the new copy on 192.168.53.1, the primary's path. cs229's
// call counters stayed at "call kept 1 repicked 0 nocand 0": the call
// placement's light pick (no download, no stripe, no other copy, not
// degraded) found nothing during the drain, so pickPlaneCall fell back to
// the plain pickPlane, which has no call path rule; and cs229/cs225 only
// re-pick onto "healthy" paths (not cooled, not lowered, half the links
// good), which a cut wave can empty too. The 130-136 s cut of the primary's
// path then darkened both subs with no switch target.
//
// The guard runs on every placement of a call sub (callAvoidMark: upgrade,
// pair subs, raw upgrade of a call; callRescueMark): when the chosen link
// rides a path one of the avoided (live/dead sub) links rides, it re-picks on
// another path, best first: light and not lowered/cooled; not lowered/cooled;
// lowered or cooled but not degraded; degraded. Never draining, retiring or
// suspect. A lowered other path still beats both copies on one path. When
// every other path is also an avoided one (e.g. the dead copy's), a second
// round only keeps off the chosen link's own path. No other path: the pick
// stays (nocand).
var cs286Apart atomic.Bool

func init() { cs286Apart.Store(envBoolDefault("HS2_CS286_CALLAPART", true)) }

var cs286Stat struct {
	kept   atomic.Uint64 // pick already on a path no other sub rides
	moved  atomic.Uint64 // pick shared a path: re-picked on another path
	moveLo atomic.Uint64 // of moved: onto a lowered/cooled/degraded path
	nocand atomic.Uint64 // shared a path, no other path at all
}

func cs286LogPart() string {
	return fmt.Sprintf(" · cs286 kept %d moved %d (low %d) nocand %d",
		cs286Stat.kept.Load(), cs286Stat.moved.Load(), cs286Stat.moveLo.Load(), cs286Stat.nocand.Load())
}

// cs286SkipLocked (m.mu held): the paths the alive avoided links ride.
func (m *LinkManager) cs286SkipLocked(avoid []any) map[string]bool {
	var skip map[string]bool
	for _, ml := range m.links {
		path := mlPath(ml)
		if path == "" || !ml.link.Alive() || !avoided(ml.link, avoid) {
			continue
		}
		if skip == nil {
			skip = map[string]bool{}
		}
		skip[path] = true
	}
	return skip
}

// cs286ApartLocked (m.mu held): ml, or a re-pick on a path none of the
// call's other subs ride.
func (m *LinkManager) cs286ApartLocked(p mPlane, avoid []any, pool *peerInfo, ml *managedLink) *managedLink {
	if ml == nil || !cs286Apart.Load() {
		return ml
	}
	skip := m.cs286SkipLocked(avoid)
	if skip == nil || !skip[mlPath(ml)] {
		cs286Stat.kept.Add(1)
		return ml
	}
	if c := m.cs286PickLocked(p, avoid, pool, func(path string) bool { return !skip[path] }); c != nil {
		return c
	}
	own := mlPath(ml)
	if c := m.cs286PickLocked(p, avoid, pool, func(path string) bool { return path != own }); c != nil {
		return c
	}
	cs286Stat.nocand.Add(1)
	return ml
}

func (m *LinkManager) cs286PickLocked(p mPlane, avoid []any, pool *peerInfo, pathOK func(string) bool) *managedLink {
	base := func(c *managedLink) bool {
		path := mlPath(c)
		return path != "" && pathOK(path) && !c.draining && !c.retiring && !c.suspect
	}
	low := func(c *managedLink) bool { return rescueLowKey(c.link) || m.pathCooledLocked(c) }
	tiers := []func(*managedLink) bool{
		func(c *managedLink) bool {
			return base(c) && !c.degraded && !low(c) && c.heavy == 0 && !stripeOn(c)
		},
		func(c *managedLink) bool { return base(c) && !c.degraded && !low(c) },
		func(c *managedLink) bool { return base(c) && !c.degraded },
		base,
	}
	for i, ok := range tiers {
		if c := m.pickPlaneTiers(p, avoid, pool, ok); c != nil {
			cs286Stat.moved.Add(1)
			if i >= 2 {
				cs286Stat.moveLo.Add(1)
			}
			return c
		}
	}
	return nil
}
