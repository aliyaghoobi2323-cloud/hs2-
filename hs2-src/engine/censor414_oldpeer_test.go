package engine

import (
	"net/netip"
	"testing"
)

// rel69r-rc8: an rc7 (or older) Iran builds its ping word as seq | idle |
// mtu flag | pmtu<<40 and leaves bits 56..61 zero. An rc8 Kharej must then
// run exactly as v1: no credit, no report sig, no reporting link, no state
// for the IP (so the detector never holds a reset for a report).
func TestCensorPeerAbortOldEdgeIsV1(t *testing.T) {
	ip := netip.MustParseAddr("192.0.2.77")
	var prev uint8
	for _, seq := range []uint64{0, 1, 77, ctrlSeqMask} {
		for _, pm := range []uint64{0, 1400, 1500, 0xFFFF} {
			for _, idle := range []uint64{0, ctrlIdleBit} {
				wire := seq | idle | ctrlMtuBit | pm<<ctrlMtuShift
				if d, sig := censorPeerAbortDelta(ip, wire, &prev); d != 0 || sig {
					t.Fatalf("rc7 wire %#x: got %d,%v want 0,false", wire, d, sig)
				}
			}
		}
	}
	if prev != 0 {
		t.Fatalf("rc7 edge marked as reporting: prev %d", prev)
	}
	if censorPeerLinkEnd(ip, prev) {
		t.Fatal("rc7 edge's link end read as an orphan")
	}
	censorPeerAborts.mu.Lock()
	_, ok := censorPeerAborts.ip[ip]
	censorPeerAborts.mu.Unlock()
	if ok {
		t.Fatal("rc7 edge left per-IP state")
	}
}

// The other mix (an rc8 Iran with an rc7 Kharej): the abort bits must not
// touch what an rc7 exit reads (pmtu: 16 bits at 40, flags 62/63) nor the
// echoed seq the edge matches (ctrlSeqMask).
func TestCensorAbortBitsDisjoint(t *testing.T) {
	ab := ctrlAbortMask << ctrlAbortShift
	if ab&(ctrlSeqMask|0xFFFF<<ctrlMtuShift|ctrlMtuBit|ctrlIdleBit) != 0 {
		t.Fatalf("abort bits %#x overlap another field", ab)
	}
	for n := uint64(1); n <= 63; n++ {
		wire := uint64(12345) | ctrlMtuBit | 1420<<ctrlMtuShift | n<<ctrlAbortShift
		if pm := uint32(wire>>ctrlMtuShift) & 0xFFFF; pm != 1420 {
			t.Fatalf("n=%d: rc7 exit reads pmtu %d", n, pm)
		}
		if wire&ctrlSeqMask != 12345 || wire&ctrlIdleBit != 0 {
			t.Fatalf("n=%d: seq/idle disturbed", n)
		}
	}
}
