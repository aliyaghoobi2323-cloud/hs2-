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

// HS2_MCONN_STRIPE_RESEND (production default ON, both sides, since rel49;
// 0 = off): a striping
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
// The connection's last sub dying is covered by the grace below
// (HS2_MCONN_STRIPE_GRACE_MS); bytes older than stripeKeepAge still unread by
// the peer are not.
//
// Status (rel49, docs/reality-architecture.md §11.6.6): on by default. rel46
// left two failures: (1) a connection whose subs all died ended — now the
// grace; (2) survivors stalled a few seconds after the kill behind a hole of
// one record — the record whose write FAILED on the dying sub: writeLoop
// released it before subDied collected the sub's chunks, so no resend carried
// it. writeLoop now keeps it too.

// stripeResend: read by loadEnvKeys (defaults22.go); production default ON
// on both sides since rel49 (envdef.Table), HS2_MCONN_STRIPE_RESEND=0 turns
// it off.
var stripeResend bool

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
	resends   atomic.Uint64 // sub deaths survived by a resend
	chunks    atomic.Uint64 // chunks queued again
	graces    atomic.Uint64 // last-sub deaths that started a grace
	rejoined  atomic.Uint64 // graces ended by a new sub
	graceEnds atomic.Uint64 // graces that ran out (the connection ended)
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
	if b == nil && !(m.graceOKLocked() && (len(m.subs) == 0 || m.onlyClosingLocked())) {
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
	if b == nil {
		// HS2_MCONN_STRIPE_GRACE_MS: no live sub; the chunks wait for the
		// next one (graceFlushLocked)
		m.graceKeepLocked(cs)
		m.releaseDeadLocked(s)
		stripeResendStat.graces.Add(1)
		if stripeLog {
			stripeLogf("stripe %x: sub- %d last sub: grace %v, %d chunks kept (iran=%v)", m.id&0xffffff, s.idx, stripeGraceDur, len(m.orphan), m.iran)
		}
		return true
	}
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
	m.releaseDeadLocked(s)
	stripeResendStat.resends.Add(1)
	stripeResendStat.chunks.Add(uint64(len(cs)))
	if stripeLog {
		stripeLogf("stripe %x: sub- %d resend %d chunks on sub %d", m.id&0xffffff, s.idx, len(cs), b.idx)
	}
	return true
}

// releaseDeadLocked drops the dead sub s's own references once its chunks
// were queued again: the kept ones and its queue (the writer releases cur and
// curMore itself; subDied drops their copy as before); each held one copy.
func (m *mConn) releaseDeadLocked(s *msub) {
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
}

// releaseSentLocked frees every kept chunk and the grace's orphans (the
// connection ended).
func (m *mConn) releaseSentLocked() {
	for _, s := range m.subs {
		if len(s.sent) > 0 {
			m.dropSentLocked(s, len(s.sent))
		}
	}
	for _, c := range m.orphan {
		c.unref()
	}
	m.orphan = nil
}

// ---- HS2_MCONN_STRIPE_GRACE_MS ---------------------------------------------
//
// With the resend, a striping connection whose LAST sub dies (u8: a
// connection striped over two links, both among the 75 % killed) used to end
// ("every sub died") — iperf3 shows the stream at 0, an app a broken upload.
// Now both sides keep the connection for stripeGraceDur: what the dead sub
// held, wrote recently or was writing (the same set as a resend) waits in
// m.orphan; Iran opens a light sub at once (role join, avoiding the dead
// links) and the exit, whose table entry stays while the connection lives,
// attaches it. The first sub that joins takes the orphans at the front of its
// queue (graceFlushLocked), in offset order; the receiver drops what it
// already has. No sub within the grace: the connection ends as before.
// Write waits meanwhile (no sub has room). No wire change.

// stripeGraceDur: 0 (HS2_MCONN_STRIPE_GRACE_MS=0) turns the grace off.
var stripeGraceDur = graceDurFromEnv()

func graceDurFromEnv() time.Duration {
	v := strings.TrimSpace(os.Getenv("HS2_MCONN_STRIPE_GRACE_MS"))
	if v == "" {
		return 3 * time.Second
	}
	ms, err := strconv.Atoi(v)
	if err != nil || ms < 0 {
		return 3 * time.Second
	}
	return time.Duration(ms) * time.Millisecond
}

// graceGap: how often Iran tries to open a sub within the grace.
const graceGap = 250 * time.Millisecond

// graceOKLocked: the connection may wait for a new sub instead of ending.
func (m *mConn) graceOKLocked() bool {
	return stripeGraceDur > 0 && !m.closing && !m.handshaking && m.mode == planeLight &&
		!(m.peerFin && m.wclosed)
}

