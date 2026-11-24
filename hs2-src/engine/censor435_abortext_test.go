package engine

import (
	"net/netip"
	"testing"
	"time"
)

// b435: the abort extension in the ping's time word. An own burst of 32+
// links on one Iran IP (the case the 6-bit count reads as a restart) is
// credited in full, whether a surviving link or only redialled links carry
// it; a new Iran instance never credits; and the 6-bit path is unchanged
// for an older edge.

func extWords(inst uint16, n uint32, legacy bool) (w0, w1 uint64) {
	w0 = 77 | ctrlMtuBit | 1400<<ctrlMtuShift
	if legacy {
		return w0 | (uint64(n%ctrlAbortMod)+1)<<ctrlAbortShift, uint64(time.Now().UnixNano())
	}
	w0 |= (uint64(n%ctrlAbortMod) + 1) << ctrlAbortShift
	w1 = ctrlAbortExtFlag | uint64(inst)<<ctrlAbortExtInstShift | uint64(n%ctrlAbortExtMod)<<ctrlAbortExtShift | 12345
	return w0, w1
}

func resetPeerAborts() {
	censorPeerAborts.mu.Lock()
	censorPeerAborts.ip = nil
	censorPeerAborts.mu.Unlock()
}

func TestCensorAbortExtBurst(t *testing.T) {
	resetPeerAborts()
	t0 := time.Now()
	for _, burst := range []uint32{31, 32, 33, 60, 63, 64, 120, 500, 2047} {
		ip := netip.AddrFrom4([4]byte{192, 0, 2, byte(burst % 250)})
		// case 1: one link survives the burst and carries it
		var a, b peerAbortPrev
		w0, w1 := extWords(7, 100, false)
		censorPeerAbortPingAt(ip, w0, w1, &a, t0)
		censorPeerAbortPingAt(ip, w0, w1, &b, t0)
		w0, w1 = extWords(7, 100+burst, false)
		if d, _ := censorPeerAbortPingAt(ip, w0, w1, &a, t0.Add(time.Second)); d != uint64(burst) {
			t.Fatalf("burst %d survivor: credited %d", burst, d)
		}
		if d, _ := censorPeerAbortPingAt(ip, w0, w1, &b, t0.Add(2*time.Second)); d != 0 {
			t.Fatalf("burst %d: second link credited again %d", burst, d)
		}
		// case 2: the burst takes every link; only redialled links report
		ip2 := netip.AddrFrom4([4]byte{198, 51, 100, byte(burst % 250)})
		links := make([]peerAbortPrev, burst)
		w0, w1 = extWords(9, 5, false)
		for i := range links {
			censorPeerAbortPingAt(ip2, w0, w1, &links[i], t0)
		}
		orphan := false
		for i := range links {
			orphan = censorPeerLinkEndP(ip2, links[i])
		}
		if !orphan {
			t.Fatalf("burst %d: last link end not an orphan", burst)
		}
		var n peerAbortPrev
		w0, w1 = extWords(9, 5+burst, false)
		d, sig := censorPeerAbortPingAt(ip2, w0, w1, &n, t0.Add(10*time.Second))
		want := uint64(burst)
		if burst >= ctrlAbortExtStale {
			want = 0
		}
		if d != want || !sig {
			t.Fatalf("burst %d all links: credited %d sig %v, want %d true", burst, d, sig, want)
		}
	}
}

// A restarted Iran (new instance) never credits, even when its count is
// above the old one and links ended; nor does a link handed between
// instances; an old-instance link first pings as a baseline.
func TestCensorAbortExtInstance(t *testing.T) {
	resetPeerAborts()
	t0 := time.Now()
	ip := netip.MustParseAddr("203.0.113.5")
	var a peerAbortPrev
	w0, w1 := extWords(100, 3, false)
	censorPeerAbortPingAt(ip, w0, w1, &a, t0)
	censorPeerLinkEndP(ip, a)
	var b peerAbortPrev
	w0, w1 = extWords(101, 40, false) // new process, already 40 own aborts
	if d, sig := censorPeerAbortPingAt(ip, w0, w1, &b, t0.Add(5*time.Second)); d != 0 || !sig {
		t.Fatalf("new instance first ping: %d %v", d, sig)
	}
	w0, w1 = extWords(101, 45, false)
	if d, _ := censorPeerAbortPingAt(ip, w0, w1, &b, t0.Add(6*time.Second)); d != 5 {
		t.Fatalf("new instance rise: %d", d)
	}
	// an old-instance link pings in between (upgrade overlap): its first ping
	// is a baseline, a rise it then sees itself is that instance's own
	var o peerAbortPrev
	w0, w1 = extWords(100, 3, false)
	censorPeerAbortPingAt(ip, w0, w1, &o, t0.Add(6*time.Second))
	w0, w1 = extWords(100, 9, false)
	if d, _ := censorPeerAbortPingAt(ip, w0, w1, &o, t0.Add(7*time.Second)); d != 6 {
		t.Fatalf("old instance own rise: %d", d)
	}
	// a link whose own instance changes is a baseline
	w0, w1 = extWords(101, 50, false)
	if d, _ := censorPeerAbortPingAt(ip, w0, w1, &o, t0.Add(8*time.Second)); d != 0 {
		t.Fatalf("link instance change credited %d", d)
	}
	// an rc9 (6-bit) edge on the same IP is another stream (instance 0)
	var l peerAbortPrev
	w0, w1 = extWords(0, 50, true)
	if d, _ := censorPeerAbortPingAt(ip, w0, w1, &l, t0.Add(9*time.Second)); d != 0 {
		t.Fatalf("legacy first ping credited %d", d)
	}
}

