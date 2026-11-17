package engine

import (
	"net"
	"reflect"
	"sync"
	"sync/atomic"
	"time"
)

// fx212 (an210 Q1 #2): HS2_FX212_SUB_RESEND (default on; "0": the rel68c
// behaviour) trims the light-path cost of HS2_MCONN_SUB_RESEND and
// HS2_MCONN_CALL_RESEND (lab2 PR arm: crowd p99 +2..3 ms):
//
//   - a given-up sub (CLOSE-REQ, handshake not done) of a sub-resend
//     connection resends at its death only if it may hold undelivered bytes
//     (subUndeliveredLocked); r68fix2 resent for every one (callUncleanLocked);
//   - the 1 s keep poll of sub-resend connections runs in ONE shared sweeper
//     (subSweeper) instead of each connection's manager goroutine, and the
//     TCP_INFO calls run without the connection's lock;
//   - the kept gauge's max is noted per sweeper tick, not by a CAS loop on
//     every write (subKeptAdd);
//   - a resend does not queue on the target sub chunks the target itself
//     already carries (fx212SkipHeld): those were pure duplicates queued ahead
//     of the connection's new bytes.
var fx212SubResend atomic.Bool

func init() { fx212SubResend.Store(envBoolDefault("HS2_FX212_SUB_RESEND", true)) }

// fx212AgeGap: HS2_FX212_AGE_GAP (default on; audit211 M2): the write-path
// age trim (subAgeTrimLocked, sys=false) drops chunks without TCP proof; it
// now marks the sub's gap until the TCP look (the 1 s poll) heals it.
var fx212AgeGap = envBoolDefault("HS2_FX212_AGE_GAP", true)

// subUndeliveredLocked (under m.mu, s dying): s may hold bytes the peer does
// not have — a queued or in-write sequence chunk, or kept chunks the link
// cannot prove delivered (link unknown, in an RTO loss, or the newest kept
// chunk younger than subKeepAgeOf with bytes in flight, subKeepAgeMin with
// none: it may still sit in smux's send queue).
func (m *mConn) subUndeliveredLocked(s *msub, now time.Time) bool {
	if s.cur != nil && s.cur.seq() {
		return true
	}
	for _, c := range s.curMore {
		if c.seq() {
			return true
		}
	}
	for _, c := range s.q {
		if c.seq() {
			return true
		}
	}
	if len(s.sent) == 0 {
		return false
	}
	la, ok := subLinkAck(s)
	if !ok || la.rtoLoss {
		return true
	}
	lim := subKeepAgeMin
	if la.unacked > 0 {
		if la.srtt > 0 {
			s.subSrtt = la.srtt
		}
		lim = subKeepAgeOf(s)
	}
	return now.Sub(s.sent[len(s.sent)-1].t) <= lim
}

// fx212SkipHeld: cs without the chunks b holds itself (queued, in write or
// kept): b delivers them, or its own death resends them.
func fx212SkipHeld(cs []*mchunk, b *msub) []*mchunk {
	held := make(map[*mchunk]struct{}, len(b.q)+len(b.sent)+1)
	for _, c := range b.q {
		held[c] = struct{}{}
	}
	for _, r := range b.sent {
		held[r.c] = struct{}{}
	}
	if b.cur != nil {
		held[b.cur] = struct{}{}
	}
	for _, c := range b.curMore {
		held[c] = struct{}{}
	}
	out := cs[:0]
	for _, c := range cs {
		if _, ok := held[c]; ok {
			fx212Stat.skipped.Add(1)
			continue
		}
		out = append(out, c)
	}
	return out
}

var fx212Stat struct {
	skipped   atomic.Uint64 // resend chunks the target already held
	sweepConn atomic.Int64  // conns in the shared sweeper now
	sweeps    atomic.Uint64 // conn visits by the sweeper
	tcpInfo   atomic.Uint64 // sw235: TCP_INFO reads by the sweeper (one per link socket per bucket tick)
	tcpCached atomic.Uint64 // sw235: sub looks served from the bucket tick's link cache
	idle      atomic.Uint64 // sw235: visits skipped without m.mu (nothing kept since the last look)
}

