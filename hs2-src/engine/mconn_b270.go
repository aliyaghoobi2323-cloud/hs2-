package engine

import (
	"fmt"
	"sync/atomic"
	"time"
)

// b270 (HS2_B270_TXRES, default on): a call's copies stranded on a
// send-frozen (dc254 txDark) second sub are resent on a live sub.
//
// lab3 ab270 (data-dark scenario, fz.py: Iran->Kharej data >= 128 B dropped
// on one path for 5 s, ACKs and the reverse direction pass): a 2-sub lite
// call stalled 7-10 s in about half the runs on rel69j and on b266 alike.
// Trace (U1, b266 0d9684bf, both sides): Iran's primary sub 0 was briefly
// full (q19, lag 20 KB) while its link wrote slowly, so a few chunks
// (9000 B) went only on sub 1 (sub 0 noted the miss); sub 1's link then
// went data-dark. Kharej kept receiving every later byte on sub 0 (ooo grew
// to 2.2 MB) behind a 9000-byte gap at rcv next, the echo stopped, both
// sides logged "rx stalled", and the gap was filled only when sub 1's TCP
// left its RTO chain after the uncut (~10 s). Iran flagged sub 1 txDark
// ("darkcall txdark ... primary=false") 1.2 s into the stall, but:
//   - cs223's dark resend (its chunks the others lack ride a live sub) runs
//     only on the isDark edge, and a data-dark link never gets isDark: the
//     reverse direction keeps delivering, so "nothing received" never holds;
//   - dc254's txDark acts only on a txDark PRIMARY (a switch).
//
// Now a non-primary call sub that is txDark (and not isDark, which cs223
// already handles) resends, at the txDark edge and again every
// b270TxResEvery while it stays txDark, its sequenced chunks the chosen
// live sub does not hold and has not delivered (cs223GiveUpResendLocked:
// fx212SkipHeld + cs223SkipDelivered), on that live sub (cs223's choice:
// the primary, else the live sub with the least queued; never a dark or
// txDark one). The receiver drops what it already has (mrecv overlap).
// The repeat covers chunks that went on the frozen sub only after the edge
// (b262's darkwait gives up after HS2_B262_WAIT_MS and sends as before).
var b270TxResOn atomic.Bool

func init() { b270TxResOn.Store(envBoolDefault("HS2_B270_TXRES", true)) }

// b270TxResEvery: the repeat interval while a sub stays txDark.
const b270TxResEvery = 300 * time.Millisecond

var b270Stat struct {
	edge   atomic.Uint64 // half-dead edges of non-primary call subs
	rxEdge atomic.Uint64 // ... of them by receive silence only (not txDark)
	res    atomic.Uint64 // resends that queued chunks
	chunks atomic.Uint64 // chunks queued by them
	logged atomic.Int64
}

const b270LogMax = 200

func b270LogPart() string {
	return fmt.Sprintf(" · b270 txres on %v edge %d (rx %d) res %d chunks %d", b270TxResOn.Load(),
		b270Stat.edge.Load(), b270Stat.rxEdge.Load(), b270Stat.res.Load(), b270Stat.chunks.Load())
}

// b270TxResendLocked (darkcall visit of a call, after s was looked at;
// under m.mu): s's chunks the live subs lack go on a live sub while s is a
// non-primary sub whose link is half dead (b270HalfDeadLocked).
func (m *mConn) b270TxResendLocked(s *msub, now time.Time, nowc int64) {
	trig := b270TxResOn.Load() && !s.dc.isDark && s != m.primary.Load() && m.b270HalfDeadLocked(s, now, nowc)
	was := s.b270Trig
	s.b270Trig = trig
	if !trig {
		return
	}
	if was && now.Sub(s.b270ResAt) < b270TxResEvery {
		return
	}
	if !was {
		b270Stat.edge.Add(1)
		if !s.dc.txDark {
			b270Stat.rxEdge.Add(1)
		}
	}
	s.b270ResAt = now
	b := m.b270LiveLocked(s)
	if b == nil {
		return
	}
	n := m.b270ResendLocked(s, b, was, now)
	if n == 0 {
		return
	}
	b270Stat.res.Add(1)
	b270Stat.chunks.Add(uint64(n))
	if b270Stat.logged.Add(1) <= b270LogMax {
		m.dcLogf("%x b270 resend iran=%v sub %d -> %d chunks %d edge %v txdark %v", m.id, m.iran, s.idx, b.idx, n, !was, s.dc.txDark)
	}
}

