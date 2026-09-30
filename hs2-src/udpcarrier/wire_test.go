package udpcarrier

import (
	"encoding/binary"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Wire compatibility with old peers. The feedback frame grew a 5-byte extension
// (owdTicks + flags) and data datagrams grew a stamped form (tagDataTS); both
// must stay compatible with a peer that speaks only the original wire:
//
//   - a 36-byte feedback report (no extension) must still decode, and must NOT
//     be read past its end or claim OWD / stamp support;
//   - the pacer must send the unstamped tagData form until the peer's feedback
//     shows it understands stamped datagrams (fbStamps). An old peer that never
//     advertises stamps must therefore only ever receive tagData (0x00).

func TestFeedbackWireCompat(t *testing.T) {
	// A fully-populated report round-trips through the extended (41-byte) form.
	fb := feedback{
		sendNanos:      123456789,
		echoNanos:      987654321,
		echoDelayNanos: 4242,
		rxDataBytes:    1 << 40,
		lossPPM:        250000,
		owdTicks:       0xdeadbeef,
		flags:          fbOWD | fbStamps,
	}
	enc := fb.encode()
	if len(enc) != feedbackLenExt {
		t.Fatalf("encoded length %d, want %d", len(enc), feedbackLenExt)
	}
	got, ok := decodeFeedback(enc)
	if !ok || got != fb {
		t.Fatalf("41-byte round-trip: ok=%v got %+v want %+v", ok, got, fb)
	}

	// An OLD peer's report ends at byte 36 (feedbackLen). Decoding it must
	// succeed, read nothing past the buffer, and report no OWD and no stamp
	// support — a truncated buffer must not have owdTicks/flags read off its end.
	old := enc[:feedbackLen]
	go36, ok := decodeFeedback(old)
	if !ok {
		t.Fatal("36-byte (old peer) report rejected")
	}
	if go36.owdTicks != 0 || go36.flags != 0 {
		t.Fatalf("36-byte report leaked owd/flags off the end: owdTicks=%d flags=%#x", go36.owdTicks, go36.flags)
	}
	if go36.flags&fbOWD != 0 || go36.flags&fbStamps != 0 {
		t.Fatal("36-byte report must not claim OWD or stamp support")
	}
	// The base fields must still decode from the 36-byte form.
	if go36.sendNanos != fb.sendNanos || go36.echoNanos != fb.echoNanos ||
		go36.echoDelayNanos != fb.echoDelayNanos || go36.rxDataBytes != fb.rxDataBytes ||
		go36.lossPPM != fb.lossPPM {
		t.Fatalf("36-byte base fields wrong: %+v", go36)
	}
	// Anything shorter than the minimum is rejected (no short read).
	if _, ok := decodeFeedback(enc[:feedbackLen-1]); ok {
		t.Fatal("a 35-byte buffer must be rejected")
	}
	if _, ok := decodeFeedback(nil); ok {
		t.Fatal("an empty buffer must be rejected")
	}
}

func TestStampArithmetic(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	// A 10 ms one-way delay is 10ms / 125us = 80 stampTicks.
	send := stampOf(base)
	recv := stampOf(base.Add(10 * time.Millisecond))
	if d := recv - send; d != uint32(10*time.Millisecond/stampTick) {
		t.Fatalf("10ms delay = %d ticks, want %d", d, 10*time.Millisecond/stampTick)
	}
	// Differences are taken modulo 2^32, so they are correct across a wrap: a
	// send just below the 32-bit wrap and a receive just past it still yield the
	// true delay (the carrier's noteOWD uses exactly this int32(recv-send)).
	hi := time.Unix(0, int64(stampTick)*(1<<32-40)) // stamp ≈ 2^32 - 40
	lo := hi.Add(80 * stampTick)                    // wraps past 2^32
	if d := int32(stampOf(lo) - stampOf(hi)); d != 80 {
		t.Fatalf("delay across the 32-bit wrap = %d ticks, want 80", d)
	}
}

// The pacer stamps a data datagram only once the peer has shown (via fbStamps in
// its feedback) that it understands the stamped form. A peer that never
// advertises stamps — an old peer — must only ever receive the unstamped tagData
// datagram it understands. This is the carrier's send path (newConn wires the
// pacer's stamps flag to c.peerStamps, set by onFeedback on fbStamps).
func TestPacerStampsOnlyForStampingPeer(t *testing.T) {
	var mu sync.Mutex
	var sent [][]byte
	write := func(b []byte) error {
		mu.Lock()
		sent = append(sent, append([]byte(nil), b...))
		mu.Unlock()
		return nil
	}
	rc := newRateControl()
	stamps := new(atomic.Bool)
	p := newPacer(rc, write, 64, stamps)
	defer p.close()

	// A data shard: byte 5 is zero so the pacer treats it as data (not parity;
	// fec.IsParity is pkt[5] != 0), taking the paced data path.
	shard := make([]byte, 120)
	waitForSent := func(n int) []byte {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			have := len(sent)
			var b []byte
			if have >= n {
				b = sent[n-1]
			}
			mu.Unlock()
			if b != nil {
				return b
			}
			time.Sleep(2 * time.Millisecond)
		}
		t.Fatalf("pacer did not emit datagram %d", n)
		return nil
	}

	// Peer has NOT advertised stamps: the datagram must be unstamped tagData
	// (5-byte header [tag][wireSeq:4], no send stamp).
	p.enqueue(shard)
	dg := waitForSent(1)
	if dg[0] != tagData {
		t.Fatalf("peer without stamps got tag %#x, want tagData %#x", dg[0], tagData)
	}
	if len(dg) != 5+len(shard) {
		t.Fatalf("unstamped datagram len %d, want %d", len(dg), 5+len(shard))
	}

	// Once the peer advertises stamps, the pacer stamps: tagDataTS, 9-byte header
	// [tag][wireSeq:4][sendStamp:4], and the stamp is ~now.
	stamps.Store(true)
	p.enqueue(shard)
	dg = waitForSent(2)
	if dg[0] != tagDataTS {
		t.Fatalf("stamping peer got tag %#x, want tagDataTS %#x", dg[0], tagDataTS)
	}
	if len(dg) != 9+len(shard) {
		t.Fatalf("stamped datagram len %d, want %d", len(dg), 9+len(shard))
	}
	st := binary.BigEndian.Uint32(dg[5:9])
	if d := int32(stampOf(time.Now()) - st); d < 0 || d > int32(2*time.Second/stampTick) {
		t.Fatalf("send stamp %d is not near now (delta %d ticks)", st, d)
	}
}
