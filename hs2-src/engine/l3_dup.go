package engine

import (
	"encoding/binary"
	"os"
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
var l3DupOn = os.Getenv("HS2_L3_DUP") == "1"

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

// l3Dedup remembers the last l3DupWin ids (31-bit, wrapping) one sender used.
// The zero value is ready. Every link's reader calls accept, hence the lock;
// only TypeDataDup frames take it.
type l3Dedup struct {
	mu   sync.Mutex
	init bool
	top  uint32 // the highest id seen
	bits [l3DupWin / 64]uint64
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
	diff := seqDiff(id, d.top)
	switch {
	case !d.init || diff <= -l3DupWin:
		// first id, or one far behind the window: the other server restarted
		// (its counter starts at random) — start over at this id.
		d.init, d.top = true, id
		clear(d.bits[:])
	case diff > 0:
		if diff >= l3DupWin {
			clear(d.bits[:])
		} else {
			for k := uint32(1); k <= uint32(diff); k++ {
				i := (d.top + k) & (l3DupWin - 1)
				d.bits[i/64] &^= 1 << (i % 64)
			}
		}
		d.top = id & l3DupIDMask
	default:
		i := id & (l3DupWin - 1)
		if d.bits[i/64]&(1<<(i%64)) != 0 {
			return false
		}
	}
	i := id & (l3DupWin - 1)
	d.bits[i/64] |= 1 << (i % 64)
	return true
}
