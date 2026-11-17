package engine

import (
	"log"
	"sync/atomic"
	"time"
)

// r68fix2: HS2_MCONN_SUB_RESEND ("1" = on, default off; each side alone) —
// every light connection, not only a striped download (HS2_MCONN_STRIPE_RESEND)
// or a call (HS2_MCONN_CALL_RESEND), keeps what its subs wrote until the
// peer's mconn reader took it (the KEEPSTALL smux ack marks), and a sub that
// dies with another live sub left has its unacked chunks queued again on that
// sub instead of the connection being reset ("a sub died holding the only
// copy of some bytes", errMConnLost).
//
// Why (rel68 soak, test downloads failing right at the 30 % RST onset on
// port 2096: "unable to send control message", a 0-byte run): iperf3's
// control connection is short-lived and quiet; its second sub joins with a
// hole (holeRecent), so when the RST kills the first sub's link that sub is
// the only one trusted with the bytes in flight — the old rule resets the
// connection mid-test. The bytes are still on the sender until the peer's
// reader acked them: sending them again on the live sub (the receiver drops
// what it already has, by offset) keeps the connection.
//
// Memory: chunks go once the peer acked them (KEEPSTALL marks), so a
// connection keeps about what is in flight on its subs; above subKeepConnMax
// per connection the oldest go (that connection is back to the old rule for
// them), and no new chunk is kept while every connection together keeps more
// than subKeepGlobalMax (stripes and calls are not affected by that cap).
var mconnSubResend atomic.Bool

// rel68c: read in loadEnvKeys (defaults22.go); the envdef table has it on for
// both sides, HS2_MCONN_SUB_RESEND=0 turns it off.

// r68fix3 (B1/B2 of r68fix2: Kharej RSS +130 MB, the 48 MiB global cap hit
// all along — a heavy download kept its whole unacked smux window — and one
// call stalled ~20 s in a blackhole: with no ack-stall target for a
// non-striped connection the resend went to the shortest queue, a dark sub):
//
//   - scope: only a connection that has sent at most subResendScope bytes so
//     far in this direction keeps its chunks (HS2_MCONN_SUB_RESEND_SCOPE_KB,
//     default 256): control, handshake and request connections. A larger
//     transfer is the stripe's (STRIPE_RESEND / KEEPSTALL) or not protected;
//     once over the scope a connection keeps nothing new (what it kept goes
//     as the peer acks it) and the old rule applies to it again.
//   - target: a dead sub's chunks go only to a sub that read something within
//     subResendFresh (its own record, or any byte on its link) on another link
//     than the dead one, and whose link is not in an RTO loss; none: the old
//     rule (a fast reset beats a long stall on a dark path). A call takes such
//     a sub first and falls back to the r68fix choice.
//   - no partial resend: a connection that wrote a chunk it did not keep (the
//     global cap) or dropped a kept chunk the peer may not have (the conn cap)
//     is marked (subGap) and keeps the old rule — a resend with a hole in it
//     would stall the connection until the receiver's gap timeout (30 s) —
//     and keeps nothing new.
//   - age: a small connection's consumed count often never moves (smux
//     reports it past half the stream window), so its chunks stayed kept
//     until it went quiet for 30 s (r68fix3 C1 first try: 3000 crowd
//     connections held the 24 MiB cap within a minute). A sub's chunks older
//     than subKeepAge go while its link is fresh (it read within
//     subResendFresh; on the 1 s tick: older than 4 srtt too and the link
//     not in an RTO loss, TCP_INFO): TCP has delivered them.
//
// r68fix4 (r68fix3 lab C1/C2: Iran ~1 MB kept, but the exit sat at 21–22 MB
// of the 24 MiB cap — ~3000 crowd connections x ~7 KB, about 1 s of echo
// replies — and every refused write marked its connection gapped for good:
// ~1000 gapped connections per segment, 31–36 lost-copy resets on the exit):
//
//   - age: max(subKeepAgeMin, subKeepAgeK x srtt) of the sub's link (its
//     srtt from the last 1 s tick; unknown: subKeepAge). A chunk that old on
//     a link that read recently and is not in an RTO loss is delivered — at
//     the lab's ~100 ms RTT that is ~300 ms of replies kept instead of 1 s.
//   - gap per sub, and it heals: a chunk written (or dropped) without a kept
//     copy marks only its sub (subGapT: the newest such chunk's write time);
//     the gap closes by the same age rule a kept chunk of that time would go
//     by (TCP has delivered it). Only a dead sub with an open gap keeps the
//     old rule; another sub's death is still resent. A refused write no
//     longer stops a connection from keeping for the rest of its life.
//   - global cap 32 MiB (was 24).
var subResendScope = uint64(envIntDef("HS2_MCONN_SUB_RESEND_SCOPE_KB", 256)) << 10

