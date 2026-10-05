package engine

import (
	"encoding/binary"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Per-flow TCP reorder buffer on the receive side, between a datagram carrier
// and the TUN.
//
// Why: the carriers deliver TCP segments out of order. The FEC decoder hands a
// packet it rebuilt from parity to the TUN AFTER the packets that were sent
// behind it (the group's parity arrives only when the group closes, up to the
// FEC window later), and real paths reorder too. Under ordinary path loss that
// is tens of thousands of out-of-order segments a minute per connection (lab:
// 66-82k in 17 s with bursty loss and NO path reordering). The receiving TCP
// pays twice: the sender reads the duplicate ACKs as loss and retransmits
// spuriously, cutting its window; and the out-of-order data fills the receive
// buffer until the kernel clamps the advertised window to two segments — the
// field's after-download stall (~0.5 Mbit/s per connection, rwnd-limited).
//
// What: for each TCP flow, a segment that arrives ahead of a gap is held for at
// most dgReorderHold; when the missing segment arrives (typically rebuilt by FEC
// within its window) everything is released in sequence order, so the TCP
// above sees an ordered stream with a short delay instead of reordering. If the
// gap is a real loss it never fills; after dgReorderHold the held segments are
// released anyway and TCP recovers the loss as usual. Only data-bearing,
// unfragmented TCP is ever held: pure ACKs, SYN/RST, retransmissions (data at
// or below what was already released), non-TCP and fragments pass straight
// through. It changes nothing on the wire, so mixed-version peers are fine.
//
// One reorderer per carrier: a flow is pinned to one carrier, which is where
// the reordering happens, so there is no cross-carrier state to share.

// dgReorderHold is how long a segment may wait for the gap ahead of it. Every
// gap that never fills (a loss FEC could not rebuild) costs the flow this much
// extra delay before TCP's own recovery, so it trades the reordering it hides
// against that cost. Measured in the netns lab (8 TCP flows through the
// forwarder, 80 ms RTT): 15 ms kept nearly all of the benefit — sender-seen
// reordering down 80-99.9% and retransmissions down 67-98% across bursty loss,
// uniform loss and a loss-free but congested 300 Mbit/s path — with throughput
// never below no-hold. 25 ms and 40 ms cost more on bursty loss than they saved
// (40 ms: ~10% lower throughput there). HS2_TUN_REORDER_MS overrides it; 0
// turns reordering off (every packet passes straight through).
var dgReorderHold = func() time.Duration {
	if v := strings.TrimSpace(os.Getenv("HS2_TUN_REORDER_MS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return time.Duration(n) * time.Millisecond
		}
	}
	return 15 * time.Millisecond
}()

const (
	reorderFlowMax  = 256              // held segments per flow before the gap is given up
	reorderTotalMax = 4096             // held segments per carrier
	reorderIdle     = 60 * time.Second // flow state kept this long without traffic
	reorderSweep    = 4096             // pushes between idle-flow sweeps
)

type rflowKey struct {
	src, dst     [16]byte
	sport, dport uint16
	v6           bool
}

type rheld struct {
	seq, end uint32
	at       time.Time
	b        []byte
}

type rflow struct {
	next uint32  // next sequence number expected (everything below was released)
	held []rheld // waiting for the gap at next, sorted by seq
	last time.Time
}

// reorderStats counts what the reorderer did (tests, diagnostics).
type reorderStats struct {
	Held     uint64 // segments held behind a gap
	Filled   uint64 // gaps that filled while held (reordering hidden from TCP)
	TimedOut uint64 // gaps given up after the hold (real loss, or a very late packet)
	Overflow uint64 // gaps given up because too much was held
}

type reorderer struct {
	mu    sync.Mutex
	hold  time.Duration
	write func([]byte) // delivers to the TUN; called with mu held, in order
	// flush, when set, is called after the timer or Close released packets
	// (write may only queue them for a batched TUN write: tunBatch).
	flush  func()
	clock  func() time.Time
	flows  map[rflowKey]*rflow
	nheld  int
	pushes int
	timer  *time.Timer
	armed  bool
	closed bool
	stats  reorderStats
}

func newReorderer(hold time.Duration, write func([]byte)) *reorderer {
	return &reorderer{hold: hold, write: write, flows: map[rflowKey]*rflow{}}
}

func (r *reorderer) now() time.Time {
	if r.clock != nil {
		return r.clock()
	}
	return time.Now()
}

// Stats returns a snapshot of the counters.
func (r *reorderer) Stats() reorderStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stats
}

