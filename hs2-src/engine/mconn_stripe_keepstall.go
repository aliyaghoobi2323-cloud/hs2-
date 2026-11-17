package engine

import (
	"errors"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// HS2_MCONN_STRIPE_KEEPSTALL (hs112, both sides; lab key, off by default):
// a striped heavy download survives one of its paths going dark.
//
// mx110 / hs112 trace (lab1, rel61 both sides, path 49.2->53.1 blackholed
// one way): Iran's receive side stopped at a hole of ONE chunk (16 KiB) 0.3 s
// after the dark began (rx next=2220345333, ooo1=next+16373) — the chunk the
// exit had just written on a sub of the dark path. The other subs kept going
// until the reorder buffer was full (oooB 64 MiB, every sub's reader then
// blocked in deliver). The dark subs died 3-20 s later (Iran's silent rule,
// the exit's write timeout) and the exit's stripe RESEND ran, but with "resend
// 0 chunks": keepSentLocked's per-connection bound (stripeKeepBytes, 16 MiB)
// evicts the oldest kept chunks of ANY sub, and at ~1 Gbit/s the other subs'
// writes pushed the dark sub's out within ~100 ms. The hole was never filled;
// gapCheck reset the connection 30 s later ("missing bytes never arrived")
// and iperf3 sat at 0 to the end (or lost its control socket).
//
// With the key on:
//   - sender: each kept chunk records the sub's smux written count after it;
//     a chunk is released once the peer's consumed count (smux window
//     updates: the peer's mconn reader took it) passes that mark, and NOT
//     before (neither by age nor by the per-connection bound) unless the
//     connection keeps more than keepStallHard — what a dark path swallowed
//     is never acked, so it stays for the resend at the sub's death. Unacked
//     bytes per sub are bounded by the smux stream window;
//   - receiver: a stripe whose reorder buffer is full while the hole is
//     older than gapRoomAfter accepts up to gapRoomBytes more out of order,
//     so the sub carrying the resend (queued behind what that sub already had
//     in flight) can be read up to it instead of every reader waiting on the
//     hole forever; past that (and the span, gapFillLocked) each sub may put
//     its own smux window more for that hole (hs119, holeSubRoomLocked).
// hs133: the receiver's rooms only toward a peer announcing capExtMConnKeepStall
// (ext bit10), and capped (holeRoomConnMax, holeRoomGlobalMax).

var keepStallOn atomic.Bool // loadEnvKeys: HS2_MCONN_STRIPE_KEEPSTALL

// keepStallFromEnv: hs128 default on (envdef.Table, both sides);
// HS2_MCONN_STRIPE_KEEPSTALL=0 turns it off.
func keepStallFromEnv() bool { return envDefault("HS2_MCONN_STRIPE_KEEPSTALL") == "1" }

func init() {
	keepStallOn.Store(keepStallFromEnv())
	keepStallLazy.Store(keepStallLazyFromEnv())
}

// keepStallLazy (hs128, HS2_MCONN_STRIPE_KEEPSTALL_LAZY, default on with the
// key): the sender's 100 ms ack-stall poll runs only while a sub is SUSPECT
// (ksSuspectLocked: data waiting to go, no ack progress for ackStallLazy,
// its oldest kept chunk that old). The writers look (ksLookLocked, at most
// every ackStallPoll per connection) and keep each sub's ack progress
// (ksTrackLocked), so the rule's clock is the one the always-on poll had.
// =0: the hs113..hs126 behaviour (poll from the first kept chunk on).
var keepStallLazy atomic.Bool

func keepStallLazyFromEnv() bool { return os.Getenv("HS2_MCONN_STRIPE_KEEPSTALL_LAZY") != "0" }

// ackStallLazy: no ack progress this long with data waiting makes a sub
// suspect (well under ackStallAfter: the rule's first look is not delayed).
const ackStallLazy = 300 * time.Millisecond

const (
	keepStallHard = 96 << 20
	// gapRoomAfter: the hole age before any hole room (gap room, gap fill,
	// hole sub room). hs131 (lab knock-out, key on Iran only): at 500 ms the
	// rooms opened in the ordinary 0.5-1.6 s stalls of a lossy quiet path
	// (gap_room 140..3241 chunks per quiet d8 run) — the heavy stripes kept
	// filling links the call shares instead of being held by the full
	// reorder buffer, and call stalls > 500 ms grew. A resend cannot arrive
	// before the sender's ack-stall rule closes the dark sub: ackStallAfter
	// (800 ms) + the sub's TCP frozen for max(ackStallTCPMin 1 s, 8 x srtt),
	// so >= 1.8 s; 1.5 s keeps the rooms open before it arrives.
	gapRoomAfter = 1500 * time.Millisecond
	gapRoomBytes = 32 << 20
)

// keepStallGlobal: kept bytes over every connection; above it each
// connection drops its oldest unacked chunks as it writes (a var: tests
// lower it).
var keepStallGlobal int64 = 320 << 20

var keepStallStat struct {
	marked   atomic.Uint64 // chunks kept with an ack mark
	unmarked atomic.Uint64 // chunks kept without one (the old rule)
	acked    atomic.Uint64 // kept chunks released by the peer's ack
	evicted  atomic.Uint64 // unmarked chunks dropped by stripeKeepBytes
	hard     atomic.Uint64 // unacked chunks dropped by keepStallHard / the global cap
	gapRoom  atomic.Uint64 // chunks accepted over the reorder bound for a hole
	gapFill  atomic.Uint64 // ... inside the reorder range (hs113)
	holeSub  atomic.Uint64 // ... past every limit, a sub's own hole room (hs119)
	stalls   atomic.Uint64 // subs closed by the ack-stall rule
	targeted atomic.Uint64 // resends sent to a sub with recent acks
	cand     atomic.Uint64 // hs115: stall episodes the ack rule saw (with proof)
	vetoTCP  atomic.Uint64 // ... left alone: the sub's TCP link is acked (not dark)
	noSock   atomic.Uint64 // ... closed with no TCP socket to ask (after ackStallNoSockK x)
	kept     atomic.Int64  // bytes kept over every connection (key on)
	ticks    atomic.Uint64 // hs128: ack-stall polls run
	lazyOn   atomic.Uint64 // hs128: polls started by a suspect sub (lazy)
	lazyOff  atomic.Uint64 // hs128: polls stopped, no suspect sub left (lazy)
}

// subAckMarks: s's stream marks (tests replace it).
var subAckMarks = func(s *msub) (written, consumed uint32, ok bool) { return ackMarksOf(s.rw) }

// ackMarksOf: v's smux marks. hs113: the exit's stripe subs are
// exitCountedStream (an embedded *smux.Stream: AckMarks is promoted, the
// lower-case ackMarks is not) — hs112 looked only for ackMarks, so every
// chunk the exit (a download's sender) kept was unmarked and the old
// eviction applied (lab K1a: "resend 3-66 chunks", the hole never filled).
func ackMarksOf(v any) (written, consumed uint32, ok bool) {
	switch a := v.(type) {
	case interface {
		ackMarks() (uint32, uint32, bool)
	}:
		return a.ackMarks()
	case interface{ AckMarks() (uint32, uint32) }:
		w, c := a.AckMarks()
		return w, c, true
	}
	return 0, 0, false
}

// keepStallMarkLocked: the written mark for chunks s just wrote (ok=false:
// no marks, the old rule applies to them).
func (m *mConn) keepStallMarkLocked(s *msub) (uint32, bool) {
	if !keepStallOn.Load() {
		return 0, false
	}
	w, _, ok := subAckMarks(s)
	if ok {
		if !m.ksTicking && m.stN > 0 && !keepStallLazy.Load() {
			// hs113 (lab N1): the exit's manager ticked only when kicked —
			// the ack-stall rule first looked 7 s into the dark. Its tick
			// now polls every ackStallPoll while chunks are kept.
			m.ksTicking = true
			m.kickMgr()
		}
		keepStallStat.marked.Add(1)
	} else {
		keepStallStat.unmarked.Add(1)
	}
	return w, ok
}

// keepStallTrimLocked (key on): drop acked chunks — on a write (s != nil)
// only s's, unless a bound is crossed (hs123: the per-write scan of every
// sub's marks and the bound loops were quiet-run cost); from the tick
// (s == nil) every sub's. Then, over the hard bound, the oldest of any sub.
// Chunks without a mark follow the old rule (age on s, then the bound).
func (m *mConn) keepStallTrimLocked(s *msub, now time.Time) {
	if s != nil {
		m.keepStallTrimSubLocked(s, s, now)
		if m.keptBytes <= stripeKeepBytes {
			return
		}
	}
	for _, x := range m.subs {
		m.keepStallTrimSubLocked(x, s, now)
	}
	for m.keptBytes > stripeKeepBytes {
		// unmarked chunks first (the old rule), oldest first
		var o *msub
		for _, x := range m.subs {
			if len(x.sent) > 0 && !x.sent[0].marked && (o == nil || x.sent[0].t.Before(o.sent[0].t)) {
				o = x
			}
		}
		if o == nil {
			break
		}
		keepStallStat.evicted.Add(1)
		m.dropSentLocked(o, 1)
	}
	for m.keptBytes > keepStallHard || m.keptBytes > 0 && keepStallStat.kept.Load() > keepStallGlobal {
		// hs113 (lab N1: 970 hard drops, the hole among them — it is the
		// oldest): the oldest chunk of the sub acked most recently goes
		// first; a stalled sub's chunks (the hole) last.
		var o *msub
		for _, x := range m.subs {
			if len(x.sent) == 0 {
				continue
			}
			if o == nil || x.ks.at.After(o.ks.at) || x.ks.at.Equal(o.ks.at) && x.sent[0].t.Before(o.sent[0].t) {
				o = x
			}
		}
		if o == nil {
			break
		}
		keepStallStat.hard.Add(1)
		m.dropSentLocked(o, 1)
	}
}

// keepStallTrimSubLocked: drop x's acked prefix (s: the writer, whose
// unmarked chunks also go by age).
func (m *mConn) keepStallTrimSubLocked(x, s *msub, now time.Time) {
	if len(x.sent) == 0 {
		return
	}
	_, c, ok := subAckMarks(x)
	k := 0
	for k < len(x.sent) {
		r := x.sent[k]
		if r.marked {
			if !ok || int32(c-r.w) < 0 {
				break
			}
		} else if !(x == s && now.Sub(r.t) > stripeKeepAge) {
			break
		}
		k++
	}
	if k > 0 {
		if r := x.sent[k-1]; r.marked && r.t.After(x.ks.ackedT) {
			x.ks.ackedT = r.t // the peer read what x wrote at r.t
		}
		keepStallStat.acked.Add(uint64(k))
		for _, r := range x.sent[:k] {
			if !r.marked {
				x.noteUnprov(r.c.off + uint64(r.c.n)) // cs223: by age, no ack proof
			}
		}
		m.dropSentRawLocked(x, k) // cs223: marked ones are acked (proof)
	}
}

// gapRoomLocked (under r.mu, the reorder buffer full): the hole is old
// enough and the extra room not used up — take the chunk anyway.
func (r *mrecv) gapRoomLocked(now time.Time, n int) bool {
	if !keepStallOn.Load() || len(r.ooo) == 0 { // hs113: the exit (an upload's receiver) too
		return false
	}
	if now.Sub(r.lastProgress) < gapRoomAfter {
		return false
	}
	if r.oooBytes+n > max(r.oooMax, mRecvBuf)+gapRoomBytes {
		return false
	}
	keepStallStat.gapRoom.Add(1)
	return true
}

// ---- hs113: the ack-stall rule (sender side, key on) -----------------------
//
// hs112 lab K1a: with the marks the dark sub's chunks were kept, but the
// resend ran only when the sub died (3-20 s later) and went to the sub with
// the shortest queue — often another sub on the same dark path, dying
// itself. The hole stayed open and gapCheck reset the connection at +30 s.
//
// Now the sender watches each sub's ack progress (the peer's smux consumed
// count). A sub is STALLED when its oldest kept chunk is older than
// ackStallAfter, its consumed count has not moved for ackStallAfter, and
// another live sub's peer has consumed a chunk written at least
// ackStallLead AFTER that oldest chunk — proof the receiver reads, just not
// this sub. The stalled sub is closed at once (subDied); the stripe resend
// puts everything it held, wrote and kept (the marks keep every unacked
// chunk) at the front of a sub with recent acks (ackStallTargetLocked),
// never on another stalled one. The receiver drops what it already has, so
// a chunk that did arrive is harmless. No wire change: an older receiver
// sees DATA records it may already hold (exactly a rel49 resend); an older
// sender just does not do it (the old behaviour).
//
// Bounds: striped connections only (heavy; calls never stripe); one round
// per connection per ackStallGap; at most ackStallPerSec subs per second over
// the process; never the last sub with acks.

var errStripeAckStall = errors.New("mconn: stripe sub stopped being acked (dark path), its chunks moved")

const (
	ackStallAfter  = 800 * time.Millisecond // hs113 lab U1: 500 ms closed slow subs on a 2.5 % loss path
	ackStallLead   = 200 * time.Millisecond
	ackStallPoll   = 100 * time.Millisecond
	ackStallGap    = time.Second
	ackStallPerSec = 16
	// hs115: the stall time is relative to the sub — at least ackStallAfter,
	// ackStallGapK x its recent gap between acks (at most ackStallMax) — and
	// then the sub's TCP link must show the dark: data in flight and its
	// bytes_acked frozen since the sub became a candidate, for ackStallRTTK x
	// its srtt (at least ackStallTCPMin) or with an RTO's loss recovery on.
	// A sub with no socket to ask (not a TCP carrier) needs ackStallNoSockK x
	// the stall time.
	ackStallGapK     = 2           // hs115 lab S1: 4 x a ~380 ms gap held the dark subs 1.5 s; Iran's reorder room (96 MiB) was full by then at ~1 Gbit/s
	ackStallRTTK     = 8           // hs119: 4 x srtt (400 ms) closed live subs after one tail loss (hs117 z3/n1: frozen 403/415 ms)
	ackStallTCPMin   = time.Second // hs119 (was 300 ms); hs121: the RTO path too
	ackStallMax      = 2 * time.Second
	ackStallNoSockK  = 3
	ackStallGapLimit = 2 * time.Second // one gap sample counts at most this
)

// ackStallRTONow: an RTO's loss recovery closes without the frozen time
// (hs115..hs119 behaviour). hs121: false; only the older tests set it, to keep
// their timelines.
var ackStallRTONow = false

// linkAck: a TCP_INFO snapshot of the link a sub rides.
type linkAck struct {
	srtt    time.Duration
	acked   uint64 // bytes_acked
	unacked uint32
	rtoLoss bool
}

// subLinkAck: the TCP state of the link s rides (ok=false: unknown; tests
// replace it).
var subLinkAck = subLinkAckSys // sw235: body in mconn_fx212.go

// ackStallTime: how long x must go without ack progress (hs115).
func (x *msub) ackStallTime() time.Duration {
	d := max(ackStallAfter, ackStallGapK*x.ks.gap)
	return min(d, ackStallMax)
}

// ksSub: a sub's ack progress (under m.mu).
type ksSub struct {
	c       uint32        // the peer's consumed count last seen
	at      time.Time     // when c last moved (or nothing was unacked)
	ackedT  time.Time     // write time of the newest kept chunk the peer consumed
	stalled bool          // closed by the rule (not a resend target)
	gap     time.Duration // hs115: smoothed time between ack steps
	seen    bool          // hs115: this stall episode is counted (cand)
	vetoed  bool          // ... and its TCP veto
	tAcked  uint64        // ... the link's bytes_acked when it began
	tAt     time.Time     // ... when it began
	lastW   time.Time     // hs123: the sub last wrote (kept a marked chunk)
}

var ackStallRate struct {
	mu  sync.Mutex
	sec int64
	n   int
}

func ackStallAllow() bool {
	ackStallRate.mu.Lock()
	defer ackStallRate.mu.Unlock()
	s := time.Now().Unix()
	if s != ackStallRate.sec {
		ackStallRate.sec, ackStallRate.n = s, 0
	}
	if ackStallRate.n >= ackStallPerSec {
		return false
	}
	ackStallRate.n++
	return true
}

func (x *msub) ksLive() bool {
	return !x.dead && !x.retired && !x.closeHS && !x.closeAfter && !x.ks.stalled
}

// ackStallTickLocked (from tick, under m.mu): releases acked chunks, updates
// each sub's ack progress and returns the subs to close; d: when to look
// again (0: nothing kept, no need).
func (m *mConn) ackStallTickLocked(now time.Time) (stalled []*msub, d time.Duration) {
	if keepStallOn.Load() && m.stN == 0 && (m.keptBytes > 0 || m.subGapOpenLocked()) && !m.callKeepLocked() && !m.ended && !m.closing {
		// r68fix3: a sub-resend connection's kept chunks (mconn_sub_resend.go):
		// the acked prefix, then the age / link-acked trim and the quiet drop
		m.ksTicking = false
		m.keepStallTrimLocked(nil, now)
		if m.ksSwept {
			m.ksIdle = false
			return nil, 0 // fx212: the shared sweeper polls it
		}
		return nil, m.keepIdleLocked(now)
	}
	if !keepStallOn.Load() || m.stN == 0 || m.ended || m.closing {
		// hs115: Iran (an upload's sender) too — hs113 lab C1's false closes
		// (~0.6 subs/s: the exit's acks for upload subs come in coarser
		// steps) are what the relative time and the TCP check stop.
		m.ksTicking, m.ksIdle = false, false
		return nil, 0
	}
	keepStallStat.ticks.Add(1)
	m.keepStallTrimLocked(nil, now)
	m.keepStallStatsLog(now)
	if !m.ksTrackLocked(now) {
		m.ksTicking = false
		return nil, m.keepIdleLocked(now) // mb174: unmarked chunks too
	}
	if keepStallLazy.Load() && !m.ksSuspectLocked(now) {
		// hs128: nothing looks stalled; the writers look again
		m.ksTicking = false
		keepStallStat.lazyOff.Add(1)
		return nil, m.keepIdleLocked(now) // mb174: a slow poll frees what the peer acks later
	}
	if len(m.subs) < 2 {
		return nil, ackStallPoll
	}
	if now.Before(m.ksNext) {
		return nil, ackStallPoll
	}
	cand := func(x *msub) bool {
		d := x.ackStallTime()
		return x.ksLive() && len(x.sent) > 0 && x.sent[0].marked &&
			now.Sub(x.sent[0].t) >= d && now.Sub(x.ks.at) >= d &&
			x.ksActive(now)
	}
	for _, x := range m.subs {
		if !cand(x) {
			continue
		}
		t0 := x.sent[0].t.Add(ackStallLead)
		proof := false
		for _, y := range m.subs {
			if y != x && y.ksLive() && !cand(y) && y.ks.ackedT.After(t0) {
				proof = true
				break
			}
		}
		if !proof {
			continue
		}
		first := !x.ks.seen
		if first {
			x.ks.seen = true
			keepStallStat.cand.Add(1)
		}
		// hs115: the sub's own link must show the dark
		la, ok := subLinkAck(x)
		if ok {
			if first {
				x.ks.tAcked, x.ks.tAt = la.acked, now
			}
			if la.unacked == 0 || la.acked != x.ks.tAcked {
				// the link moves (or has nothing in flight): slow, not dark;
				// a new window starts
				x.ks.tAcked, x.ks.tAt = la.acked, now
				if !x.ks.vetoed {
					x.ks.vetoed = true
					keepStallStat.vetoTCP.Add(1)
				}
				continue
			}
			// hs121: an RTO's loss recovery no longer closes at once (hs119
			// quiet false closes, all tcp=rto: frozen 200/647 ms on live
			// subs); it needs the same frozen time as the plain path.
			if !(la.rtoLoss && ackStallRTONow) && now.Sub(x.ks.tAt) < max(ackStallTCPMin, ackStallRTTK*la.srtt) {
				continue
			}
		} else if now.Sub(x.ks.at) < ackStallNoSockK*x.ackStallTime() {
			continue
		}
		if !ackStallAllow() {
			continue
		}
		if !ok {
			keepStallStat.noSock.Add(1)
		}
		x.ks.stalled = true
		stalled = append(stalled, x)
		keepStallStat.stalls.Add(1)
		if stripeLog {
			// hs117: which check passed (tcp=rto: RTO loss; tcp=frozen:
			// bytes_acked frozen with data in flight; tcp=nosock: no TCP info)
			why := "nosock"
			if ok {
				why = "frozen"
				if la.rtoLoss {
					why = "rto"
				}
			}
			stripeLogf("stripe %x: sub %d ack stall: oldest kept off=%d %v old, no ack for %v (gap %v); closing, its %d kept chunks move (iran=%v) tcp=%s frozen=%v srtt=%v unacked=%d",
				m.id&0xffffff, x.idx, x.sent[0].c.off, now.Sub(x.sent[0].t).Round(time.Millisecond), now.Sub(x.ks.at).Round(time.Millisecond), x.ks.gap.Round(time.Millisecond), len(x.sent), m.iran,
				why, now.Sub(x.ks.tAt).Round(time.Millisecond), la.srtt.Round(time.Millisecond), la.unacked)
		}
	}
	if len(stalled) > 0 {
		m.ksNext = now.Add(ackStallGap)
	}
	return stalled, ackStallPoll
}

// ackStallTargetLocked (key on): the resend target — the live, not stalled
// sub whose peer acked within ackStallAfter, shortest queue first; nil: the
// old choice.
func (m *mConn) ackStallTargetLocked(now time.Time) *msub {
	if !keepStallOn.Load() {
		return nil
	}
	// the least queued plus unacked in flight: what the receiver must take
	// into its reorder buffer before it reaches the resend
	var best *msub
	var bestB int64
	for _, x := range m.subs {
		if !x.ksLive() || x.gated || x.ks.at.IsZero() || now.Sub(x.ks.at) >= ackStallAfter {
			continue
		}
		b := int64(x.qBytes)
		if w, c, ok := subAckMarks(x); ok && int32(w-c) > 0 {
			b += int64(int32(w - c))
		}
		if best == nil || b < bestB {
			best, bestB = x, b
		}
	}
	if best != nil {
		keepStallStat.targeted.Add(1)
	}
	return best
}

var keepStallLog struct {
	mu sync.Mutex
	at time.Time
}

// keepStallStatsLog: once every 10 s over the process (HS2_MCONN_STRIPE_LOG).
func (m *mConn) keepStallStatsLog(now time.Time) {
	if !stripeLog {
		return
	}
	keepStallLog.mu.Lock()
	if !keepStallLog.at.IsZero() && now.Sub(keepStallLog.at) < 10*time.Second {
		keepStallLog.mu.Unlock()
		return
	}
	keepStallLog.at = now
	keepStallLog.mu.Unlock()
	st := &keepStallStat
	stripeLogf("keepstall: kept_marked=%d kept_unmarked=%d released_by_ack=%d evicted=%d hard=%d gap_room=%d gap_fill=%d hole_sub=%d room_old_peer=%d room_capped=%d room_bytes=%d ack_stalls=%d targeted=%d kept_bytes=%d resends=%d resend_chunks=%d stall_cand=%d stall_veto_tcp=%d stall_nosock=%d ticks=%d lazy=%v lazy_on=%d lazy_off=%d gate_cut=%d gate_band=%d gate_grow=%d gate_n=%d idle_polls=%d idle_drop=%d idle_drop_kb=%d",
		st.marked.Load(), st.unmarked.Load(), st.acked.Load(), st.evicted.Load(), st.hard.Load(), st.gapRoom.Load(), st.gapFill.Load(), st.holeSub.Load(), holeRoomStat.oldPeer.Load(), holeRoomStat.capped.Load(), holeRoomBytes.Load(),
		st.stalls.Load(), st.targeted.Load(), st.kept.Load(), stripeResendStat.resends.Load(), stripeResendStat.chunks.Load(),
		st.cand.Load(), st.vetoTCP.Load(), st.noSock.Load(), st.ticks.Load(), keepStallLazy.Load(), st.lazyOn.Load(), st.lazyOff.Load(),
		stripeDiag.dec[gwCut].Load(), stripeDiag.dec[gwBand].Load(), stripeDiag.dec[gwGrow].Load(), stripeDiag.iDec.Load(),
		keepIdleStat.polls.Load(), keepIdleStat.dropped.Load(), keepIdleStat.dropB.Load()>>10)
}

// gapFillLocked (under r.mu, the reorder buffer and the gap room full; hs113
// lab D2): the hole is old and the chunk ends inside what the buffer already
// spans — take it. The resend's target sub had a window of older data in
// flight in front of the resent hole; every reader was held on the full
// buffer, so the target's reader never reached it (rx stuck for 30 s, gapCheck
// RST). That backlog was written before the data the other subs ran ahead
// with, so it lies inside the span; the span bounds what this adds.
func (r *mrecv) gapFillLocked(now time.Time, end uint64) bool {
	if !keepStallOn.Load() || len(r.ooo) == 0 || now.Sub(r.lastProgress) < gapRoomAfter {
		return false
	}
	l := r.ooo[len(r.ooo)-1]
	if end > l.off+uint64(len(l.b)) {
		return false
	}
	keepStallStat.gapFill.Add(1)
	return true
}

// ---- hs119: per-sub hole room (receiver, key on) ---------------------------
//
// hs117 lab s7/s8: the ack-stall rule resent the hole chunk at the front of
// a live sub's local queue, but that sub had up to its smux window in
// flight ahead of it. The other subs filled the reorder buffer, the gap room
// and the span (gapFill) within 1-2 s, every reader blocked, the target
// sub's backlog was never read and the hole never closed (deadlock, gapCheck
// RST at +30 s).
//
// Now, while the hole is old (gapRoomAfter), every sub may put up to
// holeSubBytes() past every limit for that hole — what a sub can have in
// flight ahead of a resend at the front of its queue (its smux window). So
// the sub carrying the resend always reaches it. Bounded: per sub per hole
// (reset when r.next moves) and holeConnBytes per connection per hole.

const holeConnBytes = 64 << 20

var holeSubOn = true // tests turn it off to show the deadlock it fixes

// holeSubBytes: a sub's smux window plus slack for record framing.
func holeSubBytes() int {
	return min(max(SmuxStreamBuffer, 2<<20)+(256<<10), 32<<20)
}

func (r *mrecv) holeSubRoomLocked(s *msub, now time.Time, n int) bool {
	if !holeSubOn || !keepStallOn.Load() || s == nil || len(r.ooo) == 0 || now.Sub(r.lastProgress) < gapRoomAfter {
		return false
	}
	if r.hxNext != r.next {
		r.hxNext, r.hxBytes = r.next, 0
	}
	if s.hxNext != r.next {
		s.hxNext, s.hxBytes = r.next, 0
	}
	if s.hxBytes+n > holeSubBytes() || r.hxBytes+n > holeConnBytes {
		return false
	}
	s.hxBytes += n
	r.hxBytes += n
	keepStallStat.holeSub.Add(1)
	return true
}

// ackStallActive (hs123): a sub is an ack-stall candidate only while it
// writes — it kept a chunk within this long, or bytes wait in its queue.
// hs121 qu8on2 / hs119 c2: a sub that wrote a little 57 s earlier and whose
// peer's consumed count never moved (smux sends a window update only past
// a threshold) was closed while its shared TCP link sat in a 1.8 s
// retransmit tail on a lossy path: an idle sub is not "dark". A dark sub
// fills its smux window within ~0.3 s and blocks; the rule closes it
// ackStallTime + max(ackStallTCPMin, ackStallRTTK x srtt) later, inside this.
const ackStallActive = 6 * time.Second

func (x *msub) ksActive(now time.Time) bool {
	return x.qBytes > 0 || x.cur != nil || x.ks.seen || !x.ks.lastW.IsZero() && now.Sub(x.ks.lastW) <= ackStallActive
}

// ksTrackLocked (under m.mu): each sub's ack progress (the peer's consumed
// count, when it last moved, the smoothed gap between moves); true: some
// sub keeps chunks.
func (m *mConn) ksTrackLocked(now time.Time) bool {
	kept := false
	for _, x := range m.subs {
		w, c, ok := subAckMarks(x)
		if !ok {
			continue
		}
		if c != x.ks.c && !x.ks.at.IsZero() {
			g := min(now.Sub(x.ks.at), ackStallGapLimit)
			if x.ks.gap == 0 {
				x.ks.gap = g
			} else {
				x.ks.gap = (3*x.ks.gap + g) / 4
			}
		}
		if c != x.ks.c || x.ks.at.IsZero() || int32(w-c) <= 0 {
			x.ks.c, x.ks.at = c, now
			x.ks.seen, x.ks.vetoed = false, false
		}
		if len(x.sent) > 0 {
			kept = true
		}
	}
	return kept
}

// ksSuspectLocked (hs128, lazy): a live sub has data waiting to go (queued
// or in a write), its peer's consumed count has not moved for ackStallLazy
// and its oldest kept chunk is that old: what a dark path looks like in its
// first few hundred ms (the writer blocks on the smux window). A sub with
// only an unacked tail (smux sends window updates past a threshold) and
// nothing to send is not suspect; one the rule already counts (ks.seen)
// stays suspect until its acks move.
func (m *mConn) ksSuspectLocked(now time.Time) bool {
	for _, x := range m.subs {
		if !x.ksLive() || len(x.sent) == 0 || !x.sent[0].marked || x.ks.at.IsZero() {
			continue
		}
		if x.qBytes == 0 && x.cur == nil && !x.ks.seen {
			continue
		}
		if now.Sub(x.ks.at) >= ackStallLazy && now.Sub(x.sent[0].t) >= ackStallLazy {
			return true
		}
	}
	return false
}

// ksLookLocked (hs128, lazy; a writer under m.mu): at most every
// ackStallPoll per connection, track the ack progress and start the poll
// when a sub is suspect.
func (m *mConn) ksLookLocked(now time.Time) {
	if m.ksTicking || m.stN == 0 || now.Sub(m.ksLookAt) < ackStallPoll {
		return
	}
	m.ksLookAt = now
	m.ksTrackLocked(now)
	if m.ksSuspectLocked(now) {
		m.ksTicking = true
		keepStallStat.lazyOn.Add(1)
		m.kickMgr()
	}
}

// ---- hs133: hole rooms only toward a resending peer, and capped -----------
//
// hs132 lab B (rel63 edge, rel61 exit, dark download): the edge's hole rooms
// (gap room, gap fill, hole sub) opened for a hole the old exit never resends
// (its kept chunks were evicted long before its sub died) — gap_fill 136383,
// hole_sub 6241, edge RSS 2672 MB and growing: holeConnBytes is per hole and
// starts again each time r.next moves, gapFill had no byte bound at all.
//
// Now:
//   - a sender with the key on announces capExtMConnKeepStall (ext bit10 of
//     kindInfo); a receiver opens hole rooms only for a sub whose link's peer
//     announced it (an older peer: the rel61 receiver, no extra room);
//   - every byte a receiver holds out of order over its normal bound
//     (max(oooMax, mRecvBuf)) is counted (r.ksX) per connection and over the
//     process (holeRoomBytes); a hole room is refused past holeRoomConnMax
//     for the connection or holeRoomGlobalMax for the process, and the
//     connection falls back to the old behaviour (its readers wait on the
//     hole; gapCheck resets it if the hole never fills).

// capExtMConnKeepStall: ext bit10 of kindInfo (peerinfo.go): this server's
// stripe sender keeps every unacked chunk and resends it when a sub goes
// dark (HS2_MCONN_STRIPE_KEEPSTALL on), so the receiver's hole rooms fill.
const capExtMConnKeepStall = 1 << 10

// holeRoomConnMax / holeRoomGlobalMax: out-of-order bytes over the bound,
// one connection / every connection of the process. HS2_MCONN_HOLE_ROOM_MB /
// HS2_MCONN_HOLE_ROOM_GLOBAL_MB (MiB, lab keys) override them.
var (
	holeRoomConnMax   = envMiB("HS2_MCONN_HOLE_ROOM_MB", 256)
	holeRoomGlobalMax = envMiB("HS2_MCONN_HOLE_ROOM_GLOBAL_MB", 512)
	holeRoomBytes     atomic.Int64
)

func envMiB(k string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(k)); err == nil && n > 0 {
		return n << 20
	}
	return def << 20
}

