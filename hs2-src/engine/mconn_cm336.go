package engine

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

// b336: HS2_B336_CALLMOVE (Iran, default on since rel69p-rc3, =0 off) — call re-placement.
//
// lab/rb331 v6 (48 probe calls): a call's two copies are placed at the
// call's start and stay on the same links for the whole call unless a path
// is cut; call p99 then follows the base loss of the paths they landed on
// (+138 ms per 1 % on the better copy's path, +62 ms per 1 % on the worse
// one's; 0.1 % path 120 ms, 0.4 % 171 ms).
//
// With the key on, Iran's manager tick (lite, the connection copies as a
// call, two live subs or more) compares each copy's path with the paths the
// steering judges (the smoothed loss of lb auto2's judge, q2Path.ewma, the
// dialing side's sender loss when its report is fresh), refreshed each
// steering tick (cmSnapLocked):
//
//   - the copy on the lossiest path, when that path's smoothed loss is at
//     least cmDelta (0.15 % absolute) above the best ELIGIBLE path none of
//     the call's other copies rides (a reading, not cut, not lowered, a usable
//     link), without a break for cmSustain (10 s), is moved:
//   - one fresh sub is opened with the call placement on that best path
//     (callMoveMark: only links of that path; none there: no sub, the move
//     ends); the old copy gets no call pair copy once two others are live
//     (callLate, callPairLocked) and is given up through the close
//     handshake (HS2_MCONN_CLOSE_HS: the exit stops sending on it and names
//     its end; nothing rides only there) once the fresh sub has read a byte
//     from the peer — the call never has fewer than two copies, nothing is
//     lost, and the receiver's ordered reassembly drops the duplicates;
//     nothing read within cmGiveUp: the old copy stays and keeps its copies;
//   - at most one move per call per cmGap (30 s), cmPerMin (20) per minute
//     over the process; never while a stall/darkcall/late rescue of the
//     connection runs or ran within cmGap, never while a sub is opening,
//     never without the close handshake on the old copy's link.
//
// HS2_CALLPATH_LOG=1 logs each move, give-up and completion.
var b336CallMove atomic.Bool

// cmHold (lab only, HS2_B338_CM_HOLD_MS, default 0): a move is not released
// before it is this old, so a lab can hit a pending move with a cut.
var cmHold time.Duration

func init() {
	b336CallMove.Store(envBoolDefault("HS2_B336_CALLMOVE", true))
	if v, err := strconv.Atoi(os.Getenv("HS2_B338_CM_HOLD_MS")); err == nil && v > 0 {
		cmHold = time.Duration(v) * time.Millisecond
	}
}

// Tunables (vars for tests).
var (
	cmDelta   = 0.0015           // loss fraction above the target path's
	cmSustain = 10 * time.Second // the gap unbroken for
	cmGap     = 30 * time.Second // per call
	cmGiveUp  = 5 * time.Second  // the fresh sub must read within
	cmFresh   = 5 * time.Second  // the snapshot's age limit
	cmPoll    = time.Second
	cmPerMin  = int64(20)
	// b338 damper (lab/rb337: with 3 % on all 13 paths a call moved every
	// cmGap, 5-6 times): the source must also be cmRatio x the target's
	// loss, the target under cmBestMax; a call that moved twice within
	// cmWindow waits cmGap << (moves in the window - 1), at most cmWindow.
	cmRatio   = 2.0
	cmBestMax = 0.01
	cmWindow  = 5 * time.Minute
)

// cmCanClose: the old copy's link reads CLOSE-REQ (a var for tests).
var cmCanClose = func(s *msub) bool { return mconnCloseHS.Load() && subCloseHS(s.rw) }

// counters (status/log)
var cmMoves, cmDone, cmGaveUp, cmRefused, cmNoLink atomic.Int64

// b338: moves abandoned because the old copy left before the fresh one read
// (given up by another rescue: closing / died / gone), and gaps held back by
// the relative damper (cmRatio, cmBestMax).
var cmAbandoned, cmDamped atomic.Int64

