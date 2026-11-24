package censor

import (
	"testing"
)

// b414 v4: Iran's own aborts take the last links on ipA while its handshakes
// are dropped; no ping can carry the report until a new link's first ping
// ~50 s later. With the orphan hold the resets wait for that report and are
// claimed; without a report they are counted after PeerOrphanHold.
func orphanBurst(t *testing.T, report bool) *sim {
	s := newSim(t, SideKharej)
	s.ev(event{kind: evPeerAbort, remote: ipA, n: 0}) // a reporting Iran
	s.quiet(3)
	// b414 v5: an earlier own-abort round on ipA, reported by the links it
	// left: the evidence the orphan hold needs
	s.death(ipA, ipK1, Death{Class: DeathRST, LifeMs: 90000, Bytes: 50 << 20, HaveTCP: true, LastSentMs: 2, MinRTTUs: 44000})
	s.ev(event{kind: evPeerAbort, remote: ipA, n: 1})
	s.quiet(3)
	for k := 0; k < 6; k++ {
		s.death(ipA, ipK1, Death{Class: DeathRST, LifeMs: uint32(200000 + 1000*k), Bytes: 200 << 20,
			HaveTCP: true, LastSentMs: 2, MinRTTUs: 44000})
	}
	s.ev(event{kind: evPeerOrphan, remote: ipA})
	s.tick()
	for i := 0; i < 10; i++ { // 50 s: no link gets through
		s.healthy(ipB)
		s.tick()
	}
	if sc := s.e.scopeOf(ipA); sc == nil || len(sc.held) != 6 {
		t.Fatalf("orphaned resets not held: %+v", sc)
	}
	if report {
		s.ev(event{kind: evPeerAbort, remote: ipA, n: 6}) // the new link's first ping
	}
	s.quiet(40)
	return s
}

func TestPeerOrphanHeldUntilReport(t *testing.T) {
	s := orphanBurst(t, true)
	for _, l := range s.lines {
		if field(l, "class") == "rst-inject" {
			t.Fatalf("own aborts read as rst-inject: %s", l)
		}
	}
	if sc := s.e.scopeOf(ipA); len(sc.held) != 0 || sc.peerOrphan {
		t.Fatalf("held %d orphan %v", len(sc.held), sc.peerOrphan)
	}
}

func TestPeerOrphanReleasedAfterHold(t *testing.T) {
	s := orphanBurst(t, false)
	sc := s.e.scopeOf(ipA)
	if len(sc.held) != 0 {
		t.Fatalf("still held after %v: %d", 40*tick, len(sc.held))
	}
	if 40*tick < PeerOrphanHold {
		t.Fatal("test too short")
	}
}

// An older Iran (no report ever): an orphan event changes nothing.
func TestPeerOrphanOldPeer(t *testing.T) {
	s := newSim(t, SideKharej)
	s.ev(event{kind: evPeerOrphan, remote: ipA})
	s.death(ipA, ipK1, Death{Class: DeathRST, LifeMs: 1000, HaveTCP: true})
	if sc := s.e.scopeOf(ipA); sc != nil && (sc.peerOrphan || len(sc.held) != 0) {
		t.Fatal("orphan without a reporting Iran")
	}
}

// b414 v5 (held-out #5 run-c): an injected reset kills the IP's last links
// with no own-abort rise in the window before; the orphan must not hold the
// resets (v1 peerHold), so the episode still sees them as resets.
func TestPeerOrphanNoRiseNotHeld(t *testing.T) {
	s := newSim(t, SideKharej)
	s.ev(event{kind: evPeerAbort, remote: ipA, n: 0})
	s.ev(event{kind: evPeerAbort, remote: ipA, n: 2}) // an old churn round
	// ... older than the window
	s.quiet(int(PeerOrphanHold/tick) + 2)
	for k := 0; k < 6; k++ {
		s.death(ipA, ipK1, Death{Class: DeathRST, LifeMs: uint32(200000 + 1000*k), Bytes: 200 << 20,
			HaveTCP: true, LastSentMs: 2, MinRTTUs: 44000})
	}
	s.ev(event{kind: evPeerOrphan, remote: ipA})
	sc := s.e.scopeOf(ipA)
	if sc.peerOrphan {
		t.Fatal("orphan hold with no own-abort rise")
	}
	s.tick()
	s.tick()
	if len(sc.held) != 0 {
		t.Fatalf("resets still held after peerHold: %d", len(sc.held))
	}
}

// b414 v5: a rise inside the window arms the hold; a later report clears it.
func TestPeerOrphanRecentRiseHeld(t *testing.T) {
	s := newSim(t, SideKharej)
	s.ev(event{kind: evPeerAbort, remote: ipA, n: 0})
	s.ev(event{kind: evPeerAbort, remote: ipA, n: 3})
	s.quiet(3)
	s.ev(event{kind: evPeerOrphan, remote: ipA})
	if !s.e.scopeOf(ipA).peerOrphan {
		t.Fatal("no orphan hold after a recent own-abort rise")
	}
	s.ev(event{kind: evPeerAbort, remote: ipA, n: 0})
	if s.e.scopeOf(ipA).peerOrphan {
		t.Fatal("orphan hold kept after the next link's first ping")
	}
}
