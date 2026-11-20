package engine

import (
	"encoding/binary"
	"sync"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
)

// Sending interactive TUN packets twice (HS2_L3_DUP=1, off by default).
//
// On a lossy path most of the TUN side channel's ping spikes are not queues
// but TCP loss recovery on the one link a flow rides: a lost segment holds
// everything behind it until it is retransmitted, and for sparse traffic that
// recovery is a tail-loss probe or an RTO (200–600 ms). Measured on an idle
// tunnel at 80 ms RTT: 0% loss p99 82 ms; 1% loss 3.5% of pings over 120 ms,
// p99 300–370 ms. Two links lose independently, so a packet sent over both
// arrives at the earlier of the two.
//
//   - Sender: a packet of a flow the lane (l3_lane.go) calls interactive goes
//     over the lane's link AND the next-best rendezvous link (preferring one
//     carrying no download, else any live one), as a TypeDataDup frame:
//     [id u32][packet]. id is a per-l3Set counter (random start); its top bit
//     marks the second copy, so the receiver can count the packets the second
//     link saved. One live link: one plain TypeData copy. Bulk flows: as
//     before. The dup turns the lane's classification on (newL3Set).
//   - Receiver: the first copy of an id goes to the TUN, a later one is
//     dropped (l3Dedup: a 4096-id sliding bitmap per l3Set — each set has one
//     sender, the other server's set).
//   - Wire: a server announces capL3Dup over kindInfo only with the key on,
//     and sends TypeDataDup only over a link whose peer announced it. An older
//     peer never sees the frame; with the key off the bytes are as before.
//     Every hs2 that has this code reads TypeDataDup whatever its key.
//
// Both copies still obey l3MaxSojourn: a copy stuck in a slow link's queue is
// dropped there and its twin is the one that arrives.

// l3DupOn: HS2_L3_DUP=1.
var l3DupOn bool // loadEnvKeys

// l3DupCap is the capL3Dup bit this server announces (0 with the key off, so
// its kindInfo bytes stay as before).
func l3DupCap() uint8 {
	if l3DupOn {
		return capL3Dup
	}
	return 0
}

const (
	l3DupSecondBit = 1 << 31 // id flag: the copy sent over the second link
	l3DupIDMask    = l3DupSecondBit - 1
	l3DupWin       = 4096 // ids the receiver remembers (a power of two)
	l3DupHdr       = 4
)

// canDup: the link's peer reads TypeDataDup frames.
func (l *l3Link) canDup() bool { return l.dupOK != nil && l.dupOK() }

// dupSecond is the second link for flow hash h after first: the best
// rendezvous link other than first whose peer reads TypeDataDup, carrying no
// download if there is one; nil if there is no other such live link. With
// HS2_DUP_PATHS on: a light link on another path than first's, then a light
// one on any path, then another path, then any (dup_paths.go) — a copy behind
// a download waits in its queue, which costs more than sharing a path.
func dupSecond(links []*l3Link, h uint32, first *l3Link) *l3Link {
	if l3CutAvoid { // HS2_LB_RESCUE_CUT_L3 (l3_cutavoid.go)
		if first.cut.any() {
			if l := dupSecondWhere(links, h, first, func(l *l3Link) bool { return !l3OnCut(l) }); l != nil {
				l3CutDupOff.Add(1)
				return l
			}
		}
	}
	if dupPathsOn && first.path != "" {
		other := func(l *l3Link) bool { return l.path != "" && l.path != first.path }
		light := func(l *l3Link) bool { return !l.isHeavy() }
		if l := dupSecondWhere(links, h, first, func(l *l3Link) bool { return other(l) && light(l) }); l != nil {
			return l
		}
		if l := dupSecondWhere(links, h, first, light); l != nil {
			return l
		}
		if l := dupSecondOtherPath(links, h, first); l != nil {
			return l
		}
	}
	return dupSecondWhere(links, h, first, nil)
}

