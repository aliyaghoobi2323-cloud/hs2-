package engine

import (
	"fmt"
	"sync/atomic"
	"time"
)

// l477 HS2_L477_TAILRES (default off, lab A/B; no wire change): a lite
// (non-call) conn's sub that goes dark while it has bytes in flight resends
// only the TAIL of its undelivered chunks (at most l477TailMax bytes, the last
// request or reply) once per dark episode on a live sub of the same conn: the
// hedge/other sub if one is alive, else the fresh sub when it joins.
//
// Lab l476 (lab3, 1/4 of the links blackholed 5 s per window): ~17-21 crowd
// conns per window had their last request only on dead subs and waited for
// the dead link's own TCP retransmission (5.3 s / 9.2 s modes); b449's fresh
// sub carried only new bytes. l476 resent EVERY kept chunk of every dark sub
// on the fresh sub (and moved the primary): crowd late +21.6, CPU Iran +3.5,
// and it did nothing for conns whose primary died while the hedge sub lived.
//
// Dark: the sub's link heard nothing and a write of ours waited unanswered
// for >= max(l477DarkMin, darkcallAfter, 3 x srtt) (cs226Silent and the
// cs228 meter), with bytes in flight on the stream (cs228InFlight). The
// episode is keyed by the link's lastRd: a new read starts a new one.
// The receiver drops bytes it already has by offset (TestL477TailExact);
// chunks the target already holds or the peer acked are skipped
// (fx212SkipHeld, cs223SkipDelivered).
var l477TailRes atomic.Bool

func init() { l477TailRes.Store(envBoolDefault("HS2_L477_TAILRES", false)) }

const (
	l477TailMax = 16 << 10
	l477DarkMin = 400 * time.Millisecond
)

var l477Stat struct {
	eps    atomic.Uint64 // dark episodes resent
	chunks atomic.Uint64
	bytes  atomic.Uint64
	join   atomic.Uint64 // ... of them on a joining sub
	noTgt  atomic.Uint64 // dark-in-flight visits with no live target yet
	big    atomic.Uint64 // tails whose last chunk alone was over l477TailMax
}

func l477LogPart() string {
	return fmt.Sprintf(" · l477 tailres on %v eps %d join %d chunks %d bytes %d notgt %d big %d", l477TailRes.Load(),
		l477Stat.eps.Load(), l477Stat.join.Load(), l477Stat.chunks.Load(), l477Stat.bytes.Load(), l477Stat.noTgt.Load(), l477Stat.big.Load())
}

// l477SilForHook: tests replace the silence duration of a sub.
var l477SilForHook atomic.Pointer[func(*msub, int64) (time.Duration, int64)]

// l477SilentFor: how long s's link was silent while pending (0: not), and
// its lastRd (the episode key).
func l477SilentFor(s *msub, nowc int64) (time.Duration, int64) {
	if h := l477SilForHook.Load(); h != nil {
		return (*h)(s, nowc)
	}
	if !cs226Silent(s, nowc) {
		return 0, 0
	}
	mtr := dcMeterOf(s)
	if mtr == nil {
		return 0, 0
	}
	rd, ps := mtr.lastRd.Load(), mtr.pendSince.Load()
	return time.Duration(nowc - max(rd, ps)), rd
}

func l477DarkAfter(s *msub) time.Duration {
	return max(l477DarkMin, darkcallAfter(), 3*s.subSrtt)
}

// l477OnLocked: m is a kept non-call lite conn.
func (m *mConn) l477OnLocked(now time.Time) bool {
	return l477TailRes.Load() && stripeResend.Load() && m.lite && m.mode == planeLight && m.stN == 0 &&
		!m.ended && !m.heavy && m.callLikeAt.IsZero() && !m.dcIsCallLocked(now) && m.subKeepLocked()
}