var holeRoomStat struct {
	oldPeer atomic.Uint64 // hole-room asks refused: the peer does not resend
	capped  atomic.Uint64 // ... refused by holeRoomConnMax / holeRoomGlobalMax
}

// ksPeerOK: s's link's peer announced capExtMConnKeepStall (tests replace it).
var ksPeerOK = ksPeerOKLink

func ksPeerOKLink(s *msub) bool {
	var mtr *linkMeter
	if l, ok := s.key.(Link); ok {
		mtr = linkMeterOf(l)
	} else if e, ok := s.rw.(exitCountedStream); ok && e.c != nil {
		mtr = e.c.mtr // the exit: subs arrive without a key
	}
	if mtr == nil {
		return false
	}
	pi := mtr.peerInfo.Load()
	return pi != nil && pi.Ext&capExtMConnKeepStall != 0
}

// holeRoomOKLocked (under r.mu, the reorder buffer full): a hole room may be
// considered for n more bytes from s.
func (r *mrecv) holeRoomOKLocked(s *msub, now time.Time, n int) bool {
	if !keepStallOn.Load() || s == nil || len(r.ooo) == 0 || now.Sub(r.lastProgress) < gapRoomAfter {
		return false
	}
	if !ksPeerOK(s) {
		holeRoomStat.oldPeer.Add(1)
		return false
	}
	over := r.oooBytes + n - max(r.oooMax, mRecvBuf)
	if over > holeRoomConnMax || holeRoomBytes.Load()-int64(r.ksX)+int64(over) > int64(holeRoomGlobalMax) {
		holeRoomStat.capped.Add(1)
		return false
	}
	return true
}

// ksSyncLocked (under r.mu, after the out-of-order bytes changed): charge
// what lies over the normal bound to holeRoomBytes.
func (r *mrecv) ksSyncLocked() {
	x := max(r.oooBytes-max(r.oooMax, mRecvBuf), 0)
	if x != r.ksX {
		holeRoomBytes.Add(int64(x - r.ksX))
		r.ksX = x
	}
}
