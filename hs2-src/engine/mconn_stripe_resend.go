package engine

import (
	"slices"
	"sync/atomic"
	"time"
)

// HS2_MCONN_STRIPE_RESEND (default OFF, lab only; 1 = on): a striping
// connection survives the death of one of its subs.
//
// A stripe puts each chunk on ONE sub. When a link dies (reset, ss -K, a
// middlebox), whatever that sub had written but the peer had not yet read —
// the link's send buffer, the bytes in flight, the peer kernel's receive
// buffer — is gone. The old rule (mconn_stripe.go "Failure") reset the whole
// connection then. With a 75 % link kill (lab u8) nearly every striped
// upload lost a sub and was reset: iperf3 showed 0 bit/s for the rest of the
// run, a real app a broken upload. Production (r31 defaults) had it too.
//
// Now the sender keeps every chunk a sub wrote for stripeKeepAge (bounded by
// stripeKeepBytes per connection) and, when the sub dies while another live
// sub remains, queues those chunks plus the dead sub's queue at the front of
// the live sub with the shortest queue, in offset order. The receiver
// reassembles by offset and drops what it already has (mrecv.deliver), so a
// chunk that did arrive is harmless; one that did not fills the gap. No wire
// change: an older peer just sees DATA records it may already hold.
//
// Not covered (unchanged): the connection's last sub dying (no live sub to
// resend on), and bytes older than stripeKeepAge still unread by the peer.
//
// Status (rel46, docs/reality-architecture.md §11.6): off by default. In the
// lab u8 it ends the resets (no errMConnLost) but not the freeze: a
// connection whose subs all die still ends ("every sub died"), and some
// survivors stall a few seconds after the kill with a gap the resend did not
// fill — for a real app a hang until mGapTimeout, worse than the old reset.

var stripeResend = envBoolDefault("HS2_MCONN_STRIPE_RESEND", false)

const (
	// stripeKeepAge: how long a written chunk is kept for a resend. A link
	// holds far less than this in its buffers (RTT ~75 ms plus a 100 ms
	// queue in the lab; production RTT ~60 ms).
	stripeKeepAge = 2 * time.Second
	// stripeKeepBytes: kept bytes per connection (at most stripeMaxConns
	// connections stripe at once: 8 × 16 MiB).
	stripeKeepBytes = 16 << 20
)

var stripeResendStat struct {
	resends atomic.Uint64 // sub deaths survived by a resend
	chunks  atomic.Uint64 // chunks queued again
}

// sentRec is a chunk a sub wrote, kept for a resend.
type sentRec struct {
	c *mchunk
	t time.Time
}

// stripeKeepOnLocked: chunks written on s are kept (under m.mu).
func (m *mConn) stripeKeepOnLocked(s *msub) bool {
	return stripeResend && m.stN > 0 && !m.ended && !s.dead
}

// keepSentLocked takes over the writer's references to the chunks it just
// wrote on s: sequence chunks are kept, the rest released (under m.mu).
func (m *mConn) keepSentLocked(s *msub, now time.Time, cs ...*mchunk) {
	for _, c := range cs {
		if c == nil {
			continue
		}
		if !c.seq() {
			c.unref()
			continue
		}
		s.sent = append(s.sent, sentRec{c, now})
		m.keptBytes += c.n
	}
	for len(s.sent) > 0 && now.Sub(s.sent[0].t) > stripeKeepAge {
		m.dropSentLocked(s, 1)
	}
	for m.keptBytes > stripeKeepBytes {
		// over the bound: the oldest kept chunk of any sub goes first
		var o *msub
		for _, x := range m.subs {
			if len(x.sent) > 0 && (o == nil || x.sent[0].t.Before(o.sent[0].t)) {
				o = x
			}
		}
		if o == nil {
			o = s
		}
		if len(o.sent) == 0 {
			break
		}
		m.dropSentLocked(o, 1)
	}
}

// dropSentLocked releases s's k oldest kept chunks (under m.mu).
func (m *mConn) dropSentLocked(s *msub, k int) {
	for _, r := range s.sent[:k] {
		m.keptBytes -= r.c.n
		r.c.unref()
	}
	clear(s.sent[:k])
	s.sent = s.sent[k:]
	if len(s.sent) == 0 {
		s.sent = nil
	}
}

// resendTargetLocked: the live sub that takes a dead sub's chunks.
func (m *mConn) resendTargetLocked() *msub {
	var best *msub
	for _, x := range m.subs {
		if x.dead || x.retired || x.closeHS || x.closeAfter || x.gated {
			continue
		}
		if best == nil || x.qBytes < best.qBytes {
			best = x
		}
	}
	return best
}

// stripeResendLocked (from subDied, s already removed from m.subs, its
// queue untouched): if a resend applies, queues every sequence chunk s
// held, wrote recently or is writing at the front of a live sub and
// releases s's own references. false: nothing done (the old rule applies).
func (m *mConn) stripeResendLocked(s *msub) bool {
	if !stripeResend || m.stN == 0 || m.ended {
		return false
	}
	b := m.resendTargetLocked()
	if b == nil {
		return false
	}
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
	cs = slices.CompactFunc(cs, func(a, b *mchunk) bool { return a == b })
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
	// s's own references: the kept ones and its queue (the writer releases
	// cur and curMore itself); each held one copy
	for _, r := range s.sent {
		r.c.copies--
	}
	m.dropSentLocked(s, len(s.sent))
	for _, c := range s.q {
		if c.seq() {
			c.copies--
		}
		c.unref()
	}
	s.q, s.qBytes = nil, 0
	// (cur and curMore: subDied drops their copy as before)
	stripeResendStat.resends.Add(1)
	stripeResendStat.chunks.Add(uint64(len(cs)))
	if stripeLog {
		stripeLogf("stripe %x: sub- %d resend %d chunks on sub %d", m.id&0xffffff, s.idx, len(cs), b.idx)
	}
	return true
}

// releaseSentLocked frees every kept chunk (the connection ended).
func (m *mConn) releaseSentLocked() {
	for _, s := range m.subs {
		if len(s.sent) > 0 {
			m.dropSentLocked(s, len(s.sent))
		}
	}
}
