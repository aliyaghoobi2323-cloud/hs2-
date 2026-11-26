package engine

import (
	"errors"
	"log"
	"sync/atomic"
	"time"
)

// b449: a lite connection's one-copy primary that stops writing.
//
// Lab b446 (lab2 Iran <-> lab4 Kharej, HS2_MCONN_TRACE): whenever the Iran
// pool shrank (a link-retire wave) a heavy download's stripe subs ended (EOF)
// and the stripe width fell to 0; the one-copy path (dupOff) then rides only
// the primary, and the primary's stream had stopped: its queue sat at ~1.6 MB
// with nothing written (kernel unacked=0 notsent=0) while Write waited at the
// full queue. In lite the primary is never switched (maybeSwitchPrimaryLocked
// is not lite's), so the fresh sub Iran added every ~11 s never got a byte,
// was closed as idle after mLiteIdle, and the connection hung until Iran
// reset it 63-87 s later; the client never saw EOF.
//
// Now, both sides, lite and one copy only: the manage loop watches the
// primary's writer while its queue is not empty. No dequeue for b449StuckFor
// while another usable sub exists ends the primary like a dead link
// (errB449Stuck: close and subGone, as the ack-stall rule does): subGone's
// resend (stripeResendLocked) queues every chunk it wrote recently, holds or
// is writing on a live sub, the receiver drops what it already has by
// offset, and the primary moves to the live sub. A connection whose chunks
// are not kept (no stripe, call or sub resend in scope) is moved only after
// b449HardFor: subGone then rescues or resets it (an end the client sees)
// instead of a hang. Iran, with no other sub, opens one on another link
// first (Kharej cannot open subs; Iran's fresh sub is used at once).
//
// A primary that writes is never touched: the rule needs zero dequeues for
// the whole window. HS2_B449_PSTUCK=0 turns it off.

var (
	b449On       atomic.Bool
	b449StuckFor = time.Duration(envIntDef("HS2_B449_PSTUCK_MS", 1500)) * time.Millisecond
	b449HardFor  = time.Duration(envIntDef("HS2_B449_PSTUCK_HARD_MS", 8000)) * time.Millisecond
	b449Poll     = 250 * time.Millisecond

	b449Stat struct {
		moves     atomic.Uint64 // stuck primaries ended with a live sub to take over
		hard      atomic.Uint64 // ... of which without kept chunks (after b449HardFor)
		opens     atomic.Uint64 // Iran: replacement subs opened for a stuck primary
		noTarget  atomic.Uint64 // stuck past the window with no other sub (Kharej waits for Iran's)
		darkOpens atomic.Uint64 // Iran: subs opened as every live sub's link went dark (b449DarkOpenLocked)
		closeLate atomic.Uint64 // b449b: stuck primaries closed after b449CloseWait with alt not yet written
		// Kharej stall-to-recovery of the no-other-sub cases: from the
		// primary's last dequeue to its move (a case that ends otherwise,
		// the conn ending or the primary writing again, is not counted)
		ntN, ntMs, ntMax, ntOver2 atomic.Uint64
	}
)

var errB449Stuck = errors.New("mconn: lite primary stuck")

func init() { b449On.Store(envBoolDefault("HS2_B449_PSTUCK", true)) }

// b449ArmLocked (under m.mu): a one-copy Write waits on a full primary; the
// manage loop starts watching it.
func (m *mConn) b449ArmLocked() {
	if m.lite && !m.b449Arm && b449On.Load() {
		m.b449Arm = true
		m.kickMgr()
	}
}

