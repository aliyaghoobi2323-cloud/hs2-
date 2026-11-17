package engine

import (
	"log"
	"slices"
	"sync/atomic"
	"time"
)

// HS2_DL242_RESCUE (dl242, the exit = a download's sender; default OFF until
// the lab proves it, =1 on; no wire change, Iran unchanged): a young stripe
// sub whose FIRST chunk the peer has not consumed after
// max(dl242Min, 2 x the primary's srtt) gets its outstanding chunks (kept
// unacked + queued) re-sent at the front of the primary (or the best proven
// sub), and takes no new chunk until the peer consumed something on it.
//
// Why (dl240/dl241, prod 399, 259, 12, 1591): when a download stripes, the
// new sub's link (cold / set-aside / queued) can take ~1 s to deliver its
// first chunk. Iran's in-order reassembly waits at that chunk (rx next = the
// young sub's first offset) while everything the primary sends after it
// parks in the reorder buffer (24 MB in dl241 B1), then the second ends in
// a catch-up burst. Capping the young sub's queue (HS2_DL240_YOUNG) does not
// move the hole: the hole is the first chunk itself.
//
// Duplicates are safe on every Iran build since rel49 (STRIPE_RESEND):
// mrecv.deliver drops a chunk whose [off,end) is already delivered or held
// out of order (end <= next || coveredLocked), insertLocked trims overlaps.
// The young sub's later copy of the same chunk is dropped the same way.
var dl242Rescue atomic.Bool

// dl243: the key is HS2_DL243_RESCUE (HS2_DL242_RESCUE kept as an alias for
// the lab harness); the trigger is the young sub's own TCP link, not the
// coarse smux consumed count (dl242 R2: 73 of 126 healthy subs "rescued").
//
// rel69f (dl244): default ON on the exit through the envdef table (read in
// defaults22.go loadEnvKeys via dl243RescueKey); an explicit
// HS2_DL243_RESCUE=0 turns it off, HS2_DL242_RESCUE=1 still turns it on.
func dl243RescueKey() bool {
	switch envDefault("HS2_DL243_RESCUE") {
	case "1":
		return true
	case "0":
		return false
	}
	return envBoolDefault("HS2_DL242_RESCUE", false)
}

const (
	// dl242Min: the least time a young sub's oldest unacked chunk waits
	// before its rescue (production RTT ~60 ms, lab 75-200 ms).
	dl242Min = 300 * time.Millisecond
	// dl242Watch: how long after joining a sub is watched.
	dl242Watch = 3 * time.Second
	// dl242Poll: the watch timer's step.
	dl242Poll = 50 * time.Millisecond
	// dl242HoldMax: a rescued sub that still delivered nothing takes chunks
	// again after this (its death / the ack-stall rule cover a dark link).
	dl242HoldMax = 3 * time.Second
	// dl243MaxBytes: a rescue re-sends only the hole Iran waits for: the
	// young sub's lowest-offset unacked chunks, at most this many bytes.
	dl243MaxBytes = 256 << 10
	// dl243ConnMax rescues per connection per dl243ConnWin.
	dl243ConnMax = 2
	dl243ConnWin = 10 * time.Second
	// dl243FastAfter: a rescued sub whose link delivers its first chunk within
	// this after the rescue was probably slow, not stuck (a "false" trigger).
	dl243FastAfter = 250 * time.Millisecond
)

// dl243Link: the young sub's link TCP state plus its socket's outq (unacked
// + unsent bytes); tests replace it.
var dl243Link = func(s *msub) (linkAck, uint64, bool) {
	tc := subTCP(s)
	if tc == nil {
		return linkAck{}, 0, false
	}
	la, ok := subTCPAck(tc)
	if !ok {
		return linkAck{}, 0, false
	}
	q, ok := tcpOutq(tc)
	return la, q, ok
}