// An older edge (rc9: no flag in the time word) is read exactly as before;
// the 32-rise limit stays there (the case the extension removes).
func TestCensorAbortExtOldEdge(t *testing.T) {
	resetPeerAborts()
	t0 := time.Now()
	ip := netip.MustParseAddr("203.0.113.9")
	var a peerAbortPrev
	w0, w1 := extWords(0, 10, true)
	if d, sig := censorPeerAbortPingAt(ip, w0, w1, &a, t0); d != 0 || !sig {
		t.Fatal("old edge first ping")
	}
	w0, w1 = extWords(0, 15, true)
	if d, _ := censorPeerAbortPingAt(ip, w0, w1, &a, t0); d != 5 {
		t.Fatalf("old edge rise: %d", d)
	}
	w0, w1 = extWords(0, 15+33, true)
	if d, _ := censorPeerAbortPingAt(ip, w0, w1, &a, t0); d != 0 {
		t.Fatalf("old edge 33-rise: %d (read as a restart)", d)
	}
	// an old edge that does not report: nothing ever
	if d, sig := censorPeerAbortPingAt(netip.MustParseAddr("203.0.113.10"), 77|ctrlMtuBit, uint64(t0.UnixNano()), &peerAbortPrev{}, t0); d != 0 || sig {
		t.Fatal("non-reporting old edge")
	}
}

// The time word: an older edge's UnixNano never has the flag; the edge
// reads both forms back; a new edge's ping keeps every field an older exit
// reads (word 0) and the extension does not touch the 40-bit time.
func TestCensorAbortExtWire(t *testing.T) {
	now := time.Now().UnixNano()
	if uint64(now)&ctrlAbortExtFlag != 0 {
		t.Fatal("UnixNano has bit 63")
	}
	ext := ctrlAbortExtFlag | ctrlAbortExtInstMask<<ctrlAbortExtInstShift | ctrlAbortExtMask<<ctrlAbortExtShift
	if ext&ctrlTimeMask != 0 || ctrlAbortExtInstMask<<ctrlAbortExtInstShift&ctrlAbortExtMask<<ctrlAbortExtShift != 0 ||
		(ctrlAbortExtInstMask<<ctrlAbortExtInstShift|ctrlAbortExtMask<<ctrlAbortExtShift)&ctrlAbortExtFlag != 0 {
		t.Fatal("extension fields overlap")
	}
	for _, rtt := range []int64{1, 1000, 35e6, 3e9, 60e9} {
		w := ctrlPingTime(now-rtt, ext)
		if got := ctrlPongRTT(w, now); got != rtt {
			t.Fatalf("ext rtt %d: %d", rtt, got)
		}
		if got := ctrlPongRTT(ctrlPingTime(now-rtt, 0), now); got != rtt {
			t.Fatalf("plain rtt %d: %d", rtt, got)
		}
	}
	// across a 2^40 wrap of the low time bits
	base := int64(1)<<40*1000 - 5
	if got := ctrlPongRTT(ctrlPingTime(base, ext), base+10); got != 10 {
		t.Fatalf("wrap rtt %d", got)
	}
	// b435 v2: a wall clock stepped back between ping and pong is a
	// negative round trip (dropped), never ~2^40 ns, in both forms
	for _, back := range []int64{1, 50e6, 2e9, 300e9} {
		if got := ctrlPongRTT(ctrlPingTime(now, ext), now-back); got != -back {
			t.Fatalf("ext clock step back %d: rtt %d", back, got)
		}
		if got := ctrlPongRTT(ctrlPingTime(now, 0), now-back); got >= 0 {
			t.Fatalf("plain clock step back %d: rtt %d", back, got)
		}
		if got := ctrlPongRTT(ctrlPingTime(base, ext), base+5-back); got != 5-back {
			t.Fatalf("wrap clock step back %d: rtt %d", back, got)
		}
	}
	if censorInstance == 0 || uint64(censorInstance) > ctrlAbortExtInstMask {
		t.Fatalf("instance %d", censorInstance)
	}
	ip := netip.MustParseAddr("192.0.2.200")
	w0, e := censorAbortWords(ip)
	if w0 != censorAbortBits(ip) || e&ctrlAbortExtFlag == 0 || uint16(e>>ctrlAbortExtInstShift&ctrlAbortExtInstMask) != censorInstance {
		t.Fatalf("words %#x %#x", w0, e)
	}
	if a, b := censorAbortWords(netip.Addr{}); a != 0 || b != 0 {
		t.Fatal("invalid ip")
	}
}
