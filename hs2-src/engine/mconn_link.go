package engine

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xtaci/smux"
)

// HS2_MCONN=1: a user TCP connection rides several sub-streams on different
// links of the pool (mConn, mconn.go) instead of one stream on one link, so a
// light connection never stalls behind a heavy one or behind one link's loss
// recovery, and a heavy one moves off the light links. This file is the
// tunnel side of it:
//
//   - Wire: a server announces capMConn over kindInfo only with the key on.
//     The edge opens kindMConn sub-streams only when the key is on AND the
//     exit announced capMConn; otherwise (and when no sub can be opened at
//     all) it uses the one kindTCP/kindTCPPort stream, byte for byte as
//     before. Every exit that has this code reads kindMConn whatever its key.
//     Sub header: [kindMConn][port u16][conn id u64][role u8]; role 0 is the
//     first sub (the exit creates the connection and dials the target), role
//     1 a join (the exit attaches it, holding it up to mconnJoinWait if it
//     came before role 0). Role 2 (lite, the exit announced capMConnRaw) is a
//     first sub whose bytes are raw until an upgrade (mconn_raw.go).
//   - Edge: the opener places every sub with the pool's own bookkeeping (a
//     sub counts as one user connection on its link and is released when it
//     closes). planeLight: a link that carries no heavy stream (the pool's
//     own heavy measure: heavyRate, or queue-building with HS2_HEAVY_BACKLOG),
//     the least loaded one; failing that, any other live link. planeBulk: a
//     link that already carries heavy streams and is not pressed, so the
//     downloads gather on few links and the others stay clean; failing that,
//     the least loaded link.
//   - Lane (autopilot, HS2_PICK_LANE): its floor is the serving links with a
//     heavy stream plus `lane` free ones. Bulk subs gathering on the links
//     that are already heavy keep that count, and with it the floor, low;
//     light subs avoid heavy links, so they land on the lane's free links.
//   - Exit: a table of open connections by id; each sub counts toward its
//     link's heavy streams (exitTraffic.openSub), the connection itself once
//     as a user connection.
//
// UDP and the TUN side channel are not touched.

// mconnOn: HS2_MCONN=1 or HS2_MCONN=lite (atomic so tests can switch it).
// mconnLite: HS2_MCONN=lite, the adaptive-redundancy mode (mconn.go, "lite").
var mconnOn, mconnLite atomic.Bool

func setMConnMode(v string) {
	mconnOn.Store(v == "1" || v == "lite")
	mconnLite.Store(v == "lite")
}

// mconnCap is the capMConn bit this server announces (0 with the key off, so
// its kindInfo bytes stay as before).
func mconnCap() uint8 {
	if mconnOn.Load() {
		return capMConn | capMConnRaw
	}
	return 0
}

const (
	mconnRoleFirst byte = 0
	mconnRoleJoin  byte = 1
	// mconnRoleFirstRaw: a lite first sub that carries the user's bytes raw
	// until an upgrade (mconn_raw.go); only to an exit announcing capMConnRaw.
	mconnRoleFirstRaw byte = 2
	mconnHdrLen            = 1 + 2 + 8 + 1

	// mconnFirstWait: how long the first sub waits for a link (as pickWait).
	// mconnNextWait: a later sub (a second light one, a bulk one) waits less;
	// the connection runs on what it has meanwhile.
	mconnFirstWait = 6 * time.Second
	mconnNextWait  = 2 * time.Second
	mconnPickGap   = 150 * time.Millisecond
)

// mconnJoinWait: how long the exit holds a join (role 1) that arrived before
// its connection's first sub. A variable so tests can shorten it.
var mconnJoinWait = 5 * time.Second

var errMConnNoLink = errors.New("mconn: no link for a sub")

// mconnHeader opens a sub on the wire.
func mconnHeader(port int, id uint64, role byte) []byte {
	if port < 0 || port > 65535 {
		port = 0
	}
	b := make([]byte, mconnHdrLen)
	b[0] = kindMConn
	binary.BigEndian.PutUint16(b[1:3], uint16(port))
	binary.BigEndian.PutUint64(b[3:11], id)
	b[11] = role
	return b
}

func newMConnID() uint64 {
	var b [8]byte
	crand.Read(b[:])
	return binary.BigEndian.Uint64(b[:])
}

// ---- counters (hs2 status) ------------------------------------------------

var mconnStat struct {
	open       atomic.Int64  // connections open (edge: users; exit: relays)
	light      atomic.Int64  // edge: subs open on light links
	bulk       atomic.Int64  // edge: subs open on bulk links
	exitSubs   atomic.Int64  // exit: subs attached (the plane is not on the wire)
	total      atomic.Uint64 // connections ever opened
	joinMissed atomic.Uint64 // exit: joins whose connection never came (or had ended)
}