// b270HalfDeadLocked: s's link sends nothing (dc254 txDark), or receives
// nothing for darkcallAfter while this connection's receive keeps advancing
// (over another sub): the peer's sends on that link are not arriving, so
// the peer->us direction of the link is dead, and with it the peer's view
// of what s carried (lab3 K3: Kharej's copy on the data-dark link's sub
// never reached Iran's stream although Kharej's TCP had it acked; Kharej's
// sub showed rx idle 1-9 s, unacked 0, no T/D flag).
func (m *mConn) b270HalfDeadLocked(s *msub, now time.Time, nowc int64) bool {
	d := &s.dc
	if d.txDark {
		return true
	}
	after := darkcallAfter()
	return d.ok && d.lastRd != 0 && time.Duration(nowc-d.lastRd) >= after &&
		!m.dc.rxAt.IsZero() && now.Sub(m.dc.rxAt) < after/2
}

// b270ResendLocked: cs223GiveUpResendLocked for a txDark sub. At the edge
// every sequenced chunk of s that b neither holds nor delivered goes on b;
// a repeat (rep) takes only chunks ending past what an earlier b270 resend
// of s already covered (s.b270End), so a long freeze does not re-queue the
// same bytes every b270TxResEvery when b's delivery is unknown
// (cs223SkipDelivered keeps everything then).
func (m *mConn) b270ResendLocked(s, b *msub, rep bool, now time.Time) int {
	if !m.callKeepLocked() {
		return 0
	}
	return m.b270ResendKeptLocked(s, b, rep, now)
}

// b270ResendKeptLocked: b270ResendLocked's resend without its call gate (l476
// gates on any kept lite conn).
func (m *mConn) b270ResendKeptLocked(s, b *msub, rep bool, now time.Time) int {
	if !cs223CallResend.Load() || !stripeResend.Load() || m.stN != 0 || m.ended {
		return 0
	}
	all := b270SeqChunks(s, now)
	if len(all) == 0 {
		return 0
	}
	end := uint64(0)
	for _, c := range all {
		end = max(end, c.off+uint64(c.n))
	}
	cs := fx212SkipHeld(all, b)
	cs = m.cs223SkipDelivered(cs, b)
	if rep {
		k := 0
		for _, c := range cs {
			if c.off+uint64(c.n) > s.b270End {
				cs[k] = c
				k++
			}
		}
		cs = cs[:k]
	}
	s.b270End = max(s.b270End, end)
	if len(cs) == 0 {
		return 0
	}
	s.gaveTo, s.gaveEnd = b, max(s.gaveEnd, end) // a later death resend skips these (skipGave)
	pushFrontLocked(b, cs)
	nb := 0
	for _, c := range cs {
		nb += c.n
	}
	obs.mconnDupBytes.Add(uint64(nb))
	m.tr.ev("b270: txDark sub %d resends %d chunks (%d B) on sub %d", s.idx, len(cs), nb, b.idx)
	m.sendCond.Broadcast()
	return len(cs)
}

// b274 (HS2_B270_RECENT_MS, default 600000 = in effect unbounded, b270 as
// on lab3; envIntDefault: 0 = unset). Lab1 TH1 with 3000: 15 edges, 0
// chunks resent, i.e. 3 s neuters b270 there; enable only after a lab3
// data-dark check:
// a resend takes s's sent chunks only when they went out within the last
// b270Recent (plus everything still queued on s). Lab1 one-direction data
// cuts (b274 TG1): the rx-silence edge fired on subs whose OUR direction was
// fine and the edge resend pushed 450-1260 chunks (s's whole sent history,
// cs223SkipDelivered keeps all of it while b's delivery is unknown) in front
// of b's fresh call data; with the cut it marked them given to b (gaveTo),
// and the call stalled >= 1 s in 3/14 cuts. A stranded copy is one sent after
// the link died, i.e. within darkcallAfter + an RTO of the edge.
var b270Recent = time.Duration(envIntDefault("HS2_B270_RECENT_MS", 600000)) * time.Millisecond

// b270SeqChunks: subSeqChunks with s's sent records older than b270Recent
// left out.
func b270SeqChunks(s *msub, now time.Time) []*mchunk {
	if b270Recent <= 0 {
		return subSeqChunks(s)
	}
	old := 0
	for _, r := range s.sent {
		if now.Sub(r.t) > b270Recent {
			old++
		}
	}
	if old == 0 {
		return subSeqChunks(s)
	}
	keep := s.sent
	s.sent = nil
	for _, r := range keep {
		if now.Sub(r.t) <= b270Recent {
			s.sent = append(s.sent, r)
		}
	}
	cs := subSeqChunks(s)
	s.sent = keep
	return cs
}

// b270LiveLocked: the sub to resend s's chunks on: the primary when it is
// live, else the live sub with the least queued (nil: none).
func (m *mConn) b270LiveLocked(s *msub) *msub {
	ok := func(x *msub) bool {
		return x != nil && x != s && !x.dc.isDark && !x.dc.txDark && !x.dead && !x.retired &&
			!x.closeHS && !x.closeAfter && !x.gated && !x.rrAband
	}
	if p := m.primary.Load(); ok(p) {
		return p
	}
	var b *msub
	for _, x := range m.subs {
		if ok(x) && (b == nil || x.qBytes < b.qBytes) {
			b = x
		}
	}
	return b
}
