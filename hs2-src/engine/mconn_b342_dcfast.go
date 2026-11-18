package engine

import (
	"fmt"
	"net"
	"sync/atomic"
	"time"
)

// b342 HS2_B342_DCFAST (default on since rel69p-rc3, "0" off; each side its own sending
// direction, no wire change): darkcall switches a call's primary off a
// stalled link without waiting for RTO evidence.
//
// rb339 R1 / rb340: a call's primary sat on a path being cut and the call
// stalled 4.3 s. darkcall's DARK and dc254's txDark both want RTO evidence
// (backoff >= 2 or >= 2 retransmits past the last ack progress) on top of
// darkcallAfter (1 s) with no ack progress. A link's RTO is
// srtt + 4 rttvar, at least 200 ms; on a loaded shared link srtt and rttvar
// are inflated (bufferbloat), the RTO sits near 1 s and doubles per
// backoff, so backoff 2 comes 3 s or more after the cut: the stalled sub
// read "noack 1 s backoff 0" in rb339.
//
// With the key on, a call connection's (dcIsCallLocked) primary that is not
// already DARK/txDark is switched when:
//   - it has bytes in flight with no ack progress for at least
//     max(3 x its srtt, b342Floor (300 ms)) (srtt as the kernel last had it;
//     it does not move while nothing is acked);
//   - there is a candidate: another live sub on another socket and another
//     path, not DARK/txDark, not on a cut-marked link, heard from within
//     darkcallAfter, whose own link shows recent ack progress (nothing in
//     flight, or ack progress within max(3 x its srtt, b342Floor)) and less
//     ack-stalled than the primary. A candidate that is itself stalled or
//     unknown is never taken (never switch to a worse copy).
//
// The switch is darkcall's own (dcSwitchLocked: resend from the records
// ring, queue moved, xs261 tells the peer), so it shares darkcall's
// per-connection gap (darkcallGap, 3 s): at most one action per gap, which
// is the hysteresis that keeps a call from flapping between two copies.
var b342DCFast atomic.Bool

// b342Floor: the stall threshold's floor (tests may lower it).
var b342FloorNs atomic.Int64

// b342SRTT: a link's smoothed RTT (tests replace it).
var b342SRTTP atomic.Pointer[func(*net.TCPConn) (time.Duration, bool)]

func init() {
	b342DCFast.Store(envBoolDefault("HS2_B342_DCFAST", true))
	b342FloorNs.Store(int64(300 * time.Millisecond))
	f := tcpSRTT
	b342SRTTP.Store(&f)
}

func b342SRTT(tc *net.TCPConn) (time.Duration, bool) {
	if tc == nil {
		return 0, false
	}
	return (*b342SRTTP.Load())(tc)
}

var b342Stat struct {
	sw     atomic.Uint64 // fast switches
	noCand atomic.Uint64 // primary stalled, no clean candidate (per stall episode)
	maxAge atomic.Int64  // longest primary ack stall at a fast switch, ms
}

func b342LogPart() string {
	if !b342DCFast.Load() && b342Stat.sw.Load() == 0 {
		return ""
	}
	return fmt.Sprintf(" · b342 dcfast %v sw %d nocand %d maxage %d ms", b342DCFast.Load(), b342Stat.sw.Load(), b342Stat.noCand.Load(), b342Stat.maxAge.Load())
}

// b342Thresh: max(3 x srtt, floor) for s's link (srtt unknown: floor).
func b342Thresh(s *msub) time.Duration {
	th := time.Duration(b342FloorNs.Load())
	if r, ok := b342SRTT(s.dc.tc); ok && 3*r > th {
		th = 3 * r
	}
	return th
}

// b342StalledLocked: p has bytes in flight with no ack progress for its
// threshold; returns the stall age and threshold.
func b342StalledLocked(p *msub, now time.Time) (bool, time.Duration, time.Duration) {
	if p == nil || p.dead || p.retired || !p.dc.ok || p.dc.unacked == 0 || p.dc.isDark || p.dc.txDark {
		return false, 0, 0
	}
	age := now.Sub(p.dc.ackAt)
	if age < time.Duration(b342FloorNs.Load()) {
		return false, age, 0 // cheap gate: no srtt read
	}
	th := b342Thresh(p)
	return age >= th, age, th
}

// b342PickLocked (under m.mu, after this visit's dcLookLocked): the clean
// candidate for a stalled primary p (nil: none).
func (m *mConn) b342PickLocked(p *msub, live []*msub, now time.Time, nowc int64, pAge time.Duration) *msub {
	var to *msub
	for _, s := range live {
		if s == p || s.dead || s.retired || !s.dc.ok || s.dc.isDark || s.dc.txDark || s.dc.tc == p.dc.tc {
			continue
		}
		if s.dc.path != "" && s.dc.path == p.dc.path {
			continue
		}
		if rescueCutKey(s.key) {
			continue // b340: its path is being cut
		}
		if s.dc.lastRd == 0 || time.Duration(nowc-s.dc.lastRd) >= darkcallAfter() {
			continue // not heard from: no proof it lives
		}
		if s.dc.unacked > 0 {
			age := now.Sub(s.dc.ackAt)
			if age >= pAge || age >= b342Thresh(s) {
				continue // stalled too: not better
			}
		}
		if dv, tv := dcPathDiv(p.dc.path, s.dc.path), dcPathDiv(p.dc.path, toPathOf(to)); to == nil || dv > tv || dv == tv && s.dc.lastRd > to.dc.lastRd {
			to = s
		}
	}
	return to
}

// b342FastLocked (darkcall visit of a call, past the gap): the fast switch;
// true when it switched.
func (m *mConn) b342FastLocked(p *msub, live []*msub, now time.Time, nowc int64, next uint64) bool {
	st, age, th := b342StalledLocked(p, now)
	if !st {
		p.dcB342No = false
		return false
	}
	to := m.b342PickLocked(p, live, now, nowc, age)
	if to == nil {
		if !p.dcB342No {
			p.dcB342No = true
			b342Stat.noCand.Add(1)
		}
		return false
	}
	p.dcB342No = false
	b342Stat.sw.Add(1)
	ms := age.Milliseconds()
	for {
		o := b342Stat.maxAge.Load()
		if ms <= o || b342Stat.maxAge.CompareAndSwap(o, ms) {
			break
		}
	}
	m.dcLogf("%x darkcall b342 fast iran=%v sub %d path %s noack %v thresh %v backoff %d rt+%d -> sub %d path %s ackage %v",
		m.id, m.iran, p.idx, p.dc.path, age.Round(time.Millisecond), th.Round(time.Millisecond), p.dc.backoff, p.dc.retrans-p.dc.rtAck,
		to.idx, to.dc.path, b342AckAge(to, now).Round(time.Millisecond))
	m.dcSwitchLocked(p, to, now, next)
	m.xsSendLocked(to, now, false) // xs261: the peer leaves p too
	return true
}

func b342AckAge(s *msub, now time.Time) time.Duration {
	if s.dc.unacked == 0 {
		return 0
	}
	return now.Sub(s.dc.ackAt)
}