const (
	// subKeepConnMax: kept bytes per sub-resend-only connection (at most the
	// scope anyway; kept as a guard).
	subKeepConnMax = 512 << 10
	// subResendFresh: a resend target must have read within this.
	subResendFresh = time.Second
	// subKeepAge: a kept chunk this old on a fresh link is delivered (the
	// link's srtt unknown).
	subKeepAge = time.Second
	// r68fix4: with the link's srtt known, max(subKeepAgeMin, subKeepAgeK x srtt).
	subKeepAgeMin = 300 * time.Millisecond
	subKeepAgeK   = 3
)

// subKeepGlobalMax: no new keep for a sub-resend-only connection while those
// connections together keep more (a var: tests lower it).
var subKeepGlobalMax int64 = 32 << 20

var subResendStat struct {
	resends  atomic.Uint64 // sub deaths a connection survived by the resend
	capped   atomic.Uint64 // writes not kept: over subKeepGlobalMax
	evicted  atomic.Uint64 // kept chunks dropped over subKeepConnMax
	noTarget atomic.Uint64 // r68fix3: no fresh target: the old rule
	scoped   atomic.Uint64 // r68fix3: connections that went over the scope
	kept     atomic.Int64  // r68fix3: bytes kept by sub-resend-only connections now
	keptMax  atomic.Int64  // ... the most since the last log line
	gapped   atomic.Uint64 // r68fix4: sub gaps opened (a chunk without a kept copy)
	healed   atomic.Uint64 // r68fix4: sub gaps closed (that chunk delivered)
	aged     atomic.Uint64 // r68fix3: kept chunks dropped as delivered (age / link acked)
	noFull   atomic.Uint64 // r68fix3: sub deaths with no resend: the dead sub had an open gap
	ageGap   atomic.Uint64 // fx212: age trims without TCP proof that opened a gap
	// b271 Q5: of the noTarget deaths, those where the data was actually
	// lost (the conn ended by the old rule: a lost-copy reset or every sub
	// died) vs survived (another copy, or an rr155/rr156 rescue took over)
	noTgtLost atomic.Uint64
	noTgtSurv atomic.Uint64
	// keptHist: per write kept by a sub-resend-only connection, its kept
	// bytes then: < 4K, < 16K, < 64K, < 256K, more
	keptHist [5]atomic.Uint64
}

// subKeepLocked (under m.mu): m is a light, non-striped connection that keeps
// its chunks for a resend under HS2_MCONN_SUB_RESEND (a call has its own key)
// and has not gone over the scope.
func (m *mConn) subKeepLocked() bool {
	if !(mconnSubResend.Load() && stripeResend.Load() && keepStallOn.Load() &&
		m.stN == 0 && m.mode == planeLight && !m.handshaking) {
		return false
	}
	if m.sendOff > subResendScope {
		if !m.subScoped {
			m.subScoped = true
			subResendStat.scoped.Add(1)
		}
		return false
	}
	return true
}

// subOnlyLocked: m keeps chunks only for the sub resend (not a stripe or a call).
func (m *mConn) subOnlyLocked() bool {
	return m.stN == 0 && !m.callKeepLocked() && m.subKeepLocked()
}

// anyKeepLocked: a non-striped connection whose chunks are kept (call or sub resend).
func (m *mConn) anyKeepLocked() bool {
	return m.callKeepLocked() || m.subKeepLocked()
}

// subKeepRoomLocked: a write on s of a sub-resend-only connection may keep
// its chunks (the global cap); refused: s has a gap from now (it heals).
func (m *mConn) subKeepRoomLocked(s *msub) bool {
	if m.stN > 0 || m.callKeepLocked() {
		return true
	}
	if subResendStat.kept.Load() > subKeepGlobalMax {
		subResendStat.capped.Add(1)
		subGapLocked(s, time.Now())
		m.keepIdleArmLocked() // r68fix4: a tick heals the gap
		return false
	}
	return true
}

// subGapLocked (under m.mu): s wrote or dropped a chunk written at t that the
// peer may not have without a kept copy: no sub resend from s until it is
// delivered (subAgeTrimLocked heals it).
func subGapLocked(s *msub, t time.Time) {
	if s.subGapT.IsZero() {
		subResendStat.gapped.Add(1)
	}
	if t.After(s.subGapT) {
		s.subGapT = t
	}
	if s.m != nil && s.m.ksSwept {
		s.m.noteSwKeptLocked() // sw235: the sweeper heals it
	}
}

// subGapOpenLocked: a live sub of m has an open gap.
func (m *mConn) subGapOpenLocked() bool {
	for _, x := range m.subs {
		if !x.subGapT.IsZero() && !x.dead {
			return true
		}
	}
	return false
}

