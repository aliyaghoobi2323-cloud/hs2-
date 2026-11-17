package engine

import "sync/atomic"

// HS2_MCONN_STRIPE_RES_GUARD=1 (dr118, Iran = the edge; off by default): a
// link the stripe holds — set aside for it (stripeAdoptLocked, the
// HS2_MCONN_STRIPE_FAST_RESERVE standing reserve) or carrying a stripe sub —
// is taken by a new light connection only when no other link can take it.
//
// Why (dr116 lab, arm L, d1 with HS2_MCONN_STRIPE_DL_FAST): the download's
// fast decision came at 1.6 s, but its first pick found no set-aside link
// ("stripe pick: none (free=98 empty=0)") and the first stripe sub came only
// at ~4.1 s. The reserve HAD been built (links 73-75 at 2.0 s, 102-104 at
// 4.2 s, 106/107 ...), but light users kept landing on it: pickLocked ranks
// a stripe link last by path, yet "pressed" ranks before path, and during a
// crowd's arrival every ordinary link counts as pressed by its recent picks
// (newPickKey) — the untouched set-aside link was the one unpressed link
// and won ("stripe light picks 5s: on-stripe=242", "newconn ... stripe=true
// users=1"). A set-aside link with a user no longer counts (pickStripe
// skips it), so the reserve was eaten as fast as the pool rebuilt it, and a
// light user sat on a link a stripe was about to fill (the tunnel-vision
// rule: a light user never behind a heavy one).
//
// With the key, in pickLocked (a plain TCP user) and planeKeyLocked (an
// mconn's light / call sub — the crowd's own picks, pickPlaneTiers):
//   - a stripe link's key is pressed (it ranks after every unpressed link,
//     then by path as before: stripeSteerRank, after all others);
//   - (pickLocked) under the refill hold's cap (limit > 0) a stripe link is skipped: the
//     waiter waits for a link with room, as the hold's bound already does
//     (pickByBytesLocked skips stripe links), and the plain pick (limit 0)
//     still takes it when it is the only link at all.
//
// The reserve then waits for the stripe: a fast download's (or upload's)
// first pick takes a set-aside link at the decision. No wire change.
var stripeResGuard atomic.Bool

// stripeResGuardStat: light picks that a held stripe link lost (diagnostics).
var stripeResGuardStat struct {
	skipped atomic.Uint64 // skipped under the hold's cap
}

// stripeGuardSkip (pickLocked): skip ml for a light pick under the hold's cap.
func stripeGuardSkip(ml *managedLink, limit int) bool {
	if limit <= 0 || !stripeResGuard.Load() || !stripeOn(ml) {
		return false
	}
	stripeResGuardStat.skipped.Add(1)
	return true
}

// stripeGuardPressed (pickLocked): a held stripe link ranks as pressed.
func stripeGuardPressed(ml *managedLink) bool {
	return stripeResGuard.Load() && stripeOn(ml)
}
