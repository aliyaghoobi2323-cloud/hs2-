package engine

import (
	"log"
	"sync/atomic"
	"time"
)

// HS2_MCONN_STRIPE_DL_FAIR=1 (cs141, Iran; with HS2_MCONN_STRIPE_DL_FAST;
// default on, edge, via envdef since rel62k3; =0 turns it off): the download fast rule's slot is a short lease, not a
// connection's for life.
//
// Why (production, tunnel 1 at night, ~80 links): single test downloads were
// bimodal — 807-1054 Mbit/s when they striped, 329-380 Mbit/s (one link)
// when they never did: the logs had one "download stripe at" for four
// downloads. The solo rule (stripeDlFastMax = 1) counts a connection in
// stripeDlActive from its fast growth until its last stripe sub is gone, so
// one long background download that once took the fast rule held it for its
// whole life (minutes); every later download went the usual way —
// stripeHotTicks 1 s ticks, then the raw handshake — and a 10-15 s test
// spent most of its time on one link.
//
// With the key:
//   - a connection holds the fast slot for stripeDlLease after it took it
//     (and while its doubling chain or an open is still going on), then
//     gives it back; it keeps its subs and grows on the usual rules;
//   - only a download's first growth (raw → stripe, n == 1) takes the slot:
//     a background download that is already striped never takes it again,
//     so the slot goes round the new downloads one at a time (solo is kept:
//     the rel54b lesson, at most stripeDlFastMax fast starts at once).
//   - the stripe slots (stripeMaxConns, 8 a process) are full of background
//     streams at night (the cs141 lab: 6-8 users at 20-200 Mbit/s each grew
//     to 2-8 subs, the 8 slots were taken and the test download was refused
//     every tick: one link for its whole life); a download at
//     stripeDlFastRate or more may take one of stripeDlFairExtra slots past
//     the cap (only a fast download, only its first growth).
//
// No wire change. Off: nothing changes.
var stripeDlFair atomic.Bool

// stripeDlLease: how long a download holds the fast slot after taking it.
const stripeDlLease = 3 * time.Second

// stripeDlFairStat: diagnostics.
var stripeDlFairStat struct {
	released, skipped, extra atomic.Uint64
}

// stripeDlFairSkipLocked (stripeDlPlanLocked): a growth n → n+1 that does not
// take the fast slot under the key (not a first growth, slot not held).
func (m *mConn) stripeDlFairSkipLocked(n int) bool {
	if stripeDlFair.Load() && n > 1 && !m.stDlCnt {
		stripeDlFairStat.skipped.Add(1)
		return true
	}
	return false
}

// stripeDlLeaseLocked (every stripe tick): give the fast slot back once the
// lease is over and nothing of the fast start is pending.
func (m *mConn) stripeDlLeaseLocked(now time.Time) {
	if !stripeDlFair.Load() || !m.stDlCnt || m.stFastMore > 0 || m.stOpening || m.stDlAt.IsZero() || now.Sub(m.stDlAt) < stripeDlLease {
		return
	}
	m.stripeDlCountLocked(false)
	n := stripeDlFairStat.released.Add(1)
	if n <= 20 || n%100 == 0 {
		log.Printf("mconn %x: dlfair: fast slot released after %v (over %d subs, released %d, skipped %d)", m.id&0xffffff,
			now.Sub(m.stDlAt).Round(time.Millisecond), 1+m.liveStripeLocked(), n, stripeDlFairStat.skipped.Load())
	}
}

// stripeDlFairExtra: stripe slots past stripeMaxConns that only a fast
// download may take under the key (process-wide).
const stripeDlFairExtra = 4

// stripeDlFairExtraLocked (a first growth found every stripe slot taken: c is
// the count with this one): a download at stripeDlFastRate or more may take
// one of stripeDlFairExtra slots past the cap.
func (m *mConn) stripeDlFairExtraLocked(c int32, up bool, rate float64) bool {
	if !stripeDlFair.Load() || up || rate < float64(stripeDlFastRate.Load()) || c > stripeMaxConns+stripeDlFairExtra {
		return false
	}
	n := stripeDlFairStat.extra.Add(1)
	if n <= 20 || n%100 == 0 {
		log.Printf("mconn %x: dlfair: stripe slot %d past the cap of %d (%.0f Mbit/s, extra %d)", m.id&0xffffff, c, stripeMaxConns, rate*8/1e6, n)
	}
	return true
}