// dupSecondWhere is dupSecond over the links ok accepts (nil: all).
func dupSecondWhere(links []*l3Link, h uint32, first *l3Link, ok func(*l3Link) bool) *l3Link {
	var best, bestLight *l3Link
	var bestW, bestLightW uint32
	// d421: the second copy goes to first's peer process. Unknown (an old
	// peer, HS2_ZDU off): the loop as before.
	if fi := first.peerInst(); fi.known() {
		for _, l := range links {
			if l == first || !l.Alive() || !l.canDup() || ok != nil && !ok(l) {
				continue
			}
			w := mix32(h ^ l.id)
			top := best == nil || w > bestW
			topLight := !l.isHeavy() && (bestLight == nil || w > bestLightW)
			if !top && !topLight || l.peerInst() != fi { // checked only for a would-be winner
				continue
			}
			if top {
				best, bestW = l, w
			}
			if topLight {
				bestLight, bestLightW = l, w
			}
		}
	} else {
		for _, l := range links {
			if l == first || !l.Alive() || !l.canDup() || ok != nil && !ok(l) {
				continue
			}
			w := mix32(h ^ l.id)
			if best == nil || w > bestW {
				best, bestW = l, w
			}
			if !l.isHeavy() && (bestLight == nil || w > bestLightW) {
				bestLight, bestLightW = l, w
			}
		}
	}
	if bestLight != nil {
		return bestLight
	}
	return best
}

// sendDup queues bp on first and a copy on second, both as TypeDataDup with
// the next id. A queue that is full drops its copy; the packet is counted as
// dropped only when both are.
func (s *l3Set) sendDup(bp *[]byte, first, second *l3Link) {
	id := s.dupSeq & l3DupIDMask
	s.dupSeq++
	cp, _ := s.pool.Get().(*[]byte)
	if cp == nil || cap(*cp) < len(*bp) {
		b := make([]byte, len(*bp), cap(*bp))
		cp = &b
	}
	*cp = append((*cp)[:0], *bp...)
	ok1 := first.enqueueDup(bp, id)
	ok2 := second.enqueueDup(cp, id|l3DupSecondBit)
	if !ok1 {
		s.pool.Put(bp)
	}
	if !ok2 {
		s.pool.Put(cp)
	}
	if !ok1 && !ok2 {
		s.drops.Add(1)
		return
	}
	obs.l3DupSent.Add(1)
}

// enqueueDup is enqueue for a TypeDataDup copy.
func (l *l3Link) enqueueDup(b *[]byte, id uint32) bool {
	select {
	case l.q <- qpkt{b: b, t: time.Now(), id: id, dup: true}:
		return true
	default:
		return false
	}
}

// appendDupFrame appends a TypeDataDup frame (the AppendFrame layout, no
// padding) carrying id and pkt.
func appendDupFrame(dst []byte, id uint32, pkt []byte) []byte {
	n := l3DupHdr + len(pkt)
	dst = append(dst, core.TypeDataDup, byte(n>>16), byte(n>>8), byte(n), 0, 0, 0)
	dst = binary.BigEndian.AppendUint32(dst, id)
	return append(dst, pkt...)
}

// l3Dedup remembers the last l3DupWin ids (31-bit, wrapping) of up to two id
// streams (l3Wins). The zero value is ready. The l3Set reads through l3Rx,
// which keeps one l3Wins per sending process.
type l3Dedup struct {
	mu sync.Mutex
	w  l3Wins
}

// seqDiff is a-b in the 31-bit id space (wrapping).
func seqDiff(a, b uint32) int32 { return int32((a-b)<<1) >> 1 }

// accept returns the packet of a TypeDataDup payload if this is the first
// copy of its id, nil if it is a repeat (or malformed).
func (d *l3Dedup) accept(payload []byte) []byte {
	if len(payload) < l3DupHdr {
		return nil
	}
	raw := binary.BigEndian.Uint32(payload)
	if !d.first(raw & l3DupIDMask) {
		obs.l3DupDropped.Add(1)
		return nil
	}
	if raw&l3DupSecondBit != 0 {
		obs.l3DupSaved.Add(1)
	}
	return payload[l3DupHdr:]
}

// first reports whether id is new, and records it.
func (d *l3Dedup) first(id uint32) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.w.first(id)
}

// d421: two senders' id streams at once. During a ZDU handover both processes
// of one side read the shared TUN for ~45 s and each numbers its dups from
// its own random start, so one window that restarted on every far id let
// second copies through (s417: 123 ICMP DUP replies). The receiver now keeps
//   - one l3Wins per sending process (l3Rx, keyed by the link's peer ZDU
//     instance from kindInfo; zero = a peer without one), and
//   - two windows inside each l3Wins: an id far from the current window
//     (another stream: a peer without instance ids running two processes, or
//     a restart) opens the other window instead of clearing the only one, so
//     two interleaved streams both dedupe.
// The sender keeps both copies on links of one peer process (dupSecondWhere,
// pickAll). No wire change.

// l3Win is one id stream's sliding bitmap.
type l3Win struct {
	init bool
	top  uint32 // the highest id seen
	bits [l3DupWin / 64]uint64
}

