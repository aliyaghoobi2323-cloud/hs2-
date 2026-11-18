package engine

import (
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// er91: RESCUE of a raw first sub whose path went silent (HS2_MCONN_RESCUE=1,
// default off, both sides).
//
// A light connection whose only sub is raw (mconn_raw.go) loses what is in
// flight when its path blackholes: the upgrade handshake must cross the dead
// sub, and a copy opened later carries only new bytes. With the key on:
//
//   - each side keeps a ring of the last raw bytes it wrote on the first sub
//     (rescueRingCap per connection, rescueMemMax for the process; a ring idle
//     for rescueKeep is freed, so is every ring of a connection that is no
//     longer raw); only connections opened with role 3 (mconnRoleFirstRawRescue,
//     sent only to an exit announcing capExtMConnRescue) keep one;
//   - when the first sub's link sits on a path the silence test marks
//     (pathSilentMark: >= 80 % of >= 3 links heard nothing for rescueSilence
//     while another path hears), Iran opens a join on another link and sends
//     RESCUE(Bi, Ri) as its first record: Bi = raw bytes it wrote, Ri = raw
//     bytes it read. Iran stops using the first sub at once (no boundary wait,
//     nothing has to cross the dead sub) and writes records from Bi;
//   - Kharej stops its raw reader and writer (a write stuck on the dead sub is
//     cut by a write deadline), answers RESCUE-ACK(Bk, Rk) and resends [Ri, Bk)
//     from its ring as DATA records; Iran then resends [Rk, Bi). Reassembly
//     drops what was already delivered (by offset), so nothing is delivered
//     twice and a false trigger on a live path costs only the resend;
//   - the first sub is abandoned (closed with FIN, never RST).
//
// A peer without the bit (rel58/rel59) never sees role 3 nor RESCUE: no
// rescue, nothing else changes. A gap the ring no longer holds ends the
// connection (counted: rescue nocover) — the old outcome, only earlier.

const (
	mRecRescue    = 0x0d // Iran → Kharej: kind + u64 Bi + u64 Ri
	mRecRescueAck = 0x0e // Kharej → Iran: kind + u64 Bk + u64 Rk
	// mRecRescueNak (er94): Kharej → Iran: kind + u64 Bk + u64 Rk: the ring no
	// longer holds [Ri, Bk); Iran reads them on the first sub itself (its raw
	// reader back on, up to Bk) and resends [Rk, Bi) as for an ACK.
	mRecRescueNak = 0x0f
	mRescueLen    = 1 + 8 + 8

	// mconnRoleFirstRawRescue: a raw first sub whose connection keeps rings.
	mconnRoleFirstRawRescue byte = 3

	// mconnRoleRescueJoin (er94): the rescue join of a connection that has
	// read nothing yet (Ri 0) and whose ring still holds every byte it wrote.
	// The exit attaches it like a join when it knows the id; when it does not
	// (the role-3 first sub was lost in the blackhole: er94 lab B94_1, 126 of
	// 125 rescues ended "join missed"), it opens the connection on this sub
	// in records mode, answers RESCUE with ACK(0,0) and Iran resends [0, Bi).
	mconnRoleRescueJoin byte = 4

	// capExtMConnRescue: ext bit8 of kindInfo: this server takes role 3 and
	// RESCUE records.
	capExtMConnRescue = 1 << 8
)

var rescueOn atomic.Bool // loadEnvKeys: HS2_MCONN_RESCUE

// rescueRingCapV: HS2_MCONN_RESCUE_KB in bytes, the per-connection ring
// cap. Atomic since b279: tests change it while sweepers and conn loops of
// earlier tests may still read it.
var rescueRingCapV atomic.Int64

func init() { rescueRingCapV.Store(int64(envIntDef("HS2_MCONN_RESCUE_KB", 128) << 10)) }

// rescueRingCap returns the per-connection ring cap in bytes.
func rescueRingCap() int { return int(rescueRingCapV.Load()) }

var (
	rescueMemMax  int64 = int64(envIntDef("HS2_MCONN_RESCUE_MB", 64)) << 20
	rescueKeep          = 10 * time.Second
	rescueSilence       = time.Duration(envIntDef("HS2_MCONN_RESCUE_MS", 1500)) * time.Millisecond
	rescMaxPerSec       = int64(envIntDef("HS2_MCONN_RESCUE_PERSEC", 300))
)

func envIntDef(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil && v > 0 {
		return v
	}
	return def
}

// rescueStat: process counters (hs2 status / logs / tests).
var rescueStat struct {
	mem     atomic.Int64 // bytes held by rings
	rings   atomic.Int64 // rings registered
	overCap atomic.Uint64
	started atomic.Uint64 // Iran: RESCUE sent
	served  atomic.Uint64 // Kharej: RESCUE answered
	acked   atomic.Uint64 // Iran: ACK read, resend queued
	noCover atomic.Uint64 // a gap the ring no longer held (connection ended)
	failed  atomic.Uint64 // refused (state) or the open failed
	resent  atomic.Uint64 // bytes resent from rings (both sides)
	limited atomic.Uint64 // refused by rescMaxPerSec
	trigger atomic.Uint64 // silent-path events seen with the key on
	joins   atomic.Uint64 // er92 Kharej: joins attached to a ring (role 3) connection
	sameP   atomic.Uint64 // er92 Iran: rescue join opened on the silent path anyway
	silentJ atomic.Uint64 // er92 Iran: rescue join opened on a link of the silent set
	// er94: every way a RESCUE can go unanswered is counted (never silent)
	joinEnded atomic.Uint64 // Kharej: a join came for a ring conn already ended or closing
	lostJoin  atomic.Uint64 // Iran: the rescue join died before the ACK
	refused   atomic.Uint64 // Kharej: RESCUE refused by state (also in failed)
	cutSlow   atomic.Uint64 // either side: taking the raw reader/writer off took > 1 s
	endWait   atomic.Uint64 // Iran: the conn ended while waiting for the ACK
	fresh     atomic.Uint64 // Iran: rescue joins sent as role 4 (Ri 0, ring from 0)
	freshNew  atomic.Uint64 // Kharej: role 4 opened the connection (its first sub never came)
	nak       atomic.Uint64 // Kharej: NAK sent (ring short, conn kept)
	nakd      atomic.Uint64 // Iran: NAK read, raw reader back on to Bk
}

// rescueFreshMark in an avoid list asks mconnEdge.open for role 4.
type rescueFreshMark struct{}

func takeRescueFreshMark(avoid []any) ([]any, bool) {
	for i, a := range avoid {
		if _, ok := a.(rescueFreshMark); ok {
			return append(avoid[:i:i], avoid[i+1:]...), true
		}
	}
	return avoid, false
}

// er94: HS2_MCONN_RESCUE_TRACE=1 logs one line per rescue step on both sides
// (conn id, link, offsets, ms), at most rescueTraceMax lines per process.
var (
	rescueTrace    = os.Getenv("HS2_MCONN_RESCUE_TRACE") == "1"
	rescueTraceN   atomic.Int64
	rescueTraceMax int64 = 4000
)

func rtrace(format string, a ...any) {
	if !rescueTrace || rescueTraceN.Add(1) > rescueTraceMax {
		return
	}
	log.Printf("rescue-trace "+format, a...)
}

// rescueRing: the last raw bytes this side wrote, ending at rawSent. Under
// m.wmu.
type rescueRing struct {
	on   bool
	rec  bool // er97: a records ring (rrRingCap, its own budget rrMemMax)
	buf  []byte
	head int
	n    int
	last atomic.Int64 // monoNano of the last append
	// cd152: a records ring's write rate (bytes in the last full second),
	// for its rate-scaled cap (rrCapFor)
	winT0 int64 // monoNano: the current window's start
	winN  int   // bytes added since winT0
	rate  int   // bytes/s of the last window
	big   int   // bytes of buf above rrRingCap, counted in rrStat.big
	// mb174: the big budget's tiers and the shrink (mconn_rescue_bigring.go)
	susN  int   // full windows in a row whose rate needs more than rrRingCap
	call  bool  // the conn is copied as a call (callOn), set by rrKeepLocked
	lowT0 int64 // monoNano: since when the rate wants a ring at most half this size
	bigC  int   // of big, the bytes counted in rrStat.bigCall
	// ul268 (mconn_ul268.go): an Iran raw ring sized to a fast writer
	u268  bool         // this ring may grow past rescueRingCap (Iran, key on)
	uT0   int64        // monoNano: the rate window's start
	uN    int          // bytes in the window
	uRate int          // bytes/s
	ulBig int          // bytes of buf above rescueRingCap, counted in ul268Stat.mem
	uTot  int64        // bytes ever added
	uOver bool         // the budget refused a growth
	uCov  atomic.Bool  // ul268Covered: a data-dark rescue may start (read under mu)
	uHeld atomic.Int64 // n, readable under mu (ul268CoverNowLocked)
	uTotA atomic.Int64 // uTot, readable under mu
}

func rescueMemTake(n int) bool {
	if rescueStat.mem.Add(int64(n)) > rescueMemCap() { // rs145: HS2_MCONN_RESCUE_SCALE
		rescueStat.mem.Add(-int64(n))
		return false
	}
	return true
}

func (r *rescueRing) capB() int {
	if r.rec {
		return rrCapFor(r.rate)
	}
	if r.u268 {
		return ul268RawCap(r.uRate)
	}
	return rescueRingCap()
}

// noteRate (records rings): the bytes of the last full second.
func (r *rescueRing) noteRate(now int64, n int) {
	switch d := now - r.winT0; {
	case r.winT0 == 0:
		r.winT0 = now
	case d >= int64(time.Second):
		if d >= 2*int64(time.Second) {
			r.rate = 0 // a pause: the window says nothing about now
		} else {
			r.rate = int(int64(r.winN) * int64(time.Second) / d)
		}
		r.winT0, r.winN = now, 0
		r.noteSustained() // mb174
	}
	r.winN += n
}

func (r *rescueRing) memTake(n int) bool {
	if !r.rec {
		return rescueMemTake(n)
	}
	if rrStat.mem.Add(int64(n)) > rrMemCap() { // rs145: HS2_MCONN_RESCUE_SCALE
		rrStat.mem.Add(-int64(n))
		return false
	}
	return true
}

// add appends p (the ring then ends at the new rawSent).
func (r *rescueRing) add(p []byte) {
	if !r.on || len(p) == 0 {
		return
	}
	now := monoNano(time.Now())
	r.last.Store(now)
	if r.rec {
		r.noteRate(now, len(p))
	} else if r.u268 {
		r.ul268Note(now, len(p))
		r.uTot += int64(len(p))
		r.uTotA.Store(r.uTot)
	}
	if need := r.n + len(p); need > len(r.buf) && len(r.buf) < r.capB() {
		size := 4 << 10
		for size < need && size < r.capB() {
			size <<= 1
		}
		size = min(size, r.capB())
		if r.rec && len(r.buf) < rrRingCap {
			size = min(size, rrRingCap) // the first rrRingCap from rrMemCap
		}
		if !r.rec && r.u268 && len(r.buf) < rescueRingCap() {
			size = min(size, rescueRingCap()) // the first rescueRingCap from rescueMemCap
		}
		if !r.rec && r.u268 && len(r.buf) >= rescueRingCap() {
			// ul268: growth past rescueRingCap from its own budget
			if ul268Take(size - len(r.buf)) {
				if r.ulBig == 0 {
					ul268Stat.grown.Add(1)
				}
				r.ulBig += size - len(r.buf)
				r.uOver = false // review-rel69k #2: a later growth clears a refusal
			} else {
				size = len(r.buf)
				r.uOver = true
			}
		} else if r.rec && len(r.buf) >= rrRingCap {
			// cd152: growth past the old fixed cap comes from its own
			// budget (rrBigMax): the crowd's rings fill rrMemCap. Not
			// there: the ring stays as it is.
			if r.bigTake(size - len(r.buf)) { // mb174: tiers
				r.big += size - len(r.buf)
			} else {
				size = len(r.buf)
			}
		} else if !r.memTake(size - len(r.buf)) {
			rescueStat.overCap.Add(1)
			r.release()
			r.on = false // this connection cannot be rescued now
			return
		}
		if size > len(r.buf) {
			nb := rpGet(size) // gc296: pooled size classes
			k := r.copyOut(nb)
			rpPut(r.buf)
			r.buf, r.head, r.n = nb, 0, k
		}
	}
	size := len(r.buf)
	if len(p) >= size {
		p = p[len(p)-size:]
		r.head, r.n = 0, 0
	}
	if over := r.n + len(p) - size; over > 0 {
		r.head = (r.head + over) % size
		r.n -= over
	}
	tail := (r.head + r.n) % size
	k := copy(r.buf[tail:], p)
	copy(r.buf, p[k:])
	r.n += len(p)
	if r.u268 {
		if len(r.buf) >= r.capB() {
			r.uOver = false // at its cap: nothing was cut short
		}
		r.uHeld.Store(int64(r.n))
		r.ul268Cover()
	}
}

// copyOut copies the held bytes, oldest first, into dst.
func (r *rescueRing) copyOut(dst []byte) int {
	if r.n == 0 {
		return 0
	}
	k := copy(dst, r.buf[r.head:min(len(r.buf), r.head+r.n)])
	if k < r.n {
		k += copy(dst[k:], r.buf[:r.n-k])
	}
	return k
}

// get returns the bytes [from, end) where end is the ring's end offset; false
// when the ring no longer holds from.
func (r *rescueRing) get(from, end uint64) ([]byte, bool) {
	if from > end {
		return nil, false
	}
	if from == end {
		return nil, true
	}
	if end-from > uint64(r.n) {
		return nil, false
	}
	all := make([]byte, r.n)
	r.copyOut(all)
	return all[uint64(r.n)-(end-from):], true
}

func (r *rescueRing) release() {
	if r.buf != nil {
		if r.rec {
			rrStat.mem.Add(-int64(len(r.buf) - r.big))
			rrStat.big.Add(-int64(r.big))
			rrStat.bigCall.Add(-int64(r.bigC))
		} else {
			rescueStat.mem.Add(-int64(len(r.buf) - r.ulBig))
			ul268Stat.mem.Add(-int64(r.ulBig))
		}
		rpPut(r.buf) // gc296
	}
	r.buf, r.head, r.n, r.big, r.bigC, r.lowT0, r.ulBig = nil, 0, 0, 0, 0, 0, 0
	r.uCov.Store(false)
	// review-rel69k #2: the rate window and a budget refusal start over;
	// uTot keeps counting (a fresh ring never holds the bytes written before
	// the release, so "holds every byte" must stay false)
	r.uOver, r.uRate, r.uT0, r.uN = false, 0, 0, 0
	r.uHeld.Store(0)
}

// rescueState: per connection.
type rescueState struct {
	ring       rescueRing
	rscCap     bool          // role 3 connection (under mu after enable)
	rscCut     atomic.Bool   // a rescue took the raw writer off (writeRaw)
	rscDone    chan struct{} // closed once the switch to records is done
	rscPhase   int           // under mu: 0 none, 1 Iran waits for ACK, 2 done
	rscBi      uint64        // Iran: Bi sent in RESCUE
	rscSub     *msub         // the rescue join
	rscOld     *msub         // the abandoned raw first sub (closed on ACK or end)
	rscT0      time.Time     // Iran: RESCUE queued (trace)
	rscFresh   bool          // Kharej: opened by a role-4 rescue join (under mu)
	rscRi      uint64        // Iran: Ri sent in RESCUE (rs145 retry)
	rscRetried bool          // Iran: the lost join was replaced once (rs145)
	rscAband   []*msub       // Iran: rescue joins a retry replaced (rs169: their answer still counts; closed at the end)
	rscSrv     rescueServed  // Kharej: the answer given (rs169: a repeated RESCUE gets it again)
	rscFreshI  bool          // Iran: the RESCUE went on a role-4 join (rs169: so does its retry)
	rrState                  // er97: records-mode rescue (mconn_rescue_rec.go)
	rstState                 // rr155: unclean sub death (mconn_rescue_rst.go)
}

// linkName: a link key for the trace.
func linkName(k any) string {
	if l, ok := k.(Link); ok {
		if mtr := linkMeterOf(l); mtr != nil {
			return fmt.Sprintf("%p/%s", l, mtr.path)
		}
		return fmt.Sprintf("%p", l)
	}
	return fmt.Sprintf("%v", k)
}

// rescueSubDiedLocked (under mu, subDied): the rescue join died before the ACK.
func (m *mConn) rescueSubDiedLocked(s *msub, cause error) {
	if m.iran && m.rscPhase == 1 && s == m.rscSub {
		rescueStat.lostJoin.Add(1)
		rtrace("%x iran join died before ACK after %v: %v", m.id, time.Since(m.rscT0).Round(time.Millisecond), cause)
		m.rescueRetryLocked(s) // rs145: HS2_MCONN_RESCUE_RETRY
	}
}

// rescue registry: connections holding a ring (sweeper, trigger).
var (
	rescueConns     sync.Map // *mConn → struct{}
	rescueSweepOnce sync.Once
	rescueSilentSet atomic.Pointer[map[any]bool]
	rescSec         atomic.Int64
	rescSecN        atomic.Int64
)

// rescueEnable: this raw connection keeps a ring (role 3, key on).
func (m *mConn) rescueEnable() {
	m.wmu.Lock()
	m.ring.on = true
	m.ring.u268 = m.iran && ul268On.Load() // ul268 (mconn_ul268.go)
	m.wmu.Unlock()
	m.mu.Lock()
	m.rscCap = true
	m.mu.Unlock()
	rescueConns.Store(m, struct{}{})
	rescueStat.rings.Add(1)
	rescueSweepOnce.Do(func() { go rescueSweeper() })
}

// rescueForget frees m's ring (under wmu) and unregisters it.
func (m *mConn) rescueForgetLocked() {
	m.ring.release()
	m.ring.on = false
	if _, ok := rescueConns.LoadAndDelete(m); ok {
		rescueStat.rings.Add(-1)
	}
}

func (m *mConn) rescueEndedHook() {
	m.mu.Lock()
	old := m.rscOld
	aband := m.rscAband
	m.rscAband = nil
	m.rscSrv.data = nil
	m.mu.Unlock()
	if old != nil {
		old.close()
	}
	for _, s := range aband { // rs169
		go s.close()
	}
	if m.wmu.TryLock() {
		m.rescueForgetLocked()
		m.wmu.Unlock()
	} // else the sweeper frees it
}

func rescueSweeper() {
	var last string
	for i := 1; ; i++ {
		time.Sleep(time.Second)
		rescueSweep(time.Now())
		rrSweep(time.Now()) // er97
		if i%10 == 0 {
			if l := rescueLogLine() + " | " + rescueRecLogLine() + " | " + rescue145LogLine() + " | " + rescue169LogLine() + " | " + rescueRstLogLine() + " | " + darkcallLogLine() + " | " + ul268LogLine() + " | " + ul268ScaleLogLine() + " | " + rpLogLine(); l != last {
				last = l
				log.Print(l)
			}
		}
	}
}

// rescueLogLine: the counters as one log line (printed every 10 s when they
// changed).
func rescueLogLine() string {
	s := &rescueStat
	return fmt.Sprintf("mconn rescue: rings %d (%d KB) · triggers %d · started %d acked %d served %d · nocover %d failed %d limited %d overcap %d · resent %d KB · joins %d samepath %d silentjoin %d · joinended %d joinmissed %d lostjoin %d refused %d cutslow %d endwait %d · fresh %d freshnew %d · nak %d nakd %d · jm271 rst %d limited %d",
		s.rings.Load(), s.mem.Load()>>10, s.trigger.Load(), s.started.Load(), s.acked.Load(), s.served.Load(),
		s.noCover.Load(), s.failed.Load(), s.limited.Load(), s.overCap.Load(), s.resent.Load()>>10, s.joins.Load(), s.sameP.Load(), s.silentJ.Load(),
		s.joinEnded.Load(), mconnStat.joinMissed.Load(), s.lostJoin.Load(), s.refused.Load(), s.cutSlow.Load(), s.endWait.Load(), s.fresh.Load(), s.freshNew.Load(), s.nak.Load(), s.nakd.Load(),
		jm271Stat.sent.Load(), jm271Stat.limited.Load())
}

// rescueSweep frees rings idle for rescueKeep and those of connections that
// ended or are no longer raw.
func rescueSweep(now time.Time) {
	rescueConns.Range(func(k, _ any) bool {
		m := k.(*mConn)
		if !m.wmu.TryLock() {
			return true // a raw write in flight: next time
		}
		ended := false
		select {
		case <-m.die:
			ended = true
		default:
		}
		// er94: a rescue is under way (Iran resends from the ring on the ACK;
		// the ACK/NAK handlers and the end hook free it). Atomic: the sweeper
		// holds wmu here and must not wait for mu.
		waiting := m.rscCut.Load()
		switch {
		case waiting && !ended:
			// keep it (er94 lab B95: 24-29 per run "nocover" on the ACK, the
			// sweeper had freed the ring because rawW is off after the switch)
		case ended || !m.rawW.Load():
			m.rescueForgetLocked()
		case m.ring.buf != nil && monoNano(now)-m.ring.last.Load() > int64(rescueKeep):
			m.ring.release()
		}
		m.wmu.Unlock()
		return true
	})
}

func rescueAllowNow(now time.Time) bool {
	sec := now.Unix()
	if old := rescSec.Load(); old != sec && rescSec.CompareAndSwap(old, sec) {
		rescSecN.Store(0)
	}
	if rescSecN.Add(1) > rescMaxPerSec {
		rescueStat.limited.Add(1)
		return false
	}
	return true
}

// rescueSilentLinks (Iran, LinkManager health tick): links on a path whose
// links went silent together. Each conn whose raw first sub sits on one of
// them (and wrote raw bytes within rescueKeep) is rescued.
func rescueSilentLinks(set map[any]bool) {
	if len(set) == 0 {
		rescueSilentSet.Store(nil)
		rescueEr92On.Store(false)
		return
	}
	rescueSilentSet.Store(&set)
	var only map[any]bool
	if p := ul268DarkOnly.Load(); p != nil {
		only = *p
	}
	now := time.Now()
	callSet := ul268CallSet(set) // ul268: what a call sees (no data-dark-only links)
	if len(set) <= len(only) {
		// ul268 (mconn_ul268_scale.go): data-dark-only links: visit only
		// the conns with a sub on them, not every Iran conn
		rescueEr92On.Store(false)
		ul268VisitDark(set, callSet, only, now)
		return
	}
	if !rescueEr92On.Swap(true) {
		rescueStat.trigger.Add(1) // a new er92 silent episode (not every watch tick)
	}
	ul268Scale.full.Add(1)
	rescueConns.Range(func(k, _ any) bool {
		m := k.(*mConn)
		if !m.iran {
			return true
		}
		_, stop := rescueVisitRaw(m, now, set, callSet)
		return !stop
	})
	// er97: records-mode connections (mconn_rescue_rec.go)
	rrConns.Range(func(k, _ any) bool {
		m := k.(*mConn)
		if !m.iran {
			return true
		}
		_, stop := rescueVisitRec(m, now, set, callSet)
		return !stop
	})
}

// rescueEr92On: an er92 silent episode is running (trigger counts its start).
var rescueEr92On atomic.Bool

// rescueVisitRaw (Iran, watch tick): rescue m's raw first sub when it sits
// on a link of set. stop: the per-second rescue limit is reached.
func rescueVisitRaw(m *mConn, now time.Time, set, callSet map[any]bool) (started, stop bool) {
	m.mu.Lock()
	set = m.ul268SetForLocked(now, set, callSet)
	m.rescueRetrySilentLocked(set) // rs146: HS2_MCONN_RESCUE_RETRY, join on a silent link
	ok := m.rescueEligibleLocked(now) && set[m.raw1.key]
	dark := ok && ul268DarkOnlyHas(m.raw1.key)
	if dark { // ul268: data-dark, not silent (never a call: ul268SetForLocked)
		switch {
		case m.ul268DcRecentLocked(now):
			ok = false
			ul268Scale.dcSkip.Add(1)
		case !m.ul268CoverNowLocked():
			ok = false
			ul268Stat.noCov.Add(1)
		}
	}
	if ok {
		m.lightOpening = true
		if dark {
			m.u268At = now
		}
	}
	m.mu.Unlock()
	if !ok {
		return false, false
	}
	if !rescueAllowNow(now) {
		m.mu.Lock()
		m.lightOpening = false
		m.u268At = time.Time{}
		m.mu.Unlock()
		return false, true
	}
	if dark {
		ul268Stat.rescued.Add(1)
	}
	go m.rescueStart()
	return true, false
}

// rescueVisitRec (Iran, watch tick): the records-mode rescue (er97) of m when
// every live sub of it is on a link of set.
func rescueVisitRec(m *mConn, now time.Time, set, callSet map[any]bool) (started, stop bool) {
	m.mu.Lock()
	set = m.ul268SetForLocked(now, set, callSet) // ul268: a call on a data-dark link is darkcall's (dc254)
	m.rescueRetrySilentLocked(set)               // rs146
	ok := m.rrEligibleLocked(now, set)
	dark := false
	if ok {
		for _, s := range m.rrLiveSubsLocked() {
			if ul268DarkOnlyHas(s.key) {
				dark = true
				break
			}
		}
		if dark && m.ul268DcRecentLocked(now) { // ul268 vs darkcall: one resend per episode
			ok = false
			ul268Scale.dcSkip.Add(1)
		}
	}
	if ok {
		m.lightOpening = true
		if dark {
			m.u268At = now
		}
	}
	m.mu.Unlock()
	if !ok {
		return false, false
	}
	if !rescueAllowNow(now) {
		m.mu.Lock()
		m.lightOpening = false
		m.u268At = time.Time{}
		m.mu.Unlock()
		return false, true
	}
	go m.rrStart()
	return true, false
}

// rescueEligibleLocked (Iran, under mu): still pure raw, no other open or
// handshake running, a ring with recent bytes.
func (m *mConn) rescueEligibleLocked(now time.Time) bool {
	if !rescueOn.Load() || !m.iran || !m.rscCap || m.rscPhase != 0 || m.raw1 == nil || m.ended || m.closing ||
		!m.rawW.Load() || !m.rawR.Load() || m.handshaking || m.lightOpening || m.stOpening || m.wclosed || m.peerFin { // hc183
		return false
	}
	last := m.ring.last.Load()
	return last != 0 && monoNano(now)-last <= int64(rescueKeep)
}

// cutRaw takes this side's raw reader and writer off the first sub: a
// blocked read or write is woken by a deadline. Caller then holds rmu and
// wmu (in that order) and must call rescueSwitched once the switch is done.
func (m *mConn) cutRaw() {
	m.rscDone = make(chan struct{})
	m.rscCut.Store(true)
	m.mu.Lock()
	m.rawR.Store(false)
	m.interruptRead()
	if d, ok := m.raw1.rw.(interface{ SetWriteDeadline(time.Time) error }); ok {
		d.SetWriteDeadline(time.Unix(1, 0))
	}
	m.mu.Unlock()
	t0 := time.Now()
	m.rmu.Lock()
	m.wmu.Lock()
	if d := time.Since(t0); d > time.Second {
		rescueStat.cutSlow.Add(1)
		rtrace("%x cut slow %v iran=%v", m.id, d.Round(time.Millisecond), m.iran)
	}
}

func (m *mConn) rescueSwitched(next uint64) {
	r := &m.r
	r.mu.Lock()
	if r.next < next {
		r.next = next
	}
	r.pullLocked()
	r.lastProgress = time.Now()
	r.cond.Broadcast()
	r.mu.Unlock()
	m.wmu.Unlock()
	m.rmu.Unlock()
	close(m.rscDone)
}

// abandonRawLocked (under mu): the first sub carries nothing more.
func (m *mConn) abandonRawLocked() {
	s := m.raw1
	s.gated = true
	s.retired = true
	m.removeSubLocked(s)
	m.rscOld = s
	if m.rawTimer != nil {
		m.rawTimer.Stop()
	}
}

func rescueRec(kind byte, a, b uint64) *mchunk {
	c := getChunkN(kind, mRescueLen)
	binary.BigEndian.PutUint64(c.buf[1:9], a)
	binary.BigEndian.PutUint64(c.buf[9:17], b)
	return c
}

// pushResendLocked (under mu) queues data at offset off on s as DATA records.
func pushResendLocked(s *msub, off uint64, data []byte) {
	rescueStat.resent.Add(uint64(len(data)))
	for len(data) > 0 {
		k := min(len(data), mChunkMax)
		c := getChunkN(mRecData, mDataHdr+k)
		copy(c.buf[mDataHdr:], data[:k])
		c.n = k
		binary.BigEndian.PutUint16(c.buf[9:11], uint16(k))
		c.setOff(off)
		s.push(c)
		off += uint64(k)
		data = data[k:]
	}
}

// rescueStart (Iran): open the join, send RESCUE, switch to records.
// lightOpening was set by the caller.
func (m *mConn) rescueStart() {
	m.mu.Lock()
	avoid := append([]any{avoidKey(m.raw1.key)}, m.deadAvoidLocked()...)
	if sp := rescueSilentSet.Load(); sp != nil {
		for l := range *sp { // every link the trigger saw silent
			if l != m.raw1.key {
				avoid = append(avoid, avoidKey(l))
			}
		}
	}
	p := callPathOf(m.raw1.key)
	if l, ok := m.raw1.key.(Link); ok && p == "" {
		if mtr := linkMeterOf(l); mtr != nil {
			p = mtr.path // er92: the steering tick's path map may be empty
		}
	}
	if p != "" {
		// er92: off every link of the silent path (a join on another link of
		// it never reaches the exit: er92 lab B1/B2, 0 of 289 served)
		avoid = append(avoid, callStuckMark{path: p})
	}
	if sp := rescueSilentSet.Load(); sp != nil {
		avoid = rescueJoinAvoid(avoid, []any{m.raw1.key}, *sp) // rs145
	} else {
		avoid = rescueJoinAvoid(avoid, []any{m.raw1.key}, nil)
	}
	m.mu.Unlock()
	// er94: nothing read yet and the ring holds every byte written: the exit
	// may never have seen the first sub, so the join can open the conn too
	fresh := false
	if m.rawRecvd.Load() == 0 {
		m.wmu.Lock()
		fresh = m.ring.on && int64(m.ring.n) >= m.rawSent.Load()
		m.wmu.Unlock()
	}
	if fresh {
		avoid = append(avoid, rescueFreshMark{})
	}
	sub, key, err := m.open(planeLight, avoid)
	if err == nil {
		if l, ok := key.(Link); ok {
			if sp := rescueSilentSet.Load(); sp != nil && (*sp)[l] {
				rescueStat.silentJ.Add(1)
			}
			if mtr := linkMeterOf(l); mtr != nil && mtr.path == p && p != "" {
				rescueStat.sameP.Add(1)
			}
		}
	}
	m.mu.Lock()
	// er92: lightOpening stays set until rawW is off: no upgrade (raw timer,
	// raw stripe) may start between this check and the switch (TestRescueVsUpgradeRace).
	if err != nil || m.ended || m.closing || !m.rawW.Load() || !m.rawR.Load() || m.handshaking {
		m.lightOpening = false
		if err == nil {
			m.mu.Unlock()
			sub.Close()
		} else {
			m.lightRetryAt = time.Now().Add(mReopenBackoff)
			m.mu.Unlock()
		}
		rescueStat.failed.Add(1)
		rtrace("%x iran start failed err=%v", m.id, err)
		return
	}
	m.mu.Unlock()
	m.cutRaw()
	ri := uint64(m.rawRecvd.Load())
	m.mu.Lock()
	bi := uint64(m.rawSent.Load())
	m.rawW.Store(false)
	m.lightOpening = false
	m.sendOff = bi
	m.abandonRawLocked()
	s2 := m.addSubLocked(sub, key, false)
	s2.push(rescueRec(mRecRescue, bi, ri))
	m.rscPhase, m.rscBi, m.rscSub = 1, bi, s2
	m.rscRi = ri
	m.rscT0 = time.Now()
	m.rscFreshI = fresh // rs169: a retry goes as role 4 too
	if fresh {
		rescueStat.fresh.Add(1)
	}
	rtrace("%x iran RESCUE(%d,%d) fresh=%v raw1 %v join %v", m.id, bi, ri, fresh, linkName(m.rscOld.key), linkName(key))
	ended := m.ended
	m.tr.ev("rescue: RESCUE(%d,%d) on sub %d", bi, ri, s2.idx)
	m.mu.Unlock()
	m.rescueSwitched(ri)
	rescueStat.started.Add(1)
	if ended {
		sub.Close()
		return
	}
	m.startSub(s2)
	m.goManage()
}

// onRescue (Kharej): RESCUE(bi, ri) read on s (false: protocol error).
func (m *mConn) onRescue(s *msub, bi, ri uint64) bool {
	m.mu.Lock()
	if !m.iran && m.rscFresh && m.raw1 == nil && m.rscPhase == 0 && ri == 0 {
		// er94: this sub opened the connection (role 4): nothing came raw,
		// nothing went raw; Iran resends [0, Bi) on the ACK
		ok := !m.ended
		if ok {
			s.push(rescueRec(mRecRescueAck, m.sendOff, 0))
			m.rscPhase, m.rscSub = 2, s
			m.rescueFreshServedLocked(bi) // rs169
		}
		m.mu.Unlock()
		if ok {
			rescueStat.served.Add(1)
			rtrace("%x exit served fresh RESCUE(%d,0)", m.id, bi)
		}
		return true
	}
	if !m.iran && m.rscPhase == 2 && m.rscSrv.set && m.rscSub != nil && s != m.rscSub && bi == m.rscSrv.bi && ri == m.rscSrv.ri {
		return m.rescueRepeatLocked(s) // rs169 (H1): Iran's retry of a RESCUE served here
	}
	ok := !m.iran && m.rscCap && m.rscPhase == 0 && m.raw1 != nil && s != m.raw1 &&
		m.rawW.Load() && m.rawR.Load() && !m.handshaking && !m.upgraded && !m.ended
	m.mu.Unlock()
	if !ok {
		rescueStat.failed.Add(1)
		rescueStat.refused.Add(1)
		rtrace("%x exit RESCUE(%d,%d) refused", m.id, bi, ri)
		return false
	}
	rtrace("%x exit RESCUE(%d,%d) read", m.id, bi, ri)
	m.cutRaw()
	rk := uint64(m.rawRecvd.Load())
	m.mu.Lock()
	bk := uint64(m.rawSent.Load())
	data, covered := m.ring.get(ri, bk)
	if ri > bk && rs169Fix.Load() {
		// rs169 (M5): Iran read past Bk: a cut raw write went out whole
		// but counted short (smux giveUp sends the detached frame). Those
		// bytes lead what Write sends next as records from Bk (Iran drops
		// them by offset): nothing to resend.
		data, covered = nil, true
		rs169Stat.overRead.Add(1)
		rtrace("%x exit RESCUE ri %d > bk %d: over-read, nothing to resend", m.id, ri, bk)
	}
	if ri > bk && !covered {
		m.mu.Unlock()
		m.rescueSwitched(rk)
		rescueStat.noCover.Add(1)
		rtrace("%x exit nocover ri %d > bk %d", m.id, ri, bk)
		m.end(errMConnLost, true)
		return true
	}
	kind := byte(mRecRescueAck)
	if !covered {
		// er94: NAK, the conn stays: Iran reads [Ri, Bk) on the first sub
		// (closed after them: its FIN follows the bytes), nothing resent here
		kind, data = mRecRescueNak, nil
	}
	m.rawW.Store(false)
	m.sendOff = bk
	m.abandonRawLocked()
	m.upgraded = true
	s.push(rescueRec(kind, bk, rk))
	pushResendLocked(s, ri, data)
	if m.wclosed { // hc183: the raw stream was half-closed at bk: the FIN as a record
		c := getChunk(mRecFin)
		c.setOff(m.sendOff)
		s.push(c)
	}
	m.rscPhase, m.rscSub = 2, s
	m.rescueServedLocked(bi, ri, bk, rk, kind, data) // rs169
	old := m.rscOld
	m.tr.ev("rescue: served RESCUE(%d,%d), ACK(%d,%d), resent %d", bi, ri, bk, rk, len(data))
	m.rescueForgetLocked()
	m.mu.Unlock()
	m.rescueSwitched(rk)
	rescueStat.served.Add(1)
	if kind == mRecRescueNak {
		rescueStat.nak.Add(1)
	}
	rtrace("%x exit served kind %#x (%d,%d) resent %d", m.id, kind, bk, rk, len(data))
	go old.close() // Iran reads it no more
	m.goManage()
	return true
}

// onRescueAck (Iran): ACK(bk, rk): resend [rk, Bi).
func (m *mConn) onRescueAck(s *msub, bk, rk uint64) bool {
	_ = bk
	m.wmu.Lock()
	m.mu.Lock()
	late, handled := m.rescueAnswerFromLocked(s) // rs169 (H1)
	if handled {
		m.mu.Unlock()
		m.wmu.Unlock()
		return true
	}
	if !m.iran || m.rscPhase != 1 || s != m.rscSub && !late {
		m.mu.Unlock()
		m.wmu.Unlock()
		return false
	}
	if !late {
		s.rrAband = false // it answered: alive (a retry under way stands down)
	}
	data, covered := m.rescueAnsRangeLocked(rk)
	if !covered {
		m.mu.Unlock()
		m.wmu.Unlock()
		rescueStat.noCover.Add(1)
		m.end(errMConnLost, true)
		return true
	}
	pushResendLocked(s, rk, data)
	m.rescueLateResendLocked(s, late, rk, data) // rs169
	m.rscPhase = 2
	old := m.rscOld
	m.rescueForgetLocked()
	m.tr.ev("rescue: ACK(%d,%d), resent %d late=%v", bk, rk, len(data), late)
	m.mu.Unlock()
	m.wmu.Unlock()
	rescueStat.acked.Add(1)
	rtrace("%x iran ACK(%d,%d) after %v", m.id, bk, rk, time.Since(m.rscT0).Round(time.Millisecond))
	go old.close()
	return true
}

// onRescueNak (Iran): NAK(bk, rk): resend [rk, Bi) as for an ACK, and read
// [Ri, bk) on the first sub with the raw reader (back on, limit bk).
func (m *mConn) onRescueNak(s *msub, bk, rk uint64) bool {
	m.wmu.Lock()
	m.mu.Lock()
	late, handled := m.rescueAnswerFromLocked(s) // rs169 (H1)
	if handled {
		m.mu.Unlock()
		m.wmu.Unlock()
		return true
	}
	if !m.iran || m.rscPhase != 1 || s != m.rscSub && !late || m.raw1 == nil {
		m.mu.Unlock()
		m.wmu.Unlock()
		return false
	}
	if !late {
		s.rrAband = false
	}
	data, covered := m.rescueAnsRangeLocked(rk)
	if !covered {
		m.mu.Unlock()
		m.wmu.Unlock()
		rescueStat.noCover.Add(1)
		m.end(errMConnLost, true)
		return true
	}
	pushResendLocked(s, rk, data)
	m.rescueLateResendLocked(s, late, rk, data) // rs169
	m.rscPhase = 2
	m.rescueForgetLocked()
	got := uint64(m.rawRecvd.Load())
	m.rLimit.Store(int64(bk))
	if got < bk && !m.raw1.dead {
		m.rawR.Store(true)
	}
	reraw := m.rawR.Load()
	old := m.rscOld
	m.tr.ev("rescue: NAK(%d,%d), resent %d, raw reader to %d (at %d)", bk, rk, len(data), bk, got)
	m.mu.Unlock()
	m.wmu.Unlock()
	rescueStat.nakd.Add(1)
	rtrace("%x iran NAK(%d,%d) after %v, raw reader %v to %d (at %d)", m.id, bk, rk, time.Since(m.rscT0).Round(time.Millisecond), reraw, bk, got)
	if reraw {
		r := &m.r
		r.mu.Lock()
		r.cond.Broadcast() // a Read waiting on reassembly goes back to raw
		r.mu.Unlock()
	} else {
		go old.close()
	}
	return true
}

// rescuePeerOK: the exit behind link l announced capExtMConnRescue.
func rescuePeerOK(l Link) bool {
	mtr := linkMeterOf(l)
	if mtr == nil {
		return false
	}
	pi := mtr.peerInfo.Load()
	return pi.mconnRaw() && pi.Ext&capExtMConnRescue != 0
}

// mconnRescueSub: a raw first sub opened with role 3.
type mconnRescueSub interface{ mconnRescue() bool }
