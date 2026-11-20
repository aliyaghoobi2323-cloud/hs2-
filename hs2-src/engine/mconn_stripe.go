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
// every key off: the message's bytes stay as before; hc183's capExtHalfClose
// is on by default, HS2_HALF_CLOSE=0 clears it).
func infoExt() uint32 {
	var e uint32
	if mconnOn.Load() && mconnLite.Load() && rescueOn.Load() {
		e |= capExtMConnRescue // HS2_MCONN_RESCUE (mconn_rescue.go)
		if rescueRecOn.Load() {
			e |= capExtMConnRescueRec // er97 (mconn_rescue_rec.go)
			if rescueRstOn.Load() {
				e |= capExtMConnRescueRst // rr155 (mconn_rescue_rst.go)
				e |= capExtMConnRstEnd    // rr157: reads the role-6 end notice
			}
		}
	}
	if mconnOn.Load() && mconnLite.Load() && stripeK.Load() > 0 {
		e |= capExtMConnStripe
		if stripeUp.Load() {
			e |= capExtMConnStripeUp // mconn_stripe_up.go
		}
		e |= capExtMConnOverlap // mconn_stripe_overlap.go
		if stripeResend.Load() {
			e |= capExtMConnResend // mconn_stripe_evac.go
		}
		if keepStallOn.Load() {
			e |= capExtMConnKeepStall // hs133 (mconn_stripe_keepstall.go)
		}
	}
	if mconnOn.Load() && mconnLite.Load() && xs261On.Load() {
		e |= capExtMConnXS // xs261 (mconn_xs261.go)
	}
	if halfCloseOn.Load() {
		e |= capExtHalfClose // hc183 (halfclose.go)
	}
	e |= zduInfoExt() // zdu350 (zdu.go): HS2_ZDU=1 only
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
	// stSteerHeldN (sd151): stripe opens the steering refused in a row (a cut
	// path's link only, a healthy sub): the next try waits longer
	stSteerHeldN int

	stSample time.Time // Iran: the rate sample
	stSent   uint64
	stRecvd  uint64
	stRR     int  // sender: round-robin start among equal queues
	stLogged int  // Iran: the largest stripe logged
	stN      int  // subs ever marked stripe (0: the stripe rule is never looked at)
	stHot    int  // Iran: consecutive samples at the growth rate
	stUp     bool // Iran: the upload is gated too (HS2_MCONN_STRIPE_UP, mconn_stripe_up.go)
	stGrowUp bool // Iran: the last growth was for an upload (HS2_MCONN_STRIPE_XDIR, mconn_stripe_xdir.go)

	stUpCnt    bool          // Iran: counted in stripeUpActive (HS2_MCONN_STRIPE_FAST_SOLO, mconn_stripe_fast.go)
	stFastMore int           // Iran: subs still to open in the doubling chain (HS2_MCONN_STRIPE_FAST, mconn_stripe_fast.go)
	stFastRate float64       // Iran: the rate that started the chain
	stDlHot    int           // Iran: early download samples in a row at stripeDlFastRate (HS2_MCONN_STRIPE_DL_FAST, mconn_stripe_dlfast.go)
	stDlCnt    bool          // Iran: counted in stripeDlActive (HS2_MCONN_STRIPE_DL_FAST)
	stDlT      time.Time     // Iran: the download sampler's last sample (HS2_MCONN_STRIPE_DL_FAST)
	stDlS      uint64        // Iran: sent at stDlT
	stDlR      uint64        // Iran: received at stDlT
	stDlAt     time.Time     // Iran: when it took the download fast slot (HS2_MCONN_STRIPE_DL_FAIR, mconn_stripe_dlfair.go)
	stEarlyW   time.Duration // Iran: this tick's sample window is an early one (HS2_MCONN_STRIPE_FAST_EARLY_MS, mconn_stripe_early.go)

	stWarmUntil time.Time // Iran: its stripe subs are kept until then (HS2_MCONN_STRIPE_SHARE, mconn_stripe_share.go)

	firstRx atomic.Int64 // diagnostics (HS2_MCONN_STRIPE_LOG): unix ns of the first byte read raw

	stAW stripeAW // Iran: the adaptive smux window of a settled stripe (smux_autowin.go)

	stripeGate // sender: the scavenger's width (mconn_stripe_scav.go)

	stPool *poolPeak // Iran: the pool's rebuild window (HS2_MCONN_STRIPE_REBUILD; nil: none)

	stTrace stTr      // dl184 diagnostics (mconn_stripe_dltrace.go)
	stBorn  time.Time // ul197: the first stripe tick (HS2_MCONN_STRIPE_LOG only)

	stEvacAt time.Time // Iran: no stripe evacuation before this (HS2_MCONN_STRIPE_EVAC, mconn_stripe_evac.go)
	stUpNow  bool      // Iran: the last rate sample's faster direction was the upload (HS2_MCONN_STRIPE_EVAC)
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
	if stripeLog && m.stBorn.IsZero() {
		m.stBorn = now // ul197 (mconn_stripe_dltrace.go)
	}
	m.stripeAutoWinLocked(now) // HS2_SMUX_AUTOWIN (smux_autowin.go)
	m.stripeWarmLocked(now)    // HS2_MCONN_STRIPE_SHARE (mconn_stripe_share.go)
	m.stripeDlLeaseLocked(now) // HS2_MCONN_STRIPE_DL_FAIR (mconn_stripe_dlfair.go)
	if m.stK < 2 || m.ended || m.closing || m.peerFin || m.wclosed {
		if stripeLog && m.stN > 0 && !m.ended {
			log.Printf("stripe %x: tick off (closing=%v peerFin=%v wclosed=%v)", m.id&0xffffff, m.closing, m.peerFin, m.wclosed)
		}
		m.trLocked(trOff)
		return false
	}
	if m.stFastMore > 0 && m.stripeOverlapLocked() && !m.stOpening && !m.rawW.Load() && m.mode == planeLight && m.stripeFastNextLocked() {
		return true // HS2_MCONN_STRIPE_FAST: the doubling chain after a raw upgrade (mconn_stripe_fast.go)
	}
	if m.stSample.IsZero() {
		m.stSample, m.stSent, m.stRecvd = now, sent, recvd
		m.ul257SampleStartLocked(now)
		return false
	}
	w := now.Sub(m.stSample)
	minW := mLitePoll * 9 / 10
	if m.stEarlyW > 0 {
		minW = m.stEarlyW * 9 / 10 // HS2_MCONN_STRIPE_FAST_EARLY_MS (mconn_stripe_early.go)
	}
	dlGo, dlRate := m.stripeDlSampleLocked(now, sent, recvd) // HS2_MCONN_STRIPE_DL_FAST (mconn_stripe_dlfast.go)
	if w < minW && !dlGo {
		m.trLocked(trWin)
		return false
	}
	moved := max(sent-m.stSent, recvd-m.stRecvd)
	up := sent-m.stSent > recvd-m.stRecvd // the upload is the faster direction
	if dlGo {
		up = false
	}
	m.stUpNow = up // HS2_MCONN_STRIPE_EVAC (mconn_stripe_evac.go)
	stuck := false
	if m.rawW.Load() {
		// ul257: a raw upload the user waits on while its first sub's link
		// is stuck grows now (mconn_ul257.go)
		stuck = m.ul257StuckLocked(ul257Up(sent-m.stSent, recvd-m.stRecvd, dlGo), 1+m.liveStripeLocked(), float64(moved)/w.Seconds(), w, m.rawBlockedLocked(now))
	}
	if m.stEarlyW > 0 && !dlGo && !stuck && !m.stripeEarlyHotAnyLocked(up, float64(moved)/w.Seconds()) {
		m.stSample, m.stSent, m.stRecvd = now, sent, recvd
		m.trLocked(trEarly)
		return false // an early sample grows only a fast upload
	}
	m.stSample, m.stSent, m.stRecvd = now, sent, recvd
	if m.stOpening || m.handshaking || m.lightOpening || now.Before(m.stRetryAt) || m.callOn || m.callUp {
		if stripeLog && m.stN > 0 {
			log.Printf("stripe %x: tick held (opening=%v hs=%v light=%v retry=%v call=%v/%v)", m.id&0xffffff, m.stOpening, m.handshaking, m.lightOpening, now.Before(m.stRetryAt), m.callOn, m.callUp)
		}
		switch {
		case m.callOn || m.callUp:
			m.trLocked(trHeldCall)
		case m.handshaking:
			m.trLocked(trHeldHs)
		case now.Before(m.stRetryAt):
			m.trLocked(trHeldRetry)
		default:
			m.trLocked(trHeldOpen)
		}
		return false // a call (HS2_MCONN_CALL) is copied, never striped
	}
	n := 1 + m.liveStripeLocked()
	rate := float64(moved) / w.Seconds()
	if dlGo {
		rate = dlRate
	}
	m.stripeNoteRateLocked(now, rate) // HS2_MCONN_STRIPE_SHARE (mconn_stripe_share.go)
	if !up {
		m.trRateLocked(rate)
	} else if stripeLog && rate > m.stTrace.upMax {
		m.stTrace.upMax = rate // ul197
	}
	if !m.stripeKCapLocked(n, rate) { // n >= stK (past it only with HS2_MCONN_STRIPE_UP)
		m.trLocked(trKCap)
		return false
	}
	low := rate < float64(n)*float64(stripeSubRate.Load()) && !stuck
	evRaw := low && m.stripeEvacRawGrowLocked(n, rate, now) // HS2_MCONN_STRIPE_EVAC_RAW (mconn_stripe_evac.go)
	if stuck {
		nU := ul257Stat.Add(1)
		m.tr.ev("stripe: ul257 stuck raw upload (%.1f Mbit/s), grow now", rate*8/1e6)
		// once per raw upload, but at 4x scale on a congested Iran uplink
		// every tunnel-bound upload > 1 MB can qualify: rate-limited (b264),
		// #n is the running total so dropped lines still show in the count
		if ul257Log.ok(now) {
			log.Printf("mconn %x: ul257 stuck raw upload #%d (%.1f Mbit/s over %v, %d KB raw), grow now", m.id&0xffffff, nU, rate*8/1e6, w.Round(time.Millisecond), m.rawSent.Load()>>10)
		}
	}
	if low && !evRaw {
		if stripeLog && m.stN > 0 && rate > 1e6 {
			log.Printf("stripe %x: under the growth rate (%.1f Mbit/s over %d)", m.id&0xffffff, rate*8/1e6, n)
		}
		m.stHot = 0
		m.trLocked(trLow)
		if m.stN > 0 && rate > 1e6 {
			stripeDiag.growRate.Add(1) // a stripe that slowed (not every idle connection)
		}
		return false
	}
	if m.stHot++; !evRaw && !stuck && n == 1 && stripeAlone && m.stHot < int(stripeHotTicks.Load()) && !m.stripeFastOnLocked(up) &&
		!dlGo && !m.stripeDlHotOKLocked(up, rate) { // HS2_MCONN_STRIPE_DL_FAST (mconn_stripe_dlfast.go)
		// only a long download stripes: a reel or a page's burst is over in
		// a second or two, and each would take (and the pool dial) links
		m.trLocked(trNotHot)
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
		m.trLocked(trUpRoom)
		return false // mconn_stripe_up.go: a sub more only while the gate would widen
	}
	if stripeQTarget.Load() > 0 && !used(now) {
		if stripeLog {
			log.Printf("stripe %x: no growth, a sub unused (%.1f Mbit/s over %d)", m.id&0xffffff, rate*8/1e6, n)
		}
		stripeDiag.growUnused.Add(1)
		m.trLocked(trUnused)
		// the sender's scavenger gate leaves a sub unused (the path has a
		// queue): one more would only hold a link light users could take
		return false
	}
	if !m.stActive {
		if c := stripeStat.active.Add(1); c > stripeMaxConns && !m.stripeDlFairExtraLocked(c, up, rate) && !m.dl241SlowSlotLocked(c, up, rate, now) && !m.ul281SlotLocked(c, up, rate, now) { // HS2_MCONN_STRIPE_DL_FAIR: a fast download past the cap (mconn_stripe_dlfair.go)
			stripeStat.active.Add(-1)
			stripeStat.refused.Add(1)
			m.trLocked(trSlot)
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
	m.stGrowUp = up
	m.stripeUpCountLocked(up) // HS2_MCONN_STRIPE_FAST_SOLO (mconn_stripe_fast.go)
	if n == 1 && m.stripeFastOnLocked(up) && m.stHot < int(stripeHotTicks.Load()) {
		stripeFastStat.early.Add(1)
	}
	m.stFastMore = m.stripeFastPlanLocked(n, rate, up) // HS2_MCONN_STRIPE_FAST (mconn_stripe_fast.go)
	if up {
		m.stripeDlCountLocked(false)
	} else if stripeDlFast.Load() {
		m.stFastMore = m.stripeDlPlanLocked(n, rate, up) // HS2_MCONN_STRIPE_DL_FAST (mconn_stripe_dlfast.go)
	}
	m.stFastRate = rate
	m.tr.ev("stripe: grow %d -> %d (%.1f Mbit/s)", n, n+1, rate*8/1e6)
	m.trLocked(trGrow)
	if dlGo {
		log.Printf("mconn %x: download stripe at %v (%.0f Mbit/s, chain %d)", m.id&0xffffff, now.Sub(m.rawBorn).Round(time.Millisecond), rate*8/1e6, m.stFastMore) // HS2_MCONN_STRIPE_DL_FAST
	}
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
			avoid = append(avoid, avoidKey(s.key))
		}
	}
	avoid = append(avoid, m.deadAvoidLocked()...)
	if stripeXDir {
		avoid = append(avoid, stripeDirHint{up: m.stGrowUp}) // HS2_MCONN_STRIPE_XDIR (mconn_stripe_xdir.go)
	}
	if m.stFastMore > 0 && m.stripeChainHintLocked() {
		avoid = append(avoid, stripeWantHint{n: 1 + m.stFastMore}) // HS2_MCONN_STRIPE_FAST (mconn_stripe_fast.go)
	}
	avoid = m.stripeNoShareHintLocked(avoid) // HS2_MCONN_STRIPE_UP_NOSHARE (mconn_stripe_noshare.go)
	avoid = m.stripeCopyUpHintLocked(avoid)  // HS2_MCONN_STRIPE_SHARE_COPIES: uploads only (mconn_stripe_sharecopy.go)
	avoid = m.stripeDlShareHintLocked(avoid) // HS2_MCONN_STRIPE_DL_SHARE (mconn_stripe_dlshare.go)
	return avoid
}