// near: id falls inside this window (either side of top).
func (w *l3Win) near(id uint32) bool {
	if !w.init {
		return false
	}
	d := seqDiff(id, w.top)
	return d > -l3DupWin && d < l3DupWin
}

// reset starts the window over at id.
func (w *l3Win) reset(id uint32) {
	w.init, w.top = true, id&l3DupIDMask
	clear(w.bits[:])
}

// mark records id (near, or just reset) and reports whether it was new.
func (w *l3Win) mark(id uint32) bool {
	i := id & (l3DupWin - 1)
	if diff := seqDiff(id, w.top); diff > 0 {
		for k := uint32(1); k <= uint32(diff); k++ {
			j := (w.top + k) & (l3DupWin - 1)
			w.bits[j/64] &^= 1 << (j % 64)
		}
		w.top = id & l3DupIDMask
	} else if w.bits[i/64]&(1<<(i%64)) != 0 {
		return false
	}
	w.bits[i/64] |= 1 << (i % 64)
	return true
}

// l3Wins is two l3Win; c is the one the last id fell in.
type l3Wins struct {
	c uint8
	w [2]l3Win
}

// first reports whether id is new, and records it: in the current window if
// id is near it, else in the other one if near, else the other window starts
// over at id (the current one stays as the previous stream's).
func (ws *l3Wins) first(id uint32) bool {
	w := &ws.w[ws.c]
	if !w.near(id) {
		ws.c ^= 1
		w = &ws.w[ws.c]
		if !w.near(id) {
			w.reset(id)
		}
	}
	return w.mark(id)
}

// l3RxSlots: sending processes remembered per l3Set (two during a handover,
// one spare for links whose peer instance is not known yet).
const l3RxSlots = 3

// l3Rx is an l3Set's receive dedup: one l3Wins per sending process instance,
// the least recently used evicted. The zero value is ready. Every link's
// reader calls accept, hence the lock; only TypeDataDup frames take it.
type l3Rx struct {
	mu   sync.Mutex
	tick uint64
	s    [l3RxSlots]l3RxSlot
}

type l3RxSlot struct {
	inst zduInst
	used uint64 // 0: free
	w    l3Wins
}

// accept is l3Dedup.accept for a frame from a link whose other server is
// process inst (zero: unknown).
func (r *l3Rx) accept(inst zduInst, payload []byte) []byte {
	if len(payload) < l3DupHdr {
		return nil
	}
	raw := binary.BigEndian.Uint32(payload)
	r.mu.Lock()
	ok := r.slot(inst).first(raw & l3DupIDMask)
	r.mu.Unlock()
	if !ok {
		obs.l3DupDropped.Add(1)
		return nil
	}
	if raw&l3DupSecondBit != 0 {
		obs.l3DupSaved.Add(1)
	}
	return payload[l3DupHdr:]
}

// first is l3Dedup.first in the windows of the sender seen last (HS2_L3_FEC
// repairs: the parity packet's sender was just accepted). An empty l3Rx
// starts a zero-instance slot.
func (r *l3Rx) first(id uint32) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := 0
	for i := range r.s {
		if r.s[i].used > r.s[v].used {
			v = i
		}
	}
	if r.s[v].used == 0 {
		return r.slot(zduInst{}).first(id)
	}
	return r.s[v].w.first(id)
}

// slot is inst's windows; a new inst takes the least recently used slot.
// Caller holds r.mu.
func (r *l3Rx) slot(inst zduInst) *l3Wins {
	r.tick++
	v := 0
	for i := range r.s {
		e := &r.s[i]
		if e.used != 0 && e.inst == inst {
			e.used = r.tick
			return &e.w
		}
		if e.used < r.s[v].used {
			v = i
		}
	}
	e := &r.s[v]
	e.inst, e.used = inst, r.tick
	e.w = l3Wins{}
	return &e.w
}

// peerInst is the process instance of the link's other server (its ZDU
// instance id from kindInfo); zero when it announced none or is not known yet.
func (l *l3Link) peerInst() zduInst {
	if l.peer == nil {
		return zduInst{}
	}
	if pi := l.peer.Load(); pi.zdu() {
		return pi.Inst
	}
	return zduInst{}
}

// sameInst: a dup's second copy over l reaches the process its first copy
// (whose link's peer is fi) reaches; true when fi is unknown (d421).
func sameInst(fi zduInst, l *l3Link) bool { return !fi.known() || l.peerInst() == fi }
