package engine

import (
	"sync/atomic"
	"time"
)

// A call's late copy (HS2_LB_CALL_LATE, docs/reality-architecture.md 8.12).
//
// Lab trace (8.12, scenario c, HS2_MCONN_TRACE per-sub lines): after path B
// degrades, a call with one sub on B and one on another path keeps both
// copies, but B's copy arrives 325-900 ms after the other (B's socket srtt
// 240 ms vs 80 ms) and is first for no byte: the call has one useful copy.
// Each loss recovery on the good sub's link then stalls the call for
// 200-250 ms (the gap filled only by B's late copy), the probe's 450-575 ms
// seconds. Neither rescue moves it: the stuck trigger needs every sub on one
// path, the lowered-path rescue every sub on a lowered link.
//
// With the key, on Iran, for a call (callRescueOnLocked's call test, without
// needing HS2_LB_CALL_RESCUE) with two live subs or more on two known paths
// or more, sampled every callStuckPoll:
//   - a sub whose socket srtt (TCP_INFO) is at least callLateExtra above the
//     best sub's on another path and at least twice it, without a break for
//     callLateFor, is the late copy;
//   - then one fresh sub with the call placement off every link of the late
//     sub's path (callStuckMark);
//   - from the trigger on, the late sub gets no call pair copy while two
//     other subs are live (callPairLocked; the fresh sub makes three, so
//     both copies of each chunk ride timely subs), and it is given up through
//     the close handshake once a sub opened after the trigger has read a byte
//     from the peer (never the last sub); nothing read within
//     callStuckGiveUp: nothing closed; a late sub already handled with two
//     timely subs beside it triggers nothing more;
//   - one trigger per connection per callStuckEvery, callStuckPerSec in the
//     process (the stuck trigger's bounds, shared).
//
// Equal or steady paths do not hold one call socket's srtt 100 ms and twice
// above another's for a second (counted in the lab, 8.12).
var lbCallLate bool // loadEnvKeys: HS2_LB_CALL_LATE

const (
	callLateExtra = 100 * time.Millisecond
	callLateFor   = time.Second
)

// callLateOpened / callLateMoved: fresh subs opened for a late copy; late
// subs given up once the fresh one read.
var callLateOpened, callLateMoved atomic.Int64

// callLateSRTT: s's socket srtt (a var for tests).
var callLateSRTT = func(s *msub) (time.Duration, bool) {
	tc := subTCP(s)
	if tc == nil {
		return 0, false
	}
	return tcpSRTT(tc)
}

// callLateOnLocked: the key, Iran, lite, and the connection is a call.
func (m *mConn) callLateOnLocked(now time.Time) bool {
	return lbCallLate && lbRescue && m.iran && m.lite &&
		(m.callOn || m.callUp || (!m.callLikeAt.IsZero() && now.Sub(m.callLikeAt) < callRescueTTL))
}

// callLateLocked (Iran's manager, lite, two live subs or more, under m.mu).
// gave: a sub was given up; poll: look again within it (0: no need).
func (m *mConn) callLateLocked(live []*msub, now time.Time) (gave bool, poll time.Duration) {
	if !m.callLateOnLocked(now) || m.peerFin || m.wclosed || m.handshaking {
		m.lateSince, m.lateKey = time.Time{}, nil
		return false, 0
	}
	if m.lateKey != nil {
		return m.callLateMoveLocked(live, now), callStuckPoll
	}
	var best, worst *msub
	var bestR, worstR time.Duration
	var bestP, worstP string
	n := 0
	for _, s := range live {
		if s.retired || s.dead || s.closeHS {
			continue
		}
		p := callPathOf(s.key)
		if p == "" {
			continue
		}
		r, ok := callLateSRTT(s)
		if !ok {
			continue
		}
		n++
		if best == nil || r < bestR {
			best, bestR, bestP = s, r, p
		}
		if worst == nil || r > worstR {
			worst, worstR, worstP = s, r, p
		}
	}
	if n < 2 {
		m.lateSince = time.Time{}
		return false, 0
	}
	good := 0 // subs whose copy is timely (srtt within callLateExtra of the best)
	for _, s := range live {
		if s.retired || s.dead || s.closeHS || s.callLate {
			continue
		}
		if r, ok := callLateSRTT(s); ok && r < bestR+callLateExtra {
			good++
		}
	}
	if worst.callLate && good >= 2 {
		m.lateSince = time.Time{}
		return false, callStuckPoll // already handled: two timely copies, the late one gets none
	}
	if worst == best || worstP == bestP || worstR < bestR+callLateExtra || worstR < 2*bestR {
		m.lateSince = time.Time{}
		return false, callStuckPoll
	}
	if m.lateSince.IsZero() {
		m.lateSince = now
	}
	if now.Sub(m.lateSince) < callLateFor {
		return false, callStuckPoll
	}
	if now.Before(m.callStuckAt) || m.lightOpening || now.Before(m.lightRetryAt) || m.stuckKeys != nil {
		return false, callStuckPoll
	}
	if !callStuckAllow(now) {
		return false, callStuckPoll
	}
	m.callStuckAt = now.Add(callStuckEvery)
	m.lateOpenedAt = now
	m.lateSince = time.Time{}
	m.lateKey = worst.key
	worst.callLate = true // no call pair copy on it once two others are live (callPairLocked)
	m.lateLive = m.lateLive[:0]
	avoid := make([]any, 0, len(live)+len(m.deadKeys)+1)
	for _, s := range live {
		avoid = append(avoid, s.key)
		m.lateLive = append(m.lateLive, s.key)
	}
	avoid = append(append(avoid, m.deadKeys...), callStuckMark{path: worstP})
	m.lightOpening = true
	callLateOpened.Add(1)
	m.tr.ev("call: late copy on sub %d path %s (srtt %v vs %v): open a fresh sub off it", worst.idx, worstP, worstR, bestR)
	go m.openLight(avoid)
	return false, callStuckPoll
}

// callLateMoveLocked: once a sub opened since the trigger has read a byte
// from the peer (a sub not live at the trigger), give up the late sub.
func (m *mConn) callLateMoveLocked(live []*msub, now time.Time) bool {
	var late *msub
	fresh := false
	for _, s := range live {
		if s.retired || s.dead || s.closeHS {
			continue
		}
		if s.key == m.lateKey {
			late = s
			continue
		}
		if !containsAny(m.lateLive, s.key) && s.lastRecv.Load() >= m.lateOpenedAt.UnixNano() {
			fresh = true
		}
	}
	switch {
	case late == nil:
		m.lateKey = nil
		return false
	case !fresh:
		if now.Sub(m.lateOpenedAt) >= callStuckGiveUp {
			m.lateKey = nil
		}
		return false
	}
	m.lateKey = nil
	if !mconnCloseHS.Load() || !subCloseHS(late.rw) {
		m.tr.ev("call: late sub %d kept (no close handshake on its link); no copies on it", late.idx)
		return false
	}
	m.closeReqLocked(late)
	callLateMoved.Add(1)
	m.tr.ev("call: late sub %d given up", late.idx)
	return true
}
