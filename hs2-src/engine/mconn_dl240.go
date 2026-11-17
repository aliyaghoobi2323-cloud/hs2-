package engine

import (
	"log"
	"sync/atomic"
	"time"
)

// HS2_DL240_YOUNG (dl240, the exit = a download's sender; default OFF (lab A/B not conclusive),
// =1 turns it on): a stripe sub that just joined is on probation — it may
// hold at most max(dl240Floor, what the peer already consumed on it) unread
// (smux written - peer consumed) for its first dl240Age, so a young sub
// widens like a TCP slow start on what it has proven to deliver instead of
// taking half of every chunk from its first millisecond.
//
// Why (production tunnel 1, rel69d-pre, single 12 s downloads: per-second
// 399, 259, 12, 1591 ... and 1111, 224, 23, 1846; lab2 dl240 A1 t60: 32, 167,
// 38, 239): the second after the raw → stripe upgrade a download stalled
// ~1 s and then caught up. The trace (HS2_MCONN_STRIPE_LOG, lab2 15:57:07):
// the exit's sender spread chunks over the warm primary and the new stripe
// sub by queue length alone; the new sub's link (a set-aside / shared / cold
// link: BBR PROBE_RTT at cwnd 4, a slow-start restart, another stripe's
// backlog — hl235) took ~1 s to deliver its first chunks, Iran's in-order
// reassembly sat at the hole (rx next = the sub's first chunk offset, kept0
// unchanged for >1 s) while the reorder buffer filled from the primary, then
// everything arrived at once (the 1591 / 1846 Mbit/s catch-up). The dlfair
// "fast slot released after 3.0 s" line is only the 3 s lease ending right
// after it (the slot is bookkeeping: stripeDlActive, no data path effect).
//
// With the key, on the exit's stripe sender (stripeSendLocked), a stripe sub
// younger than dl240Age whose unread bytes are at its budget is passed over
// for this chunk — the other subs (the warm primary) take it; if no other
// sub can take it the young sub still does (never a wait added). Past
// dl240Age, or once the peer consumed enough on it, nothing changes.
// No wire change; Iran unchanged.
var dl240Young atomic.Bool

func init() { dl240Young.Store(envBoolDefault("HS2_DL240_YOUNG", false)) }

const (
	// dl240Age: how long a new stripe sub stays on probation.
	dl240Age = 2 * time.Second
	// dl240Floor: unread bytes a young sub may always hold (more than the
	// peer's smux update step at the 2 MiB fixed window / 8, so its window
	// updates keep coming).
	dl240Floor = 512 << 10
)

// dl240Stat: diagnostics (status dl240_held / dl240_fallback).
var dl240Stat struct {
	held, fallback atomic.Uint64
}

// dl240HoldLocked (the exit's stripe sender, under m.mu): s is a young
// stripe sub at its probation budget now.
func (m *mConn) dl240HoldLocked(s *msub, now time.Time) bool {
	if m.iran || s.stJoinAt.IsZero() || now.Sub(s.stJoinAt) >= dl240Age {
		return false
	}
	w, c, ok := subAckMarks(s)
	if !ok {
		return false
	}
	unread := w - c // uint32: wraps right
	if unread < uint32(max(dl240Floor, int(min(c, 1<<30)))) {
		return false
	}
	if n := dl240Stat.held.Add(1); n == 1 || n%5000 == 0 {
		log.Printf("mconn %x: dl240: young sub %d held (age %v, unread %d KB, consumed %d KB; held %d, fallback %d)", m.id&0xffffff,
			s.idx, now.Sub(s.stJoinAt).Round(time.Millisecond), unread>>10, c>>10, n, dl240Stat.fallback.Load())
	}
	return true
}

// DL240Stats: chunks a young sub was passed over for, and chunks it took as
// the only sub with room.
func DL240Stats() (held, fallback uint64) {
	return dl240Stat.held.Load(), dl240Stat.fallback.Load()
}
