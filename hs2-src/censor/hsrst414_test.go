package censor

import "testing"

// b414 v6 (held-out #6 run-b/run-c rst): on an IP with few live links an
// immediate-reset injector kills the established links and also the
// redials in their TLS stage. The handshake resets then outnumber the link
// resets and the episode read handshake-block. A handshake-block censor
// drops the hello and does not kill established links, so hello resets plus
// fast link resets on the same IP are one reset injector.

// khFewLinks runs 8 Kharej ticks on ipA with nLink established-link resets
// (fastLink of them with injection timing) spread over the first ticks and
// two failed redials per tick at stage st.
func khFewLinks(s *sim, st DialStage, nLink, fastLink int) {
	s.quiet(3)
	for i := 0; i < 8; i++ {
		s.healthy(ipA)
		s.healthy(ipB)
		s.addUsers(50)
		for k := 0; k < 3 && nLink > 0; k++ {
			sent := uint32(400) // a peer's reset: long after our last send
			if fastLink > 0 {
				sent, fastLink = 5, fastLink-1
			}
			s.death(ipA, ipK1, Death{Class: DeathRST, LifeMs: 400000 + uint32(k)*1000, Bytes: 1 << 20,
				HaveTCP: true, LastSentMs: sent, MinRTTUs: 90000})
			nLink--
		}
		s.dial(ipA, ipK1, st)
		s.dial(ipA, ipK1, st)
		s.tick()
	}
	s.quiet(24)
}

// The #6 shape: 10 link resets (3 fast), 16 handshake resets -> rst-inject.
func TestHsResetInjectIsRST(t *testing.T) {
	s := newSim(t, SideKharej)
	khFewLinks(s, DialHelloReset, 10, 3)
	s.expect(ipA, "rst-inject", confMed)
}

// A handshake-block (sni/tlsint: the hello is dropped, the handshakes time
// out) with the harness churn killing established links by the peer's reset,
// some of them fast by chance: stays handshake-block.
func TestHsTimeoutWithLinkResetsStaysHandshake(t *testing.T) {
	s := newSim(t, SideKharej)
	khFewLinks(s, DialHelloTimeout, 10, 3)
	s.expect(ipA, "handshake-block", confMed)
}

// A censor that resets the ClientHello but leaves established links alone
// (no link resets): stays handshake-block.
func TestHsResetNoLinkResetsStaysHandshake(t *testing.T) {
	s := newSim(t, SideKharej)
	khFewLinks(s, DialHelloReset, 0, 0)
	s.expect(ipA, "handshake-block", confMed)
}

// Hello resets with established links dying by the peer's reset only (no
// injection timing on any of them): no injector evidence, stays
// handshake-block.
func TestHsResetSlowLinkResetsStaysHandshake(t *testing.T) {
	s := newSim(t, SideKharej)
	khFewLinks(s, DialHelloReset, 10, 0)
	s.expect(ipA, "handshake-block", confMed)
}

func TestHsResetInjectRule(t *testing.T) {
	var c ctr
	c.stage[DialHelloReset], c.stage[DialHelloTimeout] = 5, 4
	if !hsResetInject(&c, 3, 1) {
		t.Fatal("5/9 hello resets, 3 link resets, 1 fast: want true")
	}
	if hsResetInject(&c, 2, 2) {
		t.Fatal("rst < 3: want false")
	}
	if hsResetInject(&c, 10, 0) {
		t.Fatal("no fast link reset: want false")
	}
	c.stage[DialHelloTimeout] = 5
	if hsResetInject(&c, 10, 5) {
		t.Fatal("hello resets 5/10 are not the majority: want false")
	}
	if hsResetInject(&ctr{}, 10, 5) {
		t.Fatal("no failed handshakes: want false")
	}
}
