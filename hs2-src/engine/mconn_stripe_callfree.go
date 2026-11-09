package engine

import (
	"os"
	"sync"
	"time"
)

// HS2_MCONN_STRIPE_CALLFREE=1 (off by default): a download's stripe stays off
// the links a call rides.
//
// The lab (3 Iran paths, one cut at 7% loss, +4 ms flap, crowd 3000, one
// download): HS2_LB_CUT_DRAIN alone and HS2_MCONN_STRIPE_QWIN_MS alone each
// kept the call's p99 (208 / 183 ms against 171), together they put it at
// 430 ms (4 runs, p=0.029). The drain retires the cut path's links, the
// stripe's subs on them too; the stripe — now wide, the QWIN gate open —
// grows back on the light links of the good paths (STRIPE_SHARE), which are
// the ones the call's two copies were placed on. The call copies avoid a
// stripe link only when they are placed; a stripe that comes to the call's
// link later puts the download's queue in front of the call's frames.
//
// With the key, each side marks the links of a connection copied as a call
// (callOn, refreshed every window it stays one; a mark lasts callFreeTTL):
//   - Iran's shared stripe pick (pickStripeSharedLocked) skips a marked link;
//   - the stripe sender (stripeSendLocked, either side) gives no chunk to a
//     stripe sub on a marked link (the primary keeps its chunks): the sub
//     stays open, unused, and takes chunks again once the call is over.
//
// Key off: nothing is marked and both checks are one atomic-free bool test.
var stripeCallFree = os.Getenv("HS2_MCONN_STRIPE_CALLFREE") == "1"

// callFreeTTL: a call link's mark outlives the last window that refreshed it
// by this much (a window is ~1 s; a short pause keeps the link free).
const callFreeTTL = 5 * time.Second

var callLinks struct {
	mu sync.Mutex
	at map[*linkMeter]time.Time // a link (its meter) -> the mark's expiry
}

// callMarkLocked (under m.mu; key on): m is copied as a call now — mark its
// live subs' links.
func (m *mConn) callMarkLocked(now time.Time) {
	if !stripeCallFree || !m.callOn {
		return
	}
	exp := now.Add(callFreeTTL)
	callLinks.mu.Lock()
	if callLinks.at == nil {
		callLinks.at = map[*linkMeter]time.Time{}
	}
	for _, s := range m.subs {
		if mtr := m.subMeterOf(s); !s.retired && !s.dead && mtr != nil {
			callLinks.at[mtr] = exp
		}
	}
	if len(callLinks.at) > 64 {
		for k, t := range callLinks.at {
			if now.After(t) {
				delete(callLinks.at, k)
			}
		}
	}
	callLinks.mu.Unlock()
}

// callLinkOn: the link of meter mtr carries a call's sub now (key on).
func callLinkOn(mtr *linkMeter, now time.Time) bool {
	if !stripeCallFree || mtr == nil {
		return false
	}
	callLinks.mu.Lock()
	t, ok := callLinks.at[mtr]
	callLinks.mu.Unlock()
	return ok && now.Before(t)
}