// sw235 (sc234: subSweepTick ~0.11 core per side at 9k conns, half of it one
// getsockopt(TCP_INFO) per sub, plus a map alloc and two m.mu turns per conn,
// all in one 1 s burst; and a sync.Map Store/Delete per conn every time it
// went quiet and wrote again):
//   - the sweep is spread over subSweepBuckets buckets, one per
//     keepIdlePoll/subSweepBuckets; each conn is still looked at once per
//     keepIdlePoll;
//   - a link socket's TCP_INFO is read once per bucket tick and shared by
//     every sub riding it; the socket is resolved under m.mu (sg154);
//   - a conn whose look found nothing kept stays in the set with its swKept
//     hint clear: the sweeper skips it without m.mu until a keep or a gap
//     sets the hint (no Store/Delete churn per burst); every
//     subSweepGCEvery-th round it is visited under m.mu and leaves if still
//     quiet (or ended);
//   - the looks live in reused slices (no per-conn map).
const (
	subSweepBuckets = 10
	subSweepGCEvery = 10
)

var (
	subSweepConns [subSweepBuckets]sync.Map // *mConn → struct{}, bucket = id % subSweepBuckets
	subSweepOnce  sync.Once
)

func subSweepBucketOf(m *mConn) *sync.Map { return &subSweepConns[m.id%subSweepBuckets] }

// subSweepHas: m is in the sweeper's set.
func subSweepHas(m *mConn) bool {
	_, ok := subSweepBucketOf(m).Load(m)
	return ok
}

// noteSwKeptLocked (under m.mu, m swept): m keeps chunks or has a gap; the
// sweeper looks at it on its next round (one store per quiet->busy change).
func (m *mConn) noteSwKeptLocked() {
	if !m.swKept.Load() {
		m.swKept.Store(true)
	}
}

// subSweepableLocked: m's keep poll is the sub-resend one (ackStallTickLocked's
// first branch) and may run in the shared sweeper.
func (m *mConn) subSweepableLocked() bool {
	return fx212SubResend.Load() && keepIdleOn && keepStallOn.Load() && m.stN == 0 && !m.callKeepLocked() && !m.ended && !m.closing
}

// subSweepArmLocked (under m.mu, keepIdleArmLocked): true: m is (now) polled
// by the shared sweeper.
func (m *mConn) subSweepArmLocked() bool {
	if !m.subSweepableLocked() {
		if m.ksSwept { // sw235: resident but no longer sweepable: the manager's poll
			m.ksSwept = false
			m.swKept.Store(false)
			if _, ok := subSweepBucketOf(m).LoadAndDelete(m); ok {
				fx212Stat.sweepConn.Add(-1)
			}
		}
		return false
	}
	m.noteSwKeptLocked()
	if m.ksSwept {
		return true
	}
	m.ksSwept = true
	subSweepBucketOf(m).Store(m, struct{}{})
	fx212Stat.sweepConn.Add(1)
	subSweepOnce.Do(func() { go subSweeper() })
	return true
}

// subSweepDropLocked (under m.mu): m leaves the sweeper; what it still keeps
// goes back to its manager's poll (a stripe, a call).
func (m *mConn) subSweepDropLocked() {
	if !m.ksSwept {
		return
	}
	m.ksSwept, m.ksIdle = false, false
	m.swKept.Store(false)
	if _, ok := subSweepBucketOf(m).LoadAndDelete(m); ok {
		fx212Stat.sweepConn.Add(-1)
	}
	if !m.ended && !m.closing {
		m.keepIdleArmLocked()
	}
}

// subSweepPaused: tests stop the background sweeper.
var subSweepPaused atomic.Bool

func subSweeper() {
	t := time.NewTicker(keepIdlePoll / subSweepBuckets)
	defer t.Stop()
	w := &subSweepWork{}
	b, round := 0, 0
	for now := range t.C {
		if subSweepPaused.Load() {
			continue // tests drive the buckets themselves
		}
		w.bucket(now, b, round%subSweepGCEvery == subSweepGCEvery-1)
		if b++; b == subSweepBuckets {
			b = 0
			round++
		}
	}
}

type fx212Look struct {
	la linkAck
	ok bool
}

// subSweepTCP: the link socket of s for the sweeper's cache (under m.mu;
// tests replace it).
var subSweepTCP = subTCP

// subTCPAck: one TCP_INFO read of a link socket (tests replace it).
var subTCPAck = func(tc *net.TCPConn) (linkAck, bool) {
	srtt, acked, unacked, rtoLoss, ok := tcpAckState(tc)
	return linkAck{srtt, acked, unacked, rtoLoss}, ok
}

