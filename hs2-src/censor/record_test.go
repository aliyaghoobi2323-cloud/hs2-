package censor

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// recSim is a sim whose stream is also recorded, as run() records it.
func recSim(t *testing.T, side Side) (*sim, string) {
	s := newSim(t, side)
	path := filepath.Join(t.TempDir(), "rec.bin")
	s.rec = openRecorder(path, side, false, s.e.start, func(string, ...any) {})
	if s.rec == nil {
		t.Fatal("recorder did not open")
	}
	return s, path
}

func replayLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	st, err := replay(f, "", func(_ time.Time, l string) {
		if !strings.Contains(l, "detector on") {
			out = append(out, l)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.gens != 1 || st.ticks == 0 || st.events == 0 {
		t.Fatalf("replay stats %+v", st)
	}
	return out
}

// The recording of a mixed stream (rst injection on A, delayed kill, a
// restart burst that recovers, queue, silence) replays to exactly the lines
// the live evaluator logged.
func TestRecordReplaySameLines(t *testing.T) {
	s, path := recSim(t, SideIran)
	s.quiet(3)
	for i := 0; i < 24; i++ {
		s.healthy(ipB)
		s.addUsers(50)
		s.death(ipA, ipK1, Death{Class: DeathRST, LifeMs: uint32(2000 * math.Pow(2, s.rng.Float64()*10)), Bytes: uint64(1 << (10 + s.rng.Intn(14))),
			HaveTCP: true, LastSentMs: 5, MinRTTUs: 90000})
		s.tick()
	}
	s.quiet(24)
	restartBurst(s, true)
	for i := 0; i < 20; i++ {
		s.path(ipB, ipK1, PathSample{Loss: 0.002, QueueMs: 400, RateBps: 3e6, PeerLoss: 0.01, BusyLinks: 3, PathLimited: 0.7})
		s.path(ipA, ipK2, PathSample{Loss: 1, Silent: true})
		s.dial(ipA, ipK2, DialConnTimeout)
		s.accept(ipB, ipK1, AcceptNoHello)
		s.addUsers(30)
		s.tick()
	}
	s.quiet(30)
	s.rec.flush()
	live := s.lines
	got := replayLines(t, path)
	if len(live) < 3 {
		t.Fatalf("scenario logged too little: %v", live)
	}
	if strings.Join(live, "\n") != strings.Join(got, "\n") {
		t.Fatalf("replay differs:\nlive:\n%s\nreplay:\n%s", strings.Join(live, "\n"), strings.Join(got, "\n"))
	}
}

// A process restart appends a second header; replay restarts the evaluator.
func TestReplayTwoGenerations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rec.bin")
	for g := 0; g < 2; g++ {
		r := openRecorder(path, SideKharej, false, time.Unix(1795000000+int64(g)*600, 0), func(string, ...any) {})
		r.tick(time.Unix(1795000005+int64(g)*600, 0))
		r.flush()
		r.f.Close()
	}
	f, _ := os.Open(path)
	defer f.Close()
	on := 0
	st, err := replay(f, "", func(_ time.Time, l string) {
		if strings.Contains(l, "detector on") {
			on++
		}
	})
	if err != nil || st.gens != 2 || st.ticks != 2 || on != 2 {
		t.Fatalf("gens %+v err %v on %d", st, err, on)
	}
}

// restartBurst: mechanism A (§7.3). One IP loses its links in a fast-RST
// burst (old links), then, with refill, its redials succeed and live
// (18 accepts, no fresh kill), as after a peer restart the mass rule did
// not confirm.
func restartBurst(s *sim, refill bool) {
	for i := 0; i < 2; i++ {
		s.healthy(ipB)
		s.addUsers(50)
		for k := 0; k < 10; k++ {
			s.death(ipA, ipK1, Death{Class: DeathRST, LifeMs: 600000 + uint32(s.rng.Intn(600000)), Bytes: 1 << 22,
				HaveTCP: true, LastSentMs: 5, MinRTTUs: 90000})
		}
		s.tick()
	}
	for i := 0; i < 9; i++ {
		s.healthy(ipB)
		s.addUsers(50)
		if refill {
			s.accept(ipA, ipK1, AcceptOK)
			s.accept(ipA, ipK1, AcceptOK)
		}
		s.tick()
	}
	s.quiet(24)
}

// R1: the burst IP recovers after our own redials: an anomaly at low with
// the exact class as hint, not rst-inject (fails with R1 reverted).
func TestR1RecoveryVetoesStage1(t *testing.T) {
	s := newSim(t, SideIran)
	s.quiet(3)
	restartBurst(s, true)
	s.expect(ipA, "anomaly", confLow)
	cl := s.closes(ipA)[0]
	if field(cl, "hint") != "rst-inject/recovered" || field(cl, "fam") != "-" {
		t.Fatalf("hint/fam: %s", cl)
	}
	s.noCensorship(ipA, confUnknown)
}

// R1 control: the same burst with no recovery stays rst-inject (fam reset).
func TestR1NoRecoveryKeepsReset(t *testing.T) {
	s := newSim(t, SideIran)
	s.quiet(3)
	restartBurst(s, false)
	s.expect(ipA, "rst-inject", confMed)
	if cl := s.closes(ipA)[0]; field(cl, "fam") != "reset" || field(cl, "hint") != "-" {
		t.Fatalf("fam/hint: %s", cl)
	}
}

// R2: a handful of mid-age kills (7 RSTs, no lifetime cluster of its own:
// the res10 b congestion FP) reached delayed-kill med on dkSig + a clean
// path alone. Now an anomaly at low (fails with R2 reverted).
func TestR2DelayedKillNeedsLifetimeCluster(t *testing.T) {
	s := newSim(t, SideIran)
	s.quiet(3)
	for i := 0; i < 3; i++ {
		s.healthy(ipB)
		s.healthy(ipA)
		s.addUsers(50)
		n := 2
		if i == 0 {
			n = 3
		}
		for k := 0; k < n; k++ {
			s.death(ipA, ipK1, Death{Class: DeathRST, LifeMs: 20000 + uint32(s.rng.Intn(40000)), Bytes: uint64(1 << (12 + s.rng.Intn(12))),
				HaveTCP: true, LastSentMs: 400, MinRTTUs: 90000})
		}
		s.tick()
	}
	s.quiet(24)
	s.expect(ipA, "anomaly", confLow)
	if cl := s.closes(ipA)[0]; field(cl, "hint") != "delayed-kill/nolife" {
		t.Fatalf("hint: %s", cl)
	}
}

// (z): a pool refilled at one moment, then reset at once, dies at one age
// (every link born before the episode opened). The whole-episode lifetime
// cluster is 100 %, but no redial born in the episode dies at that age:
// not delayed-kill (fails with the born-after-open rule reverted).
func TestZRefilledPoolNotDelayedKill(t *testing.T) {
	s := newSim(t, SideIran)
	s.quiet(3)
	for i := 0; i < 2; i++ {
		s.healthy(ipB)
		s.healthy(ipA)
		s.addUsers(50)
		for k := 0; k < 12; k++ {
			s.death(ipA, ipK1, Death{Class: DeathRST, LifeMs: 40000 + uint32(s.rng.Intn(1500)) + uint32(i)*5000, Bytes: uint64(1 << (12 + s.rng.Intn(12))),
				HaveTCP: true, LastSentMs: 400, MinRTTUs: 90000})
		}
		s.tick()
	}
	s.quiet(24)
	cl := s.closes(ipA)
	if len(cl) != 1 {
		t.Fatalf("closes: %v", s.lines)
	}
	if c := field(cl[0], "class"); c == "delayed-kill" && field(cl[0], "conf") != "low" {
		t.Fatalf("refilled pool read as delayed-kill: %s", cl[0])
	}
	if field(cl[0], "class") == "anomaly" && field(cl[0], "hint") != "delayed-kill/nolife" {
		t.Fatalf("hint: %s", cl[0])
	}
}

// Families: every censorship class has one, ordinary classes none.
func TestFamilies(t *testing.T) {
	want := map[int]int{ClRSTInject: FamReset, ClFingerprintKill: FamReset, ClDelayedKill: FamReset,
		ClHandshakeBlock: FamHandshake, ClBlackhole: FamBlock, ClPortBlock: FamBlock, ClIntlCut: FamBlock,
		ClThrottle: FamThrottle, ClUDPDiff: FamUDP}
	for c := 0; c < NClasses; c++ {
		if got := FamilyOf(c); got != want[c] {
			t.Fatalf("%s: family %d want %d", ClassName(c), got, want[c])
		}
		if IsCensorship(c) != (FamilyOf(c) != FamNone) {
			t.Fatalf("%s: family vs censorship", ClassName(c))
		}
	}
	if ClassName(ClAnomaly) != "anomaly" || IsCensorship(ClAnomaly) {
		t.Fatal("anomaly")
	}
}

// TestReplayFile is the offline replay tool (step back §7.5):
//
//	CENSOR_REPLAY=ir.rec[,kh.rec...] CENSOR_REPLAY_OUT=dir go test -run TestReplayFile ./censor
//
// writes <dir>/<base>.log with each line prefixed by its log time in the
// live format (15:04:05.000000, local zone), so an351 scores it as a live
// ir.log / kh.log. CENSOR_REPLAY_ISPMAP is the live HS2_CENSOR_ISPMAP.
func TestReplayFile(t *testing.T) {
	in := os.Getenv("CENSOR_REPLAY")
	if in == "" {
		t.Skip("CENSOR_REPLAY not set")
	}
	outDir := os.Getenv("CENSOR_REPLAY_OUT")
	for _, p := range strings.Split(in, ",") {
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		name := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p)) + ".log"
		o, err := os.Create(filepath.Join(outDir, name))
		if err != nil {
			t.Fatal(err)
		}
		w := bufio.NewWriter(o)
		st, err := replay(f, os.Getenv("CENSOR_REPLAY_ISPMAP"), func(at time.Time, l string) {
			fmt.Fprintf(w, "%s %s\n", at.Local().Format("15:04:05.000000"), l)
		})
		f.Close()
		w.Flush()
		o.Close()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: %d generations, %d events, %d ticks -> %s", p, st.gens, st.events, st.ticks, name)
	}
}
