package engine

import (
	"log"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// HS2_MCONN_STRIPE=K (with HS2_MCONN=lite on both servers): one very heavy
// connection is striped over up to K subs on different links, so a single
// download is no longer capped by what one TCP link carries under loss.
//
// It reuses mconn's record layer as it is: every DATA record carries its byte
// offset and the receiver already reassembles by offset (mrecv), so a stripe
// is only a different sending rule — each chunk goes ONCE, on the stripe sub
// with the shortest queue (a sub whose link is slow or in loss recovery
// fills its small queue and stops getting chunks). Nothing is duplicated.
//
//   - Who decides: Iran, which sees both directions' rates (it is the edge
//     that opens subs). Every mLitePoll it samples the faster direction; when
//     that moved at least stripeSubRate per sub it already has, it opens one
//     more sub (plane planeStripe), up to K. A source slower than the links
//     stops the growth by itself (its rate per sub falls below the rule).
//   - Wire: a new record STRIPE (0x08) on a sub tells the peer "this sub is
//     part of the stripe": the sender spreads its chunks over its first sub
//     and the subs that got STRIPE, once its direction is over mDupMaxRate
//     (lite's one-copy state). Below that, lite's rules apply unchanged.
//     A raw first sub (mconn_raw.go) is upgraded with the same handshake as
//     lite's upgrade; the new sub carries UPGRADE then STRIPE.
//   - Capability: capMConnStripe in kindInfo, announced only with the key on;
//     Iran stripes only when the exit announced it. An older peer never sees
//     a STRIPE record (it would reset the connection on it).
//   - Links: a stripe sub goes only to a serving link that carries no
//     download and no real traffic (heavy == 0, flowing == 0, not pressed),
//     an idle one (no user) first, and only while at least stripeKeepFree
//     such links stay free for light users afterwards. A picked link counts
//     as heavy for every later light pick at once (stripeHeld), before the
//     pool's sampler sees the download on it. Light users never queue behind
//     a stripe: the stripe takes no link they use.
//   - Memory: Iran's reorder bound (mrecv out-of-order bytes) is raised to
//     stripeReorder for a striping connection; at most stripeMaxConns
//     connections of the process stripe at once, so the extra reorder memory
//     is bounded by their product (plus smux's own per-stream windows, which
//     bound every sub anyway).
//   - Failure: as for lite's subs, a stripe sub dying while it held the only
//     copy of some bytes resets the connection — the outcome of today's one
//     stream on a dying link. A stripe sub idle both ways for mLiteIdle is
//     closed by lite's idle rule; the peer never puts a chunk on a stripe sub
//     that has been cold for mLiteIdle/2 (the rule lite already uses).

const (
	mRecStripe = 0x08 // Iran → peer, on a sub: this sub is part of the stripe

	capMConnStripe = 1 << 7 // reads STRIPE records (HS2_MCONN_STRIPE)

	planeStripe mPlane = 2

	// stripeKMax caps a configured K.
	stripeKMax = 16
	// stripeKeepFree: links without a download that must stay free for light
	// users after a stripe sub takes one.
	stripeKeepFree = 2
	// stripeMaxConns: connections striping at once (process-wide).
	stripeMaxConns = 8
	// stripeReorder: Iran's out-of-order bound for a striping connection.
	stripeReorder = 16 << 20
)

// stripeSubRate: bytes/s each sub must already carry before one more is
// opened (HS2_MCONN_STRIPE_MBIT, default 5 Mbit/s). A variable for tests.
var stripeSubRate atomic.Int64

// stripeK: HS2_MCONN_STRIPE (0: off). Atomic so tests can switch it.
var stripeK atomic.Int32

func init() {
	stripeK.Store(int32(parseStripe(os.Getenv("HS2_MCONN_STRIPE"))))
	mbit := 5.0
	if v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv("HS2_MCONN_STRIPE_MBIT")), 64); err == nil && v > 0 {
		mbit = v
	}
	stripeSubRate.Store(int64(mbit * 1e6 / 8))
}