// cmPath is one path's quality in the snapshot.
type cmPath struct {
	loss float64 // smoothed loss fraction
	ok   bool    // eligible as a target
}

type cmSnap struct {
	at time.Time
	p  map[string]cmPath // path key (edge view "iranIP>exitIP", callPathOf's)
}

var cmQ atomic.Pointer[cmSnap]

var (
	cmMinute atomic.Int64
	cmMinN   atomic.Int64
)

// cmAllow takes one move from the per-minute bound.
func cmAllow(now time.Time) bool {
	mn := now.Unix() / 60
	if old := cmMinute.Load(); old != mn && cmMinute.CompareAndSwap(old, mn) {
		cmMinN.Store(0)
	}
	if cmMinN.Add(1) > cmPerMin {
		cmRefused.Add(1)
		return false
	}
	return true
}

// cmSnapLocked (steering tick, m.mu held): the judged paths' smoothed loss
// and target eligibility.
func (m *LinkManager) cmSnapLocked(byPath map[string][]*managedLink, now time.Time) {
	st := m.steer
	if st == nil || st.judge == nil {
		return
	}
	out := make(map[string]cmPath, len(byPath))
	for p, ls := range byPath {
		q := st.judge.p[p]
		if q == nil || !q.have {
			continue
		}
		usable := 0
		for _, ml := range ls {
			if ml.link.Alive() && !ml.draining && !ml.retiring && !ml.suspect && !ml.degraded {
				usable++
			}
		}
		ok := usable > 0 && !q.cut && !q.remCut && !st.repCut[p]
		if f, in := st.repFactor[p]; in && f < 0.95 {
			ok = false
		}
		if q.factor > 0 && q.factor < 0.95 {
			ok = false
		}
		out[p] = cmPath{loss: q.ewma, ok: ok}
	}
	cmQ.Store(&cmSnap{at: now, p: out})
}

// cmLogPart is the steering log's line (lb_rescue.go).
func cmLogLine() string {
	return fmt.Sprintf("lb callmove: moves %d, done %d, given up %d, rate-refused %d, no link on target %d, abandoned %d, damped %d",
		cmMoves.Load(), cmDone.Load(), cmGaveUp.Load(), cmRefused.Load(), cmNoLink.Load(), cmAbandoned.Load(), cmDamped.Load())
}

// callMoveMark in an avoid list asks the opener for the call placement on a
// link of path only.
type callMoveMark struct{ path string }

func takeCallMoveMark(avoid []any) ([]any, string, bool) {
	for i, a := range avoid {
		if c, ok := a.(callMoveMark); ok {
			return append(avoid[:i:i], avoid[i+1:]...), c.path, true
		}
	}
	return avoid, "", false
}

func (m *mConn) cmLogf(f string, a ...any) {
	if cp331On.Load() {
		log.Printf("%x callmove port %d "+f, append([]any{m.id, m.cp331.port}, a...)...)
	}
}

// cmBlockedLocked: a rescue of the connection runs (or ran within cmGap), or
// a sub is opening.
func (m *mConn) cmBlockedLocked(now time.Time) bool {
	return m.stuckKeys != nil || m.lateKey != nil || !m.stuckSince.IsZero() || m.rrPhase != 0 ||
		(!m.dcSwAt.IsZero() && now.Sub(m.dcSwAt) < cmGap) ||
		(!m.stuckOpenedAt.IsZero() && now.Sub(m.stuckOpenedAt) < cmGap) ||
		(!m.lateOpenedAt.IsZero() && now.Sub(m.lateOpenedAt) < cmGap) ||
		m.lightOpening || now.Before(m.lightRetryAt)
}

