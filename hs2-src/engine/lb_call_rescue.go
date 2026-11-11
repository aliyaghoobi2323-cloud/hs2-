package engine

import (
	"sync/atomic"
	"time"
)

// A call off a lowered path (HS2_LB_CALL_RESCUE, edge, with HS2_LB_RESCUE;
// docs/reality-architecture.md 8.10).
//
// Lab (8.9, scenarios c and k): in both arms some runs had the call at
// 650-1150 ms in every second from the cut to the end of the run. The call's
// subs all sat on path B when it was lowered, and nothing moves a call: the
// rescue (lb_rescue.go) skips call-like connections by design, lite closes
// only idle second subs, and HS2_MCONN_CALL's pair subs are opened only up to
// mconnCallSubs. So, on Iran, for a call-like lite connection (callOn /
// callUp, or call-like within callFreeTTL):
//   - every live sub on a lowered or cut path: open one fresh sub with the
//     call's placement (HS2_MCONN_CALL_AVOID's filter: light, no download,
//     no other copy, not degraded) and, new, off every lowered link; at most
//     once per callRescueEvery per connection;
//   - once some live sub is off the lowered links and has carried a byte
//     from the peer, give up the subs on lowered links through the close
//     handshake (HS2_MCONN_CLOSE_HS: no byte rides only there). The good sub
//     is never closed, so a connection is never left with no sub; without
//     the handshake nothing is closed (a copy on B costs nothing: the
//     receiver reads the first copy).
//   - a call's own upgrade (one sub, lite's call rule) is placed off the
//     lowered links too, and passes the duplication budget when its one sub
//     is on a lowered link (rescueAllow's rate bound, as the light rescue).
var lbCallRescue bool // loadEnvKeys: HS2_LB_CALL_RESCUE

const callRescueEvery = time.Second

// callRescueOpened / callRescueMoved: fresh subs opened for calls all on
// lowered links; subs on lowered links given up once a call had a good one.
var callRescueOpened, callRescueMoved atomic.Int64

// callRescueMark in an avoid list asks the opener for the call placement off
// every lowered link.
type callRescueMark struct{}

// callRescueOnLocked: the key is on and m is a call (Iran, lite).
func (m *mConn) callRescueOnLocked(now time.Time) bool {
	return lbCallRescue && lbRescue && m.iran && m.lite &&
		(m.callOn || m.callUp || (!m.callLikeAt.IsZero() && now.Sub(m.callLikeAt) < callFreeTTL))
}

// callRescueAvoidLocked adds the mark for a call's sub.
func (m *mConn) callRescueAvoidLocked(avoid []any, now time.Time) []any {
	if m.callRescueOnLocked(now) {
		return append(avoid, callRescueMark{})
	}
	return avoid
}

// callRescueSingleLocked: a call whose one sub is on a lowered link may
// upgrade past the budget (rate-bounded).
func (m *mConn) callRescueSingleLocked(live *msub, now time.Time) bool {
	return m.callRescueOnLocked(now) && rescueLowKey(live.key) && rescueAllow(now)
}

// callRescueLocked (Iran's manager, lite, two live subs or more, under
// m.mu): opens a fresh sub when every live sub is on a lowered link, and
// once a good one carries data gives up the lowered ones. true: subs were
// given up (the caller refreshes the primary).
func (m *mConn) callRescueLocked(live []*msub, now time.Time) bool {
	if !m.callRescueOnLocked(now) || m.peerFin || m.wclosed || m.handshaking {
		return false
	}
	var bad []*msub
	good := false
	for _, s := range live {
		if s.retired || s.dead || s.closeHS {
			continue
		}
		if rescueLowKey(s.key) {
			bad = append(bad, s)
		} else if s.lastRecv.Load() != 0 {
			good = true // off the lowered links and the peer's bytes arrive on it
		}
	}
	if len(bad) == 0 {
		return false
	}
	if !good {
		if m.lightOpening || now.Before(m.lightRetryAt) || now.Before(m.callRescueAt) || m.hasFreshGoodLocked() {
			return false
		}
		m.callRescueAt = now.Add(callRescueEvery)
		m.lightOpening = true
		avoid := make([]any, 0, len(live)+len(m.deadKeys)+1)
		for _, s := range live {
			avoid = append(avoid, s.key)
		}
		avoid = append(append(avoid, m.deadKeys...), callRescueMark{})
		callRescueOpened.Add(1)
		m.tr.ev("call: every sub on a lowered path: open a fresh sub off it")
		go m.openLight(avoid)
		return false
	}
	if !mconnCloseHS.Load() {
		return false
	}
	gave := false
	for _, s := range bad {
		if subCloseHS(s.rw) {
			m.closeReqLocked(s)
			callRescueMoved.Add(1)
			gave = true
		}
	}
	return gave
}

// hasFreshGoodLocked: a live sub off the lowered links that has read nothing
// yet (just opened): the rescue waits for it rather than open another.
func (m *mConn) hasFreshGoodLocked() bool {
	for _, s := range m.subs {
		if !s.retired && !s.dead && !s.closeHS && !rescueLowKey(s.key) && s.lastRecv.Load() == 0 {
			return true
		}
	}
	return false
}

// takeCallRescueMark removes the mark from avoid; true if it was there.
func takeCallRescueMark(avoid []any) ([]any, bool) {
	for i, a := range avoid {
		if _, ok := a.(callRescueMark); ok {
			return append(avoid[:i:i], avoid[i+1:]...), true
		}
	}
	return avoid, false
}

// pickCallRescueLocked (under m.mu): the call placement off every lowered
// link, then any light link off them; nil when there is none.
func (m *LinkManager) pickCallRescueLocked(avoid []any, pool *peerInfo) *managedLink {
	if ml := m.pickPlaneTiers(planeLight, avoid, pool, func(ml *managedLink) bool {
		return !rescueLowKey(ml.link) && ml.heavy == 0 && !stripeOn(ml) && !ml.degraded && !ml.draining && !ml.suspect && linkCopies(ml.link) == 0
	}); ml != nil {
		return ml
	}
	return m.pickPlaneTiers(planeLight, avoid, pool, func(ml *managedLink) bool {
		return !rescueLowKey(ml.link)
	})
}