// subKeepAgeOf: how old a chunk on x must be to count as delivered (its link
// fresh and not in an RTO loss).
func subKeepAgeOf(x *msub) time.Duration {
	if x.subSrtt <= 0 {
		return subKeepAge
	}
	return max(subKeepAgeMin, subKeepAgeK*x.subSrtt)
}

// subAgeTrimLocked (a sub-resend-only connection, under m.mu): drop what is
// delivered — chunks older than subKeepAgeOf on a sub that read recently; on
// the 1 s tick (sys: a TCP_INFO call per sub, which refreshes the srtt) only
// while the link is not in an RTO loss (a dark link is in one within a few
// RTTs). Not "nothing unacked": a chunk may still sit in smux's send queue.
// r68fix4: a sub's gap heals by the same rule.
func (m *mConn) subAgeTrimLocked(now time.Time, sys bool) {
	m.subAgeTrimLookLocked(now, sys, subLinkAck)
}

// subAgeTrimLookLocked: subAgeTrimLocked with the link evidence from look.
func (m *mConn) subAgeTrimLookLocked(now time.Time, sys bool, look func(*msub) (linkAck, bool)) {
	for _, x := range m.subs {
		if x.dead || len(x.sent) == 0 && x.subGapT.IsZero() {
			continue
		}
		age := subKeepAgeOf(x)
		oldest := x.subGapT
		if len(x.sent) > 0 && (oldest.IsZero() || x.sent[0].t.Before(oldest)) {
			oldest = x.sent[0].t
		}
		if now.Sub(oldest) <= age && !sys {
			continue
		}
		if sys {
			la, ok := look(x)
			if ok && la.srtt > 0 {
				x.subSrtt = la.srtt
				age = subKeepAgeOf(x)
			}
			switch {
			case ok && la.rtoLoss:
				continue // dark or lossy: what is unacked may be lost
			case ok:
				// no RTO loss for a chunk this old: TCP delivered it
			case now.Sub(monoTime(x.lastRecv.Load())) > subResendFresh:
				continue
			}
		} else if now.Sub(monoTime(x.lastRecv.Load())) > subResendFresh {
			continue
		}
		k := 0
		for k < len(x.sent) && now.Sub(x.sent[k].t) > age {
			k++
		}
		if k > 0 {
			if !sys && fx212AgeGap && x.sent[k-1].sub {
				// fx212 (audit211 M2): dropped by age with no TCP proof (the
				// bytes reached the kernel, maybe not the peer): a gap until
				// the sweeper's TCP look heals it, so a death meanwhile takes
				// the old rule (a fast reset) instead of a resend with a hole
				subGapLocked(x, x.sent[k-1].t)
				subResendStat.ageGap.Add(1)
			}
			subResendStat.aged.Add(uint64(k))
			m.dropSentLocked(x, k)
		}
		if !x.subGapT.IsZero() && now.Sub(x.subGapT) > age && (sys || !fx212AgeGap) { // fx212 (M2): heals only on TCP evidence
			x.subGapT = time.Time{}
			subResendStat.healed.Add(1)
		}
	}
}

// subKeptAdd: the sub-resend-only kept gauge (and its max).
func subKeptAdd(n int64) {
	v := subResendStat.kept.Add(n)
	if fx212SubResend.Load() {
		return // fx212: the max is noted per sweeper tick (subKeptMaxNote), no CAS loop per write
	}
	subKeptMaxNote(v)
}

// subKeptMaxNote: keptMax = max(keptMax, v).
func subKeptMaxNote(v int64) {
	for {
		mx := subResendStat.keptMax.Load()
		if v <= mx || subResendStat.keptMax.CompareAndSwap(mx, v) {
			return
		}
	}
}

// subKeepBoundLocked (after a keep on a sub-resend-only connection): the
// oldest kept chunks go over subKeepConnMax.
func (m *mConn) subKeepBoundLocked() { m.subKeepBoundAtLocked(time.Now()) }

// subKeepBoundAtLocked: subKeepBoundLocked with the writer's clock (sw235).
func (m *mConn) subKeepBoundAtLocked(now time.Time) {
	if m.stN > 0 || m.callKeepLocked() || m.keptBytes == 0 {
		return
	}
	m.subAgeTrimLocked(now, false)
	for m.keptBytes > subKeepConnMax {
		var o *msub
		for _, x := range m.subs {
			if len(x.sent) > 0 && (o == nil || x.sent[0].t.Before(o.sent[0].t)) {
				o = x
			}
		}
		if o == nil {
			return
		}
		subResendStat.evicted.Add(1)
		if o.sent[0].sub {
			subGapLocked(o, o.sent[0].t) // r68fix4: o has a gap until that chunk is delivered
		}
		m.dropSentLocked(o, 1)
	}
	i := 4
	switch k := m.keptBytes; {
	case k < 4<<10:
		i = 0
	case k < 16<<10:
		i = 1
	case k < 64<<10:
		i = 2
	case k < 256<<10:
		i = 3
	}
	subResendStat.keptHist[i].Add(1)
	subResendLog()
}