// l477TargetOK: x may carry a dark sub's tail (live, not dark itself).
func l477TargetOK(x, s *msub, nowc int64) bool {
	if x == nil || x == s || x.dead || x.retired || x.closeHS || x.closeAfter || x.gated || x.rrAband ||
		x.bulk.Load() || x.dc.isDark || x.dc.txDark {
		return false
	}
	d, _ := l477SilentFor(x, nowc)
	if d != 0 {
		return false
	}
	if l477SilForHook.Load() != nil {
		return true
	}
	// v3: positive evidence: x's link read within l477DarkMin (a sub on a dead
	// link with no write pending yet is not silent, yet no carrier: lab3 u2C/u3C
	// kept the 5 s mode with v2)
	mtr := dcMeterOf(x)
	return mtr != nil && time.Duration(nowc-mtr.lastRd.Load()) < l477DarkMin
}

// l477TailResLocked (under m.mu; the darkcall lite visit, and addSubLocked
// with nw the joining sub): resend each dark in-flight sub's tail once.
func (m *mConn) l477TailResLocked(nw *msub, now time.Time, nowc int64) {
	if !m.l477OnLocked(now) {
		return
	}
	for _, s := range m.subs {
		if s == nw || s.dead || s.retired || s.bulk.Load() {
			continue
		}
		d, rd := l477SilentFor(s, nowc)
		early := false
		if d == 0 && nw != nil {
			d, rd, early = l480JoinSilent(s, nowc) // l480: a fresh sub joins a dark conn (mconn_l480_stick.go)
		}
		if d == 0 {
			continue
		}
		same := s.l477Rd != 0 && s.l477Rd == max(rd, 1)
		if !early && d < l477DarkAfter(s) || !cs228InFlight(s) {
			continue
		}
		b := nw
		if b == nil || !l477TargetOK(b, s, nowc) {
			b = nil
			if p := m.primary.Load(); l477TargetOK(p, s, nowc) {
				b = p
			} else {
				for _, x := range m.subs {
					if l477TargetOK(x, s, nowc) && (b == nil || x.qBytes < b.qBytes) {
						b = x
					}
				}
			}
		}
		if b == nil {
			l477Stat.noTgt.Add(1)
			continue
		}
		m.l478MovePrimaryLocked(s, b) // l478 HS2_L478_FASTOPEN (mconn_l478_fastopen.go)
		cs := m.cs223SkipDelivered(fx212SkipHeld(b270SeqChunks(s, now), b), b)
		if !same {
			s.l477Rd, s.l477End = max(rd, 1), 0
		}
		// v2: within an episode only bytes past what an earlier tail covered
		// (a reply written onto the dark sub after its first tail went: lab3
		// t2B moved the 9 s mode to 5 s, the reply stuck on the dark primary)
		k := 0
		for _, c := range cs {
			if c.off+uint64(c.n) > s.l477End {
				cs[k] = c
				k++
			}
		}
		cs = cs[:k]
		if len(cs) == 0 {
			continue
		}
		s.l477End = max(s.l477End, cs[len(cs)-1].off+uint64(cs[len(cs)-1].n))
		nb := 0
		k = len(cs)
		for k > 0 && nb+cs[k-1].n <= l477TailMax {
			k--
			nb += cs[k].n
		}
		if k == len(cs) {
			if len(cs) > 0 {
				l477Stat.big.Add(1)
			}
			continue
		}
		tail := cs[k:]
		pushFrontLocked(b, tail)
		l477Stat.eps.Add(1)
		if b == nw {
			l477Stat.join.Add(1)
		}
		l477Stat.chunks.Add(uint64(len(tail)))
		l477Stat.bytes.Add(uint64(nb))
		obs.mconnDupBytes.Add(uint64(nb))
		m.tr.ev("l477: dark sub %d (silent %v) resent tail %d chunks (%d B) on sub %d", s.idx, d.Round(time.Millisecond), len(tail), nb, b.idx)
		m.sendCond.Broadcast()
	}
}