// b449TickLocked (under m.mu, from tick): the stuck-primary rule. It returns
// when to look again (0: disarmed), the sub to end (nil: none) and the sub
// that takes over.
func (m *mConn) b449TickLocked(now time.Time) (time.Duration, *msub, *msub) {
	if !m.b449Arm {
		return 0, nil, nil
	}
	p := m.primary.Load()
	if !b449On.Load() || !m.lite || !m.dupOff || m.mode != planeLight || m.ended || m.handshaking ||
		m.rawR.Load() || m.rawW.Load() || p == nil || p.dead || p.retired || p.gated || p.closeHS || p.queueLocked() == 0 {
		m.b449Arm, m.b449P = false, nil
		return 0, nil, nil
	}
	if p.b449Kill {
		return b449Poll, nil, nil // ending (subGone runs): the next primary is watched then
	}
	if p != m.b449P || p.b449N != m.b449Cnt {
		m.b449P, m.b449Cnt, m.b449At = p, p.b449N, now
		return b449Poll, nil, nil
	}
	keep := m.stN > 0 || m.callKeepLocked() || (m.subKeepLocked() && p.subGapT.IsZero())
	lim := b449StuckFor
	if !keep {
		lim = b449HardFor
	}
	stuck := now.Sub(m.b449At)
	if stuck < lim {
		return b449Poll, nil, nil
	}
	var anyAlt *msub
	live := 0
	for _, s := range m.subs {
		if s != p && !s.dead && !s.retired && !s.closeHS && !s.closeAfter && !s.gated && !s.bulk.Load() && !s.b449Kill {
			live++
			if anyAlt == nil {
				anyAlt = s
			}
		}
	}
	alt := anyAlt
	if keep && m.stN == 0 && !m.callKeepLocked() {
		// the sub resend moves the bytes only to a fresh sub on another
		// link (subResendTargetLocked); without one it would reset the
		// connection: wait for one (Iran opens it), the hard rule after
		alt = m.subResendTargetLocked(p)
		if alt == nil && stuck >= b449HardFor {
			alt, keep = anyAlt, false
		}
	}
	if alt == nil {
		if m.iran && live < 2 && !m.lightOpening && !m.stOpening && !now.Before(m.lightRetryAt) {
			m.lightOpening = true
			b449Stat.opens.Add(1)
			m.tr.ev("b449: primary sub %d stuck %v with %d queued, no usable sub: open one", p.idx, stuck.Round(time.Millisecond), p.queueLocked())
			avoid := make([]any, 0, 1+len(m.deadKeys))
			avoid = append(avoid, avoidKey(p.key))
			avoid = append(avoid, m.deadAvoidLocked()...)
			go m.openLight(avoid)
		} else if !m.b449NoTgt {
			m.b449NoTgt = true
			b449Stat.noTarget.Add(1)
		}
		return b449Poll, nil, nil
	}
	p.b449Kill = true
	if m.b449NoTgt {
		ms := uint64(stuck / time.Millisecond)
		b449Stat.ntN.Add(1)
		b449Stat.ntMs.Add(ms)
		if ms > 2000 {
			b449Stat.ntOver2.Add(1)
		}
		for o := b449Stat.ntMax.Load(); ms > o && !b449Stat.ntMax.CompareAndSwap(o, ms); o = b449Stat.ntMax.Load() {
		}
	}
	m.b449P, m.b449NoTgt = nil, false
	b449Stat.moves.Add(1)
	if !keep {
		b449Stat.hard.Add(1)
	}
	b449Log()
	m.tr.ev("b449: primary sub %d stuck %v with %d queued (keep=%v): ended, sub %d takes over", p.idx, stuck.Round(time.Millisecond), p.queueLocked(), keep, alt.idx)
	return b449Poll, p, alt
}

// b449b: the lab flake of TestB449ExitPrimaryStuckMoves (-race under load,
// 3 of ~30): the stuck primary's stream was closed at the move, so Iran
// could read its EOF before the first resent byte reached the sub taking
// over. That sub had not read for a while (one-copy download: the exit sent
// nothing on it), so Iran's own resend found no fresh target, and with that
// sub joined (or missing a copy) < mHoleSafe ago Iran's old lost-copy rule
// reset the connection: a reset caused by the move although no byte was
// lost. Now the resend runs first and the stream is closed only once alt
// has written a chunk after the move (Iran's sub is fresh by then), or
// alt / the connection ended, at most b449CloseWait later. The stuck
// writer stays blocked until then; nothing new is queued on the dead sub.
var b449CloseWait = 1500 * time.Millisecond

func (m *mConn) b449CloseAfter(x, alt *msub) {
	defer x.close()
	if alt == nil {
		return
	}
	m.mu.Lock()
	n0 := alt.b449N
	m.mu.Unlock()
	t0 := time.Now()
	for time.Since(t0) < b449CloseWait {
		select {
		case <-m.GetDieCh():
			return
		case <-time.After(20 * time.Millisecond):
		}
		m.mu.Lock()
		n := alt.b449N - n0
		done := alt.dead || m.ended || n >= 2 || n >= 1 && len(alt.q) == 0 && alt.cur == nil
		m.mu.Unlock()
		if done {
			return
		}
	}
	b449Stat.closeLate.Add(1)
	b449Log()
}

var b449LogAt atomic.Int64

// b449Log: at most every 10 s after a move, one process line (totals).
func b449Log() {
	now := monoNano(time.Now())
	last := b449LogAt.Load()
	if last != 0 && now-last < int64(10*time.Second) || !b449LogAt.CompareAndSwap(last, now) {
		return
	}
	n := b449Stat.ntN.Load()
	log.Printf("mconn b449 lite stuck primary: %d moved (%d without kept chunks) · %d replacement opens · %d no other sub · %d dark opens · no-other-sub recovered %d avg %d ms max %d ms >2s %d · %d closeLate · keep floor %dK: %d age trims held %d over-cap keeps · %s",
		b449Stat.moves.Load(), b449Stat.hard.Load(), b449Stat.opens.Load(), b449Stat.noTarget.Load(), b449Stat.darkOpens.Load(),
		n, b449Stat.ntMs.Load()/max(n, 1), b449Stat.ntMax.Load(), b449Stat.ntOver2.Load(),
		b449Stat.closeLate.Load(), b449KeepFloor.Load()>>10, b449FloorStat.ageHeld.Load(), b449FloorStat.capHeld.Load(), b449TwLogPart())
}