// subLinkMeter: the link meter of the link s rides (nil: unknown).
func subLinkMeter(s *msub) *linkMeter {
	switch r := s.rw.(type) {
	case *mconnSub:
		if cs, ok := r.stream.(*countedStream); ok && cs.link != nil {
			return cs.link.mtr
		}
	case exitCountedStream:
		if r.c != nil {
			return r.c.mtr
		}
	}
	return nil
}

// subLinkKey: what identifies the link s rides (nil: unknown; tests replace it).
var subLinkKey = func(s *msub) any {
	if mtr := subLinkMeter(s); mtr != nil {
		return mtr
	}
	if tc := subTCP(s); tc != nil {
		return tc
	}
	return nil
}

// subFreshLocked: x read a record of its connection within subResendFresh,
// or its link read any byte that recently (the RESCUE meters, when on), and
// its link is not in an RTO loss.
func subFreshLocked(x *msub, now time.Time) bool {
	fresh := now.Sub(monoTime(x.lastRecv.Load())) <= subResendFresh
	if !fresh && rescueOn.Load() {
		if mtr := subLinkMeter(x); mtr != nil {
			if rd := mtr.lastRd.Load(); rd != 0 && time.Duration(ctrlNow()-rd) <= subResendFresh {
				fresh = true
			}
		}
	}
	if !fresh {
		return false
	}
	if la, ok := subLinkAck(x); ok && la.rtoLoss {
		return false
	}
	return true
}

// subResendTargetLocked (r68fix3): the least queued live sub that is fresh
// (subFreshLocked) on another link than dead's; nil: none.
func (m *mConn) subResendTargetLocked(dead *msub) *msub {
	now := time.Now()
	dk := subLinkKey(dead)
	var best *msub
	for _, x := range m.subs {
		if x == dead || x.dead || x.retired || x.closeHS || x.closeAfter || x.gated {
			continue
		}
		if dk != nil && subLinkKey(x) == dk {
			continue
		}
		if !subFreshLocked(x, now) {
			continue
		}
		if best == nil || x.qBytes < best.qBytes {
			best = x
		}
	}
	return best
}

// subNoTargetOutcome (subGone, under m.mu): a sub that died with no fresh
// resend target; endErr is the old rule's verdict (nil: the conn goes on).
func subNoTargetOutcome(endErr error) {
	if endErr == errMConnLost || endErr == errMConnSubsDied {
		subResendStat.noTgtLost.Add(1)
	} else {
		subResendStat.noTgtSurv.Add(1)
	}
}

// subLostResets: connections reset by the old rule (a sub died holding the
// only copy of some bytes, errMConnLost) — what the resend should avoid.
var subLostResets atomic.Uint64

var subResendLogAt atomic.Int64

// subResendLog: at most every 10 s, when a resend or a lost-copy reset
// happened, one process line (both keys' counters, totals).
func subResendLog() {
	now := monoNano(time.Now())
	last := subResendLogAt.Load()
	if last != 0 && now-last < int64(10*time.Second) || !subResendLogAt.CompareAndSwap(last, now) {
		return
	}
	h := &subResendStat.keptHist
	log.Printf("mconn sub resend (HS2_MCONN_SUB_RESEND=%v scope=%dK): %d sub deaths survived by a resend · %d call resends · %d lost-copy resets · %d no fresh target (lost %d survived %d) · %d writes not kept (global cap) · %d kept chunks evicted (conn cap) · %d conns over scope · %d sub gaps opened · %d healed · %d deaths with a gap · %d chunks aged out · sub kept now %d KB max %d KB · all kept %d KB · conn kept hist <4K %d <16K %d <64K %d <256K %d more %d · fx212 swept %d (visits %d) dup-skipped %d agegap %d",
		mconnSubResend.Load(), subResendScope>>10, subResendStat.resends.Load(), callResendStat.Load(), subLostResets.Load(),
		subResendStat.noTarget.Load(), subResendStat.noTgtLost.Load(), subResendStat.noTgtSurv.Load(), subResendStat.capped.Load(), subResendStat.evicted.Load(), subResendStat.scoped.Load(),
		subResendStat.gapped.Load(), subResendStat.healed.Load(), subResendStat.noFull.Load(), subResendStat.aged.Load(),
		subResendStat.kept.Load()>>10, subResendStat.keptMax.Swap(subResendStat.kept.Load())>>10, keepStallStat.kept.Load()>>10,
		h[0].Load(), h[1].Load(), h[2].Load(), h[3].Load(), h[4].Load(),
		fx212Stat.sweepConn.Load(), fx212Stat.sweeps.Load(), fx212Stat.skipped.Load(), subResendStat.ageGap.Load())
}
