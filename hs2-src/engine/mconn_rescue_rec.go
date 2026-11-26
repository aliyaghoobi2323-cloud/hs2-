package engine

import (
	"encoding/binary"
	"fmt"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// er97: RESCUE of a records-mode light connection (HS2_MCONN_RESCUE_REC,
// on whenever HS2_MCONN_RESCUE is on unless set to 0; both sides).
//
// er94 left ~208 crowd failures per blackhole event with RESCUE on: every one
// on a connection that had already left raw (upgraded, lite records, one or
// two light subs) whose every live sub sat on the silent path. RESCUE covers
// only a raw first sub. Now:
//
//   - each side of a role-3 connection (capExtMConnRescue) keeps, once in
//     records mode, the last bytes it wrote as DATA in a ring by offset (the
//     same rescueRing, rescueRingCap per connection, rescueMemMax for the
//     process with the raw rings; freed when idle for rescueKeep, kept while a
//     rescue waits for its ACK, freed when the connection ends);
//   - Iran: when EVERY live sub of such a connection sits on links of the
//     silent set (rescueSilentLinks: the same trigger as RESCUE), it opens a
//     join on another path, retires the old subs (no new bytes on them) and
//     sends RREC(Bi = sendOff, Ri = its receive offset) as the join's first
//     record. A connection with one live sub on a good path is not touched
//     (the copies there carry it);
//   - Kharej (only if it announced capExtMConnRescueRec): if its ring holds
//     [Ri, Bk) it retires every other sub, answers RREC-ACK(Bk, Rk) and
//     resends [Ri, Bk) on the join; else RREC-NAK(Bk, Rk) and keeps its subs
//     (the join becomes its primary). Iran resends [Rk, Bi) on ACK or NAK.
//     A FIN already sent is sent again. Reassembly drops what it already has
//     (by offset): nothing is delivered twice, a false trigger costs a resend.
//   - Iran's ring short of Rk on the ACK ends the connection (counted
//     nocover): the old outcome.
//
// A peer without capExtMConnRescueRec never sees 0x10-0x12.

const (
	mRecRRescue    = 0x10 // Iran → Kharej: kind + u64 Bi + u64 Ri
	mRecRRescueAck = 0x11 // Kharej → Iran: kind + u64 Bk + u64 Rk
	mRecRRescueNak = 0x12 // Kharej → Iran: kind + u64 Bk + u64 Rk (ring short)

	// capExtMConnRescueRec: ext bit9 of kindInfo: this server keeps records
	// rings and answers RREC.
	capExtMConnRescueRec = 1 << 9
)

var rescueRecOn atomic.Bool // loadEnvKeys: HS2_MCONN_RESCUE_REC (!= "0")

// rrRingCap / rrMemMax: a records ring holds the last rrRingCap bytes a conn
// wrote (light conns: a request or response in flight); all records rings
// share rrMemMax, apart from the raw rings' rescueMemMax (er97 lab B97_1: with
// 128 KB rings the exit's records rings took the shared 64 MB and starved the
// raw rings, overcap 1096).
var (
	rrRingCap       = envIntDef("HS2_MCONN_RESCUE_REC_KB", 32) << 10
	rrMemMax  int64 = int64(envIntDef("HS2_MCONN_RESCUE_REC_MB", 48)) << 20
	// cd152: a fixed 32 KB ring never covered a call. A 2 Mbit/s call
	// writes 250 KB/s and a records rescue starts after its links went
	// silent for seconds: lab cd152 (rel64 and rel63c alike) had every RREC
	// of a call answered short (Iran ring short of [Rk, Bi) by 64 KB, the
	// exit's NAK) and the call ended (mconn: a sub died holding the only
	// copy). A records ring now holds rrRateSecs seconds of the conn's write
	// rate (the last full second), at least rrRingCap, at most rrRingMax;
	// growth past rrRingCap that does not fit the budget keeps the ring as
	// it is. HS2_MCONN_RESCUE_REC_MAXKB=32 restores the fixed cap. The bytes
	// above rrRingCap come from their own budget rrBigMax
	// (HS2_MCONN_RESCUE_REC_BIGMB; unset: sized from RAM, rrBigInit in
	// mconn_rescue_budget.go — cd152's fixed 64 MB was ~85 calls of 750 KB): lab cd152,
	// 3000 crowd users kept ~2800 full 32 KB rings and pinned the exit's
	// rrMemCap (rel63c: overcap 341575; a shared budget left the call rings
	// at 32 KB and the exit NAKed).
	rrRateSecs       = envIntDef("HS2_MCONN_RESCUE_REC_SECS", 3)
	rrRingMax        = envIntDef("HS2_MCONN_RESCUE_REC_MAXKB", 1024) << 10
	rrBigMax   int64 = rrBigInit()
)

// rrBigTake: n bytes of a records ring above rrRingCap, from rrBigMax.
func rrBigTake(n int) bool {
	if rrStat.big.Add(int64(n)) > rrBigMax {
		rrStat.big.Add(-int64(n))
		return false
	}
	return true
}

// rrCapFor: a records ring's cap for a conn writing rate bytes/s.
func rrCapFor(rate int) int {
	c := rrRingCap
	if rrRateSecs > 0 && rate > 0 {
		if w := min(rate*rrRateSecs, rrRingMax); w > c {
			c = w
		}
	}
	return c
}

func rescueRecFromEnv() bool { return os.Getenv("HS2_MCONN_RESCUE_REC") != "0" }

var rrStat struct {
	conns    atomic.Int64  // connections keeping a records ring
	mem      atomic.Int64  // bytes in records rings (budget rrMemMax)
	big      atomic.Int64  // cd152: of those, above rrRingCap per ring (budget rrBigMax)
	started  atomic.Uint64 // Iran: RREC sent
	served   atomic.Uint64 // Kharej: ACK sent
	nak      atomic.Uint64 // Kharej: NAK sent
	acked    atomic.Uint64 // Iran: ACK read
	nakd     atomic.Uint64 // Iran: NAK read
	noCover  atomic.Uint64 // Iran: ring short of Rk on the ACK (conn ended)
	failed   atomic.Uint64 // Iran: the join could not open / state changed
	refused  atomic.Uint64 // Kharej: RREC refused by state
	resent   atomic.Uint64 // bytes resent (both sides)
	lostJoin atomic.Uint64 // Iran: the join died before the answer
	endWait  atomic.Uint64 // Iran: the conn ended waiting for the answer
	gap      atomic.Uint64 // a ring stopped (offset gap / over the cap)
	fin      atomic.Uint64 // FINs sent again
	// mb174 (mconn_rescue_bigring.go)
	bigNo    atomic.Uint64 // growth past rrRingCap refused by the budget (its tier)
	shrunk   atomic.Uint64 // rings shrunk after a low rate
	shrunkKB atomic.Uint64 // ... big KB returned
	bigCall  atomic.Int64  // big bytes held by call rings
}

// rrState: per connection (embedded in rescueState).
type rrState struct {
	rkMu      sync.Mutex
	rk        rescueRing // records ring (under rkMu)
	rkEnd     uint64     // offset after the ring's last byte (under rkMu)
	rkInit    bool       // under rkMu
	rrPeer    bool       // Iran: the exit announced capExtMConnRescueRec (under mu)
	rrPhase   int        // under mu: 0 none, 1 Iran waits, 2 done
	rrWait    atomic.Bool
	rrBi      uint64
	rrSnap    []byte // Iran: the ring's bytes [rrSnap0, rrBi) at RREC (the ring rolls on)
	rrSnap0   uint64
	rrSub     *msub
	rrOld     []*msub
	rrT0      time.Time
	rrRetried bool   // rs145: the lost join was replaced once
	rrDead    any    // rs145: the lost join's link (avoided by the retry)
	rrLate    bool   // rs169: a replaced join's answer did the resend
	rrSnapP   []byte // rs169: the replaced RREC's snapshot [rrSnapP0, its Bi), until the retry's RREC
	rrSnapP0  uint64
	rrExt0    uint64 // b375: the snapshot's start before the extension (^0: none)

	// b375: retired subs' DATA chunks below b375HoldBi (referenced, no copy)
	b375Hold   []*mchunk
	b375HoldBi uint64
}

var rrConns sync.Map // *mConn → struct{}

func rescueRecLogLine() string {
	s := &rrStat
	return fmt.Sprintf("mconn rescue-rec: conns %d (%d KB, big %d KB of %d MB, call %d KB) · started %d acked %d nakd %d · served %d nak %d refused %d · nocover %d failed %d lostjoin %d endwait %d · resent %d KB · gap %d fin %d · bigno %d shrunk %d (%d KB)",
		s.conns.Load(), s.mem.Load()>>10, s.big.Load()>>10, rrBigMax>>20, s.bigCall.Load()>>10, s.started.Load(), s.acked.Load(), s.nakd.Load(), s.served.Load(), s.nak.Load(), s.refused.Load(),
		s.noCover.Load(), s.failed.Load(), s.lostJoin.Load(), s.endWait.Load(), s.resent.Load()>>10, s.gap.Load(), s.fin.Load(),
		s.bigNo.Load(), s.shrunk.Load(), s.shrunkKB.Load()) + b375LogPart()
}

// rrKeepLocked (under mu, Write, right after sendLocked queued n bytes of p
// at sendOff-n): p's bytes go to the records ring.
func (m *mConn) rrKeepLocked(p []byte) {
	if !m.rscCap || !rescueOn.Load() || !rescueRecOn.Load() || m.mode != planeLight || m.stN > 0 || m.ended {
		return
	}
	off := m.sendOff - uint64(len(p))
	m.rkMu.Lock()
	defer m.rkMu.Unlock()
	if !m.rkInit {
		m.rkInit = true
		m.rk.on, m.rk.rec = true, true
		m.rkEnd = off
		rrConns.Store(m, struct{}{})
		rrStat.conns.Add(1)
		rescueSweepOnce.Do(func() { go rescueSweeper() })
	}
	if darkcallRunning() {
		darkcallOnce.Do(func() { go darkcallLoop() }) // cs193 (mconn_darkcall.go)
	}
	if !m.rk.on {
		return
	}
	// mb174: calls first in the big budget (copied as a call now, or
	// call-like within callFreeTTL: callOn needs two live subs)
	m.rk.call = m.callOn || !m.callLikeAt.IsZero() && time.Since(m.callLikeAt) < callFreeTTL
	if off != m.rkEnd {
		m.rrStopLocked() // a gap (a stripe or call path took bytes): no cover
		return
	}
	m.b375InflLocked() // b375: the cap's in-flight floor
	m.rk.add(p)
	m.rkEnd += uint64(len(p))
	if !m.rk.on {
		// over rrMemMax (add released it): restart empty at rkEnd, so a
		// later rescue covers what comes next (older bytes: NAK)
		rrStat.gap.Add(1)
		m.rk.on = true
	}
}

// rrStopLocked (under rkMu): this connection's ring stops for good.
func (m *mConn) rrStopLocked() {
	if m.rk.on {
		rrStat.gap.Add(1)
	}
	m.rrReleaseLocked()
	m.rk.on = false
}

func (m *mConn) rrReleaseLocked() { m.rk.release() }

// rrRangeLocked (under rkMu): bytes [from, to) from the ring.
func (m *mConn) rrRangeLocked(from, to uint64) ([]byte, bool) {
	if from >= to {
		return nil, true
	}
	if !m.rk.on || to > m.rkEnd {
		return nil, false
	}
	all, ok := m.rk.get(from, m.rkEnd)
	if !ok {
		return nil, false
	}
	return all[:to-from], true
}

// rrEndLocked (under mu, end): free the ring, unregister, close old subs.
func (m *mConn) rrEndLocked() {
	if m.iran && m.rrPhase == 1 {
		rrStat.endWait.Add(1)
		rtrace("%x iran rrec ended waiting after %v", m.id, time.Since(m.rrT0).Round(time.Millisecond))
	}
	m.rrWait.Store(false)
	m.rrSnap, m.rrSnapP = nil, nil
	m.b375ReleaseLocked()
	m.rkMu.Lock()
	m.rrReleaseLocked()
	m.rk.on = false
	m.rkMu.Unlock()
	if _, ok := rrConns.LoadAndDelete(m); ok {
		rrStat.conns.Add(-1)
	}
	for _, s := range m.rrOld {
		go s.close()
	}
	m.rrOld = nil
}

// rrSweep (sweeper): free idle rings (kept while a rescue waits).
func rrSweep(now time.Time) {
	rrConns.Range(func(k, _ any) bool {
		m := k.(*mConn)
		if !m.rkMu.TryLock() {
			return true
		}
		ended := false
		select {
		case <-m.die:
			ended = true
		default:
		}
		switch {
		case ended:
			m.rrReleaseLocked()
			m.rk.on = false
			if _, ok := rrConns.LoadAndDelete(m); ok {
				rrStat.conns.Add(-1)
			}
		case m.rrWait.Load():
		case m.rk.buf != nil && monoNano(now)-m.rk.last.Load() > int64(rescueKeep):
			// the ring restarts empty at rkEnd (older bytes are not covered)
			m.rrReleaseLocked()
		default:
			m.rk.shrinkIdle(monoNano(now)) // mb174: big capacity back after a low rate
		}
		m.rkMu.Unlock()
		return true
	})
}

// rrLiveSubsLocked: the subs that carry the connection now.
func (m *mConn) rrLiveSubsLocked() []*msub {
	var out []*msub
	for _, s := range m.subs {
		if !s.dead && !s.retired && !s.bulk.Load() && !s.closeHS && !s.closeAfter {
			out = append(out, s)
		}
	}
	return out
}

// rrEligibleLocked (Iran, under mu): a records light conn with recent bytes
// in its ring whose every live sub is on a silent link.
func (m *mConn) rrEligibleLocked(now time.Time, set map[any]bool) bool {
	if !rescueOn.Load() || !rescueRecOn.Load() || !m.iran || !m.rscCap || !m.rrPeer || m.rrPhase != 0 || m.rscPhase == 1 ||
		m.rawW.Load() || m.ended || m.closing || m.handshaking || m.lightOpening || m.stOpening || m.mode != planeLight || m.stN > 0 {
		return false
	}
	last := m.rk.last.Load()
	if last == 0 || monoNano(now)-last > int64(rescueKeep) {
		return false
	}
	live := m.rrLiveSubsLocked()
	if len(live) == 0 {
		return false
	}
	for _, s := range live {
		if !set[s.key] {
			return false
		}
	}
	return true
}

// rrStart (Iran): open the join, retire the old subs, send RREC.
// lightOpening was set by the caller.
func (m *mConn) rrStart() {
	m.mu.Lock()
	avoid := append([]any(nil), m.deadAvoidLocked()...)
	var left []any
	if m.rrDead != nil { // rs145 retry: off the lost join's link and path
		left = append(left, m.rrDead)
		avoid = append(avoid, avoidKey(m.rrDead))
		if p := callPathOf(m.rrDead); p != "" {
			avoid = append(avoid, callStuckMark{path: p})
		} else if l, ok := m.rrDead.(Link); ok {
			if mtr := linkMeterOf(l); mtr != nil && mtr.path != "" {
				avoid = append(avoid, callStuckMark{path: mtr.path})
			}
		}
		m.rrDead = nil
	}
	for _, s := range m.rrLiveSubsLocked() {
		left = append(left, s.key)
		avoid = append(avoid, avoidKey(s.key))
		p := callPathOf(s.key)
		if l, ok := s.key.(Link); ok && p == "" {
			if mtr := linkMeterOf(l); mtr != nil {
				p = mtr.path
			}
		}
		if p != "" {
			avoid = append(avoid, callStuckMark{path: p})
		}
	}
	var silent map[any]bool
	if sp := rescueSilentSet.Load(); sp != nil {
		silent = *sp
		for l := range *sp {
			avoid = append(avoid, avoidKey(l))
		}
	}
	avoid = rescueJoinAvoid(avoid, left, silent) // rs145
	avoid = l480dCallLive(l480dCallPend(avoid))  // l480d CALLPEND + CALLLIVE (mconn_l480d_callpend.go)
	m.mu.Unlock()
	sub, key, err := m.open(planeLight, avoid)
	m.mu.Lock()
	if err != nil || m.ended || m.closing || m.handshaking || m.rawW.Load() || m.rrPhase != 0 || m.mode != planeLight || m.stN > 0 {
		m.lightOpening = false
		if err == nil {
			m.mu.Unlock()
			sub.Close()
		} else {
			m.lightRetryAt = time.Now().Add(mReopenBackoff)
			m.mu.Unlock()
		}
		rrStat.failed.Add(1)
		rtrace("%x iran rrec start failed err=%v", m.id, err)
		m.mu.Lock()
		orphan := m.rrRetried && len(m.rrLiveSubsLocked()) == 0 && !m.ended
		m.mu.Unlock()
		if orphan { // rs145: the retry found no path and no sub is left
			rs145Stat.retryFail.Add(1)
			m.end(errMConnSubsDied, false)
		}
		return
	}
	m.lightOpening = false
	old := m.rrLiveSubsLocked()
	bi := m.sendOff
	tH := time.Now()
	m.b375HoldLocked(old, bi) // b375: references, before the kept chunks are dropped (no copy)
	dH, nH := time.Since(tH), len(m.b375Hold)
	for _, s := range old {
		s.retired = true
		s.rrAband = true
		m.removeSubLocked(s)
		m.abandonSentLocked(s) // fx212 (H2)
	}
	m.rrOld = append(m.rrOld, old...)
	m.prefPrimary = nil
	s2 := m.addSubLocked(sub, key, false)
	m.prefPrimary = s2
	m.setPrimaryLocked()
	m.r.mu.Lock()
	ri := m.r.next
	m.r.mu.Unlock()
	// the ring rolls on with new writes until the answer: keep what it
	// holds below Bi now (er97 unit test: 30 KB written meanwhile pushed
	// [Rk, Bi) out of a 32 KB ring)
	m.rkMu.Lock()
	if m.rk.on && m.rkEnd == bi && m.rk.n > 0 {
		m.rrSnap = make([]byte, m.rk.n)
		m.rk.copyOut(m.rrSnap)
		m.rrSnap0 = bi - uint64(m.rk.n)
	} else {
		m.rrSnap, m.rrSnap0 = nil, bi
	}
	if !m.rk.on || m.rkEnd != bi {
		m.rrSnap0 = ^uint64(0) // nothing covered (only an empty range)
	}
	m.rkMu.Unlock()
	m.rrMergeSnapLocked(bi) // rs169
	m.rrExt0 = ^uint64(0)
	s2.push(rescueRec(mRecRRescue, bi, ri))
	m.rrPhase, m.rrBi, m.rrSub, m.rrT0 = 1, bi, s2, time.Now()
	m.rrWait.Store(true)
	m.tr.ev("rescue-rec: RREC(%d,%d) on sub %d, %d old", bi, ri, s2.idx, len(old))
	m.sendCond.Broadcast()
	tg := m.rtTagLocked()
	m.mu.Unlock()
	rrStat.started.Add(1)
	rtrace("%x iran RREC(%d,%d) old %d join %v held %d in %v%s", m.id, bi, ri, len(old), linkName(key), nH, dH, tg)
	m.startSub(s2)
	m.kickMgr()
}

// onRRescue (Kharej): RREC(bi, ri) on s.
func (m *mConn) onRRescue(s *msub, bi, ri uint64) bool {
	m.mu.Lock()
	if m.iran || !rescueRecOn.Load() || !rescueOn.Load() || m.mode != planeLight || m.stN > 0 || m.rawW.Load() {
		m.mu.Unlock()
		rrStat.refused.Add(1)
		rtrace("%x exit RREC(%d,%d) refused", m.id, bi, ri)
		return false
	}
	if m.ended {
		m.mu.Unlock()
		return true
	}
	bk := m.sendOff
	m.r.mu.Lock()
	rk := m.r.next
	m.r.mu.Unlock()
	m.rkMu.Lock()
	data, ok := m.rrRangeLocked(ri, bk)
	m.rkMu.Unlock()
	if ri > bk {
		ok = false
	}
	if !ok {
		// NAK: nothing abandoned here; the join leads from now on
		s.push(rescueRec(mRecRRescueNak, bk, rk))
		m.prefPrimary = s
		m.setPrimaryLocked()
		m.rrPhase = 2
		m.tr.ev("rescue-rec: NAK(%d,%d) for RREC(%d,%d)", bk, rk, bi, ri)
		m.sendCond.Broadcast()
		m.mu.Unlock()
		rrStat.nak.Add(1)
		rtrace("%x exit RREC(%d,%d) NAK(%d,%d)", m.id, bi, ri, bk, rk)
		return true
	}
	var old []*msub
	for _, x := range append([]*msub(nil), m.subs...) {
		if x != s && !x.bulk.Load() {
			x.retired = true
			x.rrAband = true
			m.removeSubLocked(x)
			m.abandonSentLocked(x) // fx212 (H2)
			old = append(old, x)
		}
	}
	m.prefPrimary = s
	m.setPrimaryLocked()
	s.push(rescueRec(mRecRRescueAck, bk, rk))
	pushResendLocked(s, ri, data)
	rrStat.resent.Add(uint64(len(data)))
	if m.wclosed {
		c := getChunk(mRecFin)
		c.setOff(m.sendOff)
		s.push(c)
		rrStat.fin.Add(1)
	}
	m.rrPhase = 2
	m.tr.ev("rescue-rec: served RREC(%d,%d), ACK(%d,%d), resent %d, %d old", bi, ri, bk, rk, len(data), len(old))
	m.sendCond.Broadcast()
	m.mu.Unlock()
	rrStat.served.Add(1)
	rtrace("%x exit RREC(%d,%d) ACK(%d,%d) resent %d old %d", m.id, bi, ri, bk, rk, len(data), len(old))
	for _, x := range old {
		go x.close()
	}
	return true
}

// onRRescueAns (Iran): ACK or NAK(bk, rk): resend [rk, Bi).
func (m *mConn) onRRescueAns(s *msub, nak bool, bk, rk uint64) bool {
	m.mu.Lock()
	if m.iran && s != m.rrSub && rs169Fix.Load() && (m.rrRetried || slices.Contains(m.rrOld, s)) {
		return m.rrLateAnsLocked(s, nak, bk, rk) // rs169 (H1)
	}
	if m.iran && m.rrLate && m.rrPhase == 2 && s == m.rrSub && rs169Fix.Load() {
		// rs169: the retry's own answer after a late one already resent
		var old []*msub
		if !nak {
			old, m.rrOld = m.rrOld, nil
		}
		m.mu.Unlock()
		rtrace("%x iran RREC answer nak=%v (%d,%d) after a late one: nothing to do", m.id, nak, bk, rk)
		for _, x := range old {
			go x.close()
		}
		return true
	}
	if !m.iran || m.rrPhase != 1 || s != m.rrSub {
		m.mu.Unlock()
		return false
	}
	m.rrPhase = 2
	m.rrWait.Store(false)
	m.b375CoverLocked(rk) // b375: reads the held chunks only if the ring falls short
	ok := rk >= m.rrBi
	if !ok && rk >= m.rrSnap0 {
		data := m.rrSnap[rk-m.rrSnap0:]
		pushResendLocked(s, rk, data)
		rrStat.resent.Add(uint64(len(data)))
		ok = true
	}
	if ok && m.wclosed {
		c := getChunk(mRecFin)
		c.setOff(m.sendOff)
		s.push(c)
		rrStat.fin.Add(1)
	}
	if ok && rk < m.rrBi && m.rrExt0 != ^uint64(0) && rk < m.rrExt0 {
		b375Stat.full.Add(1) // covered only by the b375 extension
	}
	if !ok && b375RRCover.Load() {
		b375Stat.short.Add(1)
	}
	m.rrSnap = nil
	var old []*msub
	if !nak {
		old, m.rrOld = m.rrOld, nil
	}
	m.tr.ev("rescue-rec: answer nak=%v (%d,%d) covered=%v", nak, bk, rk, ok)
	tg := m.rtTagLocked()
	m.mu.Unlock()
	if nak {
		rrStat.nakd.Add(1)
	} else {
		rrStat.acked.Add(1)
	}
	rtrace("%x iran RREC answer nak=%v (%d,%d) after %v covered=%v%s", m.id, nak, bk, rk, time.Since(m.rrT0).Round(time.Millisecond), ok, tg)
	if !ok {
		rrStat.noCover.Add(1)
		m.end(errMConnLost, true)
		return true
	}
	for _, x := range old {
		go x.close()
	}
	return true
}

// rrSubDiedLocked (under mu, subDied): the join died before the answer.
func (m *mConn) rrSubDiedLocked(s *msub) {
	if m.iran && m.rrPhase == 1 && s == m.rrSub {
		rrStat.lostJoin.Add(1)
		m.rrRetryLocked(s) // rs145: HS2_MCONN_RESCUE_RETRY
	}
}

// rrReadRec (readLoop): one of 0x10-0x12.
func (m *mConn) rrReadRec(s *msub, kind byte, b []byte) bool {
	a, z := binary.BigEndian.Uint64(b[0:8]), binary.BigEndian.Uint64(b[8:16])
	switch kind {
	case mRecRRescue:
		return m.onRRescue(s, a, z)
	case mRecRRescueAck:
		return m.onRRescueAns(s, false, a, z)
	}
	return m.onRRescueAns(s, true, a, z)
}

// rescueRecPeerOK: the exit behind link l announced capExtMConnRescueRec.
func rescueRecPeerOK(l Link) bool {
	mtr := linkMeterOf(l)
	if mtr == nil {
		return false
	}
	pi := mtr.peerInfo.Load()
	return pi.Ext&capExtMConnRescueRec != 0
}

type mconnRescueRecSub interface{ mconnRescueRec() bool }
