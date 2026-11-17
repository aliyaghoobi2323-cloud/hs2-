package engine

import (
	"fmt"
	"sync/atomic"
	"time"
)

// b262 replace (HS2_B262_REPLACE, default OFF until an ABAB shows a gain:
// lab1 W 2026-11-17 B 8/28 stalled cuts vs A 6/42, n=2; =1 on): a call's sub that a
// darkcall switch left and that stays dark with a stale backlog in its
// link's socket is closed and replaced by a fresh sub on another link.
//
// Lab1 ZB1 2u4 (2.7 s call stall, b262v2): the call's other sub (path 1)
// was still in TCP RTO backoff from the 1u5 cut 5 s earlier, with 2.3 MB
// written-unacked in its socket, when path 2 (the primary) was cut. The
// switch had no live candidate; path 1 came back +0.6 s and had to resend
// its 2.3 MB of stale copies before the new bytes. Those bytes were all
// copies: the switch that left path 1 resent its unconsumed range to the new
// primary, and since then b262's darkwait never gives a dark sub a chunk's
// only copy. So once such a sub has been dark for b262RepDark the call
// loses nothing by leaving it: Iran sends CLOSE-REQ on it (the close
// handshake: Kharej resends what its end may not have delivered, cs223) and
// remembers its link as dead, and callMoreSubsLocked opens the pair's
// second sub again on another link (fresh TCP: no backoff, empty socket).
//
// Never in normal operation: it needs a sub with no TCP ack progress for
// b262RepDark AND >= b262RepOutq bytes held in its socket AND a live
// primary that is not dark. At most one per connection per b262RepEvery and
// b262RepRate per second process-wide (a cut path at 4x scale must not open
// links for every call at once); the open goes through the pool (its limits
// and the dead-link avoid list), so a failed open only backs off.
var b262RepOn atomic.Bool

func init() { b262RepOn.Store(envBoolDefault("HS2_B262_REPLACE", false)) }

const (
	b262RepDark  = 2 * time.Second
	b262RepOutq  = 16 << 10
	b262RepEvery = 10 * time.Second
	b262RepRate  = 20 // per second, process-wide
)

var b262RepStat struct {
	rep    atomic.Uint64 // subs closed for a fresh one
	limit  atomic.Uint64 // wanted, refused by the rate limits
	noHS   atomic.Uint64 // wanted, the link has no close handshake
	small  atomic.Uint64 // visits with a candidate whose socket held < b262RepOutq (or unknown)
	outqKB atomic.Uint64 // socket bytes of the replaced subs (KB)
}

// b262OutqOf: the bytes the sub's link socket holds unacked + unsent (a var
// for tests).
var b262OutqOf = func(s *msub) (uint64, bool) {
	tc := subTCP(s)
	if tc == nil {
		return 0, false
	}
	return tcpOutq(tc)
}

// b262RepCloseHS: the sub can be closed with the handshake (a var for tests).
var b262RepCloseHS = func(s *msub) bool { return mconnCloseHS.Load() && subCloseHS(s.rw) }

var b262RepWin struct {
	sec atomic.Int64
	n   atomic.Int64
}

// b262RepAllow: the process-wide rate limit (b262RepRate per second).
func b262RepAllow(now time.Time) bool {
	sec := now.Unix()
	if old := b262RepWin.sec.Load(); old != sec && b262RepWin.sec.CompareAndSwap(old, sec) {
		b262RepWin.n.Store(0)
	}
	return b262RepWin.n.Add(1) <= b262RepRate
}

// b262RepWant: s is a stale-backlog sub to replace (pure; dark is how long
// its link has made no ack progress).
func b262RepWant(s, p *msub, dark time.Duration, pLive bool) bool {
	return s != p && p != nil && pLive && s.dcLeft && !s.retired && !s.dead && !s.closeHS &&
		!s.bulk.Load() && s.dc.ok && s.dc.unacked > 0 && dark >= b262RepDark
}

// b262ReplaceLocked (Iran, darkcall visit of a call, under m.mu, after the
// subs' looks): close at most one stale-backlog sub; callMoreSubsLocked
// opens its replacement.
func (m *mConn) b262ReplaceLocked(p *msub, live []*msub, now time.Time, nowc int64) {
	if !b262RepOn.Load() || !darkcallOn.Load() || !m.iran || p == nil || !m.callOn || now.Sub(m.b262RepAt) < b262RepEvery {
		return
	}
	pLive := !b262DarkLocked(p) && p.dc.ok && !p.dc.stalled(now) && p.dc.lastRd != 0 &&
		time.Duration(nowc-p.dc.lastRd) < darkcallAfter()
	for _, s := range live {
		if !b262RepWant(s, p, now.Sub(s.dc.ackAt), pLive) {
			continue
		}
		q, ok := b262OutqOf(s)
		if !ok || q < b262RepOutq {
			b262RepStat.small.Add(1)
			continue
		}
		if !b262RepCloseHS(s) {
			b262RepStat.noHS.Add(1)
			m.b262RepAt = now
			return
		}
		if !b262RepAllow(now) {
			b262RepStat.limit.Add(1)
			return
		}
		m.b262RepAt = now
		b262RepStat.rep.Add(1)
		b262RepStat.outqKB.Add(q >> 10)
		m.noteDeadKeyLocked(s.key)
		m.closeReqLocked(s)
		m.setPrimaryLocked()
		m.dcLogf("%x b262 replace iran=%v sub %d(%s) dark %v unacked %d outq %d", m.id, m.iran, s.idx, s.dc.path,
			now.Sub(s.dc.ackAt).Round(time.Millisecond), s.dc.unacked, q)
		m.kickMgr() // callMoreSubsLocked opens the replacement
		return
	}
}

func b262RepLogPart() string {
	return fmt.Sprintf(" · rep on %v %d (%d KB) limit %d nohs %d small %d", b262RepOn.Load(), b262RepStat.rep.Load(), b262RepStat.outqKB.Load(), b262RepStat.limit.Load(), b262RepStat.noHS.Load(), b262RepStat.small.Load())
}
