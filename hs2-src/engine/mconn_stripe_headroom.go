package engine

import (
	"sync/atomic"
)

// HS2_MCONN_STRIPE_HEADROOM=PCT (dl186, BOTH sides; 0/unset: off): the
// stripe's own links may take the pool up to PCT % of max_links (at least 1)
// ABOVE max_links, only while a stripe holds, has set aside or still wants
// them. max_links stays the ceiling for everything else: the autopilot's
// target, the light users' pool and the drain/refill rules are unchanged.
//
// Why (dl184 trace, lab2 crowd 3000 over a pool at max_links 200, 9
// background downloads): a heavy download's growth found no link ("stripe
// pick: none (free=0 empty=0)") thousands of times per 10 min — every
// serving link carried a light user's primary or another stripe — and the
// pool's ask for a link (stripeWant, mconn_stripe_pool.go) was never dialed:
// stripeExtra's links were added to a target already at max_links and
// clamped away. A download then ran on its raw first sub's link alone: 400-
// 600 Mbit/s on a good link, 20-60 on a lossy one (bimodal tests).
//
// With the key the edge's target is min(autopilot + stripeExtra, max +
// min(headroom, stripeExtra)): the extras are exactly the links the stripe
// asked for or holds. A new link is set aside for the stripe on arrival
// (stripeAdoptLocked: heavy for every light pick and the TUN lane), so the
// extra links never carry a light user; when the stripe lets go stripeExtra
// drops, the target falls back to <= max_links and those empty links are
// the first retired (no users, nothing flowing). The exit (reverse) follows
// the edge's target and with the key lets it go the same headroom above its
// own max_links and the edge's reported one; an exit without the key clamps
// at its max as before (no extra link comes; nothing else changes).
//
// The configured max_links is therefore a soft cap for heavy flows: at most
// headroom TEMPORARY stripe-only links above it, gone when the downloads end.
var stripeHeadroomPct atomic.Int32

// stripeHeadroomPctMax bounds the key (a typo must not double the pool).
const stripeHeadroomPctMax = 50

// stripeHeadroom: the most stripe-only links over a ceiling of max (0: off).
func stripeHeadroom(ceil int) int {
	p := int(stripeHeadroomPct.Load())
	if p <= 0 || ceil <= 0 {
		return 0
	}
	return max(1, ceil*min(p, stripeHeadroomPctMax)/100)
}

// stripeHeadroomTarget (edge, decideTarget): the target with the stripe's
// links x on top of the autopilot's t, and how many of them are over max.
func stripeHeadroomTarget(t, x, ceil int) (target, over int) {
	target = min(t+x, ceil+min(stripeHeadroom(ceil), x))
	return target, max(0, target-ceil)
}

// stripeHeadroomStat: diagnostics (the largest over-max count seen, and the
// decisions that went over).
var stripeHeadroomStat struct {
	peak, overTicks atomic.Int64
}

// ceilLocked (edge): the pool's ceiling now — max_links plus the stripe's
// over-max links decided by the last decideTarget.
func (m *LinkManager) ceilLocked() int {
	return m.max + int(m.stripeOver.Load())
}

// exitHeadroomHi (exit, bounds): the ceiling hi (min of the two max_links)
// widened by the headroom (the edge asks above it only for stripe links).
func exitHeadroomHi(hi int) int {
	return hi + stripeHeadroom(hi)
}
