package engine

import (
	"fmt"
	"sync/atomic"
	"time"
)

// b274 (HS2_B274_REPAIR, default on): a call connection left with ONE live
// sub gets its second sub back at the next manager tick instead of waiting
// for the lite upgrade sampler.
//
// Lab1 b266e R1 (one-direction data cuts every 10 s): a call conn's second
// sub died 13 s after a txdark switch and the new one came only ~48 s later.
// With one sub the upgrade needs liteWantDualLocked: two call-like 1 s
// windows in a row whose busier direction stays within 2.5x of the previous
// one; the cuts (stalls, then resend bursts) keep resetting that streak.
// Meanwhile a data cut on the sole sub's path has no darkcall candidate
// (txstall, nocand) and the call stalls until the next cut ends (12.3 s).
//
// Rule (Iran's manager, lite, one live sub, no open in flight): when the
// conn was call-like within b274RecentTTL (5 s, callLikeAt; the copy stops when it
// drops to one sub, so callOn alone would forget it), want the second sub as
// a call upgrade: the call budget still applies (upgradeCappedLocked), and
// mReopenBackoff after a failed open still applies (lightRetryAt).
var b274RepairOn atomic.Bool

func init() { b274RepairOn.Store(envBoolDefault("HS2_B274_REPAIR", true)) }

// b274RecentTTL: how long after its last call-like window a one-sub conn
// still counts as a call for the repair.
const b274RecentTTL = callFreeTTL

var b274Stat struct {
	repair atomic.Uint64 // one-sub call conns that asked for their second sub
}

func b274LogPart() string {
	return fmt.Sprintf(" · b274 repair on %v asked %d", b274RepairOn.Load(), b274Stat.repair.Load())
}

// b274RepairWantLocked (under m.mu; the len(live) == 1 manager case, after
// liteWantDualLocked said no): true when the conn is a recent call that lost
// its second sub. Sets callUp so the call budget decides.
func (m *mConn) b274RepairWantLocked(now time.Time) bool {
	if !b274RepairOn.Load() || !mconnCall.Load() || !m.lite {
		return false
	}
	if m.callLikeAt.IsZero() || now.Sub(m.callLikeAt) >= b274RecentTTL {
		return false
	}
	m.callUp = true
	b274Stat.repair.Add(1)
	m.tr.ev("b274: one-sub call, reopen its second sub")
	return true
}
