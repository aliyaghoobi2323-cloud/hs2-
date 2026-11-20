package engine

import (
	"log"
	"sync/atomic"
	"time"
)

// HS2_MCONN_STRIPE_DL_SHARE=1 (dl184, Iran = the edge that opens every sub):
// a download's first stripe subs may share a link that already carries
// another connection's stripe sub (and no light user) when the pool has no
// free link for it.
//
// Why (lab2, rel67b, crowd 3000 over a pool at max_links 200, 9 background
// downloads of 20-200 Mbit/s): single 12 s test downloads were bimodal,
// ~20-40 Mbit/s or ~150-450 Mbit/s. A slow one is a download whose first
// sub sits on a lossy link: it never reaches stripeDlFastRate (80 Mbit/s),
// so it takes the usual rule (stripeHotTicks 1 s ticks), and every open it
// then tries finds no link: "stripe pick: none (free=0 empty=0 ...)" —
// every serving link carries a light user (crowd) or another stripe, and the
// pool, already at max_links, cannot dial the link it asks for. The
// background downloads keep their stripe links for their whole life
// (stripeWarm keeps a stripe above 20 Mbit/s), at 5-30 Mbit/s per sub: the
// pool's heavy links are mostly idle capacity the new download cannot use.
// The dl184 trace (mconn_stripe_dltrace.go) showed the slow downloads'
// opens failing every 2 s (openfail) until the end.
//
// With the key, after the free / set-aside / shared picks found nothing for
// a DOWNLOAD's growth while it has fewer than stripeDlShareMax subs:
//   - a link whose every user is a stripe sub or a lite copy-only sub (no
//     light user's primary: they keep off stripe links, so light users never
//     queue behind it; a copy held behind the stripe costs its user nothing
//     while its primary delivers first, as for HS2_MCONN_STRIPE_SHARE_COPIES;
//     never a link a call rides), at most stripeDlShareHolders stripe subs
//     on it already, serving, not pressed, not one this connection uses;
//   - a path not cut first, then the lowest rate10 (the most spare room).
//
// Fair sharing: only up to stripeDlShareMax subs a connection this way, so
// the heavy links are spread over the downloads instead of one taking all;
// a download past it grows only on free links as before. No wire change;
// no new link (the pool size is unchanged at max_links).
var stripeDlShare atomic.Bool

const (
	// stripeDlShareMax: a connection's subs (first included) up to which a
	// growth may take a shared heavy link.
	stripeDlShareMax = 4
	// stripeDlShareHolders: stripe subs a link may already carry.
	stripeDlShareHolders = 1
)

// stripeDlShareStat: diagnostics.
var stripeDlShareStat struct {
	picks, none atomic.Uint64
}

// stripeDlShareHint (in a stripe open's avoid list, never equal to a link):
// the open is a download's that may take a shared heavy link.
type stripeDlShareHint struct{}

// stripeDlShareHintLocked (Iran, under m.mu, a growth decided): mark a
// download's open while the connection is under stripeDlShareMax subs.
func (m *mConn) stripeDlShareHintLocked(avoid []any) []any {
	if stripeDlShare.Load() && m.iran && !m.stGrowUp && 1+m.liveStripeLocked() < stripeDlShareMax {
		avoid = append(avoid, stripeDlShareHint{})
	}
	return avoid
}

func stripeDlShareOf(avoid []any) bool {
	for _, a := range avoid {
		if _, ok := a.(stripeDlShareHint); ok {
			return true
		}
	}
	return false
}

// pickStripeDlShareLocked (pickStripeWhy, under m.mu, nothing else found):
// the heavy link a download's stripe sub may share (nil: none).
func (m *LinkManager) pickStripeDlShareLocked(avoid []any, pool *peerInfo) *managedLink {
	held := make(map[*managedLink]int)
	stripeHeld.mu.Lock()
	for ml, h := range stripeHeld.n {
		held[ml] = h
	}
	stripeHeld.mu.Unlock()
	var best *managedLink
	bestCut := false
	var full, light, other int // why the stripe links were passed over (diagnostics)
	now := time.Now()
	for _, ml := range m.links {
		h := held[ml]
		if h <= 0 {
			continue
		}
		if h > stripeDlShareHolders {
			full++
			continue
		}
		if !ml.stripeRes.IsZero() || !ml.up() || ml.pressed || avoided(ml.link, avoid) || stripeEvacSkip(ml.link) {
			other++
			continue
		}
		// a light user's primary sub, or a raw connection, rides it: never.
		// lite's copy-only subs do not count (their primary delivers first,
		// as for HS2_MCONN_STRIPE_SHARE_COPIES), unless a call's
		if int(ml.users.Load())-h-stripeLinkCopies(ml.link) > 0 || callLinkOn(ml.mtr, now) {
			light++
			continue
		}
		if m.gateInfo && ml.mtr != nil && !ml.mtr.infoDone.Load() || !linkMConn(ml, pool) {
			other++
			continue
		}
		cut := m.stripeCutLocked(ml)
		if best == nil || !cut && bestCut || cut == bestCut && ml.rate10 < best.rate10 {
			best, bestCut = ml, cut
		}
	}
	if best == nil {
		if n := stripeDlShareStat.none.Add(1); stripeLog && (n <= 20 || n%100 == 0) {
			log.Printf("stripe dlshare: none (stripe links: full %d, light %d, other %d; none %d)", full, light, other, n)
		}
		return nil
	}
	n := stripeDlShareStat.picks.Add(1)
	if stripeLog || n <= 20 || n%200 == 0 {
		log.Printf("stripe dlshare: link %d shared by a download (holders %d, rate10 %.0f Mbit/s, cut %v, picks %d none %d)",
			best.id, held[best], best.rate10*8/1e6, bestCut, n, stripeDlShareStat.none.Load())
	}
	return best
}
