package udpcarrier

import (
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
)

// Step 12 (dgsec) hardening tests. Each one failed before its fix.

// TestTrackWireForgedJumpBounded: the cleartext wireSeq is forgeable by anyone
// who can put a datagram in front of the carrier (on-path, or blind spoofing of
// the peer's address on the listener's unconnected socket). A single forged
// datagram with a wireSeq ~2^30 ahead used to make trackWire slide its window
// one seq at a time — about a billion iterations, seconds of a stalled decode
// loop per packet — and book a billion "lost" datagrams. It must now cost
// O(window), book nothing for an implausible jump, and let the real sequence
// resume being counted.
func TestTrackWireForgedJumpBounded(t *testing.T) {
	c := &Conn{}
	for s := uint32(1000); s < 1100; s++ {
		c.trackWire(s)
	}
	start := time.Now()
	c.trackWire(1100 + 1<<30) // forged: far ahead, still "ahead" in int32 terms
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("one forged wireSeq took %v in trackWire (unbounded slide)", d)
	}
	if lost := c.wireLost.Load(); lost > wireReorderWin {
		t.Fatalf("one forged wireSeq booked %d lost datagrams", lost)
	}
	r0 := c.wireRecv.Load()
	for s := uint32(1100); s < 1300; s++ {
		c.trackWire(s)
	}
	if got := c.wireRecv.Load() - r0; got < 190 {
		t.Fatalf("after the forged datagram only %d/200 real ones were counted", got)
	}
	if lost := c.wireLost.Load(); lost > wireReorderWin {
		t.Fatalf("lost=%d after resuming the real sequence", lost)
	}
}

// TestTrackWireBulkSlideExact: a real gap (moderate, below the implausible
// threshold) is still counted exactly, now in bulk instead of per seq.
func TestTrackWireBulkSlideExact(t *testing.T) {
	c := &Conn{}
	for s := uint32(0); s < 100; s++ {
		if s%10 != 3 { // 9 of the first 100 missing
			c.trackWire(s)
		}
	}
	c.trackWire(100 + 1000) // a 1000-datagram gap
	for s := uint32(1101); s < 1101+wireConfirmWin*2; s++ {
		c.trackWire(s)
	}
	// Every seq below the current base is resolved: the 10 skipped in
	// 0..99 (one per ten), plus the 1000 never sent in 100..1099.
	base := c.wireBase
	want := uint64(0)
	for s := uint32(0); s < base; s++ {
		if (s < 100 && s%10 == 3) || (s >= 100 && s < 1100) {
			want++
		}
	}
	if got := c.wireLost.Load(); got != want {
		t.Fatalf("lost=%d want %d (base %d)", got, want, base)
	}
}

// TestProbeReplayNotEchoed: the probe echo is authenticated but carried no
// freshness, so one captured probe could be replayed forever from any (spoofed)
// source: an unlimited 1:1 reflector, and an active-probing oracle (replay an
// observed probe, get an echo: this is an hs2 server). A probe is now echoed
// at most once, and only while its timestamp is inside the window.
func TestProbeReplayNotEchoed(t *testing.T) {
	shared := testShared()
	pr := newProbeResponder(shared)
	echoes := 0
	echo := func([]byte) { echoes++ }
	nonce := []byte("nonce-01")
	now := time.Now()

	p := buildProbe(shared, nonce, 0, now.UnixNano())
	if !pr.handle(p, echo) || echoes != 1 {
		t.Fatalf("a fresh probe was not echoed (echoes=%d)", echoes)
	}
	for i := 0; i < 5; i++ {
		pr.handle(append([]byte(nil), p...), echo)
	}
	if echoes != 1 {
		t.Fatalf("a replayed probe was echoed %d times", echoes)
	}
	// The next probe of the same run is fresh.
	if pr.handle(buildProbe(shared, nonce, 1, now.UnixNano()), echo); echoes != 2 {
		t.Fatalf("the next probe of a run was not echoed (echoes=%d)", echoes)
	}
	// Stale or far-future timestamps are refused (and swallowed: they are
	// authentic probes, never a candidate handshake).
	for _, off := range []time.Duration{-10 * time.Minute, 10 * time.Minute} {
		q := buildProbe(shared, []byte("nonce-02"), 0, now.Add(off).UnixNano())
		if !pr.handle(q, echo) {
			t.Fatalf("an authentic stale probe was not consumed")
		}
	}
	if echoes != 2 {
		t.Fatalf("a probe outside the time window was echoed (echoes=%d)", echoes)
	}
	// A forged probe is still not consumed (falls through to the handshake).
	bad := append([]byte(nil), p...)
	bad[len(bad)-1] ^= 1
	if pr.handle(bad, echo) {
		t.Fatalf("a forged probe was consumed")
	}
}

