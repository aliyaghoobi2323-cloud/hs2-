package engine

import (
	"fmt"
	"slices"
	"sync/atomic"
	"time"
)

// cs223: the two resends cs221 left (cs221.md §5), S1 (lab2, dlp217 +
// SHAREQ): call stalls of 10.1 s and 6.2 s. The stuck-call rule (Iran,
// lb_call_stuck.go) gave the call's subs 0 and 1 on the blackholed path up at
// 20:46:35 (CLOSE-REQ), but their chunks came back only when the subs died at
// 20:46:43 ("sub- 1 resend 1167 chunks on sub 2", "sub- 0 resend 409 chunks
// on sub 3"): ~8 s in which the only copy of those bytes sat on a dark path,
// then ~1.5 MB at the front of the live sub, much of it already delivered.
//
// HS2_CS223_CALLRESEND (default on; "0" = cs221 behaviour):
//   - give-up resend: a call sub given up (Iran: callStuckMoveLocked, once a
//     fresh sub read; Kharej: CLOSE-REQ read) queues its sequence chunks
//     (kept, queued, in write) at the front of a live sub NOW, without what
//     that sub holds or delivered; the sub records the end of what it gave
//     (gaveEnd, gaveTo) and its death resends only chunks past it while
//     gaveTo lives (gaveTo holds them, delivered them, or its own death
//     resends them);
//   - delivered skip: a resend (death or give-up) onto a live sub b leaves
//     out chunks b was given (offset >= b.missEnd, cs221) and let go with
//     proof the peer read them: not held (fx212SkipHeld), and every chunk b
//     released without that proof is below b.unprovEnd (the writer's unkept
//     writes, kept chunks dropped by age, bound or idle; only the keep-stall
//     ack trim proves). Off while b has an open gap (subGapT) or a bulk or
//     stripe chunk went out (missLost).
var cs223CallResend atomic.Bool

func init() { cs223CallResend.Store(envBoolDefault("HS2_CS223_CALLRESEND", true)) }

// HS2_CS223_DARKRESEND (default on; needs HS2_CS223_CALLRESEND): lab2 D1
// (dlp217, bhA) stalled the call 21 s. Kharej's call subs 0 and 1 were both
// on the blackholed path; the switch moved the primary off sub 0 and
// resent its range, but sub 1 (not primary) held chunks no other sub had,
// and Iran's give-up (CLOSE-REQ) never crossed the dark path: those chunks
// came back only at sub 1's death ~21 s later (Iran rx 0 KB, then 6 MB).
// Now a call sub that goes dark while it is not the primary queues on a
// live sub (the primary when it is not dark) the chunks that sub lacks:
// the give-up resend with its filters (held, switch-resent, delivered).
var cs223DarkResend atomic.Bool

func init() { cs223DarkResend.Store(envBoolDefault("HS2_CS223_DARKRESEND", true)) }

// cs223DarkResendLocked (under m.mu): s (a call sub, not the primary) just
// went dark.
func (m *mConn) cs223DarkResendLocked(s *msub) {
	if !cs223DarkResend.Load() || !cs223CallResend.Load() {
		return
	}
	b := m.primary.Load()
	if b == nil || b == s || b.dc.isDark || b.dead || b.retired || b.closeHS || b.closeAfter || b.gated {
		b = nil
		for _, x := range m.subs {
			if x == s || x.dc.isDark || x.dead || x.retired || x.closeHS || x.closeAfter || x.gated || x.rrAband {
				continue
			}
			if b == nil || x.qBytes < b.qBytes {
				b = x
			}
		}
	}
	if n := m.cs223GiveUpResendLocked(s, b); n > 0 {
		cs223Stat.darkRes.Add(1)
		m.dcLogf("%x cs223 dark resend iran=%v sub %d -> %d chunks %d", m.id, m.iran, s.idx, b.idx, n)
	}
}

var cs223Stat struct {
	darkRes   atomic.Uint64 // dark-sub resends (part of gaveUp)
	gaveUp    atomic.Uint64 // give-up resends done
	gaveC     atomic.Uint64 // chunks queued by them
	gaveB     atomic.Uint64 // bytes queued by them
	skipDel   atomic.Uint64 // resend chunks left out: the target delivered them
	skipDelB  atomic.Uint64
	skipGave  atomic.Uint64 // death resend chunks left out: given at give-up
	skipGaveB atomic.Uint64
}

func cs223LogPart() string {
	s := &cs223Stat
	return fmt.Sprintf(" · cs223 gaveup %d (dark %d, %d chunks %d KB) skipdel %d (%d KB) skipgave %d (%d KB)",
		s.gaveUp.Load(), s.darkRes.Load(), s.gaveC.Load(), s.gaveB.Load()>>10, s.skipDel.Load(), s.skipDelB.Load()>>10,
		s.skipGave.Load(), s.skipGaveB.Load()>>10)
}

// noteUnprov (under m.mu): s let go of the DATA chunk ending at end without
// proof the peer read it.
func (s *msub) noteUnprov(end uint64) {
	if end > s.unprovEnd {
		s.unprovEnd = end
	}
}