// seqLT/seqLE compare 32-bit TCP sequence numbers modulo 2^32.
func seqLT(a, b uint32) bool { return int32(a-b) < 0 }
func seqLE(a, b uint32) bool { return int32(a-b) <= 0 }

// Segment kinds tcpSeg reports.
const (
	segPass = iota // not something to reorder: pass through, untracked
	segCtl         // SYN or RST: a connection starts or ends; forget the flow
	segData        // carries sequence space (payload and/or FIN)
)

// tcpSeg classifies an IP packet and, for TCP, extracts the flow key and the
// sequence range [seq, end) the segment occupies.
func tcpSeg(b []byte) (k rflowKey, seq, end uint32, kind int) {
	if len(b) < 1 {
		return k, 0, 0, segPass
	}
	var tcp []byte
	switch b[0] >> 4 {
	case 4:
		ihl := int(b[0]&0x0f) * 4
		if ihl < 20 || len(b) < ihl+20 {
			return k, 0, 0, segPass
		}
		tot := int(binary.BigEndian.Uint16(b[2:4]))
		if tot < ihl+20 || tot > len(b) {
			return k, 0, 0, segPass
		}
		if frag := binary.BigEndian.Uint16(b[6:8]); frag&0x3fff != 0 { // MF or offset
			return k, 0, 0, segPass
		}
		if b[9] != 6 {
			return k, 0, 0, segPass
		}
		copy(k.src[:4], b[12:16])
		copy(k.dst[:4], b[16:20])
		tcp = b[ihl:tot]
	case 6:
		if len(b) < 40+20 || b[6] != 6 { // only TCP right after the fixed header
			return k, 0, 0, segPass
		}
		pl := int(binary.BigEndian.Uint16(b[4:6]))
		if pl < 20 || 40+pl > len(b) {
			return k, 0, 0, segPass
		}
		copy(k.src[:], b[8:24])
		copy(k.dst[:], b[24:40])
		k.v6 = true
		tcp = b[40 : 40+pl]
	default:
		return k, 0, 0, segPass
	}
	off := int(tcp[12]>>4) * 4
	if off < 20 || off > len(tcp) {
		return k, 0, 0, segPass
	}
	k.sport = binary.BigEndian.Uint16(tcp[0:2])
	k.dport = binary.BigEndian.Uint16(tcp[2:4])
	flags := tcp[13]
	if flags&0x06 != 0 { // SYN or RST
		return k, 0, 0, segCtl
	}
	n := uint32(len(tcp) - off)
	if flags&0x01 != 0 { // FIN takes one sequence number
		n++
	}
	if n == 0 { // a pure ACK: nothing to order
		return k, 0, 0, segPass
	}
	seq = binary.BigEndian.Uint32(tcp[4:8])
	return k, seq, seq + n, segData
}