// MConnSnap is the multi-link connections' state now (HS2_MCONN).
type MConnSnap struct {
	Open, SubsLight, SubsBulk, SubsExit int64
	Total, JoinMissed                   uint64
	// fx168/fx170: connections reset because their data was cut — exit
	// targets (mConn upload), edge users (mConn download), and either end
	// of the one-stream path
	TargetRst, UserRst, StreamRst uint64
	// hc183/hc213 half-close (halfclose.go): relays half-closed, of them
	// reset for halfCloseIdle, over halfCloseMax, ended because the peer
	// dropped its side, EOFs taken as a cut (the peer's "dropped" mark), and
	// relays waiting half-closed now
	HalfCloses, HalfCloseIdles, HalfCloseOver, HalfCloseGone, HalfCloseAborts uint64
	HalfOpen                                                                  int64
}

// MConnObserved returns the multi-link connection counters now.
func MConnObserved() MConnSnap {
	return MConnSnap{Open: mconnStat.open.Load(), SubsLight: mconnStat.light.Load(), SubsBulk: mconnStat.bulk.Load(),
		SubsExit: mconnStat.exitSubs.Load(), Total: mconnStat.total.Load(), JoinMissed: mconnStat.joinMissed.Load(),
		TargetRst: mconnTargetRst.Load(), UserRst: mconnUserRst.Load(), StreamRst: streamLocalRst.Load(),
		HalfCloses: halfCloses.Load(), HalfCloseIdles: halfCloseIdles.Load(), HalfCloseOver: halfCloseOver.Load(),
		HalfCloseGone: halfCloseGone.Load(), HalfCloseAborts: halfCloseAborts.Load(), HalfOpen: halfOpenNow.Load()}
}

func mconnPlaneGauge(p mPlane) *atomic.Int64 {
	if p != planeLight { // bulk, stripe
		return &mconnStat.bulk
	}
	return &mconnStat.light
}

// ---- edge: choosing a link for a sub --------------------------------------

// linkMConn: the exit on ml's link reads kindMConn. A link whose own kindInfo
// answer has not come yet goes by the pool's (as userStreamHeader).
func linkMConn(ml *managedLink, pool *peerInfo) bool {
	if ml.mtr == nil {
		return false
	}
	pi := ml.mtr.peerInfo.Load()
	if pi == nil && !ml.mtr.infoRefused.Load() {
		pi = pool
	}
	return pi.mconn()
}

func avoided(l Link, avoid []any) bool {
	k := avoidKey(l) // mem70: *mtcpLink entries are IDs (mconn_avoidkey.go)
	for _, a := range avoid {
		if a == k || a == any(l) {
			return true
		}
	}
	return false
}

// planeKey orders candidate links for a sub; smaller is better. class is the
// plane's own preference (light: no heavy stream; bulk: heavy and not
// pressed), then the pool's usual order (pickKey).
type planeKey struct {
	class int
	pk    pickKey
}

func (a planeKey) less(b planeKey) bool {
	if a.class != b.class {
		return a.class < b.class
	}
	return a.pk.less(b.pk)
}

func (m *LinkManager) planeKeyLocked(p mPlane, ml *managedLink) planeKey {
	return m.planeKeyFP(p, ml, ml.fpPicks.Load())
}

// planeKeyFP is planeKeyLocked with the link's fast-path placements read as
// fp (lm_fastpick.go).
func (m *LinkManager) planeKeyFP(p mPlane, ml *managedLink, fp int32) planeKey {
	picks := ml.picks + int(fp)
	k := planeKey{pk: newPickKey(ml.pressed, ml.flowing, picks, picks+ml.histPicks(), int(ml.users.Load()), m.perLink)}
	switch p {
	case planeBulk:
		// The pool's usual spread (pressed last, then least loaded): packing
		// the bulk subs onto the links that already carry downloads put 32
		// downloads on a few links, and under the path's loss one TCP link
		// carries only so much — 10% less throughput, and whatever shared
		// those links waited seconds (lab, 300 light + 32 bulk, 1% loss).
		// Light subs keep off downloads by their own order (pk.heavy).
	default:
		// The pool's own order (pickLocked with pickClass): a link that is
		// not pressed (nor took a burst of placements) first, then one with
		// no heavy stream, then the least loaded. "No heavy" must not come
		// before "not pressed": then every light sub piled onto the few
		// links without a download (300 light connections on 2–3 links in
		// the lab: a game's p50 81 → 381 ms and up to 7 s, 10% less
		// throughput).
		k.pk.heavy = ml.heavy > 0 || stripeOn(ml)
		k.pk.pressed = k.pk.pressed || stripeGuardPressed(ml) // HS2_MCONN_STRIPE_RES_GUARD (mconn_stripe_resguard.go)
	}
	return k
}