// subLinkAckSys is subLinkAck's production body.
func subLinkAckSys(s *msub) (linkAck, bool) {
	tc := subTCP(s)
	if tc == nil {
		return linkAck{}, false
	}
	return subTCPAck(tc)
}

var subLinkAckSysPtr = reflect.ValueOf(subLinkAckSys).Pointer()

// subLinkAckHooked: a test replaced subLinkAck; the sweeper then asks it per
// sub (no cache), so the older tests keep their timelines.
func subLinkAckHooked() bool {
	return reflect.ValueOf(subLinkAck).Pointer() != subLinkAckSysPtr
}

// subSweepWork: the sweeper's reused state (one goroutine owns it).
type subSweepWork struct {
	xs      []*msub
	tcs     []*net.TCPConn
	looks   []fx212Look
	cache   map[*net.TCPConn]fx212Look // valid for one bucket tick
	hook    bool
	hookSet bool
}

// subSweepTick: one full poll (every bucket, quiet conns too) of every swept
// connection: the pre-sw235 tick (tests, benchmarks).
func subSweepTick(now time.Time) {
	w := &subSweepWork{}
	for b := 0; b < subSweepBuckets; b++ {
		w.bucket(now, b, true)
	}
}

// bucket: one poll of the connections in bucket b (gc: quiet ones too, and
// those still quiet leave); the link reads run without m.mu, once per link
// socket.
func (w *subSweepWork) bucket(now time.Time, b int, gc bool) {
	subKeptMaxNote(subResendStat.kept.Load())
	if w.cache == nil {
		w.cache = make(map[*net.TCPConn]fx212Look, 64)
	}
	clear(w.cache)
	w.hookSet = false // read subLinkAck only when a look is needed (tests swap it)
	subSweepConns[b].Range(func(k, _ any) bool {
		w.visit(k.(*mConn), now, gc)
		return true
	})
}

func (w *subSweepWork) look(x *msub, tc *net.TCPConn) fx212Look {
	if !w.hookSet {
		w.hook, w.hookSet = subLinkAckHooked(), true
	}
	if w.hook {
		la, ok := subLinkAck(x)
		return fx212Look{la, ok}
	}
	if tc == nil {
		return fx212Look{}
	}
	if l, ok := w.cache[tc]; ok {
		fx212Stat.tcpCached.Add(1)
		return l
	}
	fx212Stat.tcpInfo.Add(1)
	la, ok := subTCPAck(tc)
	l := fx212Look{la, ok}
	w.cache[tc] = l
	return l
}

func (w *subSweepWork) visit(m *mConn, now time.Time, gc bool) {
	if !gc && !m.swKept.Load() {
		fx212Stat.idle.Add(1) // quiet since its last look: nothing to trim or heal
		return
	}
	fx212Stat.sweeps.Add(1)
	m.mu.Lock()
	if !m.subSweepableLocked() {
		m.subSweepDropLocked()
		m.mu.Unlock()
		return
	}
	w.xs, w.tcs = w.xs[:0], w.tcs[:0]
	for _, x := range m.subs {
		if !x.dead && (len(x.sent) > 0 || !x.subGapT.IsZero()) {
			w.xs = append(w.xs, x)
			w.tcs = append(w.tcs, subSweepTCP(x)) // sg154: s.rw under m.mu
		}
	}
	if len(w.xs) > 0 {
		m.mu.Unlock()
		w.looks = w.looks[:0]
		for i, x := range w.xs {
			w.looks = append(w.looks, w.look(x, w.tcs[i]))
		}
		m.mu.Lock()
		if !m.subSweepableLocked() {
			m.subSweepDropLocked()
			m.mu.Unlock()
			w.release()
			return
		}
	}
	m.keepStallTrimLocked(nil, now)
	d := m.keepIdleLookLocked(now, func(x *msub) (linkAck, bool) {
		for i, y := range w.xs {
			if y == x {
				return w.looks[i].la, w.looks[i].ok
			}
		}
		return linkAck{}, false // a sub new since the read: the freshness rule
	})
	if d == 0 {
		if gc {
			m.subSweepDropLocked()
		} else {
			m.swKept.Store(false) // resident and quiet: skipped until a keep or a gap
		}
	}
	m.mu.Unlock()
	w.release()
}

// release: no sub or socket references kept past a visit.
func (w *subSweepWork) release() {
	clear(w.xs)
	clear(w.tcs)
	w.xs, w.tcs = w.xs[:0], w.tcs[:0]
}