// dl242Sub: per-sub state (under m.mu).
type dl242Sub struct {
	armed  bool      // watched (key on at join, smux marks known)
	c0     uint32    // the peer's consumed count at join (diagnostic)
	first  bool      // its first marked data chunk was seen
	firstW uint32    // ... the written mark after it: consumed past it = delivered
	firstT time.Time // ... when it was written
	done   bool      // watch over (rescued, delivered, too old, gone)
	hold   bool      // rescued and nothing delivered yet: no new chunks
	holdAt time.Time // when the hold began
	// dl243: the link's TCP view of the first chunk
	a0     uint64    // the link's bytes_acked when the first chunk was seen
	q0     uint64    // ... its outq then (the first chunk is inside it)
	aLast  uint64    // the last bytes_acked seen
	aMove  time.Time // when bytes_acked last moved
	slowOK bool      // counted once as slow but moving
	post   bool      // rescued: still watched to class the trigger
	resAt  time.Time // when it was rescued
}

// dl242Stat: diagnostics.
var dl242Stat struct {
	rescues  atomic.Uint64 // young subs rescued
	chunks   atomic.Uint64 // chunks re-sent
	bytes    atomic.Uint64 // bytes re-sent
	ok       atomic.Uint64 // young subs that delivered in time (no rescue)
	held     atomic.Uint64 // chunks a rescued sub was passed over for
	noTarget atomic.Uint64 // rescues skipped: no proven sub to take them
	capped   atomic.Uint64 // dl243: rescues skipped by the per-conn cap
	slowOK   atomic.Uint64 // dl243: un-acked at the deadline but the link moving: not rescued
	fast     atomic.Uint64 // dl243: rescued subs whose link delivered within dl243FastAfter ("false")
	slow     atomic.Uint64 // dl243: rescued subs that stayed stuck longer ("true")
	noTCP    atomic.Uint64 // dl243: watched subs without a TCP view (never rescued)
}

// DL243Stats: capped, slow-but-moving, fast (false) and slow (true) rescued
// subs, subs without a TCP view.
func DL243Stats() (capped, slowOK, fast, slow, noTCP uint64) {
	return dl242Stat.capped.Load(), dl242Stat.slowOK.Load(), dl242Stat.fast.Load(), dl242Stat.slow.Load(), dl242Stat.noTCP.Load()
}

// DL242Stats: rescues, chunks, bytes, in-time subs, held chunks, no target.
func DL242Stats() (rescues, chunks, bytes, ok, held, noTarget uint64) {
	return dl242Stat.rescues.Load(), dl242Stat.chunks.Load(), dl242Stat.bytes.Load(),
		dl242Stat.ok.Load(), dl242Stat.held.Load(), dl242Stat.noTarget.Load()
}

// dl242After: the watch timer (tests replace it).
var dl242After = func(d time.Duration, f func()) { time.AfterFunc(d, f) }

// dl242Srtt: the primary's srtt reader (tests replace it).
var dl242Srtt = func(p *msub) time.Duration {
	if p == nil {
		return 0
	}
	if a, ok := subLinkAck(p); ok {
		return a.srtt
	}
	return 0
}

// dl242ArmLocked (onStripe, exit, under m.mu): watch the new stripe sub s.
func (m *mConn) dl242ArmLocked(s *msub) {
	if !dl242Rescue.Load() || m.iran || s.dl242.armed {
		return
	}
	_, c, ok := subAckMarks(s)
	if !ok {
		return
	}
	s.dl242 = dl242Sub{armed: true, c0: c}
	dl242After(dl242Poll, func() { m.dl242Tick(s) })
}

func (m *mConn) dl242Tick(s *msub) {
	m.mu.Lock()
	again := m.dl242CheckLocked(s, time.Now())
	m.mu.Unlock()
	if again {
		dl242After(dl242Poll, func() { m.dl242Tick(s) })
	}
}