// graceKeepLocked: cs (sorted, distinct) wait for the next sub; each gets a
// reference and a copy unless it is already an orphan.
func (m *mConn) graceKeepLocked(cs []*mchunk) {
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
	if m.graceUntil.IsZero() {
		m.graceUntil = time.Now().Add(stripeGraceDur)
	}
}

// graceFlushLocked (from addSubLocked): a sub joined; it takes the orphans
// first and the grace is over.
func (m *mConn) graceFlushLocked(s *msub) {
	if m.graceUntil.IsZero() {
		return
	}
	m.graceUntil, m.graceRetryAt = time.Time{}, time.Time{}
	if len(m.orphan) == 0 {
		return
	}
	for _, c := range m.orphan {
		// the orphan's reference and copy move to the queue
		if e := c.off + uint64(c.n); e > s.txEnd {
			s.txEnd = e
		}
		s.qBytes += c.qsize()
	}
	s.q = append(m.orphan, s.q...)
	stripeResendStat.chunks.Add(uint64(len(m.orphan)))
	stripeResendStat.rejoined.Add(1)
	if stripeLog {
		stripeLogf("stripe %x: grace: sub+ %d takes %d chunks (iran=%v)", m.id&0xffffff, s.idx, len(m.orphan), m.iran)
	}
	m.orphan = nil
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// graceLiveLocked: a sub that can carry the connection is attached.
func (m *mConn) graceLiveLocked() bool {
	return len(m.subs) > 0 && !m.onlyClosingLocked()
}

// graceOpenLocked (Iran, within a grace with no live sub): open a light sub
// on another link, at most every graceGap.
func (m *mConn) graceOpenLocked(now time.Time) {
	if !m.iran || m.open == nil || m.lightOpening || now.Before(m.graceRetryAt) {
		return
	}
	m.graceRetryAt = now.Add(graceGap)
	m.lightOpening = true
	avoid := append([]any(nil), m.deadKeys...)
	go m.openLight(avoid)
}

// graceTickLocked (from tick): over = the grace ran out with no sub (the
// connection ends); d = when to look again.
func (m *mConn) graceTickLocked(now time.Time) (over bool, d time.Duration) {
	if m.graceUntil.IsZero() {
		return false, 0
	}
	if m.graceLiveLocked() {
		m.graceUntil = time.Time{}
		return false, 0
	}
	if !now.Before(m.graceUntil) {
		stripeResendStat.graceEnds.Add(1)
		if stripeLog {
			stripeLogf("stripe %x: grace over, no sub joined (iran=%v)", m.id&0xffffff, m.iran)
		}
		return true, 0
	}
	m.graceOpenLocked(now)
	return false, min(graceGap, m.graceUntil.Sub(now))
}

// stripeStallLogLocked (HS2_MCONN_STRIPE_LOG, from tick, under m.mu): once a
// second a striping connection logs its receive side (next offset, first
// out-of-order offset, ooo and ready bytes) and its send side (sendOff, per
// sub: queued bytes, the offset the writer is on, the end of what it was
// given, the oldest kept offset) — to find which side and which sub holds a
// stall.
func (m *mConn) stripeStallLogLocked(now time.Time) {
	if !stripeLog || m.stN == 0 || now.Sub(m.stallLogAt) < time.Second {
		return
	}
	m.stallLogAt = now
	r := &m.r
	r.mu.Lock()
	next, ooo1, oooB, rdy, oooN := r.next, uint64(0), r.oooBytes, r.readyBytes, len(r.ooo)
	if len(r.ooo) > 0 {
		ooo1 = r.ooo[0].off
	}
	r.mu.Unlock()
	var b strings.Builder
	for _, s := range m.subs {
		cur := int64(-1)
		if s.cur != nil {
			cur = int64(s.cur.off)
		}
		kept := int64(-1)
		if len(s.sent) > 0 {
			kept = int64(s.sent[0].c.off)
		}
		fmt.Fprintf(&b, " [%d st=%v q=%d cur=%d txEnd=%d kept0=%d ret=%v]", s.idx, s.stripe, s.qBytes, cur, s.txEnd, kept, s.retired)
	}
	stripeLogf("stripe %x: tick iran=%v rx next=%d ooo1=%d oooN=%d oooB=%d ready=%d | tx sendOff=%d orphans=%d subs:%s",
		m.id&0xffffff, m.iran, next, ooo1, oooN, oooB, rdy, m.sendOff, len(m.orphan), b.String())
}