// stripeIdleLocked (Iran, manager): give the slot back once no stripe sub is
// left (lite's idle rule closed them, or they died).
func (m *mConn) stripeIdleLocked() {
	if m.stActive && !m.stOpening && m.liveStripeLocked() == 0 {
		m.stActive = false
		stripeStat.active.Add(-1)
		m.stripeUpCountLocked(false)
		m.stripeDlCountLocked(false)
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
	m.stripeTraceEndLocked() // dl184 (mconn_stripe_dltrace.go)
	if m.stActive {
		m.stActive = false
		stripeStat.active.Add(-1)
	}
	m.stripeUpCountLocked(false)
	m.stripeDlCountLocked(false)
	m.mu.Unlock()
}

// openStripe (Iran, its own goroutine): one more stripe sub. A raw
// connection is upgraded with it (mconn_raw.go).
func (m *mConn) openStripe(avoid []any, raw bool) {
	if raw {
		err := m.upgradeRawPlane(planeStripe, avoid)
		for try := 0; err != errStripeSteerHeld && m.stripeFastRawRetry(try); try++ {
			err = m.upgradeRawPlane(planeStripe, avoid) // HS2_MCONN_STRIPE_FAST (mconn_stripe_fast.go)
		}
		m.mu.Lock()
		m.stOpening = false
		switch {
		case m.rawW.Load() && err == errStripeSteerHeld:
			// sd151: the steering refused a cut path's link (the raw link is
			// on a path not cut): no fast retries, a growing wait
			m.stRetryAt = m.stripeSteerHoldLocked(time.Now())
			m.stFastMore = 0
			m.stripeIdleLocked()
			m.trLocked(trSteerHeld)
		case m.rawW.Load(): // the open failed: still raw
			m.trLocked(trOpenFail)
			m.stRetryAt = m.stripeRetryLocked(time.Now())
			stripeStat.refused.Add(1)
			m.stripeIdleLocked()
		case err == nil:
			m.stSteerHeldN = 0
		}
		// HS2_MCONN_STRIPE_FAST: a chain left (stFastMore) goes on from the
		// manager's tick once the upgrade's handshake is over — or at once
		// with HS2_MCONN_STRIPE_OVERLAP (mconn_stripe_overlap.go)
		var av []any
		next := false
		if m.stFastMore > 0 && m.handshaking && m.mode == planeLight && m.stripeOverlapLocked() && m.stripeFastNextLocked() {
			next = true
			stripeOverlapStat.Add(1)
			av = m.stripeAvoidLocked()
		}
		m.mu.Unlock()
		if next {
			m.openStripe(av, false) // the chain goes on from there
		}
		return
	}
	sub, key, err := m.open(planeStripe, avoid)
	for try := 0; err != nil && err != errStripeSteerHeld && m.stripeFastRetry(try); try++ {
		sub, key, err = m.open(planeStripe, avoid) // HS2_MCONN_STRIPE_FAST: the pool's set-aside link, soon
	}
	m.mu.Lock()
	m.stOpening = false
	if err == errStripeSteerHeld {
		// sd151: the steering refused a cut path's link (the connection has
		// a healthy sub): no fast retries, a growing wait before the next
		// try (the pool was asked once; mconn_stripe_steer.go)
		m.stRetryAt = m.stripeSteerHoldLocked(time.Now())
		m.stFastMore = 0
		m.stripeIdleLocked()
		m.mu.Unlock()
		return
	}
	if err == nil {
		m.stSteerHeldN = 0
	}
	if err != nil {
		m.stRetryAt = m.stripeRetryLocked(time.Now())
		stripeStat.refused.Add(1)
		m.stripeIdleLocked()
		m.mu.Unlock()
		m.kickMgr()
		return
	}
	if m.ended || m.closing || m.mode != planeLight {
		m.stFastMore = 0
		m.mu.Unlock()
		sub.Close()
		m.kickMgr()
		return
	}
	s := m.addSubLocked(sub, key, false)
	m.stripeJoinLocked(s)
	m.sendCond.Broadcast()
	next := m.stripeFastNextLocked() // HS2_MCONN_STRIPE_FAST (mconn_stripe_fast.go)
	var av []any
	if next {
		av = m.stripeAvoidLocked()
	}
	m.mu.Unlock()
	m.startSub(s)
	if next {
		go m.openStripe(av, false)
	}
}

// stripeJoinLocked (Iran): s carries the stripe; the peer learns it from the
// STRIPE record queued on it.
func (m *mConn) stripeJoinLocked(s *msub) {
	s.stripe = true
	m.stN++
	m.b311MarkHeavy()
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
		s.stJoinAt = time.Now() // HS2_DL240_YOUNG (mconn_dl240.go)
		m.dl242ArmLocked(s)     // HS2_DL242_RESCUE (mconn_dl242.go)
		m.stN++
		m.b311MarkHeavy()
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
		m.stripeUpSubWinLocked(s) // HS2_STRIPE_UP_SUB_WIN_MB (mconn_stripe_fast.go)
		m.stripeRxJoin()          // HS2_MCONN_STRIPE_UP (mconn_stripe_up.go)
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
		if x.stripe && x != p && !x.retired && !x.dead && !m.stripeSubCutLocked(x) && !m.dl259SubCutLocked(x) {
			r++ // HS2_LB_STRIPE_STEER: a cut path's sub takes no slot of the width
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
	var bestCut *msub // HS2_LB_STRIPE_STEER: the best sub on a cut path
	var young *msub   // HS2_DL240_YOUNG: a young sub at its probation budget (mconn_dl240.go)
	bestCutQ, healthy := 0, false
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
	if w >= 0 && p != nil && m.stripeSubCutLocked(p) {
		w++ // HS2_LB_STRIPE_STEER: a cut primary takes no slot of the width either (st150)
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
				if m.dl259SubCutLocked(s) {
					continue // HS2_DL259_PATHQ: its path's queue is over the target (mconn_dl259.go)
				}
				if dlp217ShareQ > 0 && m.dlp217HoldLocked(s, time.Now()) {
					continue // HS2_FX212_DL_SHAREQ_MS: its shared link's queue is over the bound (mconn_dlp217.go)
				}
				if s.dl242.hold && m.dl242HoldLocked(s, time.Now()) {
					continue // HS2_DL242_RESCUE: rescued, nothing delivered yet (mconn_dl242.go)
				}
				if dl240Young.Load() && m.dl240HoldLocked(s, time.Now()) {
					if young == nil && s.fits(c.n) {
						young = s // HS2_DL240_YOUNG: only when no other sub can take it (mconn_dl240.go)
					}
					continue
				}
			}
		}
		if m.stripeSubCutLocked(s) {
			// HS2_LB_STRIPE_STEER (mconn_stripe_steer.go): a cut path's sub
			// only when no sub on a path not cut is in the stripe
			if s.fits(c.n) {
				if q := s.queueLocked(); bestCut == nil || q < bestCutQ {
					bestCut, bestCutQ = s, q
				}
			}
			continue
		}
		healthy = true
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
	if best == nil && young != nil {
		best = young // HS2_DL240_YOUNG: never a wait for the probation alone
		dl240Stat.fallback.Add(1)
	}
	if best == nil && !healthy && bestCut != nil {
		best = bestCut
		stripeSteerStat.sendCut.Add(1)
	} else if bestCut != nil && best != nil {
		stripeSteerStat.sendOff.Add(1)
	}
	if best == nil {
		return stripeWait
	}
	m.stRR++
	c.setOff(m.sendOff)
	m.sendOff += uint64(c.n)
	best.push(c)
	m.missLost = true // cs221
	if best != p {
		stripeDiag.subB.Add(uint64(c.n))
	}
	now := time.Now()
	if dlp217ShareQ > 0 && !m.iran {
		if best != p && dlp217Shared(m.subMeterOf(best)) {
			dlp217Stat.sent.Add(1)
		}
		dlp217Log(now)
	}
	for _, s := range m.subs {
		if s != best && !s.retired {
			s.lastHole = now
		}
	}
	best.lastPush = now
	m.stripeUpNotePushLocked(best, now) // hl222 (mconn_stripe_upqctl.go)
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
	// lk165: the same count, readable without stripeHeld.mu (stripeOn under
	// HS2_LM_FASTPICK). Written only here, under stripeHeld.mu, so it always
	// equals the map entry (clamped at 0 the way the delete clamps it).
	ml.stripeHeldN.Store(int32(max(stripeHeld.n[ml], 0)))
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
	if lmFastPick.Load() {
		// lk165: lock-free (98.8% of the edge's mutex wait in the sc161
		// storm was this lock, taken once per link per pick scan)
		return ml.stripeHeldN.Load() > 0
	}
	stripeHeld.mu.Lock()
	n := stripeHeld.n[ml]
	stripeHeld.mu.Unlock()
	return n > 0
}

// stripeFree: a serving link that carries no download and no real traffic.
func stripeFree(ml *managedLink) bool {
	return ml.up() && ml.heavy == 0 && ml.flowing == 0 && !ml.pressed && !stripeOn(ml) // up: the ZDU pin mark picks the generation
}

// pickStripe places a stripe sub (see the file comment). With stripeAlone
// (the default): a link set aside for the stripe first (mconn_stripe_pool.go),
// else a free link no user rides, never one of the last stripeKeepFree free
// links; none: it asks the pool for a link of its own. Without stripeAlone
// (the first version): a free link, the fewest users first.
func (m *LinkManager) pickStripe(avoid []any) (Link, func(), bool) {
	l, rel, ok, _ := m.pickStripeWhy(avoid)
	return l, rel, ok
}

// pickStripeWhy is pickStripe; held (sd151): no link because the steering
// refused a cut path's link while the connection has a healthy sub (the
// caller backs off instead of trying again at once: mconn_stripe_steer.go).
func (m *LinkManager) pickStripeWhy(avoid []any) (Link, func(), bool, bool) {
	l, rel, ok, held := m.pickStripeWhyCore(avoid)
	if !ok {
		zduNoLink(avoid)
		return nil, nil, false, held
	}
	if !zduPickOK(l, rel, avoid) { // ZDU generation pin (zdu_pin.go)
		return nil, nil, false, false
	}
	return l, rel, true, held
}

// pickStripeWhyCore is pickStripeWhy without the ZDU check.
func (m *LinkManager) pickStripeWhyCore(avoid []any) (Link, func(), bool, bool) {
	pool := m.exitInfo.Load()
	m.mu.Lock()
	defer m.mu.Unlock()
	free, empty := 0, 0 // empty (diag60): free links no user rides — the only ones stripeAlone takes
	var chosen *managedLink
	var bestUsers, bestRecent int
	bestRes, bestCut := false, false // bestCut: HS2_LB_STRIPE_STEER (mconn_stripe_steer.go)
	ties := 0
	for _, ml := range m.links {
		if m.gateInfo && ml.mtr != nil && !ml.mtr.infoDone.Load() || !linkMConn(ml, pool) {
			continue
		}
		res := stripeAlone && !ml.stripeRes.IsZero()
		if res {
			// set aside: the stripe's own, unless a download or a user came
			if !ml.up() || ml.heavy > 0 || ml.flowing > 0 || ml.users.Load() > 0 {
				continue
			}
		} else if !stripeFree(ml) {
			continue
		} else {
			free++
		}
		if avoided(ml.link, avoid) || stripeEvacSkip(ml.link) { // HS2_MCONN_STRIPE_EVAC (mconn_stripe_evac.go)
			continue
		}
		u, r := int(ml.users.Load()), ml.recent
		if u == 0 {
			empty++
		}
		if stripeAlone && u > 0 {
			// a light user's sub on a stripe link waits behind the stripe's
			// loss recovery (TCP is one byte stream): every loss on the link
			// holds the game's packet a round trip. Empty links only.
			continue
		}
		cut := m.stripeCutLocked(ml)
		switch {
		case chosen != nil && cut && !bestCut: // a path not cut first
		case chosen != nil && cut == bestCut && bestRes && !res:
		case chosen == nil || !cut && bestCut || res && !bestRes || u < bestUsers || u == bestUsers && r < bestRecent:
			chosen, bestUsers, bestRecent, bestRes, bestCut, ties = ml, u, r, res, cut, 1
		case u == bestUsers && r == bestRecent:
			ties++
			if rand.IntN(ties) == 0 {
				chosen = ml
			}
		}
	}
	hint, hinted := stripeDirOf(avoid)
	shared, xdir := false, false
	dlHot := false // dlp217: shared on a download's hot tier (HS2_FX212_DL_PRIM=2)
	// HS2_LB_STRIPE_STEER: only a cut path's link here — the cross-direction
	// pick (a link only stripes ride) may find one on a path not cut, and the
	// shared pick when the keep-free rule sends it there anyway; else it goes
	// on the cut one below (never a stall)
	origNeed := chosen == nil || !bestRes && free-1 < stripeKeepFree
	cutChosen := chosen != nil && bestCut
	need := origNeed || cutChosen
	m.stSteerNoCut = cutChosen // the shared / cross-direction picks: paths not cut only
	defer func() { m.stSteerNoCut = false }()
	upg := stripeUpgOf(avoid) // HS2_MCONN_STRIPE_UPG_IDLE (mconn_stripe_upgidle.go)
	idleUpg := false
	if upg && need && chosen != nil && !cutChosen {
		// an upload's raw upgrade takes an idle link even past the
		// keep-free rule (one link, asked again from the pool below): its
		// handshake must not wait behind a shared link's queue
		need, idleUpg = false, true
		stripeUpgStat.idle.Add(1)
	}
	if need && hinted && upg {
		stripeUpgStat.deferred.Add(1)
	}
	if need && hinted && !upg {
		// HS2_MCONN_STRIPE_XDIR (mconn_stripe_xdir.go): a link only the other
		// direction's stripes ride, before a light user's link
		if ml := m.pickStripeXDirLocked(avoid, pool, hint.up); ml != nil {
			chosen, bestRes, xdir, need = ml, false, true, false
			bestUsers, bestRecent = int(ml.users.Load()), ml.recent
		}
	}
	shareWhy := "share=off"
	if need && stripeShare && stripeUpNoShare(avoid) {
		shareWhy = "share=refused-upload-noshare"
	}
	if need && stripeShare && !stripeUpNoShare(avoid) && origNeed {
		// HS2_MCONN_STRIPE_SHARE (mconn_stripe_share.go): a busy pool has no
		// link without a user — a lightly loaded one instead (never only to
		// leave a cut path's free link: a light user's link is worse)
		ml, why := m.pickStripeSharedWhyLocked(avoid, pool)
		shareWhy = "share: " + why.String()
		if ml != nil {
			chosen, bestRes, shared, need = ml, false, true, false
			bestUsers, bestRecent = int(ml.users.Load()), ml.recent
			dlHot = why.tier == 2
		}
	}
	if need && hinted && upg {
		// the upgrade's last resort: a cross-direction link still beats none
		if ml := m.pickStripeXDirLocked(avoid, pool, hint.up); ml != nil {
			chosen, bestRes, xdir, need = ml, false, true, false
			bestUsers, bestRecent = int(ml.users.Load()), ml.recent
			stripeUpgStat.lastXdir.Add(1)
		}
	}
	switch {
	case need && cutChosen && stripeAlone && m.stripeHasHealthyLocked(avoid):
		// st150: the connection already has a sub on a path not cut, so a
		// cut path's sub would take nothing (the sender keeps off it): a
		// fresh link from the pool instead (the exit dials it on a path its
		// balancer weighs); the stripe runs on what it has meanwhile
		m.stSteerNoCut = false
		m.stripeSteerAskLocked(m.now(), stripeWantOf(avoid)) // sd151: deduped
		return nil, nil, false, true
	case need && cutChosen:
		// HS2_LB_STRIPE_STEER: no link on a path not cut — the cut one, by
		// the rules it had without the key (never a stall for the steering);
		// and a fresh link asked from the pool for the next pick (st150)
		need = origNeed
		if upg && need {
			need, idleUpg = false, true
			stripeUpgStat.idle.Add(1)
		}
		m.stSteerNoCut = false
		if need && hinted {
			if ml := m.pickStripeXDirLocked(avoid, pool, hint.up); ml != nil {
				chosen, bestRes, xdir, need = ml, false, true, false
				bestUsers, bestRecent = int(ml.users.Load()), ml.recent
			}
		}
		if need && stripeShare && !stripeUpNoShare(avoid) {
			// the shared pick as without the key (a cut path's link too)
			if ml, why := m.pickStripeSharedWhyLocked(avoid, pool); ml != nil {
				chosen, bestRes, shared, need = ml, false, true, false
				bestUsers, bestRecent = int(ml.users.Load()), ml.recent
				dlHot = why.tier == 2
			}
		}
		if !need {
			stripeSteerStat.fallback.Add(1)
			if stripeAlone && m.stripeCutLocked(chosen) {
				m.stripeAskLocked(m.now(), stripeWantOf(avoid)) // st150 (none taken: asked below)
			}
		}
	case cutChosen: // a shared or cross-direction link on a path not cut
		stripeSteerStat.moved.Add(1)
	case !need && m.stripeCutLocked(chosen):
		stripeSteerStat.fallback.Add(1)
	}
	if need && stripeDlShare.Load() && stripeDlShareOf(avoid) && !(hinted && hint.up) {
		// HS2_MCONN_STRIPE_DL_SHARE (mconn_stripe_dlshare.go): a download's
		// first subs share a heavy link rather than stay on one lossy link
		if ml := m.pickStripeDlShareLocked(avoid, pool); ml != nil {
			chosen, bestRes, need = ml, false, false
			bestUsers, bestRecent = int(ml.users.Load()), ml.recent
		}
	}
	if need {
		stripeDiag.pickNone.Add(1)
		if stripeAlone {
			m.stripeAskLocked(m.now(), stripeWantOf(avoid))
		}
		if stripeLog {
			// free: links with no download and nothing flowing; empty: those
			// of them no user rides (diag60: free>0 with empty=0 is a pool
			// where every link has an idle user)
			log.Printf("stripe pick: none (free=%d empty=%d chosen=%v up=%v) %s — asked the pool for a link", free, empty, chosen != nil, hinted && hint.up, shareWhy)
			log.Printf("stripe pickwhy %s", m.stripePickWhyLocked(avoid, pool)) // dl188 (mconn_stripe_pickwhy.go)
		}
		return nil, nil, false, false
	}
	if idleUpg && stripeAlone {
		m.stripeAskLocked(m.now(), stripeWantOf(avoid)) // the free link it took, again
	}
	m.placeLocked(chosen)
	m.stripeSteerNoteLocked(chosen) // HS2_LB_STRIPE_STEER(_LOG) (mconn_stripe_steer.go)
	if bestRes {
		chosen.stripeRes = time.Time{}       // its hold passes to the sub
		m.stripeReserveRefillLocked(m.now()) // HS2_MCONN_STRIPE_FAST_RESERVE (mconn_stripe_fast.go)
	} else {
		stripeHold(chosen, 1)
	}
	if shared {
		chosen.stShared.Add(1)
		if dlHot {
			chosen.stDlHot.Add(1) // dlp217
		}
	}
	dirN := stripeDirN(chosen, hint.up, hinted)
	if dirN != nil {
		dirN.Add(1)
	}
	if chosen.mtr != nil && stripeAlone {
		// the TUN lane (HS2_L3_LANE) reads heavyN: heavy now, not at the
		// sampler's next tick (which keeps it while the link is held)
		if chosen.mtr.heavyN.Load() < 1 {
			chosen.mtr.heavyN.Store(1)
		}
	}
	if stripeLog {
		log.Printf("stripe pick: link %d set-aside=%v shared=%v xdir=%v upgrade=%v users=%d recent=%d free=%d", chosen.id, bestRes, shared, xdir, upg, bestUsers, bestRecent, free)
	}
	rel := m.releaseFor(chosen)
	var once sync.Once
	return chosen.link, func() {
		once.Do(func() {
			rel()
			stripeHold(chosen, -1)
			if shared {
				chosen.stShared.Add(-1)
				if dlHot {
					chosen.stDlHot.Add(-1)
				}
			}
			if dirN != nil {
				dirN.Add(-1)
			}
		})
	}, true, false
}

// openStripe is the edge's open for planeStripe: one try, no waiting (the
// connection runs on what it has; the manager tries again later).
func (e *mconnEdge) openStripe(avoid []any) (mSub, any, error) {
	avoid = e.zduAvoid(avoid) // ZDU generation pin (zdu_pin.go)
	e.mu.Lock()
	if e.ended || !e.first {
		e.mu.Unlock()
		return nil, nil, errMConnNoLink
	}
	if b312OpenNoLock.Load() {
		e.mu.Unlock()
		return e.openStripeNoLock(avoid) // b312 (mconn_b312_open.go)
	}
	defer e.mu.Unlock()
	b312Open.locked.Add(1)
	l, rel, ok, held := e.lm.pickStripeWhy(avoid)
	if held {
		return nil, nil, errStripeSteerHeld // sd151: no retry spin
	}
	if !ok {
		return nil, nil, errMConnNoLink
	}
	st, err := l.OpenStream()
	if err == nil {
		autoWinOffForStripe(e.lm, st) // HS2_SMUX_AUTOWIN (smux_autowin.go)
		dl76Mark(st)
		if _, err = st.Write(mconnHeader(e.port, e.id, mconnRoleJoin)); err == nil {
			// evacHS: HS2_MCONN_STRIPE_EVAC_DOWN moves a stripe sub through the
			// close handshake (mconn_stripe_evac.go)
			s := &mconnSub{stream: st, plane: planeStripe, release: rel, evacHS: peerCloseHS(l), edge: e}
			mconnPlaneGauge(planeStripe).Add(1)
			e.subs = append(e.subs, s)
			e.opened++
			e.zduPinTo(l)
			return s, l, nil
		}
		st.Close()
	}
	rel()
	return nil, nil, errMConnNoLink
}
