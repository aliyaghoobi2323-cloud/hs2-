package engine

import (
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Light users off CUT paths (HS2_LB_RESCUE_CUT=1, edge, with HS2_LB_RESCUE;
// stuck59).
//
// Production (judge58, 2026-11-11 evening): cut-drain retired 965 links with
// 62,506 connections, ~84% stayed on them until they ended by themselves; the
// rescue's own line showed "copies given past the budget so far 0" all
// evening. The lab (scenario P) shows why: nearly every single-sub light
// connection is still raw (capMConnRaw), and the raw upgrade takes the rescue
// only with HS2_LB_RESCUE_RAW (off: on a merely lowered path its handshake
// costs resets); the dual ones all on lowered links are moved 100 a second
// (rescuePerSec), thousands refused by that bound.
//
// With the key on, for links on a path the dialing side reports CUT (not
// merely lowered):
//   - a light raw connection there gets its copy past the budget (the raw
//     rescue, for cut paths only — a cut path is already lost to it);
//   - those copies and the moves of dual connections all on marked links
//     take their own bound, rescueCutPerSec (HS2_LB_RESCUE_CUT_PER_SEC,
//     default 600), instead of rescuePerSec.
var lbRescueCut bool // loadEnvKeys: HS2_LB_RESCUE_CUT

var (
	rescueCutPerSec = int64(600)
	rescueCutSec    atomic.Int64
	rescueCutN      atomic.Int64
	// rescueCutRaw / rescueCutOther: copies given past the budget on cut
	// paths to raw / non-raw single-sub connections; rescueCutRefused: by
	// the rate bound.
	rescueCutRaw, rescueCutOther, rescueCutRefused atomic.Int64
	// diagnostics (always counted, Iran lite): light raw connections on a
	// marked link at an upgrade check — capped by the budget (no rescue
	// taken) / upgraded anyway (budget open).
	rescueDiagRawCapped, rescueDiagRawUp atomic.Int64
	// rescueDiagRawCappedCut (prod73): the capped checks on CUT paths (the
	// rest are on merely lowered paths, where the raw rescue is off without
	// HS2_LB_RESCUE_RAW); each such check takes the cut copy past the budget
	// (rescueCutRaw) unless the cut bound refused it (rescueCutRefused) or
	// the copy was refused for want of lite's own rule.
	rescueDiagRawCappedCut atomic.Int64
	// rescueCutLinks: links on cut paths (subset of rescueLow).
	rescueCutLinks sync.Map
)

func init() {
	if v, err := strconv.Atoi(os.Getenv("HS2_LB_RESCUE_CUT_PER_SEC")); err == nil && v > 0 {
		rescueCutPerSec = int64(v)
	}
	// b338: HS2_B338_RC_GAP0_MS sets the first per-connection gap; 0 turns the backoff off
	if v, err := strconv.Atoi(os.Getenv("HS2_B338_RC_GAP0_MS")); err == nil && v >= 0 {
		rcGap0 = time.Duration(v) * time.Millisecond
	}
}

// rescueCutKey reports whether a sub's key is on a cut path.
func rescueCutKey(key any) bool {
	if key == nil || !lbRescueCut {
		return false
	}
	_, c := rescueCutLinks.Load(key)
	return c
}

// rescueCutMark records the links on cut paths, as rescueMark.
func rescueCutMark(prev map[any]bool, cut []any) map[any]bool {
	next := make(map[any]bool, len(cut))
	for _, l := range cut {
		next[l] = true
		if _, had := rescueCutLinks.LoadOrStore(l, true); !had {
			rescueCutCount.Add(1) // b340 (lb_b340_skipcut.go)
		}
	}
	for l := range prev {
		if !next[l] {
			if _, had := rescueCutLinks.LoadAndDelete(l); had {
				rescueCutCount.Add(-1)
			}
		}
	}
	return next
}

// b338: per-connection backoff of the cut drop. A drop is always taken
// when the connection had none within rcReset; each further one waits
// rcGap0 << n (n: drops in a row), at most rcGapMax.
var (
	rcGap0   = 500 * time.Millisecond
	rcGapMax = 16 * time.Second
	rcReset  = 30 * time.Second
	// rescueCutDamped: cut drops held back by the per-connection backoff.
	rescueCutDamped atomic.Int64
)

func rcGapFor(n int) time.Duration {
	if n > 5 {
		n = 5
	}
	if g := rcGap0 << n; g < rcGapMax {
		return g
	}
	return rcGapMax
}

// rescueCutAllow takes one from the cut bound.
func rescueCutAllow(now time.Time) bool {
	sec := now.Unix()
	if old := rescueCutSec.Load(); old != sec && rescueCutSec.CompareAndSwap(old, sec) {
		rescueCutN.Store(0)
	}
	if rescueCutN.Add(1) > rescueCutPerSec {
		rescueCutRefused.Add(1)
		return false
	}
	return true
}

// rescueTake: the rescue's rate bound for a sub on key (the cut bound on a
// cut path with the key on, else rescueAllow).
func rescueTake(key any, now time.Time) bool {
	if rescueCutKey(key) {
		if !rescueCutAllow(now) {
			return false
		}
		rescueTotal.Add(1)
		return true
	}
	return rescueAllow(now)
}

// rescueRawLocked (Iran, rawTick, the connection already light by lite's
// rule and capped by the budget): whether its raw sub takes the rescue.
func (m *mConn) rescueRawLocked(now time.Time) bool {
	if !m.rescueWantLocked(m.raw1) {
		return false
	}
	rescueDiagRawCapped.Add(1)
	if rescueCutKey(m.raw1.key) {
		rescueDiagRawCappedCut.Add(1)
	}
	switch {
	case lbRescueRaw:
		return rescueTake(m.raw1.key, now)
	case rescueCutKey(m.raw1.key):
		if rescueTake(m.raw1.key, now) {
			rescueCutRaw.Add(1)
			return true
		}
	}
	return false
}

// rescueSingleLocked (Iran, mConn.tick, a light single-sub connection capped
// by the budget): whether its one sub takes the rescue.
func (m *mConn) rescueSingleLocked(live *msub, now time.Time) bool {
	if !m.rescueWantLocked(live) || !rescueTake(live.key, now) {
		return false
	}
	if rescueCutKey(live.key) {
		rescueCutOther.Add(1)
	}
	return true
}

// Census (diagnostics, Iran lite, with HS2_LB_RESCUE): connection-seconds
// (each connection's tick runs about once a mLitePoll) by where its live subs
// are: censusRaw a raw single sub on a marked link, censusSingle a single
// sub on a marked link, censusAllLow every sub of a dual on marked links,
// censusMixed a dual with a sub on a marked link and one off it (served by
// the good copy), (all cumulative).
var (
	censusRaw, censusSingle, censusAllLow, censusMixed atomic.Int64
	// rescueCutDrop: bad subs given up (CLOSE-REQ) by duals with a good
	// copy established (HS2_LB_RESCUE_CUT).
	rescueCutDrop atomic.Int64
	censusLogged  atomic.Int64 // unix second of the last census line
)

// rescueCensusLocked counts m's place at a tick. Caller holds m.mu.
func (m *mConn) rescueCensusLocked(live []*msub) {
	if !lbRescue.Load() {
		return
	}
	low := 0
	for _, s := range live {
		if rescueLowKey(s.key) {
			low++
		}
	}
	switch {
	case low == 0:
	case len(live) == 1:
		censusSingle.Add(1)
	case low == len(live):
		censusAllLow.Add(1)
	default:
		censusMixed.Add(1)
	}
}

// rescueCutDropLocked (HS2_LB_RESCUE_CUT, Iran lite, dual): a light
// connection with a sub on a CUT path and an established copy off every
// marked link gives up the bad sub through the close handshake (the exit
// stops sending on it and names its end offset: no byte rides only there),
// the good one becoming its primary. Never the last sub; one a tick. A
// connection that ever striped is left to the stripe code (every connection
// has stK with HS2_MCONN_STRIPE: that is not a gate). From
// then on nothing of it rides the cut path: an idle close (mLiteIdle) can no
// longer leave it single there, and the cut path's links empty. Returns
// whether a CLOSE-REQ was sent. Caller holds m.mu.
func (m *mConn) rescueCutDropLocked(live []*msub, now time.Time) bool {
	if !lbRescueCut || !lbRescue.Load() || !mconnCloseHS.Load() || len(live) < 2 || m.handshaking || m.heavy || m.wantBulk || m.mode != planeLight || m.stN > 0 || m.stActive || m.stOpening {
		return false
	}
	var bad, good *msub
	for _, s := range live {
		switch {
		case s.closeHS || s.bulk.Load():
		case rescueCutKey(s.key):
			if bad == nil && subCloseHS(s.rw) {
				bad = s
			}
		case !rescueLowKey(s.key) && s.lastRecv.Load() > 0:
			good = s // established: the peer's bytes came over it
		}
	}
	if bad == nil || good == nil {
		return false
	}
	if rcGap0 > 0 && now.Before(m.rcNext) {
		// b338 (lab/rb337 CY2): a call with one good copy while most links
		// sit on cut paths had each replacement placed on a cut link again
		// and given up at once: ~145 subs a second for 13 s on one call.
		// The bad copy waits (the good one carries the call).
		rescueCutDamped.Add(1)
		return false
	}
	if !rescueCutAllow(now) {
		return false
	}
	if !m.rcLast.IsZero() && now.Sub(m.rcLast) < rcReset {
		m.rcN++
	} else {
		m.rcN = 0
	}
	m.rcLast = now
	m.rcNext = now.Add(rcGapFor(m.rcN))
	rescueCutDrop.Add(1)
	m.prefPrimary = good
	m.closeReqLocked(bad)
	m.setPrimaryLocked()
	m.refreshLiteDualLocked()
	m.tr.ev("lite: rescue cut — sub %d on a cut path given up, sub %d primary", bad.idx, good.idx)
	return true
}
