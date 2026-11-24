package engine

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// b449d target wait (HS2_B449_TGT_WAIT_MS, default 1500; 0 = off): the lost-
// copy resets left after the b449c floor are the no-fresh-target class: a
// sub-resend connection's sub dies with its kept chunks intact, but no live
// sub on another link read within subResendFresh (r68fix3 resends only to a
// fresh one), and the old rule resets because a copy is lost or every live
// sub joined or missed a copy lately (holeRecent). Lab3 b449c: Iran
// noTgtLost 13-20 a run, Kharej 12-17. Now, only where the old rule would
// reset, the dead sub's chunks wait (as m.orphan) for at most the wait for a
// target: a live sub that reads (fresh, another link) or a sub that joined
// after the death (Iran opens one at once, the grace's open). Then they are
// resent at its front, as the resend would have; at the deadline with no
// target the connection resets exactly as before, b449TgtWait later. Writes go
// on meanwhile on the live subs; the peer's reader waits on the hole at most
// that long. Never a hang: the tick ends the connection at the deadline.
var b449TgtWait atomic.Int64 // ns; 0 = off

func init() {
	b449TgtWait.Store(int64(time.Duration(b449EnvMs0("HS2_B449_TGT_WAIT_MS", 1500)) * time.Millisecond))
}

var b449TwStat struct {
	waits   atomic.Uint64 // deaths that waited instead of a reset (b271 Q5 counts them no-target survived)
	saved   atomic.Uint64 // a target came: resent, no reset
	expired atomic.Uint64 // no target by the deadline: the old reset
	msSum   atomic.Uint64 // wait-to-target time of the saved ones
	msMax   atomic.Uint64
}

// b449TwOKLocked (stripeResendLocked, a sub-resend connection's death with no
// fresh target, s already out of m.subs): the old rule would reset (a copy
// lost or holeRecent) and a live sub may still become a target.
func (m *mConn) b449TwOKLocked(s *msub) bool {
	if b449TgtWait.Load() <= 0 || m.ended || m.closing || m.handshaking || m.mode != planeLight ||
		len(m.subs) == 0 || m.onlyClosingLocked() || !m.graceUntil.IsZero() {
		return false
	}
	if m.b449TwDead != nil {
		return true // a wait is on: this death's chunks join it (same deadline)
	}
	lost := false
	for _, c := range s.q {
		if c.seq() && c.copies <= 1 {
			lost = true
		}
	}
	if c := s.cur; c != nil && c.seq() && c.copies <= 1 {
		lost = true
	}
	for _, c := range s.curMore {
		if c.seq() && c.copies <= 1 {
			lost = true
		}
	}
	return lost || m.holeRecentLocked() && !m.stripeQuietLocked(s)
}

// b449TwStartLocked: cs (sorted, distinct) wait for a target; s's own
// references go (releaseDeadLocked), each orphan holds one reference and copy.
func (m *mConn) b449TwStartLocked(s *msub, cs []*mchunk, now time.Time) {
	had := make(map[*mchunk]bool, len(m.orphan))
	for _, c := range m.orphan {
		had[c] = true
	}
	for _, c := range cs {
		if had[c] {
			continue
		}
		c.refs.Add(1)
		c.copies++
		m.orphan = append(m.orphan, c)
	}
	slices.SortFunc(m.orphan, func(a, b *mchunk) int {
		switch {
		case a.off < b.off:
			return -1
		case a.off > b.off:
			return 1
		}
		return 0
	})
	if m.b449TwDead == nil {
		m.b449TwDead, m.b449TwN = s, m.nsubs
		m.b449TwT0 = now
		m.b449TwUntil = now.Add(time.Duration(b449TgtWait.Load()))
		b449TwStat.waits.Add(1)
	}
	m.releaseDeadLocked(s)
	if m.iran && m.open != nil && !m.lightOpening {
		m.lightOpening = true // a new link: a target by joining (the grace's open)
		avoid := append([]any(nil), m.deadAvoidLocked()...)
		go m.openLight(avoid)
	}
}

// b449TwTargetLocked: a sub that joined after the death, or a fresh one on
// another link than the dead sub's.
func (m *mConn) b449TwTargetLocked() *msub {
	for _, x := range m.subs {
		if x.idx >= m.b449TwN && !x.dead && !x.retired && !x.closeHS && !x.closeAfter && !x.gated && !x.bulk.Load() {
			return x
		}
	}
	return m.subResendTargetLocked(m.b449TwDead)
}

// b449TwTickLocked (tick): over = the deadline passed with no target (the
// caller resets the connection, the old verdict); d = when to look again.
func (m *mConn) b449TwTickLocked(now time.Time) (over bool, d time.Duration) {
	if m.b449TwDead == nil {
		return false, 0
	}
	if len(m.orphan) == 0 || !m.graceUntil.IsZero() {
		m.b449TwDead = nil // the grace took them (the last sub died meanwhile)
		return false, 0
	}
	if b := m.b449TwTargetLocked(); b != nil {
		for _, c := range m.orphan { // the orphan's reference and copy move to b's queue
			if e := c.off + uint64(c.n); e > b.txEnd {
				b.txEnd = e
			}
			b.qBytes += c.qsize()
		}
		b.q = append(m.orphan, b.q...)
		b.lastPush = now
		stripeResendStat.chunks.Add(uint64(len(m.orphan)))
		m.orphan, m.b449TwDead = nil, nil
		select {
		case b.kick <- struct{}{}:
		default:
		}
		ms := uint64(now.Sub(m.b449TwT0).Milliseconds())
		b449TwStat.saved.Add(1)
		b449TwStat.msSum.Add(ms)
		for {
			mx := b449TwStat.msMax.Load()
			if ms <= mx || b449TwStat.msMax.CompareAndSwap(mx, ms) {
				break
			}
		}
		b449Log()
		return false, 0
	}
	if !now.Before(m.b449TwUntil) {
		m.b449TwDead = nil
		b449TwStat.expired.Add(1)
		subLostResets.Add(1)
		b449Log()
		return true, 0
	}
	return false, min(50*time.Millisecond, m.b449TwUntil.Sub(now))
}

// b449TwLogPart: the b449 log line's target-wait and slow-open part.
func b449TwLogPart() string {
	n := b449TwStat.saved.Load()
	return fmt.Sprintf("%d target waits: %d saved (avg %d ms max %d ms) %d expired · slow opens %d (won %d, cap-held looks %d)", b449TwStat.waits.Load(), n,
		b449TwStat.msSum.Load()/max(n, 1), b449TwStat.msMax.Load(), b449TwStat.expired.Load(),
		b449SlowOpenStat.opens.Load(), b449SlowOpenStat.won.Load(), b449SlowOpenStat.capHeld.Load()) + " · " + b449gLogPart()
}

// b449EnvMs0 (rc14b): an ms key whose documented "0 = off" must work.
// envIntDef reads 0 (and below) as unset, so HS2_B449_SLOWOPEN_MS=0 and
// HS2_B449_TGT_WAIT_MS=0 left the default on (lab1 rc14b 15a: SLOWOPEN_MS=0
// still made slow opens). Unset, empty, bad or negative: def.
func b449EnvMs0(k string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(k))); err == nil && v >= 0 {
		return v
	}
	return def
}