// parseStripe: "", "0", "off" → 0; "on" → 8; a number → that many subs
// (2..stripeKMax).
func parseStripe(v string) int {
	switch v = strings.TrimSpace(strings.ToLower(v)); v {
	case "", "0", "off", "false":
		return 0
	case "on", "true":
		return 8
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 2 {
		return 0
	}
	return min(n, stripeKMax)
}

// stripeCap is the capMConnStripe bit this server announces.
func stripeCap() uint8 {
	if mconnOn.Load() && mconnLite.Load() && stripeK.Load() > 0 {
		return capMConnStripe
	}
	return 0
}

// mconnStripe reports whether the other server reads STRIPE records.
func (pi *peerInfo) mconnStripe() bool { return pi.mconnRaw() && pi.Caps&capMConnStripe != 0 }

// counters (process-wide)
var stripeStat struct {
	active  atomic.Int32  // connections holding a stripe slot now
	subs    atomic.Uint64 // stripe subs opened (Iran) / joined (exit)
	bytes   atomic.Uint64 // payload bytes sent by the stripe rule
	refused atomic.Uint64 // growth refused: no free link or no slot
}

// stripeState is an mConn's stripe bookkeeping (under mConn.mu).
type stripeState struct {
	stK       int       // Iran: the most subs this connection may stripe over (0: never)
	stActive  bool      // Iran: holds a stripe slot
	stOpening bool      // Iran: a stripe sub is being opened
	stRetryAt time.Time // Iran: no open before this (after a failed one)
	stSample  time.Time // Iran: the rate sample
	stSent    uint64
	stRecvd   uint64
	stRR      int // sender: round-robin start among equal queues
	stLogged  int // Iran: the largest stripe logged
	stN       int // subs ever marked stripe (0: the stripe rule is never looked at)
}

// stripeN: some sub was ever marked stripe (cheap gate for the send path).
func (m *mConn) stripeN() bool { return m.stN > 0 }

// setStripe (Iran, right after newMConnIran): this connection may stripe
// over up to k subs.
func (m *mConn) setStripe(k int) {
	m.mu.Lock()
	m.stK = k
	m.mu.Unlock()
}

// liveStripeLocked: live subs that carry the stripe (the first sub excluded).
func (m *mConn) liveStripeLocked() int {
	n := 0
	for _, s := range m.subs {
		if s.stripe && !s.retired && !s.dead {
			n++
		}
	}
	return n
}

// stripeTickLocked (Iran): the growth rule, sampled every mLitePoll. sent
// and recvd are both directions' offsets now. true: open one more stripe sub
// now (stOpening was set).
func (m *mConn) stripeTickLocked(now time.Time, sent, recvd uint64) bool {
	if m.stK < 2 || m.ended || m.closing || m.peerFin || m.wclosed {
		return false
	}
	if m.stSample.IsZero() {
		m.stSample, m.stSent, m.stRecvd = now, sent, recvd
		return false
	}
	w := now.Sub(m.stSample)
	if w < mLitePoll*9/10 {
		return false
	}
	moved := max(sent-m.stSent, recvd-m.stRecvd)
	m.stSample, m.stSent, m.stRecvd = now, sent, recvd
	if m.stOpening || m.handshaking || m.lightOpening || now.Before(m.stRetryAt) {
		return false
	}
	n := 1 + m.liveStripeLocked()
	if n >= m.stK {
		return false
	}
	rate := float64(moved) / w.Seconds()
	if rate < float64(n)*float64(stripeSubRate.Load()) {
		return false
	}
	if !m.stActive {
		if stripeStat.active.Add(1) > stripeMaxConns {
			stripeStat.active.Add(-1)
			stripeStat.refused.Add(1)
			return false
		}
		m.stActive = true
		r := &m.r
		r.mu.Lock()
		r.oooMax = stripeReorder
		r.mu.Unlock()
	}
	m.stOpening = true
	m.tr.ev("stripe: grow %d -> %d (%.1f Mbit/s)", n, n+1, rate*8/1e6)
	return true
}

// stripeAvoidLocked: the links this connection already uses (and those that
// died under it).
func (m *mConn) stripeAvoidLocked() []any {
	avoid := make([]any, 0, len(m.subs)+len(m.deadKeys))
	for _, s := range m.subs {
		if s.key != nil {
			avoid = append(avoid, s.key)
		}
	}
	return append(avoid, m.deadKeys...)
}

// stripeIdleLocked (Iran, manager): give the slot back once no stripe sub is
// left (lite's idle rule closed them, or they died).
func (m *mConn) stripeIdleLocked() {
	if m.stActive && !m.stOpening && m.liveStripeLocked() == 0 {
		m.stActive = false
		stripeStat.active.Add(-1)
		r := &m.r
		r.mu.Lock()
		r.oooMax = 0
		r.mu.Unlock()
	}
}

// stripeEnded (end): give the slot back.
func (m *mConn) stripeEnded() {
	m.mu.Lock()
	if m.stActive {
		m.stActive = false
		stripeStat.active.Add(-1)
	}
	m.mu.Unlock()
}

// openStripe (Iran, its own goroutine): one more stripe sub. A raw
// connection is upgraded with it (mconn_raw.go).
func (m *mConn) openStripe(avoid []any, raw bool) {
	if raw {
		m.upgradeRawPlane(planeStripe, avoid)
		m.mu.Lock()
		m.stOpening = false
		if m.rawW.Load() { // the open failed: still raw
			m.stRetryAt = time.Now().Add(mReopenBackoff)
			stripeStat.refused.Add(1)
			m.stripeIdleLocked()
		}
		m.mu.Unlock()
		return
	}
	sub, key, err := m.open(planeStripe, avoid)
	m.mu.Lock()
	m.stOpening = false
	if err != nil {
		m.stRetryAt = time.Now().Add(mReopenBackoff)
		stripeStat.refused.Add(1)
		m.stripeIdleLocked()
		m.mu.Unlock()
		m.kickMgr()
		return
	}
	if m.ended || m.closing || m.mode != planeLight {
		m.mu.Unlock()
		sub.Close()
		m.kickMgr()
		return
	}
	s := m.addSubLocked(sub, key, false)
	m.stripeJoinLocked(s)
	m.sendCond.Broadcast()
	m.mu.Unlock()
	m.startSub(s)
}

// stripeJoinLocked (Iran): s carries the stripe; the peer learns it from the
// STRIPE record queued on it.
func (m *mConn) stripeJoinLocked(s *msub) {
	s.stripe = true
	m.stN++
	s.lastHole = time.Now() // it lacks what was sent before it joined
	c := getChunk(mRecStripe)
	s.push(c)
	stripeStat.subs.Add(1)
	if n := 1 + m.liveStripeLocked(); n > m.stLogged {
		m.stLogged = n
		log.Printf("mconn %x: striped over %d subs", m.id&0xffffff, n)
	}
}

// onStripe (peer): s carries the stripe.
func (m *mConn) onStripe(s *msub) {
	m.mu.Lock()
	if !m.ended && !s.stripe {
		s.stripe = true
		m.stN++
		stripeStat.subs.Add(1)
		m.tr.ev("stripe: sub %d joins", s.idx)
		m.sendCond.Broadcast()
	}
	m.mu.Unlock()
}

// stripeQuietLocked: s is a stripe sub that took no chunk for mHoleSafe —
// whatever it carried has long arrived, so its death (lite's idle rule on
// the peer's side, most often) loses nothing even though every other sub
// "missed" the chunks the stripe gave it (subDied's hole rule).
func (m *mConn) stripeQuietLocked(s *msub) bool {
	return s.stripe && time.Since(s.lastPush) >= mHoleSafe
}

// Results of stripeSendLocked.
const (
	stripeNone = iota // no stripe sub: the usual rules
	stripeSent        // queued
	stripeWait        // every stripe sub is full: wait
)

// stripeSendLocked queues DATA chunk c once, on the sub of the stripe (the
// primary and the live stripe subs) with the least queued, round-robin among
// equals. Once a connection has a stripe it replaces lite's rules in both
// states: below mDupMaxRate too, a chunk goes once (K copies would load K
// links).
func (m *mConn) stripeSendLocked(c *mchunk) int {
	p := m.primary.Load()
	var cold time.Time
	var best *msub
	bestQ, striped := 0, false
	nsubs := len(m.subs)
	for i := range nsubs {
		s := m.subs[(m.stRR+i)%nsubs]
		if s.retired || s.dead || (!s.stripe && s != p) {
			continue
		}
		if s != p {
			striped = true
			if !m.iran {
				// the peer closes a sub idle both ways for mLiteIdle: never
				// the only copy on one that has been cold for half of it
				if cold.IsZero() {
					cold = time.Now().Add(-mLiteIdle / 2)
				}
				if s.coldLocked(cold) {
					continue
				}
			}
		}
		if !s.fits(c.n) {
			continue
		}
		if q := s.queueLocked(); best == nil || q < bestQ {
			best, bestQ = s, q
		}
	}
	if !striped {
		return stripeNone
	}
	if best == nil {
		return stripeWait
	}
	m.stRR++
	c.setOff(m.sendOff)
	m.sendOff += uint64(c.n)
	best.push(c)
	now := time.Now()
	for _, s := range m.subs {
		if s != best && !s.retired {
			s.lastHole = now
		}
	}
	best.lastPush = now
	liteBud.add(now.Unix(), c.n, 0)
	stripeStat.bytes.Add(uint64(c.n))
	m.noteQueueLocked()
	return stripeSent
}

// ---- edge: links for stripe subs ------------------------------------------

// stripeHeld: links a stripe sub was placed on (and not yet released). They
// count as heavy for every light pick at once.
var stripeHeld struct {
	mu sync.Mutex
	n  map[*managedLink]int
}

func stripeHold(ml *managedLink, d int) {
	stripeHeld.mu.Lock()
	if stripeHeld.n == nil {
		stripeHeld.n = map[*managedLink]int{}
	}
	if stripeHeld.n[ml] += d; stripeHeld.n[ml] <= 0 {
		delete(stripeHeld.n, ml)
	}
	stripeHeld.mu.Unlock()
}

// stripeOn reports whether ml carries a stripe sub (cheap when none does).
func stripeOn(ml *managedLink) bool {
	if stripeK.Load() == 0 {
		return false
	}
	stripeHeld.mu.Lock()
	n := stripeHeld.n[ml]
	stripeHeld.mu.Unlock()
	return n > 0
}

// stripeFree: a serving link that carries no download and no real traffic.
func stripeFree(ml *managedLink) bool {
	return ml.serving() && ml.heavy == 0 && ml.flowing == 0 && !ml.pressed && !stripeOn(ml)
}

// pickStripe places a stripe sub (see the file comment): a free link not in
// avoid, one with no user first, then the fewest users; never the last
// stripeKeepFree free links.
func (m *LinkManager) pickStripe(avoid []any) (Link, func(), bool) {
	pool := m.exitInfo.Load()
	m.mu.Lock()
	defer m.mu.Unlock()
	free := 0
	var chosen *managedLink
	var bestUsers, bestRecent int
	ties := 0
	for _, ml := range m.links {
		if m.gateInfo && ml.mtr != nil && !ml.mtr.infoDone.Load() || !linkMConn(ml, pool) || !stripeFree(ml) {
			continue
		}
		free++
		if avoided(ml.link, avoid) {
			continue
		}
		u, r := int(ml.users.Load()), ml.recent
		switch {
		case chosen == nil || u < bestUsers || u == bestUsers && r < bestRecent:
			chosen, bestUsers, bestRecent, ties = ml, u, r, 1
		case u == bestUsers && r == bestRecent:
			ties++
			if rand.IntN(ties) == 0 {
				chosen = ml
			}
		}
	}
	if chosen == nil || free-1 < stripeKeepFree {
		return nil, nil, false
	}
	m.placeLocked(chosen)
	stripeHold(chosen, 1)
	rel := m.releaseFor(chosen)
	var once sync.Once
	return chosen.link, func() {
		once.Do(func() {
			rel()
			stripeHold(chosen, -1)
		})
	}, true
}

// openStripe is the edge's open for planeStripe: one try, no waiting (the
// connection runs on what it has; the manager tries again later).
func (e *mconnEdge) openStripe(avoid []any) (mSub, any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ended || !e.first {
		return nil, nil, errMConnNoLink
	}
	l, rel, ok := e.lm.pickStripe(avoid)
	if !ok {
		return nil, nil, errMConnNoLink
	}
	st, err := l.OpenStream()
	if err == nil {
		if _, err = st.Write(mconnHeader(e.port, e.id, mconnRoleJoin)); err == nil {
			s := &mconnSub{stream: st, plane: planeStripe, release: rel}
			mconnPlaneGauge(planeStripe).Add(1)
			e.subs = append(e.subs, s)
			return s, l, nil
		}
		st.Close()
	}
	rel()
	return nil, nil, errMConnNoLink
}