// TestProbeMemoryBounded: the replay memory cannot grow without bound; when
// full it refuses (fails closed) instead of echoing unchecked.
func TestProbeMemoryBounded(t *testing.T) {
	shared := testShared()
	pr := newProbeResponder(shared)
	now := time.Now()
	echoes := 0
	for i := 0; i < probeMemCap+10; i++ {
		var nonce [8]byte
		nonce[0], nonce[1], nonce[2] = byte(i), byte(i>>8), byte(i>>16)
		pr.handle(buildProbe(shared, nonce[:], 0, now.UnixNano()), func([]byte) { echoes++ })
	}
	if echoes > probeMemCap {
		t.Fatalf("echoed %d probes past a memory cap of %d", echoes, probeMemCap)
	}
	if n := pr.size(); n > probeMemCap {
		t.Fatalf("probe memory holds %d entries, cap %d", n, probeMemCap)
	}
}

// TestCachedMessage2Bounded: the listener answers a retransmitted message 1
// from the cache so a lost message 2 is recovered. It used to do so for the
// carrier's whole life, so anyone who saw message 1 could make the server send
// message 2 to that address without limit. It is now bounded by what a real
// initiator ever needs (dialHandshake makes at most 12 attempts).
func TestCachedMessage2Bounded(t *testing.T) {
	shared := testShared()
	ln, err := Listen("127.0.0.1:0", shared, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	server, client, err := staticPair(shared)
	if err != nil {
		t.Fatal(err)
	}
	ini, err := core.NewInitiator(client, server.Public, shared)
	if err != nil {
		t.Fatal(err)
	}
	m1, err := ini.WriteMessage1()
	if err != nil {
		t.Fatal(err)
	}
	uc, err := net.DialUDP("udp", nil, ln.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer uc.Close()

	const sends = 40
	for i := 0; i < sends; i++ {
		if _, err := uc.Write(m1); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	// Count the replies that are message 2 (the carrier also sends feedback).
	var m2 []byte
	got := 0
	buf := make([]byte, 2048)
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		uc.SetReadDeadline(deadline)
		n, err := uc.Read(buf)
		if err != nil {
			break
		}
		b := buf[:n]
		if m2 == nil && b[0] != tagCtrl {
			if _, _, err := ini.ReadMessage2Payload(append([]byte(nil), b...)); err == nil {
				m2 = append([]byte(nil), b...)
				got++
			}
			continue
		}
		if m2 != nil && bytes.Equal(b, m2) {
			got++
		}
	}
	if m2 == nil {
		t.Fatalf("no message 2 at all")
	}
	if got > 1+maxM2Resends {
		t.Fatalf("message 2 sent %d times for %d copies of message 1 (bound %d)", got, sends, 1+maxM2Resends)
	}
}

// TestPacingDecaysWithoutFeedback: the rate model is driven only by the peer's
// feedback. If the feedback stops (return path dead, peer gone, address
// reassigned) the sender used to keep pacing at its last rate — up to maxRate —
// until the 15 s liveness timeout. Unanswered sending must back off.
func TestPacingDecaysWithoutFeedback(t *testing.T) {
	r := newRateControl()
	t0 := time.Now()
	r.onFeedback(t0, 0, 0.03, 0, 0, 0, false)
	r.mu.Lock()
	r.rate = 50e6
	r.probeEnd = time.Time{}
	r.mu.Unlock()
	if got := r.pacingRate(t0.Add(200 * time.Millisecond)); got != 50e6 {
		t.Fatalf("with fresh feedback the rate changed: %v", got)
	}
	got := r.pacingRate(t0.Add(5 * time.Second))
	if got > 50e6/4 {
		t.Fatalf("5 s without feedback still pacing at %.0f B/s (was 50e6)", got)
	}
	if got < r.minRate {
		t.Fatalf("decayed below the floor: %v", got)
	}
	if got := r.pacingRate(t0.Add(60 * time.Second)); got != r.minRate {
		t.Fatalf("a minute without feedback: %v, want the floor %v", got, r.minRate)
	}
	// Fresh feedback restores normal pacing at once.
	r.onFeedback(t0.Add(61*time.Second), 0, 0.03, 0, 0, 0, false)
	r.mu.Lock()
	r.rate = 50e6
	r.mu.Unlock()
	if got := r.pacingRate(t0.Add(61*time.Second + 100*time.Millisecond)); got != 50e6 {
		t.Fatalf("pacing did not recover after feedback resumed: %v", got)
	}
}
