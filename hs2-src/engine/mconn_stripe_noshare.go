package engine

import (
	"os"
	"sync/atomic"
)

// HS2_MCONN_STRIPE_UP_NOSHARE=1 (Iran; up57): an UPLOAD's stripe sub never
// takes a light user's link (HS2_MCONN_STRIPE_SHARE's shared pick); with
// none of its own (empty, set aside, or another direction's stripe link)
// the pick asks the pool for one (stripeAskLocked), which
// HS2_MCONN_STRIPE_FAST's reserve and instant target supply within a few
// hundred ms.
//
// Why: with crowd 3000 over 100-150 links every link carries users, and the
// fast doubling chain asks for 3-8 links at once: those the reserve could not
// give came from the shared pick (up to HS2_MCONN_STRIPE_SHARE_K = 8 per
// connection). A light user on such a link waits behind the upload's bytes
// in the same TCP stream (head-of-line), so its RTT does not show it in the
// path's median (the up57 lab: light links' srtt median +0-1 ms, p90 +1 ms)
// while the light probes' p99 rose 7-15 ms with FAST. A download keeps the
// shared pick (its subs' queue sits at the exit, its gate reads it).
var stripeUpNoShareOn atomic.Bool

func init() { stripeUpNoShareOn.Store(os.Getenv("HS2_MCONN_STRIPE_UP_NOSHARE") == "1") }

// stripeNoShareHint (in a stripe open's avoid list, never equal to a link):
// the sub is an upload's, no shared pick.
type stripeNoShareHint struct{}

// stripeNoShareHintLocked (Iran, under m.mu): mark an upload's stripe open.
func (m *mConn) stripeNoShareHintLocked(avoid []any) []any {
	if stripeUpNoShareOn.Load() && m.stGrowUp {
		avoid = append(avoid, stripeNoShareHint{})
	}
	return avoid
}

// stripeUpNoShare (pickStripe): a shared pick is refused for this sub.
func stripeUpNoShare(avoid []any) bool {
	for _, a := range avoid {
		if _, ok := a.(stripeNoShareHint); ok {
			stripeNoShareStat.Add(1)
			return true
		}
	}
	return false
}

// stripeNoShareStat: shared picks an upload's stripe was refused.
var stripeNoShareStat atomic.Uint64
