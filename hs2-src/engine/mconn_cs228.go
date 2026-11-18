package engine

import (
	"net"
	"reflect"
	"sync/atomic"
	"time"
)

// cs228: HS2_CS226_LITEPATH split in two, each part narrowed (lab2 cs227:
// with cs226 on, crowd p99 +9 ms and game p99 +25..40 ms, call p99 better).
// HS2_CS226_LITEPATH stays the master switch (=0: cs225, both parts off).
//
//   - HS2_CS228_LITEPLACE (default 1): cs226 part A, a lite second sub on
//     another path than its primary's. Narrowed: another path is preferred
//     only while its quality is not worse than the primary's path — mean
//     control RTT within max(10 %, 5 ms) and judged loss within 1 point
//     (cs228PlaceQualLocked). A path with no RTT sample on either side does
//     not qualify. Otherwise the pick is the old one (least-loaded light
//     link, a different LINK than the live one, often the same path): a copy
//     on a slower path costs nothing only while the receiver takes the
//     first arrival, but a lite primary switch onto it does.
//   - HS2_CS228_LITEVISIT (default 1): cs226 part B, darkcall visits of
//     non-call lite conns. Narrowed: a merely idle link is not dark. A conn
//     is visited only while one of its live subs has bytes the peer has not
//     consumed (smux marks) AND that sub's link either has heard nothing for
//     darkcallAfter with a write unanswered for darkcallAfter (silent while
//     pending — er92's rescueLinkState rule with darkcall's window), or is
//     marked lowered/cut by the balancer with a write unanswered (wr > rd).
//     A visit's TCP_INFO reads run with m.mu released (darkcallTick prefetch):
//     a crowd conn's first byte after idle never waits behind a syscall.
//
// Also: darkcallAfter / darkcallGap and the darkcallRead / darkcallMarks test
// hooks are atomics (the global darkcall timer read them while tests restored
// them: cs227 race report, test-only).

var (
	cs228PlaceKey atomic.Bool
	cs228VisitKey atomic.Bool

	darkcallAfterNs atomic.Int64
	darkcallGapNs   atomic.Int64

	darkcallReadP atomic.Pointer[dcReadFn]
	// darkcallReadOSOn (fx247): darkcallRead is darkcallReadOS (the lite
	// visit resolves s.rw under m.mu and reads only TCP_INFO unlocked)
	darkcallReadOSOn atomic.Bool
	darkcallMarksP   atomic.Pointer[dcMarksFn]

	// cs228PendHook: tests replace the "link has a write unanswered" check
	cs228PendHook atomic.Pointer[func(*msub, int64) bool]
)

type (
	dcReadFn  func(s *msub) (tc *net.TCPConn, path string, lastRd int64, acked uint64, unacked uint32, backoff uint8, retrans uint32, ok bool)
	dcMarksFn func(s *msub) (written, consumed uint32, ok bool)
)

func init() {
	cs228PlaceKey.Store(envBoolDefault("HS2_CS228_LITEPLACE", false)) // rel68e: off (HS2_CS229_MINPLACE replaces it)
	cs228VisitKey.Store(envBoolDefault("HS2_CS228_LITEVISIT", true))
	// darkcallAfter: the dark evidence window (no ack progress AND nothing
	// received on the link); HS2_MCONN_DARKCALL_MS, clamped to [300, 5000].
	darkcallAfterNs.Store(int64(darkcallAfterMs(envIntDef("HS2_MCONN_DARKCALL_MS", 1000))))
	darkcallGapNs.Store(int64(3 * time.Second))
	r, mk := dcReadFn(darkcallReadOS), dcMarksFn(darkcallMarksOS)
	darkcallReadP.Store(&r)
	darkcallReadOSOn.Store(true)
	darkcallMarksP.Store(&mk)
	darkcallMarksOSOn.Store(true)
}

func darkcallAfter() time.Duration { return time.Duration(darkcallAfterNs.Load()) }
func darkcallGap() time.Duration   { return time.Duration(darkcallGapNs.Load()) }

func setDarkcallTimes(after, gap time.Duration) {
	darkcallAfterNs.Store(int64(after))
	darkcallGapNs.Store(int64(gap))
}

func darkcallRead(s *msub) (*net.TCPConn, string, int64, uint64, uint32, uint8, uint32, bool) {
	return (*darkcallReadP.Load())(s)
}

func darkcallMarks(s *msub) (uint32, uint32, bool) { return (*darkcallMarksP.Load())(s) }

