package engine

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"math/rand/v2"
	"os"
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

func init() {
	v := os.Getenv("HS2_MCONN")
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
}

// MConnObserved returns the multi-link connection counters now.
func MConnObserved() MConnSnap {
	return MConnSnap{Open: mconnStat.open.Load(), SubsLight: mconnStat.light.Load(), SubsBulk: mconnStat.bulk.Load(),
		SubsExit: mconnStat.exitSubs.Load(), Total: mconnStat.total.Load(), JoinMissed: mconnStat.joinMissed.Load()}
}

func mconnPlaneGauge(p mPlane) *atomic.Int64 {
	if p == planeBulk {
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
	for _, a := range avoid {
		if a == any(l) {
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
	k := planeKey{pk: newPickKey(ml.pressed, ml.flowing, ml.picks, ml.recentPicks(), int(ml.users.Load()), m.perLink)}
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
		k.pk.heavy = ml.heavy > 0
	}
	return k
}

// pickPlane places a sub of plane p on a link not in avoid whose exit reads
// kindMConn, and returns the release func of that placement. Serving links
// first; then healthy retiring ones; then any (as pickLocked). Ties are
// broken at random.
func (m *LinkManager) pickPlane(p mPlane, avoid []any) (Link, func(), bool) {
	pool := m.exitInfo.Load()
	m.mu.Lock()
	defer m.mu.Unlock()
	ml := m.pickPlaneLocked(p, avoid, pool)
	if ml == nil {
		return nil, nil, false
	}
	m.placeLocked(ml)
	return ml.link, m.releaseFor(ml), true
}

func (m *LinkManager) pickPlaneLocked(p mPlane, avoid []any, pool *peerInfo) *managedLink {
	if p != planeLight {
		return m.pickPlaneTiers(p, avoid, pool, nil)
	}
	// HS2_DUP_PATHS (dup_paths.go): a light sub placed beside live ones goes
	// to another healthy path first.
	skip, healthy := m.dupPathsLocked(avoid)
	if skip == nil {
		return m.pickPlaneTiers(p, avoid, pool, nil)
	}
	if healthy != nil {
		if ml := m.pickPlaneTiers(p, avoid, pool, func(ml *managedLink) bool {
			return healthy[mlPath(ml)] && !ml.degraded && !ml.draining && !ml.suspect && !m.pathCooledLocked(ml)
		}); ml != nil {
			countDupPath(true, false)
			return ml
		}
	}
	ml := m.pickPlaneTiers(p, avoid, pool, nil)
	if ml != nil {
		countDupPath(!skip[mlPath(ml)], false)
	}
	return ml
}

// pickPlaneTiers is pickPlaneLocked's search over the links ok accepts (nil:
// all); in the first pass (tier 0) ok only narrows the usual candidates.
func (m *LinkManager) pickPlaneTiers(p mPlane, avoid []any, pool *peerInfo, ok func(*managedLink) bool) *managedLink {
	for tier := 0; tier < 3; tier++ {
		var chosen *managedLink
		var best planeKey
		ties := 0
		for _, ml := range m.links {
			if !ml.link.Alive() || m.gateInfo && ml.mtr != nil && !ml.mtr.infoDone.Load() {
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
			k := m.planeKeyLocked(p, ml)
			switch {
			case chosen == nil || k.less(best):
				chosen, best, ties = ml, k, 1
			case !best.less(k):
				ties++
				if rand.IntN(ties) == 0 {
					chosen = ml
				}
			}
		}
		if chosen != nil {
			return chosen
		}
	}
	return nil
}

// ---- edge: one user connection --------------------------------------------

// mconnSub is a sub as the edge opened it: closing it releases its place on
// the link (once).
type mconnSub struct {
	stream
	raw     bool // a raw first sub (mconnRoleFirstRaw)
	plane   mPlane
	once    sync.Once
	release func()
}

func (s *mconnSub) mconnRaw() bool { return s.raw }

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
	})
	return err
}

// mconnEdge is the edge's side of one connection: its opener and its subs.
type mconnEdge struct {
	ctx  context.Context
	lm   *LinkManager
	port int
	id   uint64

	mu    sync.Mutex // serialises open (the first sub is role 0) and end
	first bool       // a role-0 sub went out
	subs  []*mconnSub
	ended bool
}

