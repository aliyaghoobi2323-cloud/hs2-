package engine

import (
	"math/rand/v2"
	"os"
	"sync/atomic"
	"time"
)

// The TUN side channel's low-latency lane (HS2_L3_LANE=1, off by default).
//
// pick spreads the TUN's flows over every link by rendezvous hashing, so a
// ping to the tunnel's inner address can land on a link that also carries a
// download: it then waits behind that link's queue and TCP's retransmit
// stalls (the measured ping p50 77 ms, but p90 201 and p99 426 ms where the
// direct path stays at 82/86). The user streams already keep downloads off
// some links (HS2_PICK_LANE); with this lane the TUN's interactive flows go
// to links that carry no download:
//
//   - interactive: ICMP / ICMPv6, and flows whose packets are small (EWMA of
//     the packet size under l3SmallEnter, until it reaches l3SmallLeave, so a
//     flow does not flap between the classes). Every other flow keeps the
//     plain rendezvous pick over all links, unchanged.
//   - an interactive flow picks by rendezvous over the links whose sampler
//     counted no heavy user stream (linkMeter.heavyN: the edge's sampleHealth,
//     the exit's exitTraffic.sample) and stays on that link while it lives
//     and stays light. It moves only when its link becomes heavy (or dies),
//     and on a heavy link at most once every l3LaneMoveGap, so its packets
//     are rarely reordered.
//   - no light link: the plain rendezvous pick over all links, as before.
//
// The data path reads one atomic per link it considers and updates one slot
// of a fixed flow table: no allocation, no lock (pumpTun is pick's only
// caller).

// l3LaneOn: HS2_L3_LANE=1.
var l3LaneOn = os.Getenv("HS2_L3_LANE") == "1"

const (
	// l3SmallEnter / l3SmallLeave: a flow is interactive while its packet
	// size EWMA (1/8) is under l3SmallEnter, and stops being so once it
	// reaches l3SmallLeave. A ping, a game, DNS, a call, or the ACK side of a
	// transfer sit well under; a bulk TCP direction is at the MSS (~1400).
	l3SmallEnter = 300
	l3SmallLeave = 600
	// l3LaneMoveGap: an interactive flow on a link that became heavy moves at
	// most this often (the heavy count changes once per healthTick).
	l3LaneMoveGap = 5 * time.Second
	// l3LaneSlots is the flow table's size (a power of two). Two flows that
	// share a slot just restart each other's size average.
	l3LaneSlots = 4096
)

// l3Flow is one flow table slot.
type l3Flow struct {
	link  *l3Link // the link an interactive flow is on (nil: none yet)
	moved int64   // ctrlNow-style time it was put on link
	key   uint32  // flowHash of the flow in the slot
	avg   uint16  // packet size EWMA, bytes
	inter bool    // interactive (rides the lane)
	used  bool
	// HS2_L3_FEC (l3_fec.go): the flow's bytes this second (from winStart),
	// its rate over the last whole second, and its striping state (nil: not
	// striped).
	winStart int64
	winBytes uint64
	rate     uint32
	fec      *l3FecFlow
}

// l3Lane is the lane state of one l3Set.
type l3Lane struct {
	now   func() int64 // monotonic ns (ctrlNow; tests replace it)
	flows [l3LaneSlots]l3Flow
}

func newL3Lane() *l3Lane { return &l3Lane{now: ctrlNow} }

// newL3Set is the TUN side channel's link set, with the lane when it is on.
func newL3Set() *l3Set {
	s := &l3Set{}
	// the dup sends what the lane calls interactive; the FEC stripes some of
	// what it does not (l3_fec.go)
	if l3LaneOn || l3DupOn || l3FecOn {
		s.lane = newL3Lane()
	}
	if l3DupOn {
		s.dup = true
	}
	if l3FecOn {
		s.fec = &l3Fec{}
	}
	if l3DupOn || l3FecOn {
		s.dupSeq = rand.Uint32()
	}
	return s
}

// heavyOf is a link meter's heavy count for an l3Link (nil: no meter).
func heavyOf(m *linkMeter) *atomic.Int32 {
	if m == nil {
		return nil
	}
	return &m.heavyN
}

// laneOf is a link meter's lane stamp for an l3Link (nil: no meter).
func laneOf(m *linkMeter) *atomic.Int64 {
	if m == nil {
		return nil
	}
	return &m.laneAt
}

// stampLane marks l as carrying an interactive flow now (a store at most
// every 100 ms; HS2_MCONN_STRIPE_SHARE keeps a shared stripe off it).
func (l *l3Link) stampLane(now int64) {
	if l.lane != nil && now-l.lane.Load() > int64(100*time.Millisecond) {
		l.lane.Store(now)
	}
}

// stripeNOf is a link meter's stripe count for an l3Link (nil: no meter).
func stripeNOf(m *linkMeter) *atomic.Int32 {
	if m == nil {
		return nil
	}
	return &m.stripeN
}

// onStripe: a stripe sub rides the link, or it is held for one
// (HS2_MCONN_STRIPE; never with the key off).
func (l *l3Link) onStripe() bool { return l.stripe != nil && l.stripe.Load() > 0 }

// isHeavy: the link's sampler counted a download on it at its last tick.
func (l *l3Link) isHeavy() bool { return l.heavy != nil && l.heavy.Load() > 0 }

// alwaysInteractive: ICMP (IPv4) or ICMPv6 — pings go on the lane whatever
// their size.
func alwaysInteractive(pkt []byte) bool {
	if len(pkt) >= 20 && pkt[0]>>4 == 4 {
		return pkt[9] == 1
	}
	if len(pkt) >= 40 && pkt[0]>>4 == 6 {
		return pkt[6] == 58
	}
	return false
}

// pick returns the link for an interactive flow and true, or false for a
// flow the plain pick handles. h is flowHash(pkt); links is the set's list
// (read-locked by the caller).
func (ln *l3Lane) pick(links []*l3Link, pkt []byte, h uint32) (*l3Link, bool) {
	n := min(len(pkt), 0xffff)
	icmp := alwaysInteractive(pkt)
	e := &ln.flows[h&(l3LaneSlots-1)]
	if !e.used || e.key != h {
		*e = l3Flow{key: h, avg: uint16(n), inter: icmp || n < l3SmallEnter, used: true}
	} else {
		e.avg = uint16(int(e.avg) + (n-int(e.avg))/8)
		switch {
		case icmp:
			e.inter = true
		case e.inter && e.avg >= l3SmallLeave:
			e.inter = false
		case !e.inter && e.avg < l3SmallEnter:
			e.inter = true
		}
	}
	if !e.inter {
		e.link = nil
		return nil, false
	}
	cur := e.link
	now := ln.now()
	// a stripe link (HS2_MCONN_STRIPE) is left at once: it is a download
	// spread to fill the link, taken while the flow was there
	if cur != nil && cur.Alive() && (!cur.isHeavy() || now-e.moved < int64(l3LaneMoveGap) && !cur.onStripe()) {
		cur.stampLane(now)
		return cur, true
	}
	best := rendezvous(links, h, true)
	if best == nil {
		best = rendezvous(links, h, false) // no light link: as before
	}
	if best != cur {
		e.link, e.moved = best, now
	}
	if best != nil {
		best.stampLane(now)
	}
	return best, true
}
