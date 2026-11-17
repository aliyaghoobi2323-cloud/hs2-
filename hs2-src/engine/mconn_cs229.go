package engine

import (
	"fmt"
	"sync/atomic"
)

// cs229: minimal placement (HS2_CS229_MINPLACE, default on).
//
// cs228's lab2 A/B: the lite second-sub placement (HS2_CS228_LITEPLACE) cost
// ~+5-6 ms crowd p99 even with its RTT/loss gate, and cs225's call/raw
// placement (HS2_CS225_CALLPATH) about as much on lab3. Both change EVERY
// placement: the pick runs over a path-filtered candidate set (cs228: other
// path + no download first), so even subs the plain pick would have put on
// another path anyway land on other links, and copies concentrate on the
// few light links of the other paths.
//
// MINPLACE runs the original pick unchanged. Only when its link rides a path
// one of the conn's live subs rides (dupPathsCoreLocked's skip set) is the
// pick repeated with the same scoring (pickPlaneTiers) narrowed to links on
// another healthy path (not cooled, not lowered/cut by the balancer); lite
// second subs also keep the original's no-download property and pass cs228's
// RTT/loss gate. No such link: the original pick stays.
//
// Precedence: on, it replaces HS2_CS228_LITEPLACE for lite second subs (the
// lite part still needs the HS2_CS226_LITEPATH master) and HS2_CS225_CALLPATH
// for call subs (callAvoidMark: call upgrades, pair subs and the raw upgrade
// of a conn that is a call; non-call raw upgrades take the lite rule).
var cs229MinPlaceKey atomic.Bool

func init() { cs229MinPlaceKey.Store(envBoolDefault("HS2_CS229_MINPLACE", true)) }

func cs229MinPlace() bool { return cs229MinPlaceKey.Load() }

var cs229Stat struct {
	kept     atomic.Uint64 // original pick already off the other subs' paths
	repicked atomic.Uint64 // original pick shared a path: re-picked on another path
	nocand   atomic.Uint64 // shared a path, no candidate elsewhere: original kept
	callKept atomic.Uint64
	callRep  atomic.Uint64
	callNo   atomic.Uint64
}

func cs229LogPart() string {
	return fmt.Sprintf(" · cs229 lite kept %d repicked %d nocand %d call kept %d repicked %d nocand %d",
		cs229Stat.kept.Load(), cs229Stat.repicked.Load(), cs229Stat.nocand.Load(),
		cs229Stat.callKept.Load(), cs229Stat.callRep.Load(), cs229Stat.callNo.Load()) + st244LogPart()
}

// cs229OtherLocked (m.mu held): the filter for a re-pick off skip's paths
// onto healthy ones, also requiring ok.
func (m *LinkManager) cs229OtherLocked(healthy map[string]bool, ok func(*managedLink) bool) func(*managedLink) bool {
	return func(ml *managedLink) bool {
		return healthy[mlPath(ml)] && !ml.degraded && !ml.draining && !ml.suspect && !m.pathCooledLocked(ml) &&
			!cs226LowLink(ml.link) && (ok == nil || ok(ml))
	}
}

// cs229LiteLocked is pickPlaneLockedFP's light branch under MINPLACE.
func (m *LinkManager) cs229LiteLocked(p mPlane, avoid []any, pool *peerInfo) (*managedLink, int32, dupNote) {
	ml, fp := m.pickPlaneTiersFP(p, avoid, pool, nil)
	if ml == nil {
		return nil, 0, dupNone
	}
	skip, healthy := m.dupPathsCoreLocked(avoid)
	if skip == nil {
		return ml, fp, dupNone // no live sub to keep apart from
	}
	if !skip[mlPath(ml)] {
		cs229Stat.kept.Add(1)
		return ml, fp, dupOther
	}
	if healthy != nil {
		healthy = m.cs228PlaceQualLocked(skip, healthy)
	}
	if len(healthy) > 0 {
		var ok func(*managedLink) bool
		if ml.heavy == 0 && !stripeOn(ml) {
			ok = func(c *managedLink) bool { return c.heavy == 0 && !stripeOn(c) }
		}
		if ml2, fp2 := m.pickPlaneTiersFP(p, avoid, pool, m.cs229OtherLocked(healthy, ok)); ml2 != nil {
			cs229Stat.repicked.Add(1)
			return ml2, fp2, dupOther
		}
	}
	cs229Stat.nocand.Add(1)
	return ml, fp, dupSame
}

// cs229CallLocked: pickPlaneCall's placement under MINPLACE; ml is the
// original (light) pick. Returns the link to place.
func (m *LinkManager) cs229CallLocked(p mPlane, avoid []any, pool *peerInfo, ml *managedLink, light func(*managedLink) bool) *managedLink {
	skip, healthy := m.dupPathsCoreLocked(avoid)
	if skip == nil || !skip[mlPath(ml)] {
		cs229Stat.callKept.Add(1)
		return ml
	}
	if healthy != nil {
		if ml2 := m.pickPlaneTiers(p, avoid, pool, m.cs229OtherLocked(healthy, light)); ml2 != nil {
			cs229Stat.callRep.Add(1)
			return ml2
		}
		if ml2 := m.pickPlaneTiers(p, avoid, pool, m.cs229OtherLocked(healthy, nil)); ml2 != nil {
			cs229Stat.callRep.Add(1)
			return ml2
		}
	}
	cs229Stat.callNo.Add(1)
	return ml
}