// open is the connection's mOpener.
func (e *mconnEdge) open(p mPlane, avoid []any) (mSub, any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	wait := mconnNextWait
	if !e.first {
		wait = mconnFirstWait
	}
	deadline := time.Now().Add(wait)
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
			l, rel, ok = e.lm.pickPlane(p, avoid)
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
			role := mconnRoleJoin
			if !e.first {
				role = mconnRoleFirst
				if raw && rawCapable(st) {
					role = mconnRoleFirstRaw
				}
			}
			if _, err = st.Write(mconnHeader(e.port, e.id, role)); err == nil {
				if role == mconnRoleJoin && p == planeLight {
					if cs, ok := st.(*countedStream); ok {
						cs.copyOnly.Store(true) // a copy of the first sub's bytes: not demand
					}
				}
				e.first = true
				s := &mconnSub{stream: st, plane: p, release: rel, raw: role == mconnRoleFirstRaw}
				mconnPlaneGauge(p).Add(1)
				e.subs = append(e.subs, s)
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
	l, rel, ok := pickWait(e.ctx, e.lm, true)
	if !ok {
		return nil, nil, false, false
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

// end closes every sub the connection opened (releasing their places) and
// refuses later opens. It returns how many subs were opened.
func (e *mconnEdge) end() int {
	e.mu.Lock()
	e.ended = true
	subs := e.subs
	e.subs = nil
	n := len(subs)
	e.mu.Unlock()
	for _, s := range subs {
		s.Close()
	}
	return n
}

// serveUserMConn carries a user connection as an mConn. false: no sub could
// be opened at all and nothing was sent — the caller uses the one stream.
func serveUserMConn(ctx context.Context, user io.ReadWriteCloser, lm *LinkManager, port int) bool {
	t0 := time.Now()
	e := &mconnEdge{ctx: ctx, lm: lm, port: port, id: newMConnID()}
	mc, err := newMConnIran(e.id, e.open)
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
	mconnStat.total.Add(1)
	mconnStat.open.Add(1)
	defer mconnStat.open.Add(-1)
	relayStream(user, mc, nil)
	mc.Close()
	e.end()
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
	ready   chan struct{} // closed when mc is set
	waiters int           // joins waiting for mc
}

func newMConnTable() *mconnTable { return &mconnTable{m: map[uint64]*mconnSlot{}} }

// exitMConns is the table of an exit whose config has none (tests that call
// serveStream directly).
var exitMConns = newMConnTable()

// create registers mc as connection id (its first sub came). false: the id
// is taken by a live connection.
func (t *mconnTable) create(id uint64, mc *mConn) bool {
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
	close(s.ready)
	return true
}

// join returns connection id, waiting up to wait for its first sub (nil: it
// did not come, or stop closed).
func (t *mconnTable) join(id uint64, wait time.Duration, stop <-chan struct{}) *mConn {
	t.mu.Lock()
	s := t.m[id]
	if s == nil {
		s = &mconnSlot{ready: make(chan struct{})}
		t.m[id] = s
	}
	if s.mc != nil {
		t.mu.Unlock()
		return s.mc
	}
	s.waiters++
	t.mu.Unlock()
	tm := time.NewTimer(wait)
	select {
	case <-s.ready:
	case <-tm.C:
	case <-stop:
	}
	tm.Stop()
	t.mu.Lock()
	defer t.mu.Unlock()
	s.waiters--
	if s.mc == nil && s.waiters == 0 && t.m[id] == s {
		delete(t.m, id)
	}
	return s.mc
}

// remove forgets connection id if it is still mc.
func (t *mconnTable) remove(id uint64, mc *mConn) {
	t.mu.Lock()
	if s := t.m[id]; s != nil && s.mc == mc {
		delete(t.m, id)
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
	switch role {
	case mconnRoleFirst, mconnRoleFirstRaw:
		target, ok := cfg.routeTable().Target(port)
		if !ok {
			cfg.noRoute.note(port, "tcp", cfg.Log)
			st.Close()
			return
		}
		var mc *mConn
		if role == mconnRoleFirstRaw {
			mc = newMConnExitRaw(id)
		} else {
			mc = newMConnExit(id)
		}
		mc.tr.setPort(port)
		if !tbl.create(id, mc) {
			st.Close()
			return
		}
		defer tbl.remove(id, mc)
		mconnStat.total.Add(1)
		mconnStat.open.Add(1)
		defer mconnStat.open.Add(-1)
		if role == mconnRoleFirstRaw {
			// no watcher goroutine: the sub is closed by the connection
			c := cfg.traffic.openSub(mtr)
			mconnStat.exitSubs.Add(1)
			mc.attachRaw(exitCountedStream{st, c}, func() {
				cfg.traffic.done(c)
				mconnStat.exitSubs.Add(-1)
			})
		} else {
			attachExitSub(st, mc, role, cfg.traffic, mtr)
		}
		up, err := dialExitTCP(target)
		if err != nil {
			mc.Close()
			return
		}
		c := cfg.traffic.open(nil) // the user connection, once; its subs count per link
		defer cfg.traffic.done(c)
		relayStream(up, exitMConnCounted{mc, c}, nil)
		mc.Close()
	case mconnRoleJoin:
		mc := tbl.join(id, mconnJoinWait, ctx.Done())
		if mc == nil {
			mconnStat.joinMissed.Add(1)
			st.Close()
			return
		}
		attachExitSub(st, mc, role, cfg.traffic, mtr)
	default:
		st.Close()
	}
}

// attachExitSub hands a sub to its connection, counted on its link (heavy
// streams) until it or the connection ends.
func attachExitSub(st *smux.Stream, mc *mConn, role byte, traffic *exitTraffic, mtr *linkMeter) {
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
