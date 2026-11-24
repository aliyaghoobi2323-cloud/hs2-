package censor

import (
	"math/rand"
	"strings"
	"testing"
)

// b414 (recall fix): a reset held for the Iran end's abort report and then
// released unclaimed is judged at its own time. The same injection must give
// the same verdict whether or not the Iran end reports (a report of 0 aborts
// turns the hold on: every reset waits peerHold, then counts). Before the fix
// a held reset that came before the open joined the episode's kill
// statistics, and an episode its held resets opened started one tick late,
// so its redial wave lost the links born in that tick: R2 then read a real
// delayed kill as anomaly/nolife (held-out #2: Kharej reset recall
// 0.906 -> 0.812 in final mode).

// heldPair runs one Kharej scenario twice, without and with the hold, and
// returns ipA's close lines.
func heldPair(t *testing.T, run func(s *sim)) (plain, held []string) {
	for _, h := range []bool{false, true} {
		s := newSim(t, SideKharej)
		if h {
			s.ev(event{kind: evPeerAbort, remote: ipA, n: 0})
		}
		s.quiet(3)
		run(s)
		s.quiet(24)
		if h {
			held = s.closes(ipA)
		} else {
			plain = s.closes(ipA)
		}
	}
	return plain, held
}

func sameVerdicts(t *testing.T, plain, held []string) {
	t.Helper()
	t.Logf("plain:\n%s\nheld:\n%s", strings.Join(plain, "\n"), strings.Join(held, "\n"))
	if len(plain) == 0 || len(plain) != len(held) {
		t.Fatalf("closes: plain %d held %d\n%s\n--\n%s", len(plain), len(held), strings.Join(plain, "\n"), strings.Join(held, "\n"))
	}
	for i := range plain {
		for _, k := range []string{"class", "conf", "fam", "hint"} {
			if a, b := field(plain[i], k), field(held[i], k); a != b {
				t.Fatalf("%s %q held %q:\n%s\n%s", k, a, b, plain[i], held[i])
			}
		}
	}
}

// A delayed kill seen from Kharej whose resets open the episode: the old
// pool dies at once (tick 0), then the redials born in the first 5 s of the
// episode die at 32 s of age (tick 7). Only the redial wave (born after the
// open) carries the age signature (R2). Held, the old pool opens the episode
// one tick late; without bornFrom the wave is "born before the open" and
// R2 vetoes a real delayed kill (held-out #2 run-a/run-b delayedkill).
func khDelayedKill(s *sim) {
	for i := 0; i < 10; i++ {
		s.healthy(ipB)
		s.healthy(ipA)
		switch i {
		case 0:
			for k := 0; k < 8; k++ {
				s.death(ipA, ipK1, Death{Class: DeathRST, LifeMs: 600000 + uint32(s.rng.Intn(600000)), Bytes: uint64(1 << (12 + s.rng.Intn(12))),
					HaveTCP: true, LastSentMs: 5, MinRTTUs: 90000})
			}
		case 7, 8:
			for k := 0; k < 5; k++ {
				s.death(ipA, ipK1, Death{Class: DeathRST, LifeMs: uint32(5000*i) - 3000 + uint32(k)*100, Bytes: uint64(1 << (12 + s.rng.Intn(12))),
					HaveTCP: true, LastSentMs: 5, MinRTTUs: 90000})
			}
		}
		s.dial(ipA, ipK1, DialOK)
		s.tick()
	}
}

func TestHeldResetDelayedKillSameVerdict(t *testing.T) {
	plain, held := heldPair(t, khDelayedKill)
	if c := field(plain[0], "class"); c != "delayed-kill" {
		t.Fatalf("control not delayed-kill: %s", plain[0])
	}
	sameVerdicts(t, plain, held)
}

// An SNI block opens the episode with failed handshakes; the injector also
// resets the open data links in the tick the trigger fires (run-c rst@19:09
// in held-out #2). Those resets came before the open: evidence of the open,
// not the episode's own kills (as kills, their lifetime cluster made it
// delayed-kill, then R2 vetoed it). j: the tick of the resets.
func khHandshakeThenResets(j int) func(s *sim) {
	return func(s *sim) {
		for i := 0; i < 8; i++ {
			s.healthy(ipB)
			s.healthy(ipA)
			if i == j {
				for k := 0; k < 16; k++ {
					s.death(ipA, ipK1, Death{Class: DeathRST, LifeMs: 100000 + uint32(s.rng.Intn(9000)), Bytes: 4 << 20,
						HaveTCP: true, LastSentMs: 5, MinRTTUs: 90000})
				}
			}
			s.dial(ipA, ipK1, DialHelloTimeout)
			s.tick()
		}
	}
}

func TestHeldResetBeforeOpenSameVerdict(t *testing.T) {
	for j := 0; j < 5; j++ {

		plain, held := heldPair(t, khHandshakeThenResets(j))
		sameVerdicts(t, plain, held)
	}
}

// R1 at the reset's own time: a one-shot reset of the pool, every redial
// succeeds and lives (vetoed as recovered both ways), and an injector that
// keeps killing the redials (not vetoed both ways).
func khOneShot(kill bool) func(s *sim) {
	return func(s *sim) {
		r := rand.New(rand.NewSource(7))
		for i := 0; i < 10; i++ {
			s.healthy(ipB)
			s.healthy(ipA)
			n := 0
			if i < 2 {
				n = 6
			} else if kill {
				n = 2
			}
			for k := 0; k < n; k++ {
				s.death(ipA, ipK1, Death{Class: DeathRST, LifeMs: 300 + uint32(r.Intn(400)), Bytes: 600, HaveTCP: true, LastSentMs: 1, MinRTTUs: 44000})
			}
			for k := 0; k < 3; k++ {
				s.dial(ipA, ipK1, DialOK)
			}
			s.tick()
		}
	}
}

func TestHeldResetRecoverySameVerdict(t *testing.T) {
	for _, kill := range []bool{false, true} {
		plain, held := heldPair(t, khOneShot(kill))
		sameVerdicts(t, plain, held)
	}
}

// R1 at the reset's own time: in the tick of the first redial success the
// injector's resets came before the success, so they are not kills after
// recovery. Released one tick late (after the success was seen) they were
// counted as killPostOK, and the one-shot reset was no longer "recovered".
func khResetsThenFirstSuccess(s *sim) {
	for i := 0; i < 6; i++ {
		s.healthy(ipB)
		s.healthy(ipA)
		n := 0
		switch i {
		case 0:
			n = 8
		case 1:
			n = 6
		}
		for k := 0; k < n; k++ {
			s.death(ipA, ipK1, Death{Class: DeathRST, LifeMs: 600000 + uint32(s.rng.Intn(600000)), Bytes: uint64(1 << (12 + s.rng.Intn(12))),
				HaveTCP: true, LastSentMs: 5, MinRTTUs: 90000})
		}
		if i > 0 {
			s.dial(ipA, ipK1, DialOK)
			s.dial(ipA, ipK1, DialOK)
		}
		s.tick()
	}
}

func TestHeldResetAfterFirstSuccessSameVerdict(t *testing.T) {
	plain, held := heldPair(t, khResetsThenFirstSuccess)
	if h := field(plain[0], "hint"); !strings.HasSuffix(h, "/recovered") {
		t.Fatalf("control not vetoed as recovered: %s", plain[0])
	}
	sameVerdicts(t, plain, held)
}
