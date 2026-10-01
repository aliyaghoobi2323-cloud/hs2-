package engine

import (
	"encoding/binary"
	"testing"
	"time"
)

// --- synthetic packets -------------------------------------------------------

const (
	fFIN = 0x01
	fSYN = 0x02
	fRST = 0x04
	fACK = 0x10
)

// ip4tcp builds an IPv4/TCP packet carrying n payload bytes at sequence seq.
func ip4tcp(sport, dport uint16, seq uint32, n int, flags byte) []byte {
	b := make([]byte, 40+n)
	b[0] = 0x45
	binary.BigEndian.PutUint16(b[2:4], uint16(40+n))
	b[8] = 64
	b[9] = 6
	copy(b[12:16], []byte{10, 0, 0, 1})
	copy(b[16:20], []byte{10, 0, 0, 2})
	t := b[20:]
	binary.BigEndian.PutUint16(t[0:2], sport)
	binary.BigEndian.PutUint16(t[2:4], dport)
	binary.BigEndian.PutUint32(t[4:8], seq)
	t[12] = 5 << 4
	t[13] = flags
	for i := 0; i < n; i++ {
		t[20+i] = byte(seq) + byte(i)
	}
	return b
}

// ip6tcp is ip4tcp over IPv6.
func ip6tcp(sport, dport uint16, seq uint32, n int, flags byte) []byte {
	b := make([]byte, 60+n)
	b[0] = 0x60
	binary.BigEndian.PutUint16(b[4:6], uint16(20+n))
	b[6] = 6
	b[7] = 64
	b[8+15] = 1
	b[24+15] = 2
	t := b[40:]
	binary.BigEndian.PutUint16(t[0:2], sport)
	binary.BigEndian.PutUint16(t[2:4], dport)
	binary.BigEndian.PutUint32(t[4:8], seq)
	t[12] = 5 << 4
	t[13] = flags
	return b
}

// seqOf reads a packet's TCP sequence number (IPv4 or IPv6).
func seqOf(b []byte) uint32 {
	if b[0]>>4 == 6 {
		return binary.BigEndian.Uint32(b[44:48])
	}
	return binary.BigEndian.Uint32(b[24:28])
}

// harness: a reorderer with a long hold (the real timer never fires during a
// test) and a fake clock the test advances before calling expire by hand.
type roHarness struct {
	r   *reorderer
	out [][]byte
	now time.Time
}

func newRoHarness(hold time.Duration) *roHarness {
	h := &roHarness{now: time.Unix(1_000_000, 0)}
	h.r = newReorderer(hold, func(b []byte) { h.out = append(h.out, b) })
	h.r.clock = func() time.Time { return h.now }
	return h
}

func (h *roHarness) seqs() []uint32 {
	s := make([]uint32, len(h.out))
	for i, b := range h.out {
		s[i] = seqOf(b)
	}
	return s
}