func darkcallReadSet(f dcReadFn) {
	darkcallReadOSOn.Store(reflect.ValueOf(f).Pointer() == reflect.ValueOf(darkcallReadOS).Pointer())
	darkcallReadP.Store(&f)
}
func darkcallMarksSet(f dcMarksFn) {
	darkcallMarksOSOn.Store(reflect.ValueOf(f).Pointer() == reflect.ValueOf(darkcallMarksOS).Pointer()) // b296
	darkcallMarksP.Store(&f)
}

func cs228Place() bool { return cs226LitePath() && cs228PlaceKey.Load() }
func cs228Visit() bool { return cs226LitePath() && cs228VisitKey.Load() }

var cs228Stat struct {
	pre       atomic.Uint64 // lite visits whose TCP_INFO reads ran unlocked
	qualNo    atomic.Uint64 // placements where no other path qualified (old pick)
	qualDrop  atomic.Uint64 // other paths dropped as worse than the primary's
	qualKnown atomic.Uint64 // placements with a known primary-path quality
}

// ---- placement ------------------------------------------------------------

// cs228 quality margins: another path qualifies while its mean control RTT
// is within max(10 %, 5 ms) of the primary path's and its judged loss within
// one point.
const (
	cs228RttAbs  = 5 * time.Millisecond
	cs228LossAdd = 0.01
)

type cs228Qual struct {
	rttSum time.Duration
	rttN   int
	loss   float64
}

func (q cs228Qual) rtt() time.Duration {
	if q.rttN == 0 {
		return 0
	}
	return q.rttSum / time.Duration(q.rttN)
}

// cs228PlaceQualLocked narrows healthy to the paths whose quality is not
// worse than the skipped (primary) paths' by more than the margins. Nil: no
// path qualifies (the caller falls back to the old pick).
func (m *LinkManager) cs228PlaceQualLocked(skip, healthy map[string]bool) map[string]bool {
	if len(healthy) == 0 {
		return healthy
	}
	q := map[string]*cs228Qual{}
	for _, ml := range m.links {
		path := mlPath(ml)
		if path == "" || !ml.link.Alive() || !skip[path] && !healthy[path] {
			continue
		}
		x := q[path]
		if x == nil {
			x = &cs228Qual{}
			q[path] = x
		}
		if r := time.Duration(ml.mtr.rttMicros.Load()) * time.Microsecond; r > 0 {
			x.rttSum += r
			x.rttN++
		}
		x.loss = max(x.loss, ml.lossFrac)
	}
	var prim cs228Qual // the worst skipped path is the reference: the primary's
	for path := range skip {
		x := q[path]
		if x == nil || x.rttN == 0 {
			continue
		}
		if r := x.rtt(); r > prim.rtt() {
			prim = cs228Qual{rttSum: r, rttN: 1, loss: prim.loss}
		}
		prim.loss = max(prim.loss, x.loss)
	}
	if prim.rttN == 0 {
		cs228Stat.qualNo.Add(1)
		return nil
	}
	cs228Stat.qualKnown.Add(1)
	pr := prim.rtt()
	lim := pr + max(pr/10, cs228RttAbs)
	var out map[string]bool
	for path := range healthy {
		x := q[path]
		if x == nil || x.rttN == 0 || x.rtt() > lim || x.loss > prim.loss+cs228LossAdd {
			cs228Stat.qualDrop.Add(1)
			continue
		}
		if out == nil {
			out = map[string]bool{}
		}
		out[path] = true
	}
	if out == nil {
		cs228Stat.qualNo.Add(1)
	}
	return out
}

// ---- visits ---------------------------------------------------------------

// cs228InFlight: s has bytes its peer has not consumed (no marks: assume so).
func cs228InFlight(s *msub) bool {
	w, c, ok := darkcallMarks(s)
	return !ok || int32(w-c) > 0
}

// cs228Pending: s's link wrote after its last read (a write unanswered).
func cs228Pending(s *msub, nowc int64) bool {
	if h := cs228PendHook.Load(); h != nil {
		return (*h)(s, nowc)
	}
	mtr := dcMeterOf(s)
	if mtr == nil {
		return false
	}
	rd, wr := mtr.lastRd.Load(), mtr.lastWr.Load()
	return rd != 0 && wr > rd
}

