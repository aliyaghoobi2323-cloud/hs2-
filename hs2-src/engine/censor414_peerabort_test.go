package engine

import (
	"net/netip"
	"testing"
	"time"
)

// b414: the Kharej end reads per-IP deltas of the edge's abort count (wire
// values 1..63); a link's first ping is a baseline, the IP's first sighting
// is the sig (the edge reports), a repeat and a stale (overtaken) ping give
// 0, a wrap is a small delta, a restarted edge (count from 1 again) gives no
// credit, and an older edge's 0 bits never give anything.
func TestCensorPeerAbortDelta(t *testing.T) {
	ip := netip.MustParseAddr("192.0.2.9")
	w := func(n uint64) uint64 { return (n&ctrlAbortMask)<<ctrlAbortShift | ctrlMtuBit | 1500<<ctrlMtuShift | 77 }
	var a, b uint8 // two links on ip
	type st struct {
		l       *uint8
		n, want uint64
		sig     bool
	}
	steps := []st{
		{&a, 5, 0, true}, {&a, 5, 0, false}, {&a, 9, 4, false}, // sig, repeat, rise
		{&b, 9, 0, false}, {&b, 11, 2, false}, {&a, 11, 0, false}, // baseline; one rise, credited once
		{&b, 10, 0, false}, {&a, 30, 19, false}, // overtaken ping; rise
		{&a, 60, 30, false}, {&a, 3, 6, false}, // big rise; wrap 60 -> 3 is 6 on the 1..63 ring
		{&b, 3, 0, false}, // b jumps with no own rise yet seen as a credit (already counted)
	}
	for i, c := range steps {
		got, sig := censorPeerAbortDelta(ip, w(c.n), c.l)
		if got != c.want || sig != c.sig {
			t.Fatalf("step %d (n=%d): got %d,%v want %d,%v", i, c.n, got, sig, c.want, c.sig)
		}
	}
	// a restarted edge: new links, count from 1 again (IP last 3)
	var r uint8
	for i, c := range []st{{&r, 1, 0, false}, {&r, 2, 0, false}, {&r, 4, 2, false}} {
		got, sig := censorPeerAbortDelta(ip, w(c.n), c.l)
		if got != c.want || sig != c.sig {
			t.Fatalf("restart step %d (n=%d): got %d,%v want %d,%v", i, c.n, got, sig, c.want, c.sig)
		}
	}
	old := netip.MustParseAddr("192.0.2.10")
	var o uint8
	for i := 0; i < 5; i++ {
		if got, sig := censorPeerAbortDelta(old, ctrlMtuBit|uint64(i), &o); got != 0 || sig {
			t.Fatalf("older edge: %d %v", got, sig)
		}
	}
	me := netip.MustParseAddr("192.0.2.11")
	if b := censorAbortBits(me) >> ctrlAbortShift; b != 1 {
		t.Fatalf("first bits %d, want 1 (reports, 0 aborts)", b)
	}
	censorNoteOwnAbort(me)
	censorNoteOwnAbort(me)
	if b := censorAbortBits(me) >> ctrlAbortShift; b != 3 {
		t.Fatalf("own bits %d", b)
	}
	for i := 0; i < 61; i++ {
		censorNoteOwnAbort(me)
	}
	if b := censorAbortBits(me) >> ctrlAbortShift; b != 1 { // 63 aborts: 63 mod 63 + 1, never 0
		t.Fatalf("wrapped bits %d", b)
	}
	if censorAbortBits(me)&ctrlSeqMask != 0 || censorAbortBits(me)&(ctrlMtuBit|ctrlIdleBit) != 0 || censorAbortBits(me)>>ctrlMtuShift&0xFFFF != 0 {
		t.Fatal("abort bits overlap seq/mtu/idle")
	}
	if censorAbortBits(netip.Addr{}) != 0 {
		t.Fatal("no IP must send 0")
	}
}

// b414 v4: Iran's own aborts take the last links on an IP before any ping
// of theirs shows the rise; the next link's first ping credits it, bounded
// by the links that ended, and only while the IP's count is fresh. The
// last link ending tells the caller (the IP is orphaned) and the next
// link's first ping comes back as sig (a report is due).
func TestCensorPeerAbortOrphan(t *testing.T) {
	ip := netip.MustParseAddr("192.0.2.40")
	w := func(n uint64) uint64 { return (n&ctrlAbortMask)<<ctrlAbortShift | 77 }
	t0 := time.Unix(1000, 0)
	var a, b, c uint8
	if _, sig := censorPeerAbortDeltaAt(ip, w(4), &a, t0); !sig {
		t.Fatal("first sighting is the sig")
	}
	censorPeerAbortDeltaAt(ip, w(4), &b, t0)
	if censorPeerLinkEnd(ip, a) {
		t.Fatal("b still up")
	}
	if !censorPeerLinkEnd(ip, b) {
		t.Fatal("last link ended: orphan")
	}
	// Iran aborted 3 more (count 7); the new link's first ping, 50 s on
	d, sig := censorPeerAbortDeltaAt(ip, w(7), &c, t0.Add(50*time.Second))
	if d != 2 || !sig { // 3 of the ring, at most the 2 links that ended
		t.Fatalf("orphan credit %d,%v want 2,true", d, sig)
	}
	if d, _ := censorPeerAbortDeltaAt(ip, w(7), &c, t0.Add(53*time.Second)); d != 0 {
		t.Fatalf("credited twice: %d", d)
	}
	// stale count: older than ctrlAbortLastAge gives no credit
	ip2 := netip.MustParseAddr("192.0.2.41")
	var x, y uint8
	censorPeerAbortDeltaAt(ip2, w(2), &x, t0)
	censorPeerLinkEnd(ip2, x)
	if d, sig := censorPeerAbortDeltaAt(ip2, w(5), &y, t0.Add(ctrlAbortLastAge+time.Second)); d != 0 || !sig {
		t.Fatalf("stale: %d,%v want 0,true", d, sig)
	}
	// restarted Iran (count from 1 again, below the IP's last): no credit
	ip3 := netip.MustParseAddr("192.0.2.42")
	var p, q uint8
	censorPeerAbortDeltaAt(ip3, w(20), &p, t0)
	censorPeerLinkEnd(ip3, p)
	if d, _ := censorPeerAbortDeltaAt(ip3, w(3), &q, t0.Add(10*time.Second)); d != 0 {
		t.Fatalf("restart: %d", d)
	}
	// no link ended: a new link's first ping above the count is a baseline
	ip4 := netip.MustParseAddr("192.0.2.43")
	var m, k uint8
	censorPeerAbortDeltaAt(ip4, w(2), &m, t0)
	if d, sig := censorPeerAbortDeltaAt(ip4, w(6), &k, t0.Add(time.Second)); d != 0 || sig {
		t.Fatalf("live baseline: %d,%v", d, sig)
	}
	// a link that never carried a count is no reporting link
	if censorPeerLinkEnd(ip4, 0) {
		t.Fatal("non-reporting link")
	}
}