// b449 dark open (Iran, HS2_B449_DARKOPEN, default on with HS2_B449_PSTUCK):
// Kharej cannot ask for a sub when its stuck primary is its only one: a
// record would wait behind the same dead link (the WAN proof: 9-13 stuck
// primaries a run with no other sub waited up to ~11 s for Iran's next
// stripe open). Iran sees the same thing from its side with no message: the
// connection's receive went silent and every live sub rides a link that is
// silent while pending (cs226Silent: nothing heard for darkcallAfter while
// a write of ours waits for an answer, so an idle link or a paused origin is
// not dark). Then Iran opens a light sub on another link at once; Kharej's
// stuck rule moves the primary there (a fresh sub on another link). cs226
// does not cover it: it visits only conns with Iran's own bytes in flight,
// and a download's sender is Kharej. No wire change, so any peer pairs.
var (
	b449DarkOn     atomic.Bool
	b449DarkRecent = 10 * time.Second       // the conn received within this before the silence
	b449DarkFast   = 250 * time.Millisecond // poll while a silence may turn dark (a link waits for an answer)
)

func init() { b449DarkOn.Store(envBoolDefault("HS2_B449_DARKOPEN", true)) }

var b449DarkSilent = func(s *msub) bool { return cs226Silent(s, ctrlNow()) }

// b449DarkOpenLocked (Iran lite, under m.mu, from tick): live are the
// connection's live subs. It returns when to look again (0: no need).
func (m *mConn) b449DarkOpenLocked(live []*msub, now time.Time) time.Duration {
	if !b449On.Load() || !b449DarkOn.Load() || !m.iran || !m.lite || m.mode != planeLight || m.ended || m.closing ||
		m.handshaking || m.peerFin || len(live) == 0 || len(live) > 3 {
		return 0
	}
	// l478 HS2_L478_FASTOPEN (mconn_l478_fastopen.go): non-call lite only
	fast := l478FastOpen.Load() && m.callLikeAt.IsZero() && !m.dcIsCallLocked(now)
	if len(live) > 2 && !fast {
		return 0
	}
	var last int64
	n := 0
	for _, s := range live {
		if s.dead {
			continue
		}
		if s.closeHS || s.closeAfter || s.gated {
			return 0
		}
		n++
		last = max(last, s.lastRecv.Load())
	}
	if n == 0 || last == 0 {
		return 0
	}
	sil := now.Sub(monoTime(last))
	minSil := darkcallAfter() / 2
	if fast {
		minSil = l478SilMin / 2
	}
	if sil < minSil || sil > b449DarkRecent {
		return 0
	}
	// poll fast only while a link waits for an answer (a lite conn idle
	// after a reply, the common case at scale, is not woken 4x a second)
	nowc, pend := ctrlNow(), false
	dark := true
	for _, s := range live {
		if s.dead {
			continue
		}
		if b449DarkSilent(s) || fast && l478Silent(s, nowc) {
			pend = true
		} else {
			dark = false
			pend = pend || cs228Pending(s, nowc)
		}
	}
	if !dark {
		if pend {
			return b449DarkFast
		}
		return 0
	}
	second := false
	if m.b449DarkRx == last {
		// one open per silence (an older exit never moves to it); l478: one
		// more once the fresh sub's own link is silent too (dark above)
		if !fast || m.l478N >= l478MaxOpens {
			return 0
		}
		second = true
	} else {
		m.l478N = 0
	}
	if m.lightOpening || m.stOpening || now.Before(m.lightRetryAt) {
		return b449DarkFast
	}
	if fast && !l478OpenOK() {
		return b449DarkFast
	}
	m.b449DarkRx = last
	if fast {
		m.l478N++
		l478Stat.opens.Add(1)
		if second {
			l478Stat.second.Add(1)
		}
	}
	m.lightOpening = true
	b449Stat.darkOpens.Add(1)
	m.tr.ev("b449: receive silent %v, every sub's link dark: open one", sil.Round(time.Millisecond))
	avoid := make([]any, 0, len(live)+len(m.deadKeys))
	for _, s := range live {
		avoid = append(avoid, avoidKey(s.key))
	}
	avoid = append(avoid, m.deadAvoidLocked()...)
	if fast {
		avoid = l480WithLive(avoid) // l480 LIVEPICK (mconn_l480_stick.go)
		go func() { m.openLight(avoid); l478OpenDone() }()
	} else {
		go m.openLight(avoid)
	}
	b449Log()
	return b449DarkFast
}