// dl242CheckLocked (under m.mu): one look at the young sub s; true: look
// again later.
//
// dl243: the first chunk is judged on the young sub's link socket. At the
// first look after it was written the link's bytes_acked (a0) and outq (q0,
// which holds the chunk) are taken; bytes_acked - a0 >= q0 means the chunk
// reached Iran. It is rescued only when, after max(dl242Min, 2 x primary
// srtt), it is not delivered, the link holds unacked bytes, and the link is
// truly stuck: bytes_acked has not moved for that long, or it is in RTO
// loss. A link that is slow but acking is left alone (slowOK).
func (m *mConn) dl242CheckLocked(s *msub, now time.Time) bool {
	d := &s.dl242
	if !d.armed || (d.done && !d.post) {
		return false
	}
	if m.ended || m.closing || s.dead || s.retired || s.closeHS || s.closeAfter || !s.stripe || !dl242Rescue.Load() ||
		now.Sub(s.stJoinAt) > 2*dl242Watch {
		if d.post {
			dl242Stat.slow.Add(1) // never delivered while watched
		}
		d.done, d.post = true, false
		return false
	}
	if !d.first {
		// dl242 R1 (65d0b9): the scavenger's width may give a joined sub its
		// first chunk ~1 s later; delivery is judged on the first DATA chunk
		if len(s.sent) == 0 || !s.sent[0].marked {
			return true
		}
		la, q, ok := dl243Link(s)
		if !ok {
			d.done = true
			dl242Stat.noTCP.Add(1)
			return false
		}
		d.first, d.firstW, d.firstT = true, s.sent[0].w, s.sent[0].t
		d.a0, d.q0, d.aLast, d.aMove = la.acked, q, la.acked, now
	}
	la, _, ok := dl243Link(s)
	if !ok {
		d.done, d.post, d.hold = true, false, false
		return false
	}
	if la.acked != d.aLast {
		d.aLast, d.aMove = la.acked, now
	}
	if la.acked-d.a0 >= d.q0 {
		// the link acked everything it held when the first chunk was seen
		if d.post {
			cls := "slow"
			if now.Sub(d.resAt) <= dl243FastAfter {
				dl242Stat.fast.Add(1)
				cls = "fast"
			} else {
				dl242Stat.slow.Add(1)
			}
			d.post, d.hold = false, false
			m.dl243ClassLog(s, cls, now.Sub(d.resAt))
		} else {
			dl242Stat.ok.Add(1)
		}
		d.done = true
		return false
	}
	if d.post {
		if now.Sub(d.resAt) > dl242HoldMax {
			dl242Stat.slow.Add(1)
			d.post = false
			m.dl243ClassLog(s, "stuck", now.Sub(d.resAt))
			return false
		}
		return true
	}
	if now.Sub(d.firstT) > dl242Watch {
		d.done = true
		return false
	}
	after := max(dl242Min, 2*dl242Srtt(m.primary.Load()))
	if now.Sub(d.firstT) < after {
		return true
	}
	stuck := (la.unacked > 0 || la.rtoLoss) && (now.Sub(d.aMove) >= after || la.rtoLoss)
	if !stuck {
		if !d.slowOK {
			d.slowOK = true
			dl242Stat.slowOK.Add(1)
		}
		return true
	}
	if !m.dl243CapLocked(now) {
		dl242Stat.capped.Add(1)
		d.done = true
		return false
	}
	if m.dl242RescueLocked(s, now) {
		m.dl243NoteLocked(now)
		d.done, d.hold, d.holdAt, d.post, d.resAt = true, true, now, true, now
		return true // keep looking: class the trigger, end the hold
	}
	return true
}

// dl243ClassLog: a rescued sub's link delivered (fast: <= dl243FastAfter
// after the rescue, a likely false trigger; slow: a true one) or never did.
func (m *mConn) dl243ClassLog(s *msub, cls string, d time.Duration) {
	if !stripeLog {
		return
	}
	capped, slowOK, fast, slow, noTCP := DL243Stats()
	log.Printf("mconn %x: dl243: rescued sub %d class %s (link delivered %v after; ok %d slowOK %d fast %d slow %d capped %d noTCP %d)",
		m.id&0xffffff, s.idx, cls, d.Round(time.Millisecond), dl242Stat.ok.Load(), slowOK, fast, slow, capped, noTCP)
}

// dl243CapLocked: this connection may rescue now (fewer than dl243ConnMax
// in the last dl243ConnWin).
func (m *mConn) dl243CapLocked(now time.Time) bool {
	n := 0
	for _, t := range m.dl243Res {
		if !t.IsZero() && now.Sub(t) < dl243ConnWin {
			n++
		}
	}
	return n < dl243ConnMax
}

// dl243NoteLocked: a rescue now (replaces the oldest entry).
func (m *mConn) dl243NoteLocked(now time.Time) {
	o := 0
	for i, t := range m.dl243Res {
		if t.IsZero() {
			o = i
			break
		}
		if t.Before(m.dl243Res[o]) {
			o = i
		}
	}
	m.dl243Res[o] = now
}