// pickPlane places a sub of plane p on a link not in avoid whose exit reads
// kindMConn, and returns the release func of that placement. Serving links
// first; then healthy retiring ones; then any (as pickLocked). Ties are
// broken at random.
func (m *LinkManager) pickPlane(p mPlane, avoid []any) (Link, func(), bool) {
	pool := m.exitInfo.Load()
	if lmFastPick.Load() {
		if l, rel, ok, done := m.pickPlaneFast(p, avoid, pool); done {
			return l, rel, ok // HS2_LM_FASTPICK (lm_fastpick.go)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ml := m.pickPlaneLocked(p, avoid, pool)
	if ml == nil {
		return nil, nil, false
	}
	if stripeLog && p == planeLight {
		stripeNoteLight(ml)
	}
	m.placeLocked(ml)
	return ml.link, m.releaseFor(ml), true
}

func (m *LinkManager) pickPlaneLocked(p mPlane, avoid []any, pool *peerInfo) *managedLink {
	ml, _, dup := m.pickPlaneLockedFP(p, avoid, pool)
	dup.count()
	return ml
}

// dupNote is where pickPlaneLockedFP's light sub went for countDupPath:
// dupNone (not counted), dupSame or dupOther.
type dupNote int8

const (
	dupNone dupNote = iota
	dupSame
	dupOther
)

func dupNoteOf(other bool) dupNote {
	if other {
		return dupOther
	}
	return dupSame
}

func (d dupNote) count() {
	if d != dupNone {
		countDupPath(d == dupOther, false)
	}
}

// pickPlaneLockedFP is pickPlaneLocked without the counting: it returns the
// link, its fast-path placements as read while ranking it, and the
// countDupPath note for the caller to record once the placement holds. It
// only reads the pool (a read lock is enough).
func (m *LinkManager) pickPlaneLockedFP(p mPlane, avoid []any, pool *peerInfo) (*managedLink, int32, dupNote) {
	if p != planeLight {
		ml, fp := m.pickPlaneTiersFP(p, avoid, pool, nil)
		return ml, fp, dupNone
	}
	if cs229MinPlace() && cs226LitePath() && !dupPathsOn { // cs229: original pick, re-pick only on a shared path
		return m.cs229LiteLocked(p, avoid, pool)
	}
	// HS2_DUP_PATHS (dup_paths.go): a light sub placed beside live ones goes
	// to another healthy path first.
	skip, healthy := m.dupPathsLocked(avoid)
	if healthy != nil && cs228Place() && !dupPathsOn { // HS2_DUP_PATHS=1 keeps its unconditional rule
		healthy = m.cs228PlaceQualLocked(skip, healthy) // cs228: no path worse than the primary's
	}
	if skip == nil {
		ml, fp := m.pickPlaneTiersFP(p, avoid, pool, nil)
		return ml, fp, dupNone
	}
	// Order: a link carrying no download on another path; then one carrying
	// no download on any path; then another path; then the usual pick. A
	// light copy behind a download waits in that download's queue, which
	// costs it more than sharing a path (lab: games did not improve while
	// the other path came first).
	otherPath := func(ml *managedLink) bool {
		return healthy[mlPath(ml)] && !ml.degraded && !ml.draining && !ml.suspect && !m.pathCooledLocked(ml) &&
			!cs226LowLink(ml.link) // cs226: not a path the balancer lowered/cut
	}
	noDownload := func(ml *managedLink) bool {
		return ml.heavy == 0 && !stripeOn(ml) && !ml.degraded && !ml.draining && !ml.suspect
	}
	if healthy != nil {
		if ml, fp := m.pickPlaneTiersFP(p, avoid, pool, func(ml *managedLink) bool { return otherPath(ml) && noDownload(ml) }); ml != nil {
			return ml, fp, dupOther
		}
	}
	if ml, fp := m.pickPlaneTiersFP(p, avoid, pool, noDownload); ml != nil {
		return ml, fp, dupNoteOf(!skip[mlPath(ml)])
	}
	if healthy != nil {
		if ml, fp := m.pickPlaneTiersFP(p, avoid, pool, otherPath); ml != nil {
			return ml, fp, dupOther
		}
	}
	ml, fp := m.pickPlaneTiersFP(p, avoid, pool, nil)
	if ml != nil {
		return ml, fp, dupNoteOf(!skip[mlPath(ml)])
	}
	return nil, 0, dupNone
}

// pickPlaneTiers is pickPlaneLocked's search over the links ok accepts (nil:
// all); in the first pass (tier 0) ok only narrows the usual candidates.
func (m *LinkManager) pickPlaneTiers(p mPlane, avoid []any, pool *peerInfo, ok func(*managedLink) bool) *managedLink {
	ml, _ := m.pickPlaneTiersFP(p, avoid, pool, ok)
	return ml
}

// pickPlaneTiersFP is pickPlaneTiers, also returning the chosen link's
// fast-path placements as read while ranking it (lm_fastpick.go).
func (m *LinkManager) pickPlaneTiersFP(p mPlane, avoid []any, pool *peerInfo, ok func(*managedLink) bool) (*managedLink, int32) {
	// HS2_LB_STEER: the path this sub should take (light: the best paths).
	drawn := m.steerDrawLocked(p == planeLight, func(ml *managedLink) bool {
		return ml.link.Alive() && m.infoReadyLocked(ml) && ml.serving() && linkMConn(ml, pool) &&
			!avoided(ml.link, avoid) && (ok == nil || ok(ml)) && !stripeOn(ml)
	})
	for tier := 0; tier < 3; tier++ {
		var chosen *managedLink
		var chosenFP int32
		var best planeKey
		ties := 0
		for _, ml := range m.links {
			if !ml.link.Alive() || !m.infoReadyLocked(ml) {
				continue
			}
			if !linkMConn(ml, pool) || avoided(ml.link, avoid) || ok != nil && !ok(ml) {
				continue
			}
			switch tier {
			case 0:
				if ml.retiring || ml.degraded || ml.draining || ml.suspect {
					continue
				}
			case 1:
				if ml.degraded || ml.draining || ml.suspect {
					continue
				}
			}
			fp := ml.fpPicks.Load()
			k := m.planeKeyFP(p, ml, fp)
			k.pk.path = m.steerRankLocked(ml, drawn)
			k.pk.cutNew = m.steerCutNewLocked(ml)
			if stripeOn(ml) {
				k.pk.path = stripeSteerRank // HS2_MCONN_STRIPE: after every path's other links
			}
			switch {
			case chosen == nil || k.less(best):
				chosen, chosenFP, best, ties = ml, fp, k, 1
			case !best.less(k):
				ties++
				if pickTie(ties) {
					chosen, chosenFP = ml, fp
				}
			}
		}
		if chosen != nil {
			return chosen, chosenFP
		}
	}
	return nil, 0
}

// ---- edge: one user connection --------------------------------------------

// mconnSub is a sub as the edge opened it: closing it releases its place on
// the link (once).
type mconnSub struct {
	stream
	raw     bool // a raw first sub (mconnRoleFirstRaw)
	rescue  bool // ... opened with role 3 (HS2_MCONN_RESCUE, mconn_rescue.go)
	rescRec bool // ... toward an exit announcing capExtMConnRescueRec (er97)
	closeHS bool // its exit reads CLOSE-REQ (flagMConnCloseHS, mconn_closehs.go)
	evacHS  bool // a stripe sub whose exit reads CLOSE-REQ (HS2_MCONN_STRIPE_EVAC_DOWN, mconn_stripe_evac.go)
	plane   mPlane
	once    sync.Once
	release func()
	edge    *mconnEdge // forgets s when it closes (mem64)
}

func (s *mconnSub) mconnRaw() bool { return s.raw }

func (s *mconnSub) mconnCloseHS() bool { return s.closeHS }

func (s *mconnSub) mconnRescue() bool { return s.rescue }

func (s *mconnSub) mconnRescueRec() bool { return s.rescRec }

// SetWriteDeadline cuts a raw write stuck on a dead sub (a rescue,
// mconn_rescue.go).
func (s *mconnSub) SetWriteDeadline(t time.Time) error {
	if d, ok := s.stream.(interface{ SetWriteDeadline(time.Time) error }); ok {
		return d.SetWriteDeadline(t)
	}
	return errors.ErrUnsupported
}

func (s *mconnSub) mconnEvacHS() bool { return s.evacHS }

// SetReadDeadline wakes a blocked raw read (mconn_raw.go). Only a raw first
// sub uses it, and only on a stream that has it (rawCapable).
func (s *mconnSub) SetReadDeadline(t time.Time) error {
	if d, ok := s.stream.(readDeadliner); ok {
		return d.SetReadDeadline(t)
	}
	return errors.ErrUnsupported
}

func (s *mconnSub) Close() error {
	err := s.stream.Close()
	s.once.Do(func() {
		s.release()
		mconnPlaneGauge(s.plane).Add(-1)
		if s.edge != nil {
			s.edge.forget(s)
		}
	})
	return err
}

// mconnEdge is the edge's side of one connection: its opener and its subs.
type mconnEdge struct {
	ctx  context.Context
	lm   *LinkManager
	port int
	id   uint64

	mu     sync.Mutex  // serialises open (the first sub is role 0) and end
	first  bool        // a role-0 sub went out
	subs   []*mconnSub // the open subs (a closed one forgets itself)
	opened int         // subs ever opened (end's result)
	ended  bool

	// diagnostics (HS2_MCONN_STRIPE_LOG): the first sub's placement
	pickDur  time.Duration
	pickLink string
}

// open is the connection's mOpener.
func (e *mconnEdge) open(p mPlane, avoid []any) (mSub, any, error) {
	if p == planeStripe {
		return e.openStripe(avoid) // mconn_stripe.go
	}
	avoid, fresh := takeRescueFreshMark(avoid) // er94 (mconn_rescue.go)
	avoid, rstJ := takeRstJoinMark(avoid)      // rr156 (mconn_rescue_rst.go)
	// mem70: the wait counts from the call, not from getting e.mu: an open
	// queued behind another one's wait is bounded by its own wait too.
	t0 := time.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	wait := mconnNextWait
	if !e.first {
		wait = mconnFirstWait
	}
	deadline := t0.Add(wait)
	for tries := 0; !e.ended && tries < 3; {
		var l Link
		var rel func()
		var ok, raw bool
		if !e.first && mconnLite.Load() {
			// lite: the first sub sits exactly where the one stream of the
			// old path would (pickWait), so the connection's placement is
			// unchanged; only a link whose exit reads kindMConn will do.
			l, rel, raw, ok = e.pickFirstLite()
		} else {
			l, rel, ok = e.lm.pickPlaneCall(p, avoid) // HS2_MCONN_CALL_AVOID (mconn_call.go)
		}
		if !ok {
			if time.Now().After(deadline) || !sleepCtx(e.ctx, mconnPickGap) {
				break
			}
			continue
		}
		tries++ // a link can die between the pick and the open: try another
		st, err := l.OpenStream()
		if err == nil {
			autoWinOffForStripe(e.lm, st) // HS2_SMUX_AUTOWIN (smux_autowin.go)
			dl76Mark(st)
			role := mconnRoleJoin
			if fresh && e.first {
				role = mconnRoleRescueJoin
			}
			if rstJ && e.first && rstLinkOK(l) {
				role = mconnRoleRstJoin // rr156: the exit answers it, with an RST when it has no conn
			}
			if !e.first {
				role = mconnRoleFirst
				if raw && rawCapable(st) {
					role = mconnRoleFirstRaw
					if rescueOn.Load() && rescuePeerOK(l) {
						role = mconnRoleFirstRawRescue // HS2_MCONN_RESCUE (mconn_rescue.go)
					}
				}
			}
			if _, err = st.Write(mconnHeader(e.port, e.id, role)); err == nil {
				if (role == mconnRoleJoin || role == mconnRoleRescueJoin || role == mconnRoleRstJoin) && p == planeLight {
					if cs, ok := st.(*countedStream); ok {
						cs.copyOnly.Store(true) // a copy of the first sub's bytes: not demand
					}
				}
				e.first = true
				s := &mconnSub{stream: st, plane: p, release: rel, raw: role == mconnRoleFirstRaw || role == mconnRoleFirstRawRescue, rescue: role == mconnRoleFirstRawRescue, rescRec: role == mconnRoleFirstRawRescue && rescueRecOn.Load() && rescueRecPeerOK(l), closeHS: peerCloseHS(l), edge: e}
				mconnPlaneGauge(p).Add(1)
				e.subs = append(e.subs, s)
				e.opened++
				return s, l, nil
			}
			st.Close()
		}
		rel()
	}
	return nil, nil, errMConnNoLink
}

// pickFirstLite places a lite connection's first sub with the old path's own
// choice (pickWait); false when that link's exit does not read kindMConn
// (the caller then falls back to the one stream, as with the key off).
func (e *mconnEdge) pickFirstLite() (Link, func(), bool, bool) {
	tp := time.Now()
	l, rel, ok := pickWait(e.ctx, e.lm, true)
	if !ok {
		return nil, nil, false, false
	}
	if stripeLog {
		e.pickDur, e.pickLink = time.Since(tp), e.lm.stripeDescribe(l)
	}
	pi := e.lm.exitInfo.Load()
	if mtr := linkMeterOf(l); mtr != nil {
		if lpi := mtr.peerInfo.Load(); lpi != nil || mtr.infoRefused.Load() {
			pi = lpi
		}
	}
	if !pi.mconn() {
		rel()
		return nil, nil, false, false
	}
	return l, rel, pi.mconnRaw(), true
}

// forget drops a closed sub from e.subs. mem64: subs were only dropped at
// end, so a long-lived connection whose subs were retired, re-opened or lost
// with their link (every link kill, every idle retire) kept every closed sub
// it ever had, and each pinned its smux stream and, through it, the stream's
// session: a dead link's whole session (streams, buffered frames, TLS
// buffers). The heap grew with time and link turnover, not with users.
func (e *mconnEdge) forget(s *mconnSub) {
	e.mu.Lock()
	if i := slices.Index(e.subs, s); i >= 0 {
		last := len(e.subs) - 1
		e.subs[i] = e.subs[last]
		e.subs[last] = nil
		e.subs = e.subs[:last]
	}
	e.mu.Unlock()
}

// end closes every sub the connection opened (releasing their places) and
// refuses later opens. It returns how many subs were opened.
func (e *mconnEdge) end() int {
	e.mu.Lock()
	e.ended = true
	subs := e.subs
	e.subs = nil
	n := e.opened
	e.mu.Unlock()
	for _, s := range subs {
		s.Close()
	}
	return n
}

// mCloseWaitEnd bounds how long serveUserMConn waits after Close for the
// queued bytes and the FIN to be written before it closes the subs (fn164).
// Most conns end within a few ms (the records go into the sub streams'
// buffers); a sub blocked on its window (a slow link) needs longer. fx168
// (audit rel66 H2): 1 s still cut uploads whose tail took longer to drain —
// the exit saw its subs die without a FIN and the target a clean FIN on a
// truncated upload. The manager itself ends the conn when everything is
// written (clean) or at mCloseLinger with an RST to the peer; wait for that,
// plus the RST's own time and a margin.
const mCloseWaitEnd = mCloseLinger + mRSTWait + 500*time.Millisecond

// mCloseWaitMax caps the connections waiting in serveUserMConn after Close
// (each holds a goroutine; the conn's queue is held by its manager anyway
// until mCloseLinger). Over the cap a closer waits only mCloseWaitShort and
// then resets what is left (an RST, never a silent cut).
const (
	mCloseWaitMax   = 8192
	mCloseWaitShort = time.Second
)

// testHookUserMConnClosed (tests only) runs right after serveUserMConn's
// mc.Close. Atomic (fx171): a test sets it while warm-up connections of
// the same tunnel may still be closing (go test -race flagged the plain var).
var testHookUserMConnClosed atomic.Pointer[func()]

// mconnCloseWaiting counts the connections waiting in closeWaitEnd.
var mconnCloseWaiting atomic.Int64

// waitMConnDie waits until mc ended, at most d; false: it had not.
func waitMConnDie(mc *mConn, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-mc.GetDieCh():
		return true
	case <-t.C:
		return false
	}
}

// closeWaitEnd (fx168) waits, after mc.Close, until the manager ended mc —
// the tail and the FIN written, or an RST at mCloseLinger — bounded by
// mCloseWaitEnd (mCloseWaitShort over mCloseWaitMax waiters). A conn still
// not ended then is aborted: the peer gets an RST, and the exit resets its
// target rather than closing it cleanly on a truncated upload.
func closeWaitEnd(mc *mConn) {
	d := mCloseWaitEnd
	if mconnCloseWaiting.Add(1) > mCloseWaitMax {
		d = mCloseWaitShort
	}
	ok := waitMConnDie(mc, d)
	mconnCloseWaiting.Add(-1)
	if !ok {
		mc.abort()
	}
}

// serveUserMConn carries a user connection as an mConn. false: no sub could
// be opened at all and nothing was sent — the caller uses the one stream.
func serveUserMConn(ctx context.Context, user io.ReadWriteCloser, lm *LinkManager, port int) bool {
	t0 := time.Now()
	e := &mconnEdge{ctx: ctx, lm: lm, port: port, id: newMConnID()}
	mc, err := newMConnIran(e.id, e.open)
	if err == nil {
		mc.mu.Lock()
		mc.rstEndFn = e.sendEndNotice // rr157 (mconn_rescue_rst.go)
		mc.mu.Unlock()
	}
	if err != nil {
		if e.end() == 0 {
			return false
		}
		obs.openErr.Add(1)
		user.Close()
		return true
	}
	obs.openLat.observe(uint64(time.Since(t0).Microseconds()))
	mc.tr.setPort(port)
	// HS2_MCONN_STRIPE needs HS2_MCONN_DEADLINE_FIX: it upgrades raw
	// connections at full rate, where the stale wake deadline reset 4 of ~30
	// striped downloads in the lab without the fix
	if k := int(stripeK.Load()); k > 0 && mconnLite.Load() && mconnDeadlineFix.Load() && lm.exitInfo.Load().mconnStripe() {
		mc.setStripe(k, &lm.peak) // HS2_MCONN_STRIPE (mconn_stripe.go)
		if stripeUp.Load() && lm.exitInfo.Load().mconnStripeUp() {
			mc.setStripeUp() // HS2_MCONN_STRIPE_UP (mconn_stripe_up.go)
		}
	}
	mconnStat.total.Add(1)
	mconnStat.open.Add(1)
	defer mconnStat.open.Add(-1)
	tOpen := time.Since(t0)
	if relayStreamHC(user, mc, nil, hcIf(lm.exitInfo.Load().halfClose(), hcEdge)) { // hc183
		mconnUserRst.Add(1) // fx170: the download was cut; the user got an RST
	}
	mc.Close()
	if h := testHookUserMConnClosed.Load(); h != nil {
		(*h)()
	}
	// fn164: e.end closes every sub at once. Right after Close the user's
	// last bytes and the FIN were often still queued: the exit saw its subs
	// die without a FIN (errMConnSubsDied) and the target got a truncated
	// upload. The manager ends the conn (die closed) once the queue and the
	// FIN are written, or at mCloseLinger; wait for that, bounded (fx168:
	// the whole linger, not 1 s).
	closeWaitEnd(mc)
	e.end()
	if stripeLog {
		if fr := mc.firstRx.Load(); fr != 0 {
			stripeLogf("newconn: pick=%v open=%v first=%v link=[%s]", e.pickDur.Round(time.Millisecond), tOpen.Round(time.Millisecond),
				time.Duration(fr-monoNano(t0)).Round(time.Millisecond), e.pickLink)
		}
	}
	return true
}

// ---- exit -----------------------------------------------------------------

// mconnTable is the exit's open multi-link connections by id. A join that
// arrives before its connection's first sub waits in a slot for it.
type mconnTable struct {
	mu sync.Mutex
	m  map[uint64]*mconnSlot
}

type mconnSlot struct {
	mc      *mConn        // nil until the first sub came
	pub     bool          // ul173: mc has its first sub attached (joins may attach)
	ready   chan struct{} // closed when mc is published
	waiters int           // joins waiting for mc
}

func newMConnTable() *mconnTable { return &mconnTable{m: map[uint64]*mconnSlot{}} }

// exitMConns is the table of an exit whose config has none (tests that call
// serveStream directly).
var exitMConns = newMConnTable()

// create registers mc as connection id and publishes it at once (reserve +
// publish). false: the id is taken by a live connection.
func (t *mconnTable) create(id uint64, mc *mConn) bool {
	if !t.reserve(id, mc) {
		return false
	}
	t.publish(id, mc)
	return true
}

// reserve registers mc as connection id (its first sub came) without letting
// joins attach yet: the caller attaches the first sub, then publishes.
// false: the id is taken by a live connection.
//
// ul173: create used to wake the joins before the first sub was attached. A
// join could then become the connection's only sub; when it ended empty —
// Iran's openLight/openBulk close a sub that opens after the user closed, so
// its stream carries the header and a FIN only — the exit ended the
// connection ("every sub died") just before the first sub, carrying the whole
// upload, attached: the upload was lost whole, and the exit reset its target.
func (t *mconnTable) reserve(id uint64, mc *mConn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.m[id]
	if s == nil {
		s = &mconnSlot{ready: make(chan struct{})}
		t.m[id] = s
	} else if s.mc != nil {
		return false
	}
	s.mc = mc
	return true
}

// publish lets the joins of a reserved connection attach.
func (t *mconnTable) publish(id uint64, mc *mConn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s := t.m[id]; s != nil && s.mc == mc && !s.pub {
		s.pub = true
		close(s.ready)
	}
}

// join returns connection id, waiting up to wait for its first sub (nil: it
// did not come, or stop closed). A connection whose first sub is being
// attached (reserved, not yet published) is waited for whatever wait is: that
// takes microseconds.
func (t *mconnTable) join(id uint64, wait time.Duration, stop <-chan struct{}) *mConn {
	t.mu.Lock()
	s := t.m[id]
	if s == nil {
		s = &mconnSlot{ready: make(chan struct{})}
		t.m[id] = s
	}
	if s.pub {
		t.mu.Unlock()
		return s.mc
	}
	s.waiters++
	reserved := s.mc != nil
	t.mu.Unlock()
	var tmC <-chan time.Time
	if !reserved {
		tm := time.NewTimer(wait)
		defer tm.Stop()
		tmC = tm.C
	}
	select {
	case <-s.ready:
	case <-tmC:
		t.mu.Lock()
		reserved = s.mc != nil && !s.pub
		t.mu.Unlock()
		if reserved { // the first sub came meanwhile: wait for its attach
			select {
			case <-s.ready:
			case <-stop:
			}
		}
	case <-stop:
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s.waiters--
	if s.mc == nil && s.waiters == 0 && t.m[id] == s {
		delete(t.m, id)
	}
	if !s.pub {
		return nil
	}
	return s.mc
}

// peek returns connection id if its first sub came (nil otherwise); it
// never waits nor leaves a slot (rr157 end notice).
func (t *mconnTable) peek(id uint64) *mConn {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s := t.m[id]; s != nil {
		return s.mc
	}
	return nil
}

// remove forgets connection id if it is still mc.
func (t *mconnTable) remove(id uint64, mc *mConn) {
	t.mu.Lock()
	if s := t.m[id]; s != nil && s.mc == mc {
		delete(t.m, id)
		if !s.pub {
			close(s.ready) // never published: its waiting joins get nil
			s.pub, s.mc = false, nil
		}
	}
	t.mu.Unlock()
}

func (t *mconnTable) size() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.m)
}

