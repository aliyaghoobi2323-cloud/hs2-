package engine

import (
	"os"
	"sync/atomic"
)

// HS2_MCONN_STRIPE_XDIR=1 (Iran; with HS2_MCONN_STRIPE): a stripe sub may
// ride a link that only the OTHER direction's stripes ride.
//
// The busy pool (production: 12-14k users over ~155 links, the pool at
// max_links; the up54 lab: crowd 3000 on 160 links plus two background
// downloads) has no link without a user. The downloads that started first
// took every free link as their own stripe links (stripeAlone: no user on
// them) and grew past K to 20-24 subs each (HS2_MCONN_STRIPE_UP's growth to
// stripeKMaxUp); the share rule (HS2_MCONN_STRIPE_SHARE) stops at half the
// pool heavy or stripe. A single upload that came later found nothing:
// "stripe pick: none" on every try (lab, r31 and rel52: stripe_refused =
// stripe_pick_none = 77 in one run, no upload stripe sub at all), so it
// stayed on its first sub, a light link shared with ~30 users — 50-450
// Mbit/s, erratic, where an upload with its stripe makes 1-2 Gbit/s.
//
// A download's stripe link carries data only exit → Iran; Iran → exit it
// carries ACKs. An upload stripe sub on it uses the idle direction: no light
// user rides the link (stripeAlone), and no other sub of the same direction
// competes for its socket. So when the usual picks find nothing (no link set
// aside, no free link, before a light user's link of the share rule), the
// key places the sub on such a link: no stripe of the same direction on it,
// at least one of the other, no user besides the stripe subs, not pressed — the fewest
// other-direction subs first. The same holds the other way (a download
// stripe on an upload's stripe link).
//
// The direction is the one the connection grew for (stripeTickLocked: the
// faster direction of the last sample), carried to the pick as a
// stripeDirHint in the avoid list (never equal to a link). Every stripe pick
// with the key on counts its link's subs per direction (stUpN / stDnN).
// Off (the default): nothing changes, no hint, no count.
var stripeXDir = os.Getenv("HS2_MCONN_STRIPE_XDIR") == "1"

// stripeDirHint: the direction a stripe sub is opened for (in avoid).
type stripeDirHint struct{ up bool }

// stripeDirOf: the hint in avoid, if any.
func stripeDirOf(avoid []any) (stripeDirHint, bool) {
	for _, a := range avoid {
		if h, ok := a.(stripeDirHint); ok {
			return h, true
		}
	}
	return stripeDirHint{}, false
}

// stripeDirN: ml's counter for a sub of direction up (nil: no hint).
func stripeDirN(ml *managedLink, up, hinted bool) *atomic.Int32 {
	if !hinted {
		return nil
	}
	if up {
		return &ml.stUpN
	}
	return &ml.stDnN
}

// pickStripeXDirLocked (under m.mu): a link only the other direction's
// stripes ride, the fewest of them first; nil: none.
func (m *LinkManager) pickStripeXDirLocked(avoid []any, pool *peerInfo, up bool) *managedLink {
	var best *managedLink
	var bestN int32
	for _, ml := range m.links {
		if !ml.serving() || ml.pressed {
			continue
		}
		if m.gateInfo && ml.mtr != nil && !ml.mtr.infoDone.Load() || !linkMConn(ml, pool) {
			continue
		}
		same, other := ml.stDnN.Load(), ml.stUpN.Load()
		if ml.users.Load() > same+other {
			continue // a user rides it (placeLocked counts each stripe sub as one)
		}
		if up {
			same, other = other, same
		}
		if same > 0 || other <= 0 || !stripeOn(ml) || avoided(ml.link, avoid) {
			continue
		}
		if best == nil || other < bestN || other == bestN && ml.recent < best.recent {
			best, bestN = ml, other
		}
	}
	if best != nil {
		stripeXDirPicks.Add(1)
	}
	return best
}

// stripeXDirPicks: picks the key made (diagnostics, tests).
var stripeXDirPicks atomic.Int64