// Push delivers one packet from the carrier: now, or later in order.
func (r *reorderer) Push(b []byte) {
	if r.hold <= 0 {
		r.write(b)
		return
	}
	k, seq, end, kind := tcpSeg(b)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		r.write(b)
		return
	}
	switch kind {
	case segPass:
		r.write(b)
		return
	case segCtl:
		if f := r.flows[k]; f != nil {
			r.releaseAll(f) // the connection is starting over or ending
			delete(r.flows, k)
		}
		r.write(b)
		return
	}
	now := r.now()
	r.pushes++
	if r.pushes%reorderSweep == 0 {
		r.sweepIdle(now)
	}
	f := r.flows[k]
	if f == nil {
		r.flows[k] = &rflow{next: end, last: now}
		r.write(b)
		return
	}
	f.last = now
	switch {
	case seq == f.next: // in order
		r.write(b)
		f.next = end
		r.drain(f)
	case seqLT(seq, f.next): // a retransmission or an overlap: pass it on
		r.write(b)
		if seqLT(f.next, end) {
			f.next = end
			r.drain(f)
		}
	default: // ahead of a gap: hold it
		if len(f.held) >= reorderFlowMax || r.nheld >= reorderTotalMax {
			r.stats.Overflow++
			r.insert(f, rheld{seq, end, now, b})
			r.releaseAll(f)
			return
		}
		r.insert(f, rheld{seq, end, now, b})
		r.stats.Held++
		r.arm()
	}
}

// insert puts h into f.held in sequence order.
func (r *reorderer) insert(f *rflow, h rheld) {
	i := len(f.held)
	for i > 0 && seqLT(h.seq, f.held[i-1].seq) {
		i--
	}
	f.held = append(f.held, rheld{})
	copy(f.held[i+1:], f.held[i:])
	f.held[i] = h
	r.nheld++
}

// drain releases the held segments the advanced f.next has reached.
func (r *reorderer) drain(f *rflow) {
	filled := false
	for len(f.held) > 0 && seqLE(f.held[0].seq, f.next) {
		h := f.held[0]
		f.held = f.held[1:]
		r.nheld--
		r.write(h.b)
		if seqLT(f.next, h.end) {
			f.next = h.end
		}
		filled = true
	}
	if filled {
		r.stats.Filled++
	}
	if len(f.held) == 0 {
		f.held = nil
	}
}

// releaseAll gives up on f's gap: everything held goes out in order and the
// flow resumes after the last of it.
func (r *reorderer) releaseAll(f *rflow) {
	for _, h := range f.held {
		r.write(h.b)
		if seqLT(f.next, h.end) {
			f.next = h.end
		}
	}
	r.nheld -= len(f.held)
	f.held = nil
}

// arm makes sure the expiry timer is running. Called with mu held.
func (r *reorderer) arm() {
	if r.armed {
		return
	}
	r.armed = true
	if r.timer == nil {
		r.timer = time.AfterFunc(r.hold, r.expire)
	} else {
		r.timer.Reset(r.hold)
	}
}

// expire releases every flow whose gap has waited dgReorderHold, and re-arms for
// the next one still waiting.
func (r *reorderer) expire() {
	if r.flush != nil {
		defer r.flush() // runs after the unlock below
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.armed = false
	if r.closed {
		return
	}
	now := r.now()
	var nextDue time.Time
	for _, f := range r.flows {
		if len(f.held) == 0 {
			continue
		}
		oldest := f.held[0].at
		for _, h := range f.held[1:] {
			if h.at.Before(oldest) {
				oldest = h.at
			}
		}
		if due := oldest.Add(r.hold); !now.Before(due) {
			r.stats.TimedOut++
			r.releaseAll(f)
		} else if nextDue.IsZero() || due.Before(nextDue) {
			nextDue = due
		}
	}
	if !nextDue.IsZero() {
		r.armed = true
		r.timer.Reset(nextDue.Sub(now))
	}
}

// sweepIdle forgets flows that hold nothing and have been quiet. mu held.
func (r *reorderer) sweepIdle(now time.Time) {
	for k, f := range r.flows {
		if len(f.held) == 0 && now.Sub(f.last) > reorderIdle {
			delete(r.flows, k)
		}
	}
}

// Close releases everything still held (in order) and stops the timer; later
// pushes pass straight through.
func (r *reorderer) Close() {
	if r == nil {
		return
	}
	if r.flush != nil {
		defer r.flush()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	for _, f := range r.flows {
		r.releaseAll(f)
	}
	if r.timer != nil {
		r.timer.Stop()
	}
}
