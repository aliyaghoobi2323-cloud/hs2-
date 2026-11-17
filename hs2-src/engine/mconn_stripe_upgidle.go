package engine

import (
	"os"
	"sync/atomic"
)

// HS2_MCONN_STRIPE_UPG_IDLE=1 (Iran; evac58, off by default): an UPLOAD's
// raw upgrade puts its new sub on an idle link — one set aside for the
// stripe (HS2_MCONN_STRIPE_FAST's reserve) or a free one, even past the
// keep-free rule (one link; the pool is asked for it again at once) —
// instead of a light user's busy link (HS2_MCONN_STRIPE_SHARE) or one
// another direction's stripe rides (HS2_MCONN_STRIPE_XDIR, then only the
// last resort).
//
// Why (production, tunnel 1): a single upload stuck at ~370 Mbit/s for ~4 s
// right after its raw upgrade, then ~1000. During the upgrade the raw first
// sub is gated (mconn_raw.go): the upload rides the new sub alone until the
// exit's UPGRADE comes back ON THAT SUB. The cross-direction pick puts it on a
// link a download's stripe rides: the exit's reply then waits behind the
// download's bytes queued in that link's send buffer (exit -> Iran, a deep
// queue under a heavy download), and the upload runs at one shared link's
// pace for the whole handshake; a shared light link's send queue (many
// users' downloads) does the same at production scale. With the key the
// upgrade's open carries stripeUpgHint; pickStripe then takes the reserve,
// else any free link, else the shared pick, else the cross-direction one. A
// download's upgrade and every later stripe sub are unchanged.
var stripeUpgIdle atomic.Bool

func init() { stripeUpgIdle.Store(os.Getenv("HS2_MCONN_STRIPE_UPG_IDLE") == "1") }

// stripeUpgHint (in a stripe open's avoid list, never equal to a link): the
// open is an upload's raw upgrade.
type stripeUpgHint struct{}

// stripeUpgHintLocked (Iran, rawTick, under m.mu): mark an upload's upgrade.
func (m *mConn) stripeUpgHintLocked(avoid []any) []any {
	if stripeUpgIdle.Load() && m.stGrowUp {
		avoid = append(avoid, stripeUpgHint{})
	}
	return avoid
}

// stripeUpgOf (pickStripe): the open is an upload's raw upgrade.
func stripeUpgOf(avoid []any) bool {
	for _, a := range avoid {
		if _, ok := a.(stripeUpgHint); ok {
			return true
		}
	}
	return false
}

// stripeUpgStat: upgrades that skipped the cross-direction pick first /
// still took it as the last resort.
var stripeUpgStat struct {
	idle     atomic.Uint64 // took a free link past the keep-free rule
	deferred atomic.Uint64
	lastXdir atomic.Uint64
}