// callMoveLocked (Iran's manager, lite, two live subs or more, under m.mu).
// gave: a sub was given up; poll: look again within it (0: no need).
func (m *mConn) callMoveLocked(live []*msub, now time.Time) (gave bool, poll time.Duration) {
	if !b336CallMove.Load() || !m.iran || !m.lite || m.peerFin || m.wclosed || m.handshaking {
		m.cmSince, m.cmFrom = time.Time{}, ""
		return false, 0
	}
	if m.cmKey != nil {
		return m.callMoveDoneLocked(live, now), callStuckPoll
	}
	if !m.callOn {
		m.cmSince, m.cmFrom = time.Time{}, ""
		return false, 0
	}
	snap := cmQ.Load()
	if snap == nil || now.Sub(snap.at) > cmFresh || m.cmBlockedLocked(now) {
		m.cmSince, m.cmFrom = time.Time{}, ""
		return false, cmPoll
	}
	var worst *msub
	worstL, worstP := 0.0, ""
	used := map[string]bool{}
	n := 0
	for _, s := range live {
		if s.retired || s.dead || s.closeHS {
			continue
		}
		p := callPathOf(s.key)
		q, ok := snap.p[p]
		if p == "" || !ok {
			m.cmSince, m.cmFrom = time.Time{}, ""
			return false, cmPoll // a copy on an unjudged path: no comparison
		}
		used[p] = true
		n++
		if worst == nil || q.loss > worstL {
			worst, worstL, worstP = s, q.loss, p
		}
	}
	if n < 2 {
		m.cmSince, m.cmFrom = time.Time{}, ""
		return false, cmPoll
	}
	to, toL := "", 0.0
	for p, q := range snap.p {
		if !q.ok || used[p] {
			continue
		}
		if to == "" || q.loss < toL || q.loss == toL && p < to {
			to, toL = p, q.loss
		}
	}
	if to == "" || worstL-toL < cmDelta {
		m.cmSince, m.cmFrom = time.Time{}, ""
		return false, cmPoll
	}
	if worstL < cmRatio*toL || toL >= cmBestMax {
		// b338: all paths about as lossy (relative gap small, or the best
		// itself bad): a move buys little and ping-pongs
		if !m.cmDampOn {
			m.cmDampOn = true
			cmDamped.Add(1) // once per damped stretch of this call
		}
		m.cmSince, m.cmFrom = time.Time{}, ""
		return false, cmPoll
	}
	m.cmDampOn = false
	if m.cmFrom != worstP || m.cmSince.IsZero() {
		m.cmSince, m.cmFrom = now, worstP
	}
	if now.Sub(m.cmSince) < cmSustain || now.Before(m.cmAt) {
		return false, cmPoll
	}
	if !cmCanClose(worst) {
		return false, cmPoll
	}
	if !cmAllow(now) {
		return false, cmPoll
	}
	m.cmAt = now.Add(m.cmGapLocked(now))
	m.cmSince, m.cmFrom = time.Time{}, ""
	m.cmOpenedAt = now
	m.cmKey = avoidKey(worst.key)
	m.cmTo = to
	worst.callLate = true // no call pair copy on it once two others are live (callPairLocked)
	m.cmLive = m.cmLive[:0]
	avoid := make([]any, 0, len(live)+len(m.deadKeys)+1)
	for _, s := range live {
		avoid = append(avoid, avoidKey(s.key))
		m.cmLive = append(m.cmLive, avoidKey(s.key))
	}
	avoid = append(append(avoid, m.deadAvoidLocked()...), callMoveMark{path: to})
	m.lightOpening = true
	cmMoves.Add(1)
	m.tr.ev("call: move sub %d off %s (loss %.2f%%) to %s (%.2f%%)", worst.idx, worstP, 100*worstL, to, 100*toL)
	m.cmLogf("start sub %d %s loss %.2f%% -> %s %.2f%% [moves %d]", worst.idx, worstP, 100*worstL, to, 100*toL, cmMoves.Load())
	go m.openLight(avoid)
	return false, callStuckPoll
}