// exitMConnCounted counts a multi-link connection's bytes as one user
// connection (relayStream reads its GetDieCh).
type exitMConnCounted struct {
	*mConn
	c *exitConn
}

func (s exitMConnCounted) Read(p []byte) (int, error) {
	n, err := s.mConn.Read(p)
	if n > 0 {
		s.c.bytes.Add(uint64(n))
	}
	return n, err
}

func (s exitMConnCounted) Write(p []byte) (int, error) {
	n, err := s.mConn.Write(p)
	if n > 0 {
		s.c.bytes.Add(uint64(n))
	}
	return n, err
}

// serveMConnSub serves one kindMConn sub-stream (its kind byte already read).
func serveMConnSub(ctx context.Context, st *smux.Stream, cfg KharejConfig, mtr *linkMeter) {
	var h [mconnHdrLen - 1]byte
	st.SetReadDeadline(time.Now().Add(kindTimeout))
	if _, err := io.ReadFull(st, h[:]); err != nil {
		st.Close()
		return
	}
	st.SetReadDeadline(time.Time{})
	port := int(binary.BigEndian.Uint16(h[0:2]))
	id := binary.BigEndian.Uint64(h[2:10])
	role := h[10]
	tbl := cfg.mconns
	if tbl == nil {
		tbl = exitMConns
	}
	if role == mconnRoleRescueJoin { // er94 (mconn_rescue.go)
		if !rescueOn.Load() {
			st.Close() // never sent to a server not announcing it
			return
		}
		if mc := tbl.join(id, 0, ctx.Done()); mc != nil {
			attachExitSub(st, mc, mconnRoleJoin, cfg.traffic, mtr)
			return
		}
	}
	if role == mconnRoleRstEnd { // rr157 (mconn_rescue_rst.go): Iran's end notice, header only
		rstEndNoticeIn(tbl, id)
		st.Close()
		return
	}
	if role == mconnRoleRstJoin { // rr156 (mconn_rescue_rst.go)
		if !rescueRstOn.Load() {
			st.Close() // never sent to a server not announcing it
			return
		}
		if mc := tbl.join(id, 0, ctx.Done()); mc != nil {
			rstStat.joinIn.Add(1)
			attachExitSub(st, mc, mconnRoleJoin, cfg.traffic, mtr)
			return
		}
		// no such conn (ended, or never here): refuse at once
		rstStat.nakSent.Add(1)
		rtrace("%x exit rst join: no conn, RST", id)
		st.SetWriteDeadline(time.Now().Add(kindTimeout))
		st.Write([]byte{mRecRst})
		st.Close()
		return
	}
	switch role {
	case mconnRoleFirst, mconnRoleFirstRaw, mconnRoleFirstRawRescue, mconnRoleRescueJoin:
		if role == mconnRoleFirstRawRescue && !rescueOn.Load() {
			st.Close() // never sent to a server not announcing it
			return
		}
		target, ok := cfg.routeTable().Target(port)
		if !ok {
			cfg.noRoute.note(port, "tcp", cfg.Log)
			st.Close()
			return
		}
		var mc *mConn
		raw := role == mconnRoleFirstRaw || role == mconnRoleFirstRawRescue
		if raw {
			mc = newMConnExitRaw(id)
		} else {
			mc = newMConnExit(id)
		}
		if role == mconnRoleRescueJoin {
			mc.mu.Lock()
			mc.rscFresh = true
			mc.mu.Unlock()
		}
		mc.tr.setPort(port)
		if !tbl.reserve(id, mc) { // ul173: joins attach only after the first sub (publish below)
			// mem64: mc's manage goroutine already runs; close it too.
			// zs291: Close ends it at once (no sub ever joined), raw or not.
			zsReserveLost[role&7].Add(1)
			mc.Close()
			if role == mconnRoleRescueJoin {
				// er94: the first sub came meanwhile: a plain join
				if mc2 := tbl.join(id, 0, ctx.Done()); mc2 != nil {
					attachExitSub(st, mc2, mconnRoleJoin, cfg.traffic, mtr)
					return
				}
			}
			st.Close()
			return
		}
		if role == mconnRoleRescueJoin {
			rescueStat.freshNew.Add(1)
			rtrace("%x exit fresh conn from a rescue join", id)
		}
		defer tbl.remove(id, mc)
		mconnStat.total.Add(1)
		mconnStat.open.Add(1)
		defer mconnStat.open.Add(-1)
		if raw {
			// no watcher goroutine: the sub is closed by the connection
			c := cfg.traffic.openSub(mtr)
			mconnStat.exitSubs.Add(1)
			mc.attachRaw(exitCountedStream{st, c}, func() {
				cfg.traffic.done(c)
				mconnStat.exitSubs.Add(-1)
			})
			if role == mconnRoleFirstRawRescue {
				mc.rescueEnable() // HS2_MCONN_RESCUE (mconn_rescue.go)
			}
		} else {
			attachExitSub(st, mc, role, cfg.traffic, mtr)
		}
		tbl.publish(id, mc)
		up, err := dialExitTCP(target)
		if err != nil {
			mc.Close()
			return
		}
		c := cfg.traffic.open(nil) // the user connection, once; its subs count per link
		defer cfg.traffic.done(c)
		relayStreamHC(exitTarget{up, mc}, exitMConnCounted{mc, c}, nil, hcIf(halfCloseLink(mtr), hcExit)) // hc183
		mc.Close()
	case mconnRoleJoin:
		mc := tbl.join(id, mconnJoinWait, ctx.Done())
		if mc == nil {
			mconnStat.joinMissed.Add(1)
			rtrace("%x exit join missed (no conn)", id)
			if ctx.Err() == nil {
				jm271MissedJoin(st) // jm271: RST, so Iran ends the conn now (mconn_jm271.go)
			}
			st.Close()
			return
		}
		attachExitSub(st, mc, role, cfg.traffic, mtr)
	default:
		st.Close()
	}
}

