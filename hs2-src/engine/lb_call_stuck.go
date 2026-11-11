package engine

import (
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// A call stuck on its path, by its own signals (HS2_LB_CALL_RESCUE with
// HS2_LB_CALL_STUCK, default on under it; docs/reality-architecture.md 8.11).
//
// Lab (8.10, scenario c): a call whose subs all sit on path B stalls at the
// cut itself, but B is lowered (rescueLow) only ~7.5 s later, and the
// lowered-path call rescue (lb_call_rescue.go) acts only then: the 8-9 s
// before it are a 60 s run's call p99. So, on Iran, for a call (the call
// rescue's own test, callRescueOnLocked) with two live subs or more, ALL on
// links of one path (mlPath, recorded per link by the steering tick):
//   - evidence, sampled every callStuckPoll while that holds: every sub's
//     socket (TCP_INFO, tcpBusy as HS2_MCONN_CALL_PICK reads it) in loss
//     recovery or holding more than callStuckUnsent unsent, without a break,
//     for callStuckFor; or the call's receive gap (no record from the peer on
//     any sub) at least callStuckGap while this side kept sending (a push
//     within 2*callStuckPoll);
//   - then at once one fresh sub with the call-rescue placement off every
//     link of that path (and off lowered links): callStuckMark;
//   - the subs live at the trigger are given up through the close handshake
//     only once a sub opened after it has read a byte from the peer (no byte
//     rides only there; never the last sub); without that within
//     callStuckGiveUp nothing is closed and the evidence is read anew;
//   - bounds: one trigger per connection per callStuckEvery (no flapping
//     between two paths), callStuckPerSec over the process.
//
// A healthy path does not hold every socket of a call in recovery or backlog
// for half a second, nor leave it a receive gap of callStuckGap while it
// sends; the lab counts the triggers on equal and steady paths (8.11).
var lbCallStuck = os.Getenv("HS2_LB_CALL_STUCK") != "0"

const (
	callStuckPoll   = 100 * time.Millisecond
	callStuckFor    = 500 * time.Millisecond
	callStuckGap    = 600 * time.Millisecond
	callStuckUnsent = 64 << 10
	callStuckEvery  = 5 * time.Second
	callStuckGiveUp = 5 * time.Second
	callStuckPerSec = 20
)

// callStuckOpened / callStuckMoved / callStuckRefused: fresh subs opened for
// stuck calls; subs given up once the fresh one read; triggers refused by the
// process rate bound. callStuckBusyN / callStuckGapN: triggers by signal.
var callStuckOpened, callStuckMoved, callStuckRefused, callStuckBusyN, callStuckGapN atomic.Int64

var (
	callStuckSec atomic.Int64
	callStuckN   atomic.Int64
)

// callStuckAllow takes one trigger from the per-second bound.
func callStuckAllow(now time.Time) bool {
	sec := now.Unix()
	if old := callStuckSec.Load(); old != sec && callStuckSec.CompareAndSwap(old, sec) {
		callStuckN.Store(0)
	}
	if callStuckN.Add(1) > callStuckPerSec {
		callStuckRefused.Add(1)
		return false
	}
	return true
}

// callLinkPath: Link (a sub's key) -> its path's name, recorded by each
// LinkManager's steering tick for its own links (callPathMark).
var callLinkPath sync.Map

// callPathMark records the paths of a manager's live links (prev: those it
// recorded last time; returned updated). Links gone are forgotten.
func callPathMark(prev map[any]bool, byPath map[string][]*managedLink) map[any]bool {
	next := make(map[any]bool, len(prev))
	for p, ls := range byPath {
		for _, ml := range ls {
			next[ml.link] = true
			callLinkPath.Store(ml.link, p)
		}
	}
	for l := range prev {
		if !next[l] {
			callLinkPath.Delete(l)
		}
	}
	return next
}

// callPathOf: the path of a sub's link ("" unknown).
func callPathOf(key any) string {
	if key == nil {
		return ""
	}
	if v, ok := callLinkPath.Load(key); ok {
		return v.(string)
	}
	return ""
}

// callStuckBusy: s's socket is in loss recovery or holds a backlog (a var
// for tests).
var callStuckBusy = func(s *msub) bool {
	tc := subTCP(s)
	if tc == nil {
		return false
	}
	v, ok := tcpBusy(tc)
	return ok && (v >= mCallStalled || v > callStuckUnsent)
}

// callStuckMark in an avoid list asks the opener for the call placement off
// every link of path.
type callStuckMark struct{ path string }

// takeCallStuckMark removes the mark from avoid.
func takeCallStuckMark(avoid []any) ([]any, string, bool) {
	for i, a := range avoid {
		if c, ok := a.(callStuckMark); ok {
			return append(avoid[:i:i], avoid[i+1:]...), c.path, true
		}
	}
	return avoid, "", false
}

// callStuckOnLocked: the stuck trigger runs for a call under
// HS2_LB_CALL_RESCUE (8.11) or under HS2_LB_CALL_LATE alone (rel51): call
// late needs the call's subs on two paths, so with every sub on one path
// (docs/reality-architecture.md §11.6.5, mixed fronts) nothing else moves
// the call while the rescue key is off.
func (m *mConn) callStuckOnLocked(now time.Time) bool {
	return m.callRescueOnLocked(now) || m.callLateOnLocked(now)
}

// callStuckLocked (Iran's manager, lite, two live subs or more, under m.mu).
// gave: subs were given up (the caller refreshes the primary); poll: look
// again within it (0: no need).
func (m *mConn) callStuckLocked(live []*msub, now time.Time) (gave bool, poll time.Duration) {
	if !lbCallStuck || !m.callStuckOnLocked(now) || m.peerFin || m.wclosed || m.handshaking {
		m.stuckSince = time.Time{}
		m.stuckKeys = nil
		return false, 0
	}
	if m.stuckKeys != nil {
		return m.callStuckMoveLocked(live, now), callStuckPoll
	}
	path := ""
	n := 0
	var lastRecv int64
	var lastPush time.Time
	for _, s := range live {
		if s.retired || s.dead || s.closeHS {
			continue
		}
		p := callPathOf(s.key)
		if p == "" || path != "" && p != path {
			m.stuckSince = time.Time{}
			return false, 0 // not all on one known path
		}
		path = p
		n++
		if r := s.lastRecv.Load(); r > lastRecv {
			lastRecv = r
		}
		if s.lastPush.After(lastPush) {
			lastPush = s.lastPush
		}
	}
	if n < 2 {
		m.stuckSince = time.Time{}
		return false, 0
	}
	gap := lastRecv != 0 && now.Sub(time.Unix(0, lastRecv)) >= callStuckGap && now.Sub(lastPush) <= 2*callStuckPoll
	busy := true
	for _, s := range live {
		if s.retired || s.dead || s.closeHS {
			continue
		}
		if !callStuckBusy(s) {
			busy = false
			break
		}
	}
	switch {
	case gap:
	case busy:
		if m.stuckSince.IsZero() {
			m.stuckSince = now
		}
		if now.Sub(m.stuckSince) < callStuckFor {
			return false, callStuckPoll
		}
	default:
		m.stuckSince = time.Time{}
		return false, callStuckPoll
	}
	if now.Before(m.callStuckAt) || m.lightOpening || now.Before(m.lightRetryAt) {
		return false, callStuckPoll
	}
	if !callStuckAllow(now) {
		return false, callStuckPoll
	}
	if gap {
		callStuckGapN.Add(1)
	} else {
		callStuckBusyN.Add(1)
	}
	m.callStuckAt = now.Add(callStuckEvery)
	m.stuckOpenedAt = now
	m.stuckSince = time.Time{}
	m.stuckKeys = make([]any, 0, len(live))
	avoid := make([]any, 0, len(live)+len(m.deadKeys)+1)
	for _, s := range live {
		if !s.retired && !s.dead && !s.closeHS {
			m.stuckKeys = append(m.stuckKeys, s.key)
		}
		avoid = append(avoid, s.key)
	}
	avoid = append(append(avoid, m.deadKeys...), callStuckMark{path: path})
	m.lightOpening = true
	callStuckOpened.Add(1)
	m.tr.ev("call: stuck on path %s (gap %v busy %v): open a fresh sub off it", path, gap, busy)
	go m.openLight(avoid)
	return false, callStuckPoll
}

// callStuckMoveLocked: after a trigger, once a sub opened since has read a
// byte from the peer, give up the subs live at the trigger.
func (m *mConn) callStuckMoveLocked(live []*msub, now time.Time) bool {
	var bad []*msub
	fresh := false
	for _, s := range live {
		if s.retired || s.dead || s.closeHS {
			continue
		}
		if containsAny(m.stuckKeys, s.key) {
			bad = append(bad, s)
		} else if s.lastRecv.Load() >= m.stuckOpenedAt.UnixNano() {
			fresh = true
		}
	}
	switch {
	case len(bad) == 0:
		m.stuckKeys = nil
		return false
	case !fresh:
		if now.Sub(m.stuckOpenedAt) >= callStuckGiveUp {
			m.stuckKeys = nil // nothing came on a fresh sub: keep all, read anew
		}
		return false
	}
	m.stuckKeys = nil
	if !mconnCloseHS.Load() {
		return false
	}
	gave := false
	for _, s := range bad {
		if subCloseHS(s.rw) {
			m.closeReqLocked(s)
			callStuckMoved.Add(1)
			gave = true
		}
	}
	return gave
}

func containsAny(l []any, k any) bool {
	for _, a := range l {
		if a == k {
			return true
		}
	}
	return false
}
