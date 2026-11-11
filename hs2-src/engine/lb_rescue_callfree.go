package engine

import (
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Rescue copies off a call's links (HS2_LB_RESCUE_CALLFREE, edge, default on
// with HS2_LB_RESCUE; docs/reality-architecture.md 8.9).
//
// Lab (all-tls, B bad at 15 s, n=6+6): with the rescue the call's p99 was
// 367 ms (170-980) against 207 (171-424) without it. When a path is lowered
// the rescue opens ~1100 copy subs and sends ~300 CLOSE-REQs within 2-3 s
// onto the good paths' light links — some of them the call's — and the
// call's frames queue behind that burst. So:
//   - Iran marks the links a call-like connection rides (refreshed each
//     window it stays call-like, a mark lasts callFreeTTL);
//   - a rescue copy (the one sub of a light connection on a lowered link)
//     is placed on an unmarked, not-lowered light link; only when there is
//     none, the usual light pick;
//   - rescuePerSec (HS2_LB_RESCUE_PER_SEC, default 100, was 400) spreads the
//     burst over a few more seconds.
var lbRescueCallFree bool // loadEnvKeys: HS2_LB_RESCUE_CALLFREE

// rescueCallFreeN: rescue copies placed off the call links and lowered paths.
var rescueCallFreeN atomic.Int64

func init() {
	if v, err := strconv.Atoi(os.Getenv("HS2_LB_RESCUE_PER_SEC")); err == nil && v > 0 {
		rescuePerSec = int64(v)
	}
}

// rescueAvoidMark in an avoid list asks the opener for the rescue placement.
type rescueAvoidMark struct{}

var rescueCallLinks struct {
	mu sync.Mutex
	at map[*linkMeter]time.Time
}

// rescueCallMarkLocked (under m.mu, Iran): m is call-like now — mark its live
// subs' links.
func (m *mConn) rescueCallMarkLocked(now time.Time, like bool) {
	if !lbRescue || !lbRescueCallFree || !m.iran || !(like || m.callOn || m.callUp) {
		return
	}
	exp := now.Add(callFreeTTL)
	rescueCallLinks.mu.Lock()
	if rescueCallLinks.at == nil {
		rescueCallLinks.at = map[*linkMeter]time.Time{}
	}
	for _, s := range m.subs {
		if mtr := m.subMeterOf(s); !s.retired && !s.dead && mtr != nil {
			rescueCallLinks.at[mtr] = exp
		}
	}
	if len(rescueCallLinks.at) > 64 {
		for k, t := range rescueCallLinks.at {
			if now.After(t) {
				delete(rescueCallLinks.at, k)
			}
		}
	}
	rescueCallLinks.mu.Unlock()
}

// rescueCallLinkOn: the link of meter mtr carries a call-like sub now.
func rescueCallLinkOn(mtr *linkMeter, now time.Time) bool {
	if mtr == nil {
		return false
	}
	rescueCallLinks.mu.Lock()
	t, ok := rescueCallLinks.at[mtr]
	rescueCallLinks.mu.Unlock()
	return ok && now.Before(t)
}

// rescueAvoidLocked adds the mark when this upgrade is a rescue copy: the one
// live sub (key) is on a lowered link.
func (m *mConn) rescueAvoidLocked(avoid []any, key any) []any {
	if lbRescue && lbRescueCallFree && rescueLowKey(key) && !m.callUp && !m.callOn {
		return append(avoid, rescueAvoidMark{})
	}
	return avoid
}

// takeRescueMark removes the mark from avoid; true if it was there.
func takeRescueMark(avoid []any) ([]any, bool) {
	for i, a := range avoid {
		if _, ok := a.(rescueAvoidMark); ok {
			return append(avoid[:i:i], avoid[i+1:]...), true
		}
	}
	return avoid, false
}

// pickRescueLocked (under m.mu): a light link with no call mark, not lowered;
// nil when there is none.
func (m *LinkManager) pickRescueLocked(avoid []any, pool *peerInfo) *managedLink {
	now := time.Now()
	return m.pickPlaneTiers(planeLight, avoid, pool, func(ml *managedLink) bool {
		return !rescueLowKey(ml.link) && !rescueCallLinkOn(ml.mtr, now)
	})
}
