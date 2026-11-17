package engine

import (
	"os"
	"sync/atomic"
	"time"
)

// ---- mb174: KEEPSTALL's kept chunks of a quiet connection ------------------
//
// Audit M2 (rel66): with KEEPSTALL a striped connection's kept chunks were
// released only by a write on the same sub (its acked prefix), by the
// ack-stall poll (lazy: only while a sub looks suspect) or at the
// connection's end. A download that ended on a keep-alive connection kept
// what its subs had in flight at their last write — every byte the peer
// acked afterwards stayed pinned, plus the unacked tail under smux's
// window-update threshold (half the stream window). ~20 such connections
// filled keepStallGlobal (320 MiB); from then on every write of an ACTIVE
// connection dropped its own unacked chunks (the hole of a dark path among
// them, last but surely): the resend had nothing and gapCheck reset the
// connection 30 s later — the hs112 failure KEEPSTALL was built for.
//
// Now, while a connection keeps chunks and the ack-stall poll is not
// running, the manager polls every keepIdlePoll: the tick's trim frees what
// the peer has acked since. A connection quiet (nothing queued, no sub in a
// write or counted by the ack-stall rule, no chunk kept) for keepIdleDrop —
// keepIdlePressDrop while the process keeps over half keepStallGlobal —
// drops what is left (the unacked tail): by then a hole of its own, if any,
// has reset the connection at the receiver (gapCheck, mGapTimeout) or is
// covered by the dark sub's resend (Iran's silent rule closes it within
// seconds); an active connection's chunks are never touched by this rule.
// HS2_MCONN_STRIPE_KEEPSTALL_IDLE=0: the old behaviour.

const (
	keepIdlePoll      = time.Second
	keepIdleDrop      = mGapTimeout
	keepIdlePressDrop = 10 * time.Second
)

var keepIdleOn = os.Getenv("HS2_MCONN_STRIPE_KEEPSTALL_IDLE") != "0"

var keepIdleStat struct {
	polls   atomic.Uint64 // slow polls run
	dropped atomic.Uint64 // chunks a quiet connection dropped
	dropB   atomic.Uint64 // ... their bytes
}

// keepIdleArmLocked (a writer, under m.mu, key on): make sure a manager tick
// comes to free what the peer acks after this write.
func (m *mConn) keepIdleArmLocked() {
	if !keepIdleOn || m.ksIdle || m.ksTicking || m.keptBytes == 0 && !m.subGapOpenLocked() {
		return
	}
	if m.subSweepArmLocked() { // fx212: the shared sweeper, not this conn's manager
		return
	}
	m.ksIdle = true
	m.kickMgr()
}

// keepIdleLocked (tick, under m.mu, after the trim; the ack-stall poll is
// not running): when to look again (0: nothing kept).
func (m *mConn) keepIdleLocked(now time.Time) time.Duration {
	return m.keepIdleLookLocked(now, subLinkAck)
}

// keepIdleLookLocked: keepIdleLocked with the link evidence from look (the
// shared sweeper reads it before it takes m.mu).
func (m *mConn) keepIdleLookLocked(now time.Time, look func(*msub) (linkAck, bool)) time.Duration {
	if !keepIdleOn || m.keptBytes == 0 && !m.subGapOpenLocked() {
		m.ksIdle = false
		return 0
	}
	keepIdleStat.polls.Add(1)
	if m.subOnlyLocked() || m.stN == 0 && !m.callKeepLocked() && mconnSubResend.Load() {
		m.subAgeTrimLookLocked(now, true, look) // r68fix3 (mconn_sub_resend.go); r68fix4: heals gaps too
		if m.keptBytes == 0 {
			if m.subGapOpenLocked() && !m.ended {
				m.ksIdle = true
				return keepIdlePoll // r68fix4: look again until the gap is delivered
			}
			m.ksIdle = false
			return 0
		}
	}
	if q, ok := m.keepQuietLocked(now); ok {
		lim := keepIdleDrop
		if keepStallStat.kept.Load() > keepStallGlobal/2 {
			lim = keepIdlePressDrop
		}
		if q >= lim {
			for _, x := range m.subs {
				if n := len(x.sent); n > 0 {
					if x.sent[n-1].sub {
						subGapLocked(x, x.sent[n-1].t) // r68fix3/4: a resend from x would have a hole
					}
					b := 0
					for _, r := range x.sent {
						b += r.c.n
					}
					keepIdleStat.dropped.Add(uint64(n))
					keepIdleStat.dropB.Add(uint64(b))
					m.dropSentLocked(x, n)
				}
			}
		}
	}
	if m.keptBytes == 0 {
		m.ksIdle = false
		return 0
	}
	m.ksIdle = true
	return keepIdlePoll
}

// keepQuietLocked: how long the connection has written nothing (ok=false:
// it is active — bytes queued, a sub in a write or counted by the ack-stall
// rule).
func (m *mConn) keepQuietLocked(now time.Time) (time.Duration, bool) {
	var last time.Time
	for _, x := range m.subs {
		if x.qBytes > 0 || x.cur != nil || x.ks.seen {
			return 0, false
		}
		if n := len(x.sent); n > 0 && x.sent[n-1].t.After(last) {
			last = x.sent[n-1].t
		}
	}
	if last.IsZero() {
		return 0, false
	}
	return now.Sub(last), true
}
