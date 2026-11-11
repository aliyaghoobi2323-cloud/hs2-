package engine

import (
	"log"
	"net"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

// Call redundancy for port-forwarded connections (HS2_MCONN_CALL, needs
// HS2_MCONN=lite; off by default).
//
// lite duplicates a connection over two links only while it moves at most
// mDupMaxRate (384 kbit/s) each way. A video call (0.3–3 Mbit/s each way)
// is above that, so it rides its one first link — the old path's placement,
// often a link that also carries downloads — and every loss on that TCP link
// stalls it for an RTT or more, behind whatever the link has queued. This
// file lets a CALL-LIKE connection keep a copy on a second light link:
//
//   - Call-like (one sample a second, both directions): the busier direction
//     moves at most mCallMaxRate, the quieter one at least mCallMinRate (a
//     two-way conversation, not a one-way stream with its small requests),
//     and the busier one stays within a factor mCallSteady of the previous
//     sample (steady frames, no bulk bursts); mCallStreak such samples in a
//     row. A page load or a reel is one or two bursts and never gets there.
//   - Upgrade (Iran): lite's own (raw first sub, UPGRADE handshake, a join
//     sub on another path's link with no download: pickPlane, copy-only), now
//     also for a call-like connection while the call budget allows it.
//   - Duplication (each side, its own sending direction): lite's dual mode
//     stops copying above mDupMaxRate; a call-like connection with two live
//     subs keeps copying up to mCallMaxRate, while the budget allows. Each
//     side decides alone from what it sees (no new record, no capability
//     bit): a peer without this code (or with the key off) just sends one
//     copy above mDupMaxRate, as before, and reads both copies as before.
//   - Budget (strict, process-wide): the call copies of the last
//     mCallBudgetWin seconds stay under mconnCallBudget bytes a second
//     (HS2_MCONN_CALL=1: 1 MB/s = 8 Mbit/s, about four 2 Mbit/s calls;
//     HS2_MCONN_CALL=N with N > 1: N Mbit/s). Over it, new upgrades are
//     refused and duplicating calls fall back to one copy at their next
//     window. Heavy links never get a copy (the join sub avoids links with a
//     download), and a connection above mCallMaxRate is never copied.
//
// Key off: mconnCall is false, every hook below returns at once, and the
// lite rules are exactly as before.

// mconnCall: HS2_MCONN_CALL set (and not 0). mconnCallBudget: bytes/s.
var (
	mconnCall       atomic.Bool
	mconnCallBudget atomic.Int64
)

func setMConnCall(v string) {
	if v == "" || v == "0" {
		mconnCall.Store(false)
		return
	}
	b := int64(mCallBudgetDef)
	if n, err := strconv.ParseFloat(v, 64); err == nil && n > 1 {
		b = int64(n * 1e6 / 8)
	}
	mconnCallBudget.Store(b)
	mconnCall.Store(true)
}

const (
	mCallMaxRate   = 400_000 // bytes/s, the busier direction (3.2 Mbit/s)
	mCallMinRate   = 3_000   // bytes/s, the quieter direction (24 kbit/s)
	mCallSteady    = 2.5     // busier direction vs. the previous sample
	mCallStreak    = 2       // call-like samples in a row
	mCallBudgetDef = 1_000_000
	mCallBudgetWin = 5 // seconds (callBud's slots)
	mCallLogEvery  = 10 * time.Second
)

// callState is an mConn's call bookkeeping (under mConn.mu).
type callState struct {
	callOn bool // this side's direction is copied as a call now
	callUp bool // Iran: the upgrade asked for is for a call (callBud)

	// Iran's upgrade sampler (liteWantDualLocked)
	upStreak int
	upPrev   float64
	// the send window (noteWriteLocked)
	txStreak int
	txPrev   float64
	txRecvd  uint64 // receive offset at the window start

	callPairAt int // paired copies: the next chunk's first sub (HS2_MCONN_CALL_SUBS)

	// HS2_MCONN_CALL_PICK: the subs' link scores, read at scoreAt
	scoreAt  time.Time
	scoreN   int
	scoreSub [mCallSubsMax + 2]*msub
	score    [mCallSubsMax + 2]int64
}

// callBud: the process's call copies (dup) and the call bytes sent while
// copying (all), per second.
var callBud liteBudget

// callStat: totals for the 10 s log line.
var callStat struct {
	upgrades, refused, dup, all, avoided atomic.Uint64
	last                                 atomic.Int64
	prevUp, prevDup, prevAll             atomic.Uint64
}

// callCapped: the call copies of the last mCallBudgetWin seconds exceed the
// budget.
func callCapped(now int64) bool {
	var dup uint64
	for i := range callBud.slots {
		s := &callBud.slots[i]
		if sec := s.sec.Load(); sec > now-mCallBudgetWin && sec <= now {
			dup += s.dup.Load()
		}
	}
	return dup > uint64(mconnCallBudget.Load())*mCallBudgetWin
}

// callLikeSample: one sample of both directions (bytes over w); prev and
// streak are the sampler's state. True once mCallStreak samples in a row
// were call-like.
func callLikeSample(sent, got uint64, w time.Duration, prev *float64, streak *int) bool {
	sec := w.Seconds()
	if sec <= 0 {
		return false
	}
	hi, lo := float64(max(sent, got))/sec, float64(min(sent, got))/sec
	ok := hi <= mCallMaxRate && lo >= mCallMinRate &&
		(*prev == 0 || hi <= *prev*mCallSteady && hi*mCallSteady >= *prev)
	*prev = hi
	if !ok {
		*streak = 0
		return false
	}
	*streak++
	return *streak >= mCallStreak
}

// callWantDualLocked (Iran, lite, one sub; liteWantDualLocked's call rule):
// the sample was over mDupMaxRate one way; true when the connection is
// call-like and the budget has room.
func (m *mConn) callWantDualLocked(sent, got uint64, w time.Duration) bool {
	m.callUp = false
	if !mconnCall.Load() {
		return false
	}
	if !callLikeSample(sent, got, w, &m.upPrev, &m.upStreak) {
		return false
	}
	m.callUp = true
	return true
}

// smallWantLocked (liteWantDualLocked's small rule result) resets the call
// sampler: a small connection upgrades by lite's own rule.
func (m *mConn) smallWantLocked() bool {
	m.callUp = false
	return true
}

// upgradeCappedLocked: the budget that refuses this upgrade (lite's 5% of
// all bytes for a small connection, the call budget for a call).
func (m *mConn) upgradeCappedLocked(now time.Time) bool {
	if m.callUp {
		if callCapped(now.Unix()) {
			callStat.refused.Add(1)
			return true
		}
		callStat.upgrades.Add(1)
		m.tr.ev("call: upgrade to dual")
		return false
	}
	return liteBud.capped(now.Unix())
}

// dupMaxLocked: the most a window may carry before this direction stops
// copying (noteWriteLocked).
func (m *mConn) dupMaxLocked() int {
	if m.callOn {
		return mCallMaxRate
	}
	return mDupMaxRate
}

// callRollLocked (noteWriteLocked, at a window's end of w; sent = its bytes):
// decides whether the next window copies as a call. dupOff is lite's own
// decision; the result replaces it.
func (m *mConn) callRollLocked(now time.Time, w time.Duration, sent int, dupOff bool) bool {
	if !mconnCall.Load() || !m.lite {
		return dupOff
	}
	r := &m.r
	r.mu.Lock()
	next := r.next
	r.mu.Unlock()
	got := next - m.txRecvd
	if next < m.txRecvd {
		got = 0
	}
	m.txRecvd = next
	like := w < 2*mHeavyWindow && callLikeSample(uint64(sent), got, w, &m.txPrev, &m.txStreak)
	was := m.callOn
	m.callOn = like && dupOff && !m.callCopyOff() && m.liveSubsLocked() >= 2 && !callCapped(now.Unix())
	if m.callOn != was {
		m.tr.ev("call: copy %v (sent=%d got=%d in %v)", m.callOn, sent, got, w.Round(time.Millisecond))
		if m.callOn && m.iran && mconnCallSubs > 2 {
			m.kickMgr() // open the pair subs
		}
	}
	callLog(now)
	m.callMarkLocked(now)             // HS2_MCONN_STRIPE_CALLFREE (mconn_stripe_callfree.go)
	m.rescueCallMarkLocked(now, like) // HS2_LB_RESCUE_CALLFREE (lb_rescue_callfree.go)
	if m.callOn {
		return false
	}
	return dupOff
}

func (m *mConn) liveSubsLocked() int {
	n := 0
	for _, s := range m.subs {
		if !s.retired && !s.dead {
			n++
		}
	}
	return n
}

// budAddLocked counts a queued chunk (all bytes) and its extra copies (dup)
// in lite's budget, or the copies in the call budget while copying as a call.
func (m *mConn) budAddLocked(now time.Time, all, dup int) {
	if m.callOn && dup > 0 {
		liteBud.add(now.Unix(), all, 0)
		callBud.add(now.Unix(), all, dup)
		callStat.dup.Add(uint64(dup))
		callStat.all.Add(uint64(all))
		return
	}
	liteBud.add(now.Unix(), all, dup)
}

// callLog writes the 10 s line (only when something was copied).
func callLog(now time.Time) {
	last := callStat.last.Load()
	if now.UnixNano()-last < int64(mCallLogEvery) || !callStat.last.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	if last == 0 {
		return
	}
	up, dup, all := callStat.upgrades.Load(), callStat.dup.Load(), callStat.all.Load()
	pu, pd, pa := callStat.prevUp.Swap(up), callStat.prevDup.Swap(dup), callStat.prevAll.Swap(all)
	if up == pu && dup == pd {
		return
	}
	log.Printf("mconn call (HS2_MCONN_CALL), 10s: %d upgrades · %d KB call bytes copied once more (%d KB sent) · %d upgrades refused in all (budget) · %d call subs placed away from other copies",
		up-pu, (dup-pd)>>10, (all-pa)>>10, callStat.refused.Load(), callStat.avoided.Load())
}

// ---- paired copies over more subs (HS2_MCONN_CALL_SUBS=3..6) ------------
//
// A TCP link stalls for about an RTT on each of ITS lost segments, so the
// share of time a link holds a call back grows with the packets the call
// puts on it: two full copies at 3% loss are each stalled most of the time.
// With mconnCallSubs subs (Iran opens the extra ones, each on another light
// link, while the connection copies as a call), every chunk still goes out
// twice, but on two of them in turn (sub i and i+1 for chunk i): each link
// carries 2/n of the call, stalls n/2 times less often, and a chunk is late
// only when both of its two links stall at once. Same extra bytes as two
// full copies. The receiver is unchanged (reassembly by offset), and the
// peer pairs its own direction the same way once it has three subs or
// more (each side by its own rule; no new record).
//
// A sub that misses a chunk on purpose has no hole: the chunk is on two
// other live subs. A chunk that got one copy only (the other sub full)
// marks every other sub's hole, as lite's dual mode does, so a sub death
// within mHoleSafe after it resets the connection rather than leave a gap.

// mconnCallSubs: subs a call connection opens (2: two full copies).
var mconnCallSubs = 2

func init() {
	if n, err := strconv.Atoi(os.Getenv("HS2_MCONN_CALL_SUBS")); err == nil && n >= 2 {
		mconnCallSubs = min(n, mCallSubsMax)
	}
}

const mCallSubsMax = 6

// callPairLocked (sendLocked, dual mode) queues c on two subs in turn;
// ok: done, wait: no room on any sub. Neither: the usual dual mode.
func (m *mConn) callPairLocked(c *mchunk) (ok, wait bool) {
	if !mconnCall.Load() || !m.lite {
		return false, false
	}
	var live [mCallSubsMax + 2]*msub
	n := 0
	for _, s := range m.subs {
		if !s.retired && !s.dead && n < len(live) {
			live[n] = s
			n++
		}
	}
	if n < 3 {
		return false, false
	}
	var pick [2]*msub
	k := 0
	if mconnCallPick {
		pick, k = m.callPickLocked(live[:n], n, c.n)
	} else {
		for i := 0; i < n && k < 2; i++ {
			if s := live[(m.callPairAt+i)%n]; s.fits(c.n) {
				pick[k] = s
				k++
			}
		}
	}
	if k == 0 {
		return false, true
	}
	m.callPairAt++
	c.setOff(m.sendOff)
	m.sendOff += uint64(c.n)
	now := time.Now()
	for _, s := range pick[:k] {
		s.push(c)
		s.lastPush = now
	}
	if k < 2 {
		for _, s := range live[:n] {
			if s != pick[0] {
				s.lastHole = now
			}
		}
		obs.mconnMissed.Add(1)
	} else {
		obs.mconnDupBytes.Add(uint64(c.n))
	}
	m.budAddLocked(now, c.n, c.n*(k-1))
	return true, false
}

// callMoreSubsLocked (Iran's manager, two live subs or more): while the
// connection copies as a call, open another sub (on a link none of them
// uses) until it has mconnCallSubs.
func (m *mConn) callMoreSubsLocked(live []*msub) time.Duration {
	if !m.callOn || len(live) >= mconnCallSubs || m.peerFin || m.wclosed {
		return 0
	}
	if m.lightOpening {
		return 200 * time.Millisecond // look again once it is open
	}
	if d := time.Until(m.lightRetryAt); d > 0 {
		return d
	}
	m.lightOpening = true
	avoid := make([]any, 0, len(live)+len(m.deadKeys))
	for _, s := range live {
		avoid = append(avoid, s.key)
	}
	avoid = m.callAvoidLocked(append(avoid, m.deadKeys...))
	m.tr.ev("call: open pair sub %d", len(live)+1)
	go m.openLight(avoid)
	return 200 * time.Millisecond
}

// ---- picking the two subs (HS2_MCONN_CALL_PICK=1) -------------------------
//
// With three subs or more, each chunk goes on the two whose links are best
// placed right now, by their sockets' TCP_INFO (read at most every
// mCallPickEvery per connection): not in loss recovery first, then the
// fewest bytes held unsent; ties in turn. A link that is repairing a loss
// holds back everything written after the hole, so a copy put there now
// arrives an RTT late; the other copies avoid it.

var mconnCallPick = os.Getenv("HS2_MCONN_CALL_PICK") == "1"

const (
	mCallStalled   = 1 << 40
	mCallPickEvery = 5 * time.Millisecond
)

// subTCP: the kernel socket of the link s rides (nil: unknown).
func subTCP(s *msub) *net.TCPConn {
	switch r := s.rw.(type) {
	case *mconnSub:
		if cs, ok := r.stream.(*countedStream); ok && cs.link != nil {
			return tcpConnOf(cs.link.tls)
		}
	case exitCountedStream:
		if r.c != nil && r.c.mtr != nil {
			return r.c.mtr.tcp
		}
	}
	return nil
}

// callScoresLocked refreshes the scores of live (in that order).
func (m *mConn) callScoresLocked(live []*msub) {
	now := time.Now()
	same := len(live) == m.scoreN
	for i := 0; same && i < len(live); i++ {
		same = m.scoreSub[i] == live[i]
	}
	if same && now.Sub(m.scoreAt) < mCallPickEvery {
		return
	}
	m.scoreAt, m.scoreN = now, len(live)
	for i, s := range live {
		m.scoreSub[i] = s
		m.score[i] = 0
		if tc := subTCP(s); tc != nil {
			if v, ok := tcpBusy(tc); ok {
				m.score[i] = v
			}
		}
	}
}

// callPickLocked chooses up to two fitting subs of live by score (ties from
// start, in turn).
func (m *mConn) callPickLocked(live []*msub, n, size int) ([2]*msub, int) {
	m.callScoresLocked(live)
	var pick [2]*msub
	var ps [2]int64
	k := 0
	for j := 0; j < n; j++ {
		i := (m.callPairAt + j) % n
		s := live[i]
		if !s.fits(size) {
			continue
		}
		v := m.score[i]
		switch {
		case k < 2:
			pick[k], ps[k] = s, v
			k++
		case v < ps[1]:
			pick[1], ps[1] = s, v
		default:
			continue
		}
		if k == 2 && ps[1] < ps[0] {
			pick[0], pick[1], ps[0], ps[1] = pick[1], pick[0], ps[1], ps[0]
		}
	}
	return pick, k
}

// ---- not costing light users (HS2_MCONN_CALL_DIR, HS2_MCONN_CALL_AVOID) ----
//
// A copy adds a call's packets to a light link; at 3% loss that link stalls
// more often, and the light flows riding it (pings, games, lite's own small
// copies) wait with it. Two ways to spend less of the light links:
//
//   - HS2_MCONN_CALL_DIR=down: Iran does not copy its own direction (the
//     upload, Iran -> Kharej); Kharej still copies the download direction.
//     Half the copy bytes, on the links Kharej writes.
//   - HS2_MCONN_CALL_AVOID=1: the call's second sub goes to a link with no
//     download AND no other connection's copy sub (lite's second subs of
//     pings, games, echoes), the least loaded of those; only when there is
//     none, the usual light pick.

var (
	mconnCallDown  = os.Getenv("HS2_MCONN_CALL_DIR") == "down"
	mconnCallAvoid bool // loadEnvKeys
)

// callAvoidMark in an avoid list asks the opener for HS2_MCONN_CALL_AVOID's
// placement (an mOpener has no other way to tell a call's sub).
type callAvoidMark struct{}

// callCopyOff: this side's direction is never copied (HS2_MCONN_CALL_DIR).
func (m *mConn) callCopyOff() bool { return mconnCallDown && m.iran }

// callAvoidLocked adds the mark for a call's upgrade or pair sub.
func (m *mConn) callAvoidLocked(avoid []any) []any {
	if mconnCallAvoid && (m.callUp || m.callOn) {
		return append(avoid, callAvoidMark{})
	}
	return avoid
}

// pickPlaneCall is pickPlane, or with the mark the call placement.
func (m *LinkManager) pickPlaneCall(p mPlane, avoid []any) (Link, func(), bool) {
	avoid, resc := takeRescueMark(avoid) // HS2_LB_RESCUE_CALLFREE (lb_rescue_callfree.go)
	if resc && p == planeLight {
		pool := m.exitInfo.Load()
		m.mu.Lock()
		if ml := m.pickRescueLocked(avoid, pool); ml != nil {
			rescueCallFreeN.Add(1)
			m.placeLocked(ml)
			m.mu.Unlock()
			return ml.link, m.releaseFor(ml), true
		}
		m.mu.Unlock()
	}
	mark := false
	for i, a := range avoid {
		if _, ok := a.(callAvoidMark); ok {
			mark = true
			avoid = append(avoid[:i:i], avoid[i+1:]...)
			break
		}
	}
	if !mark || p != planeLight {
		return m.pickPlane(p, avoid)
	}
	pool := m.exitInfo.Load()
	m.mu.Lock()
	ml := m.pickPlaneTiers(p, avoid, pool, func(ml *managedLink) bool {
		return ml.heavy == 0 && !stripeOn(ml) && !ml.degraded && !ml.draining && !ml.suspect && linkCopies(ml.link) == 0
	})
	if ml != nil {
		callStat.avoided.Add(1)
		m.placeLocked(ml)
		m.mu.Unlock()
		return ml.link, m.releaseFor(ml), true
	}
	m.mu.Unlock()
	return m.pickPlane(p, avoid)
}

// linkCopies: the copy-only streams (lite's second subs) open on l.
func linkCopies(l Link) int {
	ml, ok := l.(*mtcpLink)
	if !ok {
		return 0
	}
	n := 0
	ml.flowMu.Lock()
	for cs := range ml.flows {
		if cs.copyOnly.Load() {
			n++
		}
	}
	ml.flowMu.Unlock()
	return n
}
