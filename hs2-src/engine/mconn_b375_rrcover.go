package engine

import (
	"fmt"
	"os"
	"sort"
	"sync/atomic"
)

// ---- b375 HS2_B375_RRCOVER (default off since rel69q-rc2, "1" on; both) ---
//
// rel69q-rc2: off by default. sk393 (lab3) reproduced a drop-storm open
// slowdown (p50/p99 +30-40 %) with the key on, B0 (key off) equal to or
// faster than rel69p-rc3, and the cover never extended a snapshot in any
// lab run (ext 0). "1" turns it back on.
//
// rb372 B2 k7 (backlog 2026-11-19 01:31): a heavy lite call's primary (raw1)
// went dark in a young records phase. Records went only to the primary, its
// queue filled and it wrote ~610 KB into the dark socket. rrStart retired
// every sub and sent RREC(Bi=643001); Kharej answered ACK(Rk=579001). Iran's
// records ring (young: sized from a rate known only after one full window,
// ~32 KB) did not hold [579001, 643001) and onRRescueAns ended the conn
// (nocover, errMConnLost) although the missing bytes were still in Iran's
// memory: in the retired subs' queues (not yet written) and in their sent
// lists (written, kept for a resend).
//
// With the key on:
//
//  1. rrStart, before the retired subs drop their kept chunks, copies the
//     DATA bytes below Bi of every chunk the retired subs hold (queue, the
//     chunk being written and its batch, the sent list) and extends the
//     rescue snapshot [rrSnap0, Bi) DOWNWARDS with them as far as the bytes
//     are contiguous (the same set hsrCoverLocked uses on b367). The ring
//     snapshot stays the top; onRRescueAns is unchanged and resends exactly
//     [Rk, Bi) from the snapshot (bytes are copies made under m.mu, so they
//     equal what Write queued at those offsets: no duplicate, no missing
//     byte; the exit's reassembly drops an offset it already has). Only a
//     range that no source holds still ends the conn (nocover).
//  2. a records ring's cap is at least the conn's in-flight bytes: its
//     largest live sub queue plus the written-unacked bytes (darkcall's
//     tcp_info, at least one lite queue; the ring holds the newest bytes, so
//     it must span the queue to reach the unacked ones; kept bytes come from
//     the sent lists instead), and the rate of the running
//     (first) window counts before a full window is known. The growth past
//     rrRingCap still comes from the big budget's tiers (rrBigMax), at most
//     rrRingMax per ring: memory stays bounded as before.
//
// Counters on the rescue-rec stat line: "b375 on ext N (KB) full N short N
// infl N".

var b375RRCover atomic.Bool

// b375MSS: bytes per unacked segment in the written-unacked estimate.
const b375MSS = 1448

// ---- b375 HS2_B375_RINGFLOOR (default off; both sides) ---------------------
//
// Part 2 (the ring's in-flight floor and the young window's rate) has its
// own key: rb375 showed it costs 150-380 MB RSS (ring grow/shrink churn on
// every records conn) while part 1 alone already covers the trace's shape
// (queued + kept bytes). Values: "0"/unset off; "1" only for conns that need
// it: a call (rk.call) or a conn whose largest live sub queue is at least
// HS2_B375_RINGFLOOR_QKB (default 32 = one full lite queue); "2" every
// records conn (the rb375 behaviour).
var (
	b375RingFloor atomic.Int32
	b375FloorQ    = 32 << 10
)

func init() {
	b375RRCover.Store(envBoolDefault("HS2_B375_RRCOVER", false))
	switch os.Getenv("HS2_B375_RINGFLOOR") {
	case "1", "call":
		b375RingFloor.Store(1)
	case "2", "all":
		b375RingFloor.Store(2)
	}
	b375FloorQ = envIntDefault("HS2_B375_RINGFLOOR_QKB", 32) << 10
}

var b375Stat struct {
	ext    atomic.Uint64 // RREC snapshots extended below the ring
	extKB  atomic.Uint64 // ... bytes added (KB)
	full   atomic.Uint64 // answers covered only thanks to the extension
	short  atomic.Uint64 // answers short even with the extension (nocover)
	inflUp atomic.Uint64 // ring caps raised by the in-flight floor
	held   atomic.Uint64 // rrStarts that held retired chunks (zero copy)
}

func b375LogPart() string {
	s := &b375Stat
	return fmt.Sprintf(" · b375 on %v floor %d ext %d (%d KB) full %d short %d infl %d held %d",
		b375RRCover.Load(), b375RingFloor.Load(), s.ext.Load(), s.extKB.Load(), s.full.Load(), s.short.Load(), s.inflUp.Load(), s.held.Load())
}

// b375Piece: bytes [off, off+len(b)) of a retired sub's chunk (a view of
// the held chunk's buffer, not a copy).
type b375Piece struct {
	off uint64
	b   []byte
}

// b375HoldLocked (Iran, under mu, rrStart before the old subs are retired
// and their kept chunks dropped): takes a reference on every DATA chunk
// below bi the subs in old hold (queued, being written, written and kept).
// No byte is copied here: a DATA chunk is immutable once queued (Write fills
// buf and sendLocked sets the offset before push; nothing writes buf again
// until the last unref returns it to the pool), so the reference keeps the
// bytes. The hold is released by b375ReleaseLocked (answer, late answer,
// retry, end); only an answer the ring does not cover reads the bytes
// (b375CoverLocked).
func (m *mConn) b375HoldLocked(old []*msub, bi uint64) {
	m.b375ReleaseLocked()
	if !b375RRCover.Load() {
		return
	}
	var seen map[*mchunk]struct{}
	add := func(c *mchunk) {
		if c == nil || c.kind != mRecData || c.n <= 0 || c.off >= bi {
			return
		}
		if seen == nil {
			seen = make(map[*mchunk]struct{}, 64)
		}
		if _, ok := seen[c]; ok {
			return
		}
		seen[c] = struct{}{}
		c.refs.Add(1)
		m.b375Hold = append(m.b375Hold, c)
	}
	for _, s := range old {
		for _, c := range s.q {
			add(c)
		}
		add(s.cur)
		for _, c := range s.curMore {
			add(c)
		}
		for _, r := range s.sent {
			add(r.c)
		}
	}
	m.b375HoldBi = bi
	if len(m.b375Hold) > 0 {
		b375Stat.held.Add(1)
	}
}

