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
//   - Capability: capExtMConnStripe, ext bit 0 of kindInfo (the caps byte is
//     full; peerinfo.go's optional ext after the ports), announced only with
//     the key on;
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

// stripeHotTicks: samples (mLitePoll) in a row at the growth rate before
// the first stripe sub (stripeAlone; the first version: 1). A variable for
// tests.
var stripeHotTicks atomic.Int32

func init() { stripeHotTicks.Store(4) }

// stripeSubRate: bytes/s each sub must already carry before one more is
// opened (HS2_MCONN_STRIPE_MBIT, default 5 Mbit/s). A variable for tests.
var stripeSubRate atomic.Int64

// stripeK: HS2_MCONN_STRIPE (0: off). Atomic so tests can switch it.
var stripeK atomic.Int32

func init() {
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

// infoExt: the extended caps this server announces in kindInfo (0 with
// every key off: the message's bytes stay as before).
func infoExt() uint32 {
	var e uint32
	if mconnOn.Load() && mconnLite.Load() && stripeK.Load() > 0 {
		e |= capExtMConnStripe
		if stripeUp.Load() {
			e |= capExtMConnStripeUp // mconn_stripe_up.go
		}
	}
	return e
}

// mconnStripe reports whether the other server reads STRIPE records.
func (pi *peerInfo) mconnStripe() bool { return pi.mconnRaw() && pi.Ext&capExtMConnStripe != 0 }

// counters (process-wide)
var stripeStat struct {
	active  atomic.Int32  // connections holding a stripe slot now
	subs    atomic.Uint64 // stripe subs opened (Iran) / joined (exit)
	bytes   atomic.Uint64 // payload bytes sent by the stripe rule
	refused atomic.Uint64 // growth refused: no free link or no slot
	cuts    atomic.Uint64 // scavenger width cuts (a queue on the path)
	// HS2_MCONN_STRIPE_UP (mconn_stripe_up.go)
	rxActive    atomic.Int32  // exit receivers holding a stripe slot now
	reorderGrow atomic.Uint64 // reorder bound growths
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
	stRR      int  // sender: round-robin start among equal queues
	stLogged  int  // Iran: the largest stripe logged
	stN       int  // subs ever marked stripe (0: the stripe rule is never looked at)
	stHot     int  // Iran: consecutive samples at the growth rate
	stUp      bool // Iran: the upload is gated too (HS2_MCONN_STRIPE_UP, mconn_stripe_up.go)

	stWarmUntil time.Time // Iran: its stripe subs are kept until then (HS2_MCONN_STRIPE_SHARE, mconn_stripe_share.go)

	firstRx atomic.Int64 // diagnostics (HS2_MCONN_STRIPE_LOG): unix ns of the first byte read raw

	stAW stripeAW // Iran: the adaptive smux window of a settled stripe (smux_autowin.go)

	stripeGate // sender: the scavenger's width (mconn_stripe_scav.go)

	stPool *poolPeak // Iran: the pool's rebuild window (HS2_MCONN_STRIPE_REBUILD; nil: none)
}

// stripeN: some sub was ever marked stripe (cheap gate for the send path).
func (m *mConn) stripeN() bool { return m.stN > 0 }

// setStripe (Iran, right after newMConnIran): this connection may stripe
// over up to k subs.
func (m *mConn) setStripe(k int, pool *poolPeak) {
	m.mu.Lock()
	m.stK = k
	m.stPool = pool
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
	m.stripeAutoWinLocked(now) // HS2_SMUX_AUTOWIN (smux_autowin.go)
	m.stripeWarmLocked(now)    // HS2_MCONN_STRIPE_SHARE (mconn_stripe_share.go)
	if m.stK < 2 || m.ended || m.closing || m.peerFin || m.wclosed {
		if stripeLog && m.stN > 0 && !m.ended {
			log.Printf("stripe %x: tick off (closing=%v peerFin=%v wclosed=%v)", m.id&0xffffff, m.closing, m.peerFin, m.wclosed)
		}
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
	up := sent-m.stSent > recvd-m.stRecvd // the upload is the faster direction
	m.stSample, m.stSent, m.stRecvd = now, sent, recvd
	if m.stOpening || m.handshaking || m.lightOpening || now.Before(m.stRetryAt) || m.callOn || m.callUp {
		if stripeLog && m.stN > 0 {
			log.Printf("stripe %x: tick held (opening=%v hs=%v light=%v retry=%v call=%v/%v)", m.id&0xffffff, m.stOpening, m.handshaking, m.lightOpening, now.Before(m.stRetryAt), m.callOn, m.callUp)
		}
		return false // a call (HS2_MCONN_CALL) is copied, never striped
	}
	n := 1 + m.liveStripeLocked()
	rate := float64(moved) / w.Seconds()
	m.stripeNoteRateLocked(now, rate) // HS2_MCONN_STRIPE_SHARE (mconn_stripe_share.go)
	if !m.stripeKCapLocked(n, rate) { // n >= stK (past it only with HS2_MCONN_STRIPE_UP)
		return false
	}
	if rate < float64(n)*float64(stripeSubRate.Load()) {
		if stripeLog && m.stN > 0 && rate > 1e6 {
			log.Printf("stripe %x: under the growth rate (%.1f Mbit/s over %d)", m.id&0xffffff, rate*8/1e6, n)
		}
		m.stHot = 0
		if m.stN > 0 && rate > 1e6 {
			stripeDiag.growRate.Add(1) // a stripe that slowed (not every idle connection)
		}
		return false
	}
	if m.stHot++; n == 1 && stripeAlone && m.stHot < int(stripeHotTicks.Load()) {
		// only a long download stripes: a reel or a page's burst is over in
		// a second or two, and each would take (and the pool dial) links
		return false
	}
	used := m.stripeAllUsedLocked
	if m.stUp && up {
		used = m.stripeUpAllUsedLocked // an upload: the subs Iran sent on (mconn_stripe_up.go)
	}
	if m.stUp && up && n >= 2 && !m.stripeUpRoomLocked() {
		if stripeLog {
			log.Printf("stripe %x: no growth, upload path queue %v (%.1f Mbit/s over %d)", m.id&0xffffff, m.stQ, rate*8/1e6, n)
		}
		return false // mconn_stripe_up.go: a sub more only while the gate would widen
	}
	if stripeQTarget.Load() > 0 && !used(now) {
		if stripeLog {
			log.Printf("stripe %x: no growth, a sub unused (%.1f Mbit/s over %d)", m.id&0xffffff, rate*8/1e6, n)
		}
		stripeDiag.growUnused.Add(1)
		// the sender's scavenger gate leaves a sub unused (the path has a
		// queue): one more would only hold a link light users could take
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
		r.oooGrow = stripeUp.Load() // HS2_MCONN_STRIPE_UP: more on demand
		r.mu.Unlock()
	}
	m.stOpening = true
	m.tr.ev("stripe: grow %d -> %d (%.1f Mbit/s)", n, n+1, rate*8/1e6)
	return true
}

// stripeAllUsedLocked (Iran): every live stripe sub received data in the
// last mLitePoll (the sender's gate uses them all).
func (m *mConn) stripeAllUsedLocked(now time.Time) bool {
	t := monoNano(now.Add(-mLitePoll))
	for _, s := range m.subs {
		if s.stripe && !s.retired && !s.dead && s.lastRecv.Load() < t {
			return false
		}
	}
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
		r.releaseOOOLocked(0)
		r.oooGrow = false
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
	m.stripeSubWinLocked(s) // HS2_STRIPE_SUB_WIN_MB (smux_autowin.go)
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
		if mtr := subMeter(s); mtr != nil && stripeAlone {
			// the exit's TUN lane: heavy now and while the sub lives, used
			// or not (exitTraffic.storeHeavy; released in msub.close)
			mtr.stripeN.Add(1)
			s.stMtr.Store(mtr)
			select {
			case <-s.done: // closed meanwhile: close missed it
				if s.stMtr.Swap(nil) != nil {
					mtr.stripeN.Add(-1)
				}
			default:
			}
			if mtr.heavyN.Load() < 1 {
				mtr.heavyN.Store(1)
			}
		}
		stripeStat.subs.Add(1)
		m.stripeRxJoin() // HS2_MCONN_STRIPE_UP (mconn_stripe_up.go)
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

// stripeRankLocked: s's place among the live stripe subs other than p, in
// the order they joined.
func (m *mConn) stripeRankLocked(s, p *msub) int {
	r := 0
	for _, x := range m.subs {
		if x == s {
			return r
		}
		if x.stripe && x != p && !x.retired && !x.dead {
			r++
		}
	}
	return r
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
	// the scavenger's width (mconn_stripe_scav.go): only the first w stripe
	// subs (in the order they joined) may take chunks; -1: all
	w := m.stripeWidthLocked(p, time.Now())
	if w == 0 && m.stUp {
		w = 1 // HS2_MCONN_STRIPE_UP: never narrower than the ungated pair before it (mconn_stripe_up.go)
	}
	if m.iran {
		if c := m.stPool.rebuildCap(time.Now()); c >= 0 && (w < 0 || w > c) {
			w = c // HS2_MCONN_STRIPE_REBUILD: this pool is rebuilding after a mass loss
		}
	}
	for i := range nsubs {
		s := m.subs[(m.stRR+i)%nsubs]
		if s.retired || s.dead || (!s.stripe && s != p) {
			continue
		}
		if s != p {
			striped = true
			if w >= 0 && m.stripeRankLocked(s, p) >= w {
				continue
			}
			if stripeCallFree && callLinkOn(m.subMeterOf(s), time.Now()) {
				continue // HS2_MCONN_STRIPE_CALLFREE: a call rides this link
			}
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
	if best != p {
		stripeDiag.subB.Add(uint64(c.n))
	}
	now := time.Now()
	for _, s := range m.subs {
		if s != best && !s.retired {
			s.lastHole = now
		}
	}
	best.lastPush = now
	best.stPushed = now
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
	if ml.mtr != nil {
		ml.mtr.stripeN.Add(int32(d)) // the TUN lane leaves the link at once (l3_lane.go)
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

// pickStripe places a stripe sub (see the file comment). With stripeAlone
// (the default): a link set aside for the stripe first (mconn_stripe_pool.go),
// else a free link no user rides, never one of the last stripeKeepFree free
// links; none: it asks the pool for a link of its own. Without stripeAlone
// (the first version): a free link, the fewest users first.
func (m *LinkManager) pickStripe(avoid []any) (Link, func(), bool) {
	pool := m.exitInfo.Load()
	m.mu.Lock()
	defer m.mu.Unlock()
	free := 0
	var chosen *managedLink
	var bestUsers, bestRecent int
	bestRes := false
	ties := 0
	for _, ml := range m.links {
		if m.gateInfo && ml.mtr != nil && !ml.mtr.infoDone.Load() || !linkMConn(ml, pool) {
			continue
		}
		res := stripeAlone && !ml.stripeRes.IsZero()
		if res {
			// set aside: the stripe's own, unless a download or a user came
			if !ml.serving() || ml.heavy > 0 || ml.flowing > 0 || ml.users.Load() > 0 {
				continue
			}
		} else if !stripeFree(ml) {
			continue
		} else {
			free++
		}
		if avoided(ml.link, avoid) {
			continue
		}
		u, r := int(ml.users.Load()), ml.recent
		if stripeAlone && u > 0 {
			// a light user's sub on a stripe link waits behind the stripe's
			// loss recovery (TCP is one byte stream): every loss on the link
			// holds the game's packet a round trip. Empty links only.
			continue
		}
		switch {
		case bestRes && !res:
		case chosen == nil || res && !bestRes || u < bestUsers || u == bestUsers && r < bestRecent:
			chosen, bestUsers, bestRecent, bestRes, ties = ml, u, r, res, 1
		case u == bestUsers && r == bestRecent:
			ties++
			if rand.IntN(ties) == 0 {
				chosen = ml
			}
		}
	}
	shared := false
	if (chosen == nil || !bestRes && free-1 < stripeKeepFree) && stripeShare {
		// HS2_MCONN_STRIPE_SHARE (mconn_stripe_share.go): a busy pool has no
		// link without a user — a lightly loaded one instead
		if ml := m.pickStripeSharedLocked(avoid, pool); ml != nil {
			chosen, bestRes, shared = ml, false, true
			bestUsers, bestRecent = int(ml.users.Load()), ml.recent
		}
	}
	if !shared && (chosen == nil || !bestRes && free-1 < stripeKeepFree) {
		stripeDiag.pickNone.Add(1)
		if stripeAlone {
			m.stripeAskLocked(m.now())
		}
		if stripeLog {
			log.Printf("stripe pick: none (free=%d chosen=%v) — asked the pool for a link", free, chosen != nil)
		}
		return nil, nil, false
	}
	m.placeLocked(chosen)
	if bestRes {
		chosen.stripeRes = time.Time{} // its hold passes to the sub
	} else {
		stripeHold(chosen, 1)
	}
	if shared {
		chosen.stShared.Add(1)
	}
	if chosen.mtr != nil && stripeAlone {
		// the TUN lane (HS2_L3_LANE) reads heavyN: heavy now, not at the
		// sampler's next tick (which keeps it while the link is held)
		if chosen.mtr.heavyN.Load() < 1 {
			chosen.mtr.heavyN.Store(1)
		}
	}
	if stripeLog {
		log.Printf("stripe pick: link %d set-aside=%v shared=%v users=%d recent=%d free=%d", chosen.id, bestRes, shared, bestUsers, bestRecent, free)
	}
	rel := m.releaseFor(chosen)
	var once sync.Once
	return chosen.link, func() {
		once.Do(func() {
			rel()
			stripeHold(chosen, -1)
			if shared {
				chosen.stShared.Add(-1)
			}
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
		autoWinOffForStripe(e.lm, st) // HS2_SMUX_AUTOWIN (smux_autowin.go)
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