// exitTarget is the exit's target connection of an mConn. fx168 (audit
// rel66 H2): its Close resets (RST) instead of a FIN when the connection
// ended abnormally before the peer's FIN — a reset, every sub died, a gap —
// so a truncated upload is never handed to the target as a complete one.
type exitTarget struct {
	net.Conn
	mc *mConn
}

func (t exitTarget) Close() error {
	if t.mc.failedEnd() {
		if tc, ok := t.Conn.(*net.TCPConn); ok {
			tc.SetLinger(0)
		}
		mconnTargetRst.Add(1)
	}
	return t.Conn.Close()
}

// mconnTargetRst counts exit targets reset by exitTarget.
var mconnTargetRst atomic.Uint64

// attachExitSub hands a sub to its connection, counted on its link (heavy
// streams) until it or the connection ends.
func attachExitSub(st *smux.Stream, mc *mConn, role byte, traffic *exitTraffic, mtr *linkMeter) {
	dl76Mark(st)
	c := traffic.openSub(mtr)
	mconnStat.exitSubs.Add(1)
	go func() {
		select {
		case <-st.GetDieCh():
		case <-mc.GetDieCh():
			st.Close()
		}
		traffic.done(c)
		mconnStat.exitSubs.Add(-1)
	}()
	mc.attach(exitCountedStream{st, c}, role)
}

// ackMarks (hs112, HS2_MCONN_STRIPE_KEEPSTALL): the smux stream's written
// count and the peer's consumed count (mconn_stripe_keepstall.go).
func (s *mconnSub) ackMarks() (written, consumed uint32, ok bool) {
	return ackMarksOf(s.stream)
}