func eqSeqs(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- tests -------------------------------------------------------------------

// An in-order stream passes straight through, nothing held.
func TestReorderInOrderPassThrough(t *testing.T) {
	h := newRoHarness(time.Hour)
	for s := uint32(1000); s < 1000+10*100; s += 100 {
		h.r.Push(ip4tcp(1, 2, s, 100, fACK))
	}
	want := []uint32{1000, 1100, 1200, 1300, 1400, 1500, 1600, 1700, 1800, 1900}
	if !eqSeqs(h.seqs(), want) {
		t.Fatalf("in-order stream came out as %v", h.seqs())
	}
	if st := h.r.Stats(); st.Held != 0 {
		t.Fatalf("an in-order stream held %d segments", st.Held)
	}
}

// The FEC case: a segment rebuilt later arrives after the ones behind it. The
// later ones wait; when it arrives all go out in sequence order.
func TestReorderGapFills(t *testing.T) {
	h := newRoHarness(time.Hour)
	h.r.Push(ip4tcp(1, 2, 0, 100, fACK))   // establishes next=100
	h.r.Push(ip4tcp(1, 2, 200, 100, fACK)) // ahead of the gap at 100
	h.r.Push(ip4tcp(1, 2, 300, 100, fACK)) // also ahead
	if !eqSeqs(h.seqs(), []uint32{0}) {
		t.Fatalf("segments past the gap were not held: %v", h.seqs())
	}
	h.r.Push(ip4tcp(1, 2, 100, 100, fACK)) // the rebuilt one arrives late
	if !eqSeqs(h.seqs(), []uint32{0, 100, 200, 300}) {
		t.Fatalf("after the gap filled: %v, want 0 100 200 300", h.seqs())
	}
	st := h.r.Stats()
	if st.Held != 2 || st.Filled != 1 || st.TimedOut != 0 {
		t.Fatalf("stats %+v, want Held 2 Filled 1 TimedOut 0", st)
	}
	// and the stream carries on in order
	h.r.Push(ip4tcp(1, 2, 400, 100, fACK))
	if got := h.seqs(); got[len(got)-1] != 400 || len(got) != 5 {
		t.Fatalf("stream after the fill: %v", got)
	}
}

// A real loss: the gap never fills. After the hold the held segments go out in
// order and the flow resumes past them; TCP recovers the hole itself.
func TestReorderGapTimesOut(t *testing.T) {
	h := newRoHarness(40 * time.Millisecond)
	h.r.Push(ip4tcp(1, 2, 0, 100, fACK))
	h.r.Push(ip4tcp(1, 2, 300, 100, fACK))
	h.r.Push(ip4tcp(1, 2, 200, 100, fACK))
	h.now = h.now.Add(39 * time.Millisecond)
	h.r.expire()
	if !eqSeqs(h.seqs(), []uint32{0}) {
		t.Fatalf("released before the hold expired: %v", h.seqs())
	}
	h.now = h.now.Add(2 * time.Millisecond)
	h.r.expire()
	if !eqSeqs(h.seqs(), []uint32{0, 200, 300}) {
		t.Fatalf("after the hold: %v, want 0 200 300 (in order, gap at 100 left to TCP)", h.seqs())
	}
	if st := h.r.Stats(); st.TimedOut != 1 {
		t.Fatalf("TimedOut=%d, want 1", st.TimedOut)
	}
	h.r.Push(ip4tcp(1, 2, 400, 100, fACK)) // resumes in order, no wait
	h.r.Push(ip4tcp(1, 2, 100, 100, fACK)) // TCP's retransmission of the hole: straight through
	if got := h.seqs(); !eqSeqs(got, []uint32{0, 200, 300, 400, 100}) {
		t.Fatalf("after the timeout: %v", got)
	}
}

// A retransmission (data at or below what was released) is never held, even
// while later data waits behind a different gap.
func TestReorderRetransmitPassesThrough(t *testing.T) {
	h := newRoHarness(time.Hour)
	h.r.Push(ip4tcp(1, 2, 0, 100, fACK))
	h.r.Push(ip4tcp(1, 2, 100, 100, fACK))
	h.r.Push(ip4tcp(1, 2, 300, 100, fACK)) // held (gap at 200)
	h.r.Push(ip4tcp(1, 2, 0, 100, fACK))   // old: through at once
	if !eqSeqs(h.seqs(), []uint32{0, 100, 0}) {
		t.Fatalf("a retransmission was held: %v", h.seqs())
	}
}

// Pure ACKs, SYN, RST, non-TCP and fragments are never held.
func TestReorderPassesNonData(t *testing.T) {
	h := newRoHarness(time.Hour)
	h.r.Push(ip4tcp(1, 2, 0, 100, fACK))
	h.r.Push(ip4tcp(1, 2, 300, 100, fACK)) // held (gap at 100)
	n := len(h.out)

	h.r.Push(ip4tcp(1, 2, 999, 0, fACK)) // pure ACK
	udp := ip4tcp(1, 2, 0, 10, 0)
	udp[9] = 17 // UDP
	h.r.Push(udp)
	frag := ip4tcp(5, 6, 777, 100, fACK)
	binary.BigEndian.PutUint16(frag[6:8], 0x2000) // MF
	h.r.Push(frag)
	h.r.Push([]byte{0x45}) // truncated garbage
	if len(h.out) != n+4 {
		t.Fatalf("non-data packets were held: %d of 4 went out", len(h.out)-n)
	}

	// RST: the flow's held data goes out first, then the RST, and the flow is
	// forgotten.
	h.r.Push(ip4tcp(1, 2, 400, 0, fRST|fACK))
	if got := seqOf(h.out[len(h.out)-2]); got != 300 {
		t.Fatalf("held data was not released before the RST (got seq %d)", got)
	}
	// SYN of a new connection on the same ports starts fresh.
	h.r.Push(ip4tcp(1, 2, 5000, 0, fSYN))
	h.r.Push(ip4tcp(1, 2, 5001, 100, fACK))
	if got := seqOf(h.out[len(h.out)-1]); got != 5001 {
		t.Fatalf("data after a new SYN was held (last out seq %d)", got)
	}
}

// Sequence numbers wrap at 2^32; ordering must survive it.
func TestReorderSeqWrap(t *testing.T) {
	h := newRoHarness(time.Hour)
	s0 := uint32(1<<32 - 150)
	h.r.Push(ip4tcp(1, 2, s0, 100, fACK))     // ends at 2^32-50
	h.r.Push(ip4tcp(1, 2, s0+200, 100, fACK)) // past the wrap, ahead of the gap
	h.r.Push(ip4tcp(1, 2, s0+100, 100, fACK)) // fills it across the wrap
	if !eqSeqs(h.seqs(), []uint32{s0, s0 + 100, s0 + 200}) {
		t.Fatalf("across the wrap: %v", h.seqs())
	}
}

// Too much held behind one gap: give the gap up and release everything in
// order rather than buffering without bound.
func TestReorderOverflowReleases(t *testing.T) {
	h := newRoHarness(time.Hour)
	h.r.Push(ip4tcp(1, 2, 0, 10, fACK))
	for i := 0; i <= reorderFlowMax; i++ {
		h.r.Push(ip4tcp(1, 2, uint32(20+10*i), 10, fACK)) // gap at 10
	}
	if st := h.r.Stats(); st.Overflow != 1 {
		t.Fatalf("Overflow=%d, want 1", st.Overflow)
	}
	got := h.seqs()
	if len(got) != reorderFlowMax+2 {
		t.Fatalf("released %d, want %d", len(got), reorderFlowMax+2)
	}
	for i := 2; i < len(got); i++ {
		if !seqLT(got[i-1], got[i]) {
			t.Fatalf("overflow release out of order at %d: %v", i, got[i-1:i+1])
		}
	}
}

// A gap in one flow never holds another flow.
func TestReorderFlowsIndependent(t *testing.T) {
	h := newRoHarness(time.Hour)
	h.r.Push(ip4tcp(1, 2, 0, 100, fACK))
	h.r.Push(ip4tcp(1, 2, 200, 100, fACK)) // flow A held
	h.r.Push(ip4tcp(3, 4, 0, 100, fACK))   // flow B
	h.r.Push(ip4tcp(3, 4, 100, 100, fACK)) // flow B in order
	if len(h.out) != 3 {
		t.Fatalf("flow B was held behind flow A's gap: %d out, want 3", len(h.out))
	}
}

// Close releases everything still held, in order; later pushes pass through.
func TestReorderCloseFlushes(t *testing.T) {
	h := newRoHarness(time.Hour)
	h.r.Push(ip4tcp(1, 2, 0, 100, fACK))
	h.r.Push(ip4tcp(1, 2, 300, 100, fACK))
	h.r.Push(ip4tcp(1, 2, 200, 100, fACK))
	h.r.Close()
	if !eqSeqs(h.seqs(), []uint32{0, 200, 300}) {
		t.Fatalf("close released %v, want 0 200 300", h.seqs())
	}
	h.r.Push(ip4tcp(1, 2, 900, 100, fACK))
	if len(h.out) != 4 {
		t.Fatal("a push after close was held")
	}
	var nilR *reorderer
	nilR.Close() // nil-safe
}

// hold 0 disables reordering: everything passes in arrival order.
func TestReorderDisabled(t *testing.T) {
	h := newRoHarness(0)
	h.r.Push(ip4tcp(1, 2, 0, 100, fACK))
	h.r.Push(ip4tcp(1, 2, 200, 100, fACK))
	h.r.Push(ip4tcp(1, 2, 100, 100, fACK))
	if !eqSeqs(h.seqs(), []uint32{0, 200, 100}) {
		t.Fatalf("disabled reorderer changed the order: %v", h.seqs())
	}
}

// IPv6 TCP is reordered like IPv4.
func TestReorderIPv6(t *testing.T) {
	h := newRoHarness(time.Hour)
	h.r.Push(ip6tcp(1, 2, 0, 100, fACK))
	h.r.Push(ip6tcp(1, 2, 200, 100, fACK))
	h.r.Push(ip6tcp(1, 2, 100, 100, fACK))
	if !eqSeqs(h.seqs(), []uint32{0, 100, 200}) {
		t.Fatalf("IPv6: %v, want 0 100 200", h.seqs())
	}
}

// The real timer releases a held segment on its own (no further pushes).
func TestReorderTimerFires(t *testing.T) {
	out := make(chan []byte, 8)
	r := newReorderer(20*time.Millisecond, func(b []byte) { out <- b })
	defer r.Close()
	r.Push(ip4tcp(1, 2, 0, 100, fACK))
	r.Push(ip4tcp(1, 2, 200, 100, fACK)) // held, gap never fills
	<-out                                // seq 0
	select {
	case b := <-out:
		if seqOf(b) != 200 {
			t.Fatalf("timer released seq %d, want 200", seqOf(b))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the held segment was never released by the timer")
	}
}