// cs228PreLocked (under m.mu): the live subs whose TCP_INFO a lite visit
// will read this tick (nil: no lite visit, or the key is off).
//
// lite is the visit's cs226 lite verdict as darkcallLocked computes it; it
// holds for the visit only while m.mu stays held (pre empty: no prefetch,
// no release) — audit246 L3: the visit then reuses it instead of a second
// walk of the subs.
func (m *mConn) cs228PreLocked(now time.Time, nowc int64) (pre []*msub, lite bool) {
	if !cs228Visit() || m.ended || m.closing || m.dcIsCallLocked(now) || !m.cs226LiteVisitLocked(nowc) {
		return nil, false
	}
	return m.rrLiveSubsLocked(), true
}

// dcPreRead: one prefetched darkcallRead (under m.mu once stored).
type dcPreRead struct {
	tc      *net.TCPConn
	path    string
	lastRd  int64
	acked   uint64
	unacked uint32
	backoff uint8
	retrans uint32
	ok      bool
}

// dcReadLocked: s's prefetched link evidence if any (consumed), else a read
// now.
func (m *mConn) dcReadLocked(s *msub) (*net.TCPConn, string, int64, uint64, uint32, uint8, uint32, bool) {
	if p := s.dcPre; p != nil {
		s.dcPre = nil
		return p.tc, p.path, p.lastRd, p.acked, p.unacked, p.backoff, p.retrans, p.ok
	}
	return darkcallRead(s)
}

// darkcallVisit: one timer visit of m; a lite visit reads its subs' TCP_INFO
// with m.mu released (cs228).
func (m *mConn) darkcallVisit(now time.Time, nowc int64) bool {
	seq := m.dcWr.Load() // b296: read before any mark
	m.mu.Lock()
	pre, lite := m.cs228PreLocked(now, nowc)
	var rr bool
	if len(pre) > 0 {
		m.dcPrefetchLocked(pre) // m.mu released meanwhile: the visit looks again
		cs228Stat.pre.Add(1)
		rr = m.darkcallLocked(now, nowc)
	} else {
		rr = m.darkcallLiteLocked(now, nowc, lite) // m.mu held throughout (L3)
	}
	for _, s := range pre {
		s.dcPre = nil
	}
	if !rr {
		m.b296ProofLocked(now, seq)
	} else {
		m.dcIdleAt.Store(0)
	}
	m.mu.Unlock()
	return rr
}

// dcPrefetchLocked (under m.mu; released and taken again): pre's link
// evidence into s.dcPre. fx247 (audit246 M3, the sg154 rule as sw235): each
// sub's socket and link meter are resolved from s.rw under m.mu (releaseRaw1
// writes s.rw under it: an unlocked read could see a torn interface); only
// the TCP_INFO reads run with m.mu released.
func (m *mConn) dcPrefetchLocked(pre []*msub) {
	rd := make([]dcPreRead, len(pre))
	if !darkcallReadOSOn.Load() {
		// a test's read (its own fakes, no s.rw): unlocked, as before
		m.mu.Unlock()
		for i, s := range pre {
			x := &rd[i]
			x.tc, x.path, x.lastRd, x.acked, x.unacked, x.backoff, x.retrans, x.ok = darkcallRead(s)
		}
		m.mu.Lock()
	} else {
		for i, s := range pre {
			x := &rd[i]
			x.tc = subTCP(s)
			if mtr := dcSubMeter(s); mtr != nil {
				x.path, x.lastRd = mtr.path, mtr.lastRd.Load()
			}
		}
		m.mu.Unlock()
		for i := range rd {
			if x := &rd[i]; x.tc != nil {
				x.acked, x.unacked, x.backoff, x.retrans, x.ok = tcpDarkInfo(x.tc)
			}
		}
		m.mu.Lock()
	}
	for i, s := range pre {
		s.dcPre = &rd[i]
	}
}

// cs228SilentAt: a link's last read rd, last write wr, first write after the
// last read ps (ctrlNow): heard nothing for darkcallAfter AND a write has
// waited for darkcallAfter without an answer.
func cs228SilentAt(rd, wr, ps, nowc int64) bool {
	after := darkcallAfter()
	return rd != 0 && time.Duration(nowc-rd) >= after && wr > rd && time.Duration(nowc-ps) >= after
}

// rrRingCapDC: rrRingCap as the darkcall timer reads it (dcCapFor); tests
// change rrRingCap (setRRRingCap) while the global timer ticks, so the timer
// reads this atomic mirror (cs228 race fix; production sets both once).
var rrRingCapDC atomic.Int64

func init() { rrRingCapDC.Store(int64(rrRingCap)) }