// b375ReleaseLocked (under mu): drop the hold's references.
func (m *mConn) b375ReleaseLocked() {
	for i, c := range m.b375Hold {
		c.unref()
		m.b375Hold[i] = nil
	}
	m.b375Hold = m.b375Hold[:0]
}

// b375CoverLocked (Iran, under mu): an answer (or a retry) needs bytes
// below the ring's snapshot: extend [rrSnap0, rrBi) downwards from the held
// chunks (the only place bytes are copied, and only when needed), then
// release the hold. need: the answer's Rk (^0: a retry, extend as far as
// the hold reaches).
func (m *mConn) b375CoverLocked(need uint64) {
	if len(m.b375Hold) == 0 {
		return
	}
	bi := m.b375HoldBi
	if bi == m.rrBi && (need == ^uint64(0) || need < bi && (m.rrSnap0 == ^uint64(0) || need < m.rrSnap0)) {
		ps := make([]b375Piece, 0, len(m.b375Hold))
		for _, c := range m.b375Hold {
			n := min(uint64(c.n), bi-c.off)
			ps = append(ps, b375Piece{off: c.off, b: c.buf[mDataHdr : mDataHdr+int(n)]})
		}
		m.b375ExtendSnapLocked(ps, bi) // copies into the new snapshot
	}
	m.b375ReleaseLocked()
}

// b375ExtendSnapLocked (Iran, under mu, rrStart after the ring snapshot):
// extend [rrSnap0, bi) downwards with the pieces while they are contiguous.
// The snapshot's own bytes win where they overlap (they are the same bytes).
func (m *mConn) b375ExtendSnapLocked(ps []b375Piece, bi uint64) {
	if len(ps) == 0 {
		return
	}
	lo := m.rrSnap0
	if lo == ^uint64(0) || lo > bi {
		lo = bi // nothing from the ring: the pieces must reach bi themselves
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].off < ps[j].off })
	// the lowest lo' such that [lo', bi) is covered by the snapshot and the
	// pieces: walk down while some piece starts below lo and reaches it
	for {
		nlo := lo
		for _, p := range ps {
			if p.off < nlo && p.off+uint64(len(p.b)) >= lo {
				nlo = p.off
			}
		}
		if nlo == lo {
			break
		}
		lo = nlo
	}
	old0 := m.rrSnap0
	if old0 == ^uint64(0) || old0 > bi {
		old0 = bi
	}
	if lo >= old0 {
		return
	}
	buf := make([]byte, bi-lo)
	got := make([]bool, old0-lo) // bytes of [lo, old0) already filled
	for _, p := range ps {
		for i := range p.b {
			o := p.off + uint64(i)
			if o < lo || o >= old0 || got[o-lo] {
				continue
			}
			buf[o-lo] = p.b[i]
			got[o-lo] = true
		}
	}
	for _, g := range got {
		if !g {
			return // cannot happen (lo was proved contiguous); keep today's snapshot
		}
	}
	if old0 < bi {
		copy(buf[old0-lo:], m.rrSnap)
	}
	m.rrSnap, m.rrSnap0 = buf, lo
	b375Stat.ext.Add(1)
	b375Stat.extKB.Add((old0 - lo) >> 10)
	m.rrExt0 = old0
}

// b375InflLocked (under mu and rkMu, rrKeepLocked): the ring's in-flight
// floor: the largest live sub queue and the bytes written but not acked (darkcall's last tcp_info read of a sub, unacked segments x
// MSS; at least one lite queue when unknown). No syscall on the write path.
func (m *mConn) b375InflLocked() {
	f := b375RingFloor.Load()
	if f == 0 {
		m.rk.infl, m.rk.floor = 0, false
		return
	}
	q, w := 0, mSubQueueLite
	for _, s := range m.subs {
		if s.dead || s.bulk.Load() {
			continue
		}
		q = max(q, s.qBytes)
		if s.dc.ok { // darkcall's last tcp_info read: written, not acked
			w = max(w, int(s.dc.unacked)*b375MSS)
		}
	}
	if f == 1 && !m.rk.call && q < b375FloorQ && !m.rk.floor {
		m.rk.infl = 0 // RINGFLOOR=1: not a call, never a heavy queue (sticky once heavy)
		return
	}
	m.rk.floor = true
	v := q + w // kept bytes are covered from the sent lists (b375PiecesLocked), not the ring
	if v > rrCapFor(m.rk.rate) && v > m.rk.infl {
		b375Stat.inflUp.Add(1)
	}
	m.rk.infl = v
}

// b375Cap: a records ring's cap with the in-flight floor and the running
// window's rate (no full window yet); only with HS2_B375_RINGFLOOR and for a
// conn b375InflLocked marked (r.floor).
func (r *rescueRing) b375Cap() int {
	c := rrCapFor(r.rate)
	if b375RingFloor.Load() == 0 || !r.floor {
		return c
	}
	if r.rate == 0 && r.winT0 != 0 {
		c = max(c, rrCapFor(r.winN)) // bytes so far in under a second: a lower bound of the rate
	}
	if r.infl > c {
		c = min(r.infl, max(rrRingMax, rrRingCap))
	}
	return c
}