// cmGapLocked records a move start at now and returns the call's wait
// before the next one: cmGap, doubled for each further move within
// cmWindow (2 moves: 2 x cmGap, 3: 4 x ...), at most cmWindow.
func (m *mConn) cmGapLocked(now time.Time) time.Duration {
	h := m.cmHist[:0]
	for _, t := range m.cmHist {
		if now.Sub(t) < cmWindow {
			h = append(h, t)
		}
	}
	m.cmHist = append(h, now)
	g := cmGap
	for i := 1; i < len(m.cmHist) && g < cmWindow; i++ {
		g *= 2
	}
	if g > cmWindow {
		g = cmWindow
	}
	return g
}

// cmAbandonLocked ends a pending move whose old copy left before a fresh sub
// read (b338, lab/rb337 CY2: a rescue-cut drop gave the old copy up 350 ms
// after the trigger): the move is over, the fresh sub (if any) stays a
// normal copy, and the old one's state is not touched again.
func (m *mConn) cmAbandonLocked(fresh *msub) {
	why, idx := "gone", -1
	for _, s := range m.subs {
		if avoidKey(s.key) != m.cmKey {
			continue
		}
		idx = s.idx
		switch {
		case s.dead:
			why = "died"
		case s.closeHS:
			why = "given up by another rescue (close handshake)"
		case s.retired:
			why = "retired"
		}
		s.callLate = false
		break
	}
	fs := "none read yet"
	if fresh != nil {
		fs = fmt.Sprintf("%d on %s", fresh.idx, callPathOf(fresh.key))
	} else {
		for _, s := range m.subs {
			if !s.retired && !s.dead && !containsAny(m.cmLive, avoidKey(s.key)) {
				fs = fmt.Sprintf("%d on %s (no data yet)", s.idx, callPathOf(s.key))
				break
			}
		}
	}
	m.cmKey, m.cmLive, m.cmTo = nil, m.cmLive[:0], ""
	cmAbandoned.Add(1)
	m.tr.ev("call: move abandoned, old sub %d %s, fresh sub %s", idx, why, fs)
	m.cmLogf("abandon old sub %d %s before the fresh sub read; fresh sub %s [abandoned %d]", idx, why, fs, cmAbandoned.Load())
}

// callMoveDoneLocked: once a sub opened since the trigger has read data
// bytes from the peer (rxEnd > 0), give up the old copy through the close handshake.
func (m *mConn) callMoveDoneLocked(live []*msub, now time.Time) bool {
	var old *msub
	var fresh *msub
	for _, s := range live {
		if s.retired || s.dead || s.closeHS {
			continue
		}
		if avoidKey(s.key) == m.cmKey {
			old = s
			continue
		}
		// rb336 lab: lastRecv is set when the sub opens (the move completed
		// 1-4 ms after the trigger, before the exit had sent anything on
		// it); the fresh sub must have read data bytes from the peer.
		if !containsAny(m.cmLive, avoidKey(s.key)) && s.lastRecv.Load() >= monoNano(m.cmOpenedAt) && s.rxAny.Load() {
			fresh = s
		}
	}
	if fresh != nil && now.Sub(m.cmOpenedAt) < cmHold {
		fresh = nil // lab hook: the move stays pending
	}
	switch {
	case old == nil:
		m.cmAbandonLocked(fresh)
		return false
	case fresh == nil:
		if now.Sub(m.cmOpenedAt) >= cmGiveUp+cmHold {
			m.cmKey = nil
			old.callLate = false
			cmGaveUp.Add(1)
			m.cmLogf("give up sub %d: no fresh sub read within %v (target %s)", old.idx, cmGiveUp, m.cmTo)
		}
		return false
	}
	m.cmKey = nil
	if !cmCanClose(old) {
		old.callLate = false
		return false
	}
	m.closeReqLocked(old)
	cmDone.Add(1)
	m.tr.ev("call: moved, old sub %d given up (fresh sub %d on %s)", old.idx, fresh.idx, callPathOf(fresh.key))
	m.cmLogf("done old sub %d closed, fresh sub %d on %s after %v [done %d]", old.idx, fresh.idx, callPathOf(fresh.key), now.Sub(m.cmOpenedAt).Round(time.Millisecond), cmDone.Load())
	return true
}
