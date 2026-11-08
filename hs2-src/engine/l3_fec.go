package engine

import (
	"encoding/binary"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Partial redundancy for call-sized TUN flows (HS2_L3_FEC=1, off by default).
//
// HS2_L3_DUP sends an interactive (small-packet) TUN flow over two links. A
// video call (WebRTC: UDP, ~1000–1200-byte packets, 0.3–4 Mbit/s) is too big
// to send twice, and a copy of every Nth packet does not help it: a link is
// a TCP connection, so a loss on it holds back EVERY packet behind the lost
// segment until it is retransmitted (a stall of one RTT or more), not one
// packet. What can repair that is spreading the flow over several links and
// adding one XOR parity packet per group:
//
//   - Sender: a UDP flow the lane does not call interactive, moving at most
//     l3FecMaxRate, is striped round-robin over k light links (k = 3..5, by
//     rendezvous over the live links whose peer reads TypeDataDup and that
//     carry no download), each packet once as a TypeDataDup frame with its
//     own id. After k packets, the XOR of the k packets (zero padded) goes
//     over a (k+1)-th light link as a parity packet. One link that stalls
//     holds at most one packet of each group, which the receiver rebuilds
//     from the other k-1 and the parity as soon as they are in. Extra bytes:
//     1/k of the flow (20% at k=5, 33% at most). Fewer than four light
//     links, a flow above l3FecMaxRate, a TCP flow (striping would reorder
//     it) or the global budget spent: the plain pick, as before.
//   - Global cap: parity packets spend a per-set token bucket of
//     l3FecBudget bytes/s (burst l3FecBurst); an empty bucket sends the group
//     without parity. Heavy flows are never striped nor duplicated.
//   - Parity packet: a TypeDataDup frame (the usual [id u32] header, its own
//     id) whose packet is [l3FecMagic][k u8][k × id u32][len-xor u16][xor].
//     No new frame type and no capability bit: it goes only to peers that
//     announced capL3Dup, and an hs2 without this code writes it to its TUN,
//     where the kernel refuses it (the first nibble is neither 4 nor 6) —
//     nothing else happens there. Every hs2 with this code decodes parity
//     whatever its own key.
//   - Receiver: once the first parity packet arrived, every TypeDataDup
//     packet is also kept in a small ring (by id); a parity packet whose
//     group misses exactly one packet rebuilds it at once, one that misses
//     more waits (l3FecPend of them) for the others. A rebuilt packet goes
//     through the dedup set, so its late original is dropped as a repeat.
//
// With the key off nothing here runs on the sending side, and the receiving
// side does nothing until a parity packet arrives: the bytes are as before.

// l3FecOn: HS2_L3_FEC=1.
var l3FecOn = os.Getenv("HS2_L3_FEC") == "1" || l3FecLaneOnly

// l3FecLaneOnly: HS2_L3_FEC=lane, a lab diagnostic: the flows the FEC would
// stripe go, once, as plain TypeData over the light link the lane would
// pick (no striping, no parity) — what the light links alone give.
var l3FecLaneOnly = os.Getenv("HS2_L3_FEC") == "lane"

const (
	l3FecMagic = 0xF1 // the first byte of a parity packet (never an IP version)
	l3FecMaxK  = 5    // data packets per parity packet
	l3FecMinK  = 3    // fewer light links than l3FecMinK+1: no FEC
	// l3FecMaxRate: flows above this many bytes a second (4 Mbit/s) are not
	// striped; the first second of a flow counts as under it.
	l3FecMaxRate = 500_000
	l3FecBudget  = 1 << 20   // parity bytes per second, per set (~8 Mbit/s)
	l3FecBurst   = 256 << 10 // token bucket depth
	l3FecRing    = 512       // packets the receiver keeps (a power of two)
	l3FecPend    = 32        // parity packets waiting for more of their group
	l3FecHdr     = 2         // magic, k (then k ids, len-xor, xor)
	// l3FecRepick: a flow re-chooses its links at a group boundary this
	// often, or at once when one of them died, became heavy or lost dup.
	l3FecRepick = 5 * time.Second
)

// l3FecFlow is the sending state of one striped flow (a lane table slot).
type l3FecFlow struct {
	links  [l3FecMaxK + 1]*l3Link // data links 0..k-1, parity link k
	k      int
	picked int64 // lane clock when links were chosen
	n      int   // data packets in the current group
	ids    [l3FecMaxK]uint32
	lenX   uint16
	maxLen int
	xor    []byte
}

// l3Fec is a set's sender side: the parity budget and counters.
type l3Fec struct {
	tokens  float64
	last    int64
	striped atomic.Uint64 // data packets sent striped
	parity  atomic.Uint64 // parity packets sent
	parB    atomic.Uint64 // their bytes (packet only)
	dataB   atomic.Uint64 // striped data bytes
	capped  atomic.Uint64 // groups sent without parity (budget)
}

// fecPlan is where pumpTun sends one packet of a striped flow.
type fecPlan struct {
	data   *l3Link
	id     uint32
	par    *l3Link // non-nil: also send parity (with parID)
	parPkt *[]byte
	parID  uint32
	plain  bool // HS2_L3_FEC=lane: send once as TypeData on data
}

// isUDP: an IPv4 or IPv6 (no extension header) UDP packet.
func isUDP(pkt []byte) bool {
	if len(pkt) >= 20 && pkt[0]>>4 == 4 {
		return pkt[9] == 17
	}
	if len(pkt) >= 40 && pkt[0]>>4 == 6 {
		return pkt[6] == 17
	}
	return false
}

// fecLinks fills dst with up to l3FecMaxK+1 light, live, dup-capable links
// in rendezvous order for flow hash h and returns how many.
func fecLinks(links []*l3Link, h uint32, dst *[l3FecMaxK + 1]*l3Link) int {
	var w [l3FecMaxK + 1]uint32
	n := 0
	for _, l := range links {
		if !l.Alive() || !l.canDup() || l.isHeavy() {
			continue
		}
		x := mix32(h ^ l.id)
		// insertion into the top list
		i := n
		if n < len(dst) {
			n++
		} else if x <= w[n-1] {
			continue
		} else {
			i = n - 1
		}
		for i > 0 && w[i-1] < x {
			dst[i], w[i] = dst[i-1], w[i-1]
			i--
		}
		dst[i], w[i] = l, x
	}
	return n
}

// fecUsable: every chosen link is still live, light and dup-capable.
func (f *l3FecFlow) usable() bool {
	for i := 0; i <= f.k; i++ {
		l := f.links[i]
		if l == nil || !l.Alive() || !l.canDup() || l.isHeavy() {
			return false
		}
	}
	return true
}

// plan decides how slot e's packet pkt (flow hash h) goes out striped; false:
// the plain pick. pumpTun is its only caller (under the set's read lock).
func (s *l3Set) fecPlan(e *l3Flow, links []*l3Link, pkt []byte, h uint32, now int64) (fecPlan, bool) {
	// the flow's rate, per second
	switch el := now - e.winStart; {
	case e.winStart == 0:
		e.winStart, e.winBytes = now, 0
	case el >= int64(time.Second):
		e.rate = uint32(min(e.winBytes*uint64(time.Second)/uint64(el), 1<<31))
		e.winStart, e.winBytes = now, 0
	}
	e.winBytes += uint64(len(pkt))
	if !isUDP(pkt) || e.rate > l3FecMaxRate || len(pkt) > 0xffff {
		e.fec = nil
		return fecPlan{}, false
	}
	if l3FecLaneOnly {
		if l := rendezvous(links, h, true); l != nil {
			return fecPlan{data: l, plain: true}, true
		}
		return fecPlan{}, false
	}
	f := e.fec
	if f != nil && f.n == 0 && (!f.usable() || now-f.picked >= int64(l3FecRepick)) {
		f.k = 0 // re-choose at the group boundary
	}
	if f == nil || f.k == 0 {
		var ls [l3FecMaxK + 1]*l3Link
		n := fecLinks(links, h, &ls)
		if n < l3FecMinK+1 {
			e.fec = nil
			return fecPlan{}, false
		}
		if f == nil {
			f = &l3FecFlow{}
			e.fec = f
		}
		f.links, f.k, f.picked, f.n, f.lenX, f.maxLen = ls, n-1, now, 0, 0, 0
		clear(f.xor)
	} else if !f.links[f.n].Alive() {
		// a data link died mid-group: drop the group (sent unprotected), start
		// over on the next packet
		e.fec = nil
		return fecPlan{}, false
	}
	p := fecPlan{data: f.links[f.n], id: s.dupSeq & l3DupIDMask}
	s.dupSeq++
	if len(pkt) > len(f.xor) {
		f.xor = append(f.xor, make([]byte, len(pkt)-len(f.xor))...)
	}
	for i, c := range pkt {
		f.xor[i] ^= c
	}
	f.maxLen = max(f.maxLen, len(pkt))
	f.lenX ^= uint16(len(pkt))
	f.ids[f.n] = p.id
	f.n++
	s.fec.striped.Add(1)
	s.fec.dataB.Add(uint64(len(pkt)))
	if f.n < f.k {
		return p, true
	}
	// the group is complete: its parity, if the budget allows
	size := l3FecHdr + 4*f.k + 2 + f.maxLen
	if s.fec.take(size, now) {
		bp, _ := s.pool.Get().(*[]byte)
		if bp == nil || cap(*bp) < size {
			b := make([]byte, 0, max(size, 2048))
			bp = &b
		}
		b := append((*bp)[:0], l3FecMagic, byte(f.k))
		for i := 0; i < f.k; i++ {
			b = binary.BigEndian.AppendUint32(b, f.ids[i])
		}
		b = binary.BigEndian.AppendUint16(b, f.lenX)
		b = append(b, f.xor[:f.maxLen]...)
		*bp = b
		p.par, p.parPkt, p.parID = f.links[f.k], bp, s.dupSeq&l3DupIDMask
		s.dupSeq++
		s.fec.parity.Add(1)
		s.fec.parB.Add(uint64(size))
	} else {
		s.fec.capped.Add(1)
	}
	f.n, f.lenX = 0, 0
	clear(f.xor[:f.maxLen])
	f.maxLen = 0
	return p, true
}

// take spends n bytes of the parity budget (false: not enough left).
func (c *l3Fec) take(n int, now int64) bool {
	if c.last == 0 {
		c.tokens = l3FecBurst
	} else {
		c.tokens = min(l3FecBurst, c.tokens+float64(now-c.last)*l3FecBudget/1e9)
	}
	c.last = now
	if c.tokens < float64(n) {
		return false
	}
	c.tokens -= float64(n)
	return true
}

// sendFec queues a striped packet (and its group's parity).
func (s *l3Set) sendFec(bp *[]byte, p fecPlan) {
	if p.plain {
		if !p.data.enqueue(bp) {
			s.pool.Put(bp)
			s.drops.Add(1)
		}
		return
	}
	if !p.data.enqueueDup(bp, p.id) {
		s.pool.Put(bp)
		s.drops.Add(1)
	}
	if p.par != nil && !p.par.enqueueDup(p.parPkt, p.parID) {
		s.pool.Put(p.parPkt)
	}
}

// l3FecRx is a set's receiving side (nil until the first parity packet).
type l3FecRx struct {
	mu   sync.Mutex
	ring [l3FecRing]struct {
		id  uint32
		ok  bool
		pkt []byte
	}
	pend [l3FecPend]struct {
		used bool
		k    int
		ids  [l3FecMaxK]uint32
		lenX uint16
		xor  []byte
	}
	next      int // pend slot to overwrite next
	recovered atomic.Uint64
	late      atomic.Uint64 // parity whose group was already whole
}

func (r *l3FecRx) has(id uint32) bool {
	sl := &r.ring[id&(l3FecRing-1)]
	return sl.ok && sl.id == id
}

func (r *l3FecRx) keep(id uint32, pkt []byte) {
	sl := &r.ring[id&(l3FecRing-1)]
	sl.id, sl.ok, sl.pkt = id, true, append(sl.pkt[:0], pkt...)
}

// fecRecv handles a TypeDataDup packet the dedup let through (id is its id
// without the second-copy bit): a parity packet is decoded, any other packet
// is written to the TUN and, once parity has been seen, kept for repairs.
func (s *l3Set) fecRecv(id uint32, pkt []byte, dev tunWriter) {
	if len(pkt) == 0 || pkt[0] != l3FecMagic {
		dev.Write(pkt)
		r := s.fecRx.Load()
		if r == nil {
			return
		}
		r.mu.Lock()
		r.keep(id, pkt)
		r.retry(s, id, dev)
		r.mu.Unlock()
		return
	}
	r := s.fecRx.Load()
	if r == nil {
		s.fecRx.CompareAndSwap(nil, new(l3FecRx))
		r = s.fecRx.Load()
	}
	if len(pkt) < l3FecHdr {
		return
	}
	k := int(pkt[1])
	if k < 1 || k > l3FecMaxK || len(pkt) < l3FecHdr+4*k+2 {
		return
	}
	var ids [l3FecMaxK]uint32
	for i := 0; i < k; i++ {
		ids[i] = binary.BigEndian.Uint32(pkt[l3FecHdr+4*i:]) & l3DupIDMask
	}
	lenX := binary.BigEndian.Uint16(pkt[l3FecHdr+4*k:])
	xor := pkt[l3FecHdr+4*k+2:]
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.repair(s, ids[:k], lenX, xor, dev) {
		return
	}
	// more than one missing: wait for the others
	pd := &r.pend[r.next]
	r.next = (r.next + 1) % l3FecPend
	pd.used, pd.k, pd.ids, pd.lenX = true, k, ids, lenX
	pd.xor = append(pd.xor[:0], xor...)
}

// retry re-checks the waiting parity packets that cover id.
func (r *l3FecRx) retry(s *l3Set, id uint32, dev tunWriter) {
	for i := range r.pend {
		pd := &r.pend[i]
		if !pd.used {
			continue
		}
		for _, x := range pd.ids[:pd.k] {
			if x == id {
				if r.repair(s, pd.ids[:pd.k], pd.lenX, pd.xor, dev) {
					pd.used = false
				}
				break
			}
		}
	}
}

// repair rebuilds the one missing packet of a group, if exactly one is
// missing; true when the group needs nothing more (whole or rebuilt).
func (r *l3FecRx) repair(s *l3Set, ids []uint32, lenX uint16, xor []byte, dev tunWriter) bool {
	miss, nMiss := uint32(0), 0
	for _, id := range ids {
		if !r.has(id) {
			miss = id
			nMiss++
		}
	}
	switch {
	case nMiss == 0:
		r.late.Add(1)
		return true
	case nMiss > 1:
		return false
	}
	out := make([]byte, len(xor))
	copy(out, xor)
	n := lenX
	for _, id := range ids {
		if id == miss {
			continue
		}
		p := r.ring[id&(l3FecRing-1)].pkt
		if len(p) > len(out) {
			return true // malformed
		}
		for i, c := range p {
			out[i] ^= c
		}
		n ^= uint16(len(p))
	}
	if int(n) == 0 || int(n) > len(out) {
		return true
	}
	out = out[:n]
	r.keep(miss, out)
	if s.rx.first(miss) { // its original has not come: deliver the rebuilt one
		r.recovered.Add(1)
		dev.Write(out)
	}
	return true
}

// fecCounts are the FEC counters of a set, for logDrops' 30 s line.
type fecCounts struct{ striped, parity, parB, dataB, capped, recovered, late uint64 }

func (s *l3Set) fecCounts() fecCounts {
	var c fecCounts
	if s.fec != nil {
		c.striped, c.parity, c.parB = s.fec.striped.Load(), s.fec.parity.Load(), s.fec.parB.Load()
		c.dataB, c.capped = s.fec.dataB.Load(), s.fec.capped.Load()
	}
	if r := s.fecRx.Load(); r != nil {
		c.recovered, c.late = r.recovered.Load(), r.late.Load()
	}
	return c
}
