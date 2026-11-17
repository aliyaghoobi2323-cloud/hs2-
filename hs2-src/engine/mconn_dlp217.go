package engine

import (
	"log"
	"sync/atomic"
	"time"
)

// dlp217 (HS2_FX212_DL_SHAREQ_MS, the exit; default 0: off): a stripe sub
// on a link that other users ride (a shared pick, HS2_MCONN_STRIPE_SHARE /
// DL_COPIES) takes a chunk only while that link's own queueing delay
// (smoothed RTT over the lowest, TCP_INFO, the scavenger's 100 ms look) is
// at most this many ms. The scavenger gate reads the OTHER links' queue and
// leaves the stripe's own links out, but on a shared link the light users'
// replies ride the same TCP connection and wait in the queue the stripe
// itself builds there (an210 Q1: DL_COPIES +5..10 ms crowd p99). With the
// key the stripe fills a shared link only up to a queue the light users
// barely see; the primary and links no other user rides are unchanged, so
// the download keeps them at full rate. A stale or missing look (over
// dlp217QStale old) does not hold the sub.
var dlp217ShareQ = envMsDefault("HS2_FX212_DL_SHAREQ_MS", 0)

// dlp217QStale: an older look says nothing about the link now.
const dlp217QStale = time.Second

// dlp217Stat (diagnostics): chunks a shared sub was passed over for its
// link's queue, and chunks that went to a shared sub under the key.
var dlp217Stat struct {
	held, sent atomic.Uint64
}

// dlp217NoteQ (the scavenger's look): cache link mtr's queueing delay.
func dlp217NoteQ(mtr *linkMeter, q time.Duration, now time.Time) {
	if mtr == nil {
		return
	}
	mtr.stQUs.Store(q.Microseconds())
	mtr.stQAt.Store(monoNano(now))
}

// dlp217Shared: s (an exit stripe sub, not the primary) rides a link that
// other users ride too.
func dlp217Shared(mtr *linkMeter) bool {
	return mtr != nil && mtr.users.Load() > 1
}

// dlp217HoldLocked (the exit's stripe sender): s's link is shared and its
// queue is over the key's bound now.
func (m *mConn) dlp217HoldLocked(s *msub, now time.Time) bool {
	if dlp217ShareQ <= 0 || m.iran {
		return false
	}
	mtr := m.subMeterOf(s)
	if !dlp217Shared(mtr) {
		return false
	}
	at := mtr.stQAt.Load()
	if at == 0 || monoNano(now)-at > int64(dlp217QStale) {
		return false
	}
	if time.Duration(mtr.stQUs.Load())*time.Microsecond > dlp217ShareQ {
		dlp217Stat.held.Add(1)
		return true
	}
	return false
}

// dlp217LogAt: unix ns of the last sharedq line (one per 10 s at most).
var dlp217LogAt atomic.Int64

// dlp217Log: the counters' line, at most every 10 s (from the sender).
func dlp217Log(now time.Time) {
	n := monoNano(now)
	at := dlp217LogAt.Load()
	if n-at < int64(10*time.Second) || !dlp217LogAt.CompareAndSwap(at, n) {
		return
	}
	log.Printf("stripe sharedq: bound=%v held=%d sent=%d", dlp217ShareQ, dlp217Stat.held.Load(), dlp217Stat.sent.Load())
}