// cs223DeliveredFrom (under m.mu): every DATA chunk at or past the returned
// offset that b does not hold any more was read by the peer (^0: none known).
func (m *mConn) cs223DeliveredFrom(b *msub) uint64 {
	if !cs223CallResend.Load() || !cs221Darkcall.Load() || m.missLost || !b.subGapT.IsZero() {
		return ^uint64(0)
	}
	return max(b.missEnd, b.unprovEnd)
}

// cs223SkipDelivered (under m.mu, cs already without what b holds): cs
// without the chunks b delivered.
func (m *mConn) cs223SkipDelivered(cs []*mchunk, b *msub) []*mchunk {
	from := m.cs223DeliveredFrom(b)
	if from == ^uint64(0) && (!cs223CallResend.Load() || b.rsHi == 0) {
		return cs
	}
	out := cs[:0]
	for _, c := range cs {
		if c.kind == mRecData && (c.off >= from || // a FIN is always resent
			cs223CallResend.Load() && c.off >= b.rsLo && c.off+uint64(c.n) <= b.rsHi) { // the darkcall switch resent it onto b
			cs223Stat.skipDel.Add(1)
			cs223Stat.skipDelB.Add(uint64(c.n))
			continue
		}
		out = append(out, c)
	}
	return out
}

// cs223SkipGave (under m.mu, s dying): cs without the chunks s gave at its
// give-up while their target lives.
func cs223SkipGave(cs []*mchunk, s *msub) []*mchunk {
	if !cs223CallResend.Load() || s.gaveTo == nil || s.gaveTo.dead || s.gaveEnd == 0 {
		return cs
	}
	out := cs[:0]
	for _, c := range cs {
		if c.kind == mRecData && c.off+uint64(c.n) <= s.gaveEnd {
			cs223Stat.skipGave.Add(1)
			cs223Stat.skipGaveB.Add(uint64(c.n))
			continue
		}
		out = append(out, c)
	}
	return out
}

// subSeqChunks: s's sequence chunks (kept, queued, in write) in offset
// order, each once.
func subSeqChunks(s *msub) []*mchunk {
	var cs []*mchunk
	for _, r := range s.sent {
		cs = append(cs, r.c)
	}
	for _, c := range s.q {
		if c.seq() {
			cs = append(cs, c)
		}
	}
	if s.cur != nil && s.cur.seq() {
		cs = append(cs, s.cur)
	}
	for _, c := range s.curMore {
		if c.seq() {
			cs = append(cs, c)
		}
	}
	slices.SortFunc(cs, func(a, b *mchunk) int {
		switch {
		case a.off < b.off:
			return -1
		case a.off > b.off:
			return 1
		}
		return 0
	})
	return slices.CompactFunc(cs, func(a, b *mchunk) bool { return a == b })
}

// pushFrontLocked queues cs (offset order) at the front of b, one more copy
// each (under m.mu).
func pushFrontLocked(b *msub, cs []*mchunk) {
	front := make([]*mchunk, 0, len(cs)+len(b.q))
	for _, c := range cs {
		c.refs.Add(1)
		c.copies++
		if e := c.off + uint64(c.n); e > b.txEnd {
			b.txEnd = e
		}
		b.qBytes += c.qsize()
		front = append(front, c)
	}
	b.q = append(front, b.q...)
	b.lastPush = time.Now()
	select {
	case b.kick <- struct{}{}:
	default:
	}
}

// cs223GiveUpResendLocked (under m.mu): s (a call sub) was just given up;
// its sequence chunks go to the front of b now. Returns the chunks queued.
func (m *mConn) cs223GiveUpResendLocked(s, b *msub) int {
	if !cs223CallResend.Load() || !stripeResend.Load() || m.stN != 0 || !m.callKeepLocked() || m.ended {
		return 0
	}
	if b == nil || b == s || b.dead || b.retired || b.closeHS || b.closeAfter || b.gated || s.dead {
		return 0
	}
	all := subSeqChunks(s)
	if len(all) == 0 {
		return 0
	}
	end := uint64(0)
	for _, c := range all {
		end = max(end, c.off+uint64(c.n))
	}
	cs := fx212SkipHeld(all, b)
	cs = m.cs223SkipDelivered(cs, b)
	s.gaveTo, s.gaveEnd = b, max(s.gaveEnd, end)
	if len(cs) == 0 {
		return 0
	}
	pushFrontLocked(b, cs)
	nb := 0
	for _, c := range cs {
		nb += c.n
	}
	cs223Stat.gaveUp.Add(1)
	cs223Stat.gaveC.Add(uint64(len(cs)))
	cs223Stat.gaveB.Add(uint64(nb))
	obs.mconnDupBytes.Add(uint64(nb))
	m.tr.ev("cs223: given-up sub %d resends %d chunks (%d B) on sub %d now", s.idx, len(cs), nb, b.idx)
	if stripeLog {
		stripeLogf("stripe %x: sub %d given up: resend %d chunks on sub %d (iran=%v)", m.id&0xffffff, s.idx, len(cs), b.idx, m.iran)
	}
	m.sendCond.Broadcast()
	return len(cs)
}