// dl242TargetLocked: the sub that takes the rescue — the primary when it is
// live and not s; else the live proven stripe sub with the least queued plus
// unread.
func (m *mConn) dl242TargetLocked(s *msub) *msub {
	ok := func(x *msub) bool {
		return x != nil && x != s && !x.dead && !x.retired && !x.closeHS && !x.closeAfter && !x.gated && !x.ks.stalled &&
			!x.dl242.hold && !(x.dl242.armed && !x.dl242.done)
	}
	if p := m.primary.Load(); ok(p) {
		return p
	}
	var best *msub
	var bestB int64
	for _, x := range m.subs {
		if !x.stripe || !ok(x) {
			continue
		}
		b := int64(x.qBytes)
		if w, c, mk := subAckMarks(x); mk && int32(w-c) > 0 {
			b += int64(int32(w - c))
		}
		if best == nil || b < bestB {
			best, bestB = x, b
		}
	}
	return best
}

// dl242RescueLocked (under m.mu): queue s's outstanding sequence chunks
// (kept unacked, queued, being written) at the front of the target, lowest
// offset first, at most dl243MaxBytes (dl243: the hole Iran waits for). s keeps its own references (its later
// copies are dropped by the peer as duplicates).
func (m *mConn) dl242RescueLocked(s *msub, now time.Time) bool {
	b := m.dl242TargetLocked(s)
	if b == nil {
		dl242Stat.noTarget.Add(1)
		return false
	}
	_, c, _ := subAckMarks(s)
	var cs []*mchunk
	for _, r := range s.sent {
		if r.marked && int32(r.w-c) <= 0 {
			continue // consumed already
		}
		cs = append(cs, r.c)
	}
	for _, x := range s.q {
		if x.seq() {
			cs = append(cs, x)
		}
	}
	if s.cur != nil && s.cur.seq() {
		cs = append(cs, s.cur)
	}
	for _, x := range s.curMore {
		if x.seq() {
			cs = append(cs, x)
		}
	}
	cs = dl242Order(cs)
	if len(cs) == 0 {
		return false
	}
	front := make([]*mchunk, 0, len(cs)+len(b.q))
	n := 0
	for _, x := range cs {
		if n >= dl243MaxBytes {
			break // dl243: the hole only
		}
		x.refs.Add(1)
		x.copies++
		if e := x.off + uint64(x.n); e > b.txEnd {
			b.txEnd = e
		}
		b.qBytes += x.qsize()
		front = append(front, x)
		n += x.n
	}
	b.q = append(front, b.q...)
	b.lastPush = now
	select {
	case b.kick <- struct{}{}:
	default:
	}
	k := dl242Stat.rescues.Add(1)
	dl242Stat.chunks.Add(uint64(len(front)))
	dl242Stat.bytes.Add(uint64(n))
	if k == 1 || k%200 == 0 || stripeLog {
		log.Printf("mconn %x: dl242: young sub %d rescued (age %v, first chunk %v unacked, %d chunks %d KB from off %d on sub %d; rescues %d, ok %d)",
			m.id&0xffffff, s.idx, now.Sub(s.stJoinAt).Round(time.Millisecond), now.Sub(s.dl242.firstT).Round(time.Millisecond),
			len(front), n>>10, front[0].off, b.idx, k, dl242Stat.ok.Load())
	}
	return true
}

// dl242Order: by offset, one entry per chunk.
func dl242Order(cs []*mchunk) []*mchunk {
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

// dl242HoldLocked (the exit's stripe sender, under m.mu): s was rescued and
// the peer has consumed nothing on it since — pass it over. The hold ends
// with its first delivery or after dl242HoldMax.
func (m *mConn) dl242HoldLocked(s *msub, now time.Time) bool {
	d := &s.dl242
	if !d.hold {
		return false
	}
	if _, c, ok := subAckMarks(s); !ok || int32(c-d.firstW) >= 0 || now.Sub(d.holdAt) > dl242HoldMax || !dl242Rescue.Load() {
		d.hold = false
		return false
	}
	dl242Stat.held.Add(1)
	return true
}
