package censor

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/netip"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	ipA  = netip.MustParseAddr("10.0.0.1")
	ipB  = netip.MustParseAddr("10.0.0.2")
	ipK1 = netip.MustParseAddr("10.1.0.1")
	ipK2 = netip.MustParseAddr("10.1.0.2")
)

// sim drives an evaluator with synthetic events on a fake clock.
type sim struct {
	t     *testing.T
	e     *evaluator
	now   time.Time
	mu    sync.Mutex
	lines []string
	users uint64
	rng   *rand.Rand
	rec   *recorder // record_test: the stream as the live run() records it
}

// newSim: 03:10 UTC (off-peak, outside the xray window), detector up 1 h.
func newSim(t *testing.T, side Side) *sim {
	s := &sim{t: t, now: time.Date(2026, 11, 19, 3, 10, 0, 0, time.UTC), rng: rand.New(rand.NewSource(1))}
	s.e = newEvaluator(side, func(f string, a ...any) {
		s.mu.Lock()
		s.lines = append(s.lines, fmt.Sprintf(f, a...))
		s.mu.Unlock()
	}, s.now.Add(-time.Hour))
	s.e.now = func() time.Time { return s.now }
	return s
}

func (s *sim) ev(e event) {
	e.at = s.now.UnixNano()
	if s.rec != nil {
		s.rec.event(&e)
	}
	s.e.ingest(e)
}

// kh/ir order the addresses for the side: local, remote.
func (s *sim) lr(ir, kh netip.Addr) (netip.Addr, netip.Addr) {
	if s.e.side == SideIran {
		return ir, kh
	}
	return kh, ir
}

func (s *sim) dial(ir, kh netip.Addr, st DialStage) {
	l, r := s.lr(ir, kh)
	s.ev(event{kind: evDial, stage: uint8(st), front: FrontTLS, local: l, remote: r})
}

func (s *sim) death(ir, kh netip.Addr, d Death) {
	d.Local, d.Remote = s.lr(ir, kh)
	s.ev(event{kind: evDeath, local: d.Local, remote: d.Remote, d: d})
}

func (s *sim) path(ir, kh netip.Addr, p PathSample) {
	p.Local, p.Remote = s.lr(ir, kh)
	if p.PeerLoss == 0 {
		p.PeerLoss = -1
	}
	s.ev(event{kind: evPath, local: p.Local, remote: p.Remote, p: p})
}

func (s *sim) accept(ir, kh netip.Addr, o AcceptOutcome) {
	l, r := s.lr(ir, kh)
	s.ev(event{kind: evAccept, stage: uint8(o), local: l, remote: r})
}

func (s *sim) healthy(ir netip.Addr) {
	for _, kh := range []netip.Addr{ipK1, ipK2} {
		s.path(ir, kh, PathSample{Loss: 0.001 + s.rng.Float64()*0.004, QueueMs: 5 + s.rng.Float64()*10, RateBps: 8e6})
	}
}

func (s *sim) addUsers(perTick uint64) {
	s.users += perTick
	s.ev(event{kind: evUsers, n: s.users})
}

func (s *sim) tick() {
	if s.rec != nil {
		s.rec.tick(s.now)
	}
	s.e.step(s.now)
	s.now = s.now.Add(tick)
}

// quiet runs n healthy ticks on both IPs.
func (s *sim) quiet(n int) {
	for i := 0; i < n; i++ {
		s.healthy(ipA)
		s.healthy(ipB)
		s.addUsers(50)
		s.tick()
	}
}

// closes returns the "st=close" lines for ip.
func (s *sim) closes(ip netip.Addr) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, l := range s.lines {
		if strings.Contains(l, "ep="+ip.String()+"#") && strings.Contains(l, "st=close") {
			out = append(out, l)
		}
	}
	return out
}

func field(line, k string) string {
	for _, f := range strings.Fields(line) {
		if v, ok := strings.CutPrefix(f, k+"="); ok {
			return v
		}
	}
	return ""
}

// expect: exactly one closed episode on ip, of class cls, confidence ≥ minConf.
func (s *sim) expect(ip netip.Addr, cls string, minConf int) {
	s.t.Helper()
	cl := s.closes(ip)
	if len(cl) != 1 {
		s.t.Fatalf("%s: want 1 closed episode, got %d:\n%s", ip, len(cl), strings.Join(s.lines, "\n"))
	}
	if got := field(cl[0], "class"); got != cls {
		s.t.Fatalf("class %q, want %q:\n%s", got, cls, cl[0])
	}
	c := field(cl[0], "conf")
	ci := -1
	for i, n := range confName {
		if n == c {
			ci = i
		}
	}
	if ci < minConf {
		s.t.Fatalf("conf %s, want ≥ %s:\n%s", c, confName[minConf], cl[0])
	}
}

func (s *sim) noCensorship(ip netip.Addr, maxConf int) {
	s.t.Helper()
	for _, l := range s.closes(ip) {
		c := field(l, "class")
		for i := 0; i < NClasses; i++ {
			if className[i] == c && IsCensorship(i) {
				ci := 0
				for j, n := range confName {
					if n == field(l, "conf") {
						ci = j
					}
				}
				if ci > maxConf {
					s.t.Fatalf("ordinary event called censorship %s/%s:\n%s", c, confName[ci], l)
				}
			}
		}
	}
}

func TestClassRSTInject(t *testing.T) {
	s := newSim(t, SideIran)
	s.quiet(3)
	for i := 0; i < 24; i++ {
		s.healthy(ipB)
		s.addUsers(50)
		// Iran side: RST 5 ms after our last send on a 90 ms path (ρ ≈ 0.06).
		s.death(ipA, ipK1, Death{Class: DeathRST, LifeMs: uint32(2000 * math.Pow(2, s.rng.Float64()*10)), Bytes: uint64(1 << (10 + s.rng.Intn(14))),
			HaveTCP: true, LastSentMs: 5, MinRTTUs: 90000})
		s.tick()
	}
	s.quiet(24)
	s.expect(ipA, "rst-inject", confHigh)
	s.noCensorship(ipB, confUnknown)
}

func TestClassHandshakeBlock(t *testing.T) {
	s := newSim(t, SideKharej)
	s.quiet(3)
	for i := 0; i < 30; i++ {
		s.healthy(ipA) // established links keep acking
		s.healthy(ipB)
		for k := 0; k < 2; k++ {
			s.dial(ipA, ipK1, DialHelloTimeout)
			s.dial(ipA, ipK2, DialHelloTimeout)
		}
		s.dial(ipB, ipK1, DialOK)
		s.tick()
	}
	s.quiet(24)
	s.expect(ipA, "handshake-block", confMed)
}

func TestClassBlackhole(t *testing.T) {
	s := newSim(t, SideKharej)
	s.quiet(3)
	for i := 0; i < 150; i++ { // 12.5 min
		s.healthy(ipB)
		s.dial(ipA, ipK1, DialConnTimeout)
		s.dial(ipA, ipK2, DialConnTimeout)
		s.path(ipA, ipK1, PathSample{Loss: 1, Silent: true})
		s.path(ipA, ipK2, PathSample{Loss: 1, Silent: true})
		s.tick()
	}
	s.quiet(24)
	s.expect(ipA, "blackhole", confMed)
}

func TestClassFingerprintKill(t *testing.T) {
	s := newSim(t, SideKharej)
	s.quiet(3)
	for i := 0; i < 30; i++ {
		s.healthy(ipB)
		s.healthy(ipA)
		// fresh links die at ~10 KB, any age under 2 min
		s.death(ipA, ipK1, Death{Class: DeathRST, LifeMs: uint32(1000 + s.rng.Intn(110000)), Bytes: 10000 + uint64(s.rng.Intn(3000))})
		s.dial(ipA, ipK1, DialOK)
		s.tick()
	}
	s.quiet(24)
	s.expect(ipA, "fingerprint-kill", confMed)
}

func TestClassThrottle(t *testing.T) {
	s := newSim(t, SideKharej)
	h := s.now.UTC().Hour()
	s.quiet(1)
	sa := s.e.scopes[ipA]
	sa.rateHr[h], sa.rateDay[h] = 10e6, dayOf(s.now) // written yesterday (+1 encoding: day-1+1)
	for i := 0; i < 120; i++ {
		s.healthy(ipB)
		for _, kh := range []netip.Addr{ipK1, ipK2} {
			s.path(ipA, kh, PathSample{Loss: 0.002, QueueMs: 30 + s.rng.Float64()*5, RateBps: 2e6})
		}
		s.tick()
	}
	s.quiet(24)
	s.expect(ipA, "throttle", confLow)
	if l := s.closes(ipA)[0]; field(l, "conf") != "low" {
		t.Fatalf("throttle must cap at low until busy-link/path-limited share is wired: %s", l)
	}
}

func TestClassIntlCut(t *testing.T) {
	s := newSim(t, SideIran)
	s.e.ispMap = map[netip.Addr]string{ipA: "isp1", ipB: "isp2"}
	s.quiet(30)
	for i := 0; i < 60; i++ {
		s.addUsers(50) // domestic users keep arriving
		for _, ir := range []netip.Addr{ipA, ipB} {
			s.path(ir, ipK1, PathSample{Loss: 1, Silent: true})
			s.path(ir, ipK2, PathSample{Loss: 1, Silent: true})
		}
		s.tick()
	}
	s.quiet(24)
	s.expect(ipA, "intl-cut", confHigh)
	s.expect(ipB, "intl-cut", confHigh)
}

func TestClassDelayedKill(t *testing.T) {
	s := newSim(t, SideKharej)
	s.quiet(3)
	for i := 0; i < 40; i++ {
		s.healthy(ipB)
		s.healthy(ipA)
		s.death(ipA, ipK1, Death{Class: DeathRST, LifeMs: 300000 + uint32(s.rng.Intn(4000)), Bytes: uint64(1 << (12 + s.rng.Intn(14)))})
		s.tick()
	}
	s.quiet(24)
	s.expect(ipA, "delayed-kill", confLow)
}

func TestClassCongestion(t *testing.T) {
	s := newSim(t, SideKharej)
	s.now = time.Date(2026, 11, 19, 17, 10, 0, 0, time.UTC) // Iran evening peak
	s.e.start = s.now.Add(-time.Hour)
	s.quiet(3)
	for i := 0; i < 60; i++ {
		s.healthy(ipB)
		q := 20 + float64(i%12)*10
		for _, kh := range []netip.Addr{ipK1, ipK2} {
			s.path(ipA, kh, PathSample{Loss: 0.01 + q/3000, QueueMs: q, RateBps: 7e6})
		}
		s.tick()
	}
	s.quiet(24)
	s.expect(ipA, "congestion", confMed)
}

// slot 13: a blackhole (heavy loss, some silent readings, no queue, links
// timing out) is not congestion, on either side, even in the evening hours
// that add a congestion point; a congested path with a full queue and a
// silent reading still is.
func TestSilentLossNoQueueNotCongestion(t *testing.T) {
	for _, side := range []Side{SideIran, SideKharej} {
		s := newSim(t, side)
		s.now = time.Date(2026, 11, 19, 17, 10, 0, 0, time.UTC)
		s.e.start = s.now.Add(-time.Hour)
		// one Kharej IP, as in the lab (kh=1/1): no other-Kharej point
		// for path-outage, so the old rule gave congestion med here
		q1 := func(n int) {
			for i := 0; i < n; i++ {
				for _, ir := range []netip.Addr{ipA, ipB} {
					s.path(ir, ipK1, PathSample{Loss: 0.002, QueueMs: 8, RateBps: 8e6})
				}
				s.addUsers(50)
				s.tick()
			}
		}
		q1(3)
		for i := 0; i < 20; i++ {
			s.path(ipB, ipK1, PathSample{Loss: 0.002, QueueMs: 8, RateBps: 8e6})
			if i%4 == 0 {
				s.path(ipA, ipK1, PathSample{Loss: 1, Silent: true})
				s.death(ipA, ipK1, Death{Class: DeathTimeout, LifeMs: 600000, Bytes: 1 << 22})
			} else {
				s.path(ipA, ipK1, PathSample{Loss: 0.4, QueueMs: 1, RateBps: 1e5})
			}
			s.tick()
		}
		q1(24)
		n := 0
		for _, l := range s.closes(ipA) {
			n++
			if field(l, "class") == "congestion" {
				t.Fatalf("side %v: silent loss without a queue called congestion: %s", side, l)
			}
		}
		if n == 0 {
			t.Fatalf("side %v: no episode closed", side)
		}
		s.noCensorship(ipA, confLow)
	}
	s := newSim(t, SideKharej)
	s.now = time.Date(2026, 11, 19, 17, 10, 0, 0, time.UTC)
	s.e.start = s.now.Add(-time.Hour)
	s.quiet(3)
	for i := 0; i < 60; i++ {
		s.healthy(ipB)
		q := 150 + float64(i%12)*10
		for _, kh := range []netip.Addr{ipK1, ipK2} {
			s.path(ipA, kh, PathSample{Loss: 0.01 + q/3000, QueueMs: q, RateBps: 7e6})
		}
		if i == 30 {
			s.path(ipA, ipK1, PathSample{Loss: 1, Silent: true})
		}
		s.tick()
	}
	s.quiet(24)
	s.expect(ipA, "congestion", confMed)
}

func TestClassPathOutage(t *testing.T) {
	s := newSim(t, SideKharej)
	s.quiet(3)
	for i := 0; i < 20; i++ {
		s.healthy(ipB)
		s.path(ipA, ipK1, PathSample{Loss: 1, Silent: true})
		s.path(ipA, ipK2, PathSample{Loss: 0.002, QueueMs: 8, RateBps: 8e6})
		s.dial(ipA, ipK1, DialConnUnreach)
		s.tick()
	}
	s.quiet(24)
	s.expect(ipA, "path-outage", confMed)
	s.noCensorship(ipA, confUnknown)
}

func TestClassISPOutage(t *testing.T) {
	s := newSim(t, SideIran)
	s.e.ispMap = map[netip.Addr]string{ipA: "isp1", ipB: "isp2"}
	s.quiet(40)
	for i := 0; i < 30; i++ {
		s.addUsers(5) // that ISP's users vanish too
		s.healthy(ipB)
		s.path(ipA, ipK1, PathSample{Loss: 1, Silent: true})
		s.path(ipA, ipK2, PathSample{Loss: 1, Silent: true})
		s.tick()
	}
	s.quiet(24)
	s.expect(ipA, "isp-outage", confMed)
	s.noCensorship(ipA, confLow)
}

func TestClassPeerRestart(t *testing.T) {
	s := newSim(t, SideIran)
	s.quiet(3)
	for _, ir := range []netip.Addr{ipA, ipB} {
		for k := 0; k < 4; k++ { // reset at normal timing (ρ ≥ 1) on every IP at once
			s.death(ir, ipK1, Death{Class: DeathRST, LifeMs: 3600000, Bytes: 1 << 24, HaveTCP: true, LastSentMs: 200, MinRTTUs: 90000})
		}
	}
	s.tick()
	for i := 0; i < 3; i++ {
		s.accept(ipA, ipK1, AcceptOK)
		s.accept(ipB, ipK1, AcceptOK)
		s.quiet(1)
	}
	s.quiet(24)
	s.expect(ipA, "peer-restart", confMed)
	s.noCensorship(ipA, confLow)
	s.noCensorship(ipB, confLow)
}

func TestClassOwnRestart(t *testing.T) {
	s := newSim(t, SideKharej)
	s.e.start = s.now // just started
	for i := 0; i < 6; i++ {
		s.healthy(ipB)
		s.dial(ipA, ipK1, DialConnTimeout)
		s.dial(ipA, ipK2, DialConnTimeout)
		s.tick()
	}
	s.quiet(24)
	s.expect(ipA, "own-restart", confMed)
	s.noCensorship(ipA, confLow)
}

func TestClassBufferbloat(t *testing.T) {
	s := newSim(t, SideKharej)
	s.quiet(3)
	for i := 0; i < 20; i++ {
		s.healthy(ipB)
		s.path(ipA, ipK1, PathSample{Loss: 0.002, QueueMs: 180, RateBps: 8e6})
		s.tick()
	}
	s.quiet(24)
	s.expect(ipA, "bufferbloat", confLow)
	s.noCensorship(ipA, confUnknown)
}

func TestClassAsymLoss(t *testing.T) {
	s := newSim(t, SideKharej)
	s.quiet(3)
	for i := 0; i < 20; i++ {
		s.healthy(ipB)
		s.path(ipA, ipK1, PathSample{Loss: 0.06, QueueMs: 10, RateBps: 8e6, PeerLoss: 0.002})
		s.tick()
	}
	s.quiet(24)
	s.expect(ipA, "asym-loss", confMed)
	s.noCensorship(ipA, confUnknown)
}

// Classes level 1 cannot see: port-block needs the canary port (M12, owner
// decision D5); udp-diff needs a datagram path (production is TCP only).
// They must never be emitted, whatever the input.
func TestClassesNotInLevel1(t *testing.T) {
	s := newSim(t, SideKharej)
	s.quiet(3)
	for i := 0; i < 40; i++ {
		s.healthy(ipB)
		s.dial(ipA, ipK1, DialConnTimeout)
		s.dial(ipA, ipK2, DialConnRefused)
		s.path(ipA, ipK1, PathSample{Loss: 1, Silent: true})
		s.tick()
	}
	s.quiet(24)
	for _, l := range s.lines {
		if c := field(l, "class"); c == "port-block" || c == "udp-diff" {
			t.Fatalf("level 1 emitted %s: %s", c, l)
		}
	}
}

// Normal traffic, our own churn (retire, drain, spare, ZDU peerOld: all
// DeathLocal) and clean peer closes open no episode at all.
func TestNormalTrafficAndOwnChurnSilent(t *testing.T) {
	s := newSim(t, SideKharej)
	for i := 0; i < 720; i++ { // 1 h
		s.healthy(ipA)
		s.healthy(ipB)
		s.addUsers(50)
		for k := 0; k < 5; k++ {
			s.death(ipA, ipK1, Death{Class: DeathLocal, LifeMs: uint32(s.rng.Intn(60000)), Bytes: 3000})
			s.death(ipB, ipK2, Death{Class: DeathEOF, LifeMs: 33000, Bytes: 2000})
		}
		s.dial(ipA, ipK1, DialOK)
		s.dial(ipB, ipK2, DialOK)
		s.tick()
	}
	for _, l := range s.lines {
		if strings.Contains(l, "ep=") {
			t.Fatalf("quiet hour opened an episode: %s", l)
		}
	}
}

// The hourly xray restart window (HH:00:30-02:30) never lets censorship rise
// above low on the Kharej side.
func TestXrayWindowCapsCensorship(t *testing.T) {
	s := newSim(t, SideKharej)
	s.now = time.Date(2026, 11, 19, 3, 0, 40, 0, time.UTC)
	s.e.start = s.now.Add(-time.Hour)
	for i := 0; i < 20; i++ {
		s.healthy(ipB)
		s.death(ipA, ipK1, Death{Class: DeathRST, LifeMs: uint32(1000 + s.rng.Intn(110000)), Bytes: 10000 + uint64(s.rng.Intn(3000))})
		s.tick()
	}
	s.quiet(24)
	s.noCensorship(ipA, confLow)
}

// A plain loss burst (random loss, no queue correlation) is not censorship.
func TestPlainLossNotCensorship(t *testing.T) {
	s := newSim(t, SideKharej)
	s.quiet(3)
	for i := 0; i < 40; i++ {
		s.healthy(ipB)
		for _, kh := range []netip.Addr{ipK1, ipK2} {
			s.path(ipA, kh, PathSample{Loss: 0.02 + s.rng.Float64()*0.04, QueueMs: 10 + s.rng.Float64()*5, RateBps: 6e6})
		}
		s.tick()
	}
	s.quiet(24)
	s.noCensorship(ipA, confUnknown)
}

// Scanners (unknown IPs failing the handshake) never create state; an
// authenticated peer becomes known and its later failures count.
func TestAcceptOnlyKnownPeers(t *testing.T) {
	s := newSim(t, SideIran)
	scanner := netip.MustParseAddr("192.0.2.9")
	for i := 0; i < 50; i++ {
		s.accept(ipA, scanner, AcceptNoHello)
	}
	if len(s.e.scopes) != 0 || len(s.e.khIdx) != 0 {
		t.Fatalf("scanner created state: %d scopes, %d peers", len(s.e.scopes), len(s.e.khIdx))
	}
	s.accept(ipA, ipK1, AcceptOK)
	s.accept(ipA, ipK1, AcceptNoHello)
	if c := s.e.scopes[ipA].buckets[s.e.scopes[ipA].cur]; c.acc[AcceptNoHello] != 1 || c.acc[AcceptOK] != 1 {
		t.Fatalf("known peer not counted: %+v", c.acc)
	}
}

// --- isolation of the hooks -------------------------------------------------

func resetGlobals() {
	active.Store(false)
	disabled.Store(false)
	dropped.Store(0)
	ring = nil
	statusJSON.Store(nil)
}

func TestKeyOffZeroAllocNoGoroutine(t *testing.T) {
	resetGlobals()
	t.Setenv("HS2_CENSOR_DETECT", "0")
	t.Setenv("HS2_CENSOR", "")
	before := runtime.NumGoroutine()
	Init(SideKharej, t.Logf)
	if Active() || ring != nil {
		t.Fatal("key 0 started the detector")
	}
	time.Sleep(20 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("key 0 started goroutines: %d -> %d", before, after)
	}
	d := Death{Local: ipK1, Remote: ipA, Class: DeathRST}
	p := PathSample{Local: ipK1, Remote: ipA, Loss: 0.1}
	n := testing.AllocsPerRun(1000, func() {
		NoteDial(ipK1, ipA, FrontTLS, DialConnTimeout)
		NoteAccept(ipA, ipK1, AcceptNoHello)
		NoteDeath(d)
		NotePath(p)
		NoteUsers(5)
		_ = Active()
	})
	if n != 0 {
		t.Fatalf("key off: %v allocs per hook round, want 0", n)
	}
	if StatusJSON() != nil {
		t.Fatal("key off must publish no status block")
	}
}

func TestHooksNeverBlockAndDontAllocWhenOn(t *testing.T) {
	resetGlobals()
	ring = make(chan event, ringCap)
	active.Store(true)
	defer resetGlobals()
	d := Death{Local: ipK1, Remote: ipA, Class: DeathRST}
	n := testing.AllocsPerRun(100, func() { NoteDeath(d) })
	if n != 0 {
		t.Fatalf("key on: %v allocs per hook, want 0", n)
	}
	// nobody drains the ring: 50k hooks must return at once and count drops
	done := make(chan struct{})
	go func() {
		for i := 0; i < 50000; i++ {
			NoteDeath(d)
			NoteDial(ipK1, ipA, FrontTLS, DialOK)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("hooks blocked on a full ring")
	}
	if dropped.Load() == 0 {
		t.Fatal("a full ring must drop and count")
	}
}

func TestEvaluatorPanicSelfDisables(t *testing.T) {
	resetGlobals()
	defer resetGlobals()
	var mu sync.Mutex
	var logs []string
	e := newEvaluator(SideKharej, func(f string, a ...any) {
		mu.Lock()
		logs = append(logs, fmt.Sprintf(f, a...))
		mu.Unlock()
	}, time.Now())
	e.testPanic = true
	ring = make(chan event, ringCap)
	setStatus(e)
	active.Store(true)
	exited := make(chan struct{})
	go func() { e.run(); close(exited) }()
	NoteDial(ipK1, ipA, FrontTLS, DialOK)
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("evaluator did not stop after its panic")
	}
	if Active() || !disabled.Load() {
		t.Fatal("panic must turn the detector off")
	}
	NoteDial(ipK1, ipA, FrontTLS, DialOK) // a no-op now, must not block or panic
	mu.Lock()
	defer mu.Unlock()
	if len(logs) != 1 || !strings.Contains(logs[0], "detector disabled") {
		t.Fatalf("want one disable line, got %q", logs)
	}
	if !strings.Contains(string(StatusJSON()), `"disabled":true`) {
		t.Fatalf("status must say disabled: %s", StatusJSON())
	}
	if !strings.Contains(Render(StatusJSON()), "DISABLED") {
		t.Fatal("render must say disabled")
	}
}

func TestLineRateCap(t *testing.T) {
	s := newSim(t, SideKharej)
	ep := &episode{id: "x#1", class: ClUnknown}
	for i := 0; i < 100; i++ {
		s.e.emit(ep, "close", s.now)
	}
	if len(s.lines) != maxLinesMin {
		t.Fatalf("%d lines in one minute, want cap %d", len(s.lines), maxLinesMin)
	}
	s.now = s.now.Add(time.Minute)
	s.e.emit(ep, "close", s.now)
	if !strings.Contains(s.lines[maxLinesMin], "+70 lines folded") {
		t.Fatalf("fold line missing: %q", s.lines[maxLinesMin])
	}
}

func TestLevelAndAlias(t *testing.T) {
	for _, c := range []struct {
		detect, alias string
		want          int
	}{
		{"", "", 0}, {"0", "1", 0}, {"1", "", 1}, {"2", "", 1}, {"", "1", 1}, {"x", "", 0},
	} {
		t.Setenv("HS2_CENSOR_DETECT", c.detect)
		t.Setenv("HS2_CENSOR", c.alias)
		if got := Level(); got != c.want {
			t.Fatalf("DETECT=%q CENSOR=%q: level %d, want %d", c.detect, c.alias, got, c.want)
		}
	}
}

// rel69r-rc6: unset, the Iran side defaults on and the Kharej side off;
// an explicit value (or alias) wins on either side; legacy defaults off.
func TestLevelForSideDefaults(t *testing.T) {
	for _, c := range []struct {
		detect, alias, defs string
		iran, kharej        int
	}{
		{"", "", "", 1, 0}, {"0", "", "", 0, 0}, {"1", "", "", 1, 1}, {"", "0", "", 0, 0},
		{"", "1", "", 1, 1}, {"", "", "legacy", 0, 0}, {"1", "", "legacy", 1, 1}, {"x", "", "", 0, 0},
	} {
		t.Setenv("HS2_CENSOR_DETECT", c.detect)
		t.Setenv("HS2_CENSOR", c.alias)
		t.Setenv("HS2_DEFAULTS", c.defs)
		if g, k := LevelFor(SideIran), LevelFor(SideKharej); g != c.iran || k != c.kharej {
			t.Fatalf("DETECT=%q CENSOR=%q DEFAULTS=%q: iran %d kharej %d, want %d %d", c.detect, c.alias, c.defs, g, k, c.iran, c.kharej)
		}
	}
}

// M7 wired: busy links network-limited most of their time tell a ceiling
// from low demand, so throttle may rise to med (never high).
func TestClassThrottleCeiling(t *testing.T) {
	s := newSim(t, SideKharej)
	h := s.now.UTC().Hour()
	s.quiet(1)
	sa := s.e.scopes[ipA]
	sa.rateHr[h], sa.rateDay[h] = 10e6, dayOf(s.now)
	for i := 0; i < 120; i++ {
		s.healthy(ipB)
		for _, kh := range []netip.Addr{ipK1, ipK2} {
			s.path(ipA, kh, PathSample{Loss: 0.002, QueueMs: 30 + s.rng.Float64()*5, RateBps: 2e6, BusyLinks: 3, PathLimited: 0.85})
		}
		s.tick()
	}
	s.quiet(24)
	s.expect(ipA, "throttle", confMed)
	if l := s.closes(ipA)[0]; field(l, "conf") == "high" {
		t.Fatalf("throttle must never be high: %s", l)
	}
}

// Low demand: the rate falls but the links are idle (no busy link), so the
// cap at low stays.
func TestClassThrottleLowDemandCapped(t *testing.T) {
	s := newSim(t, SideKharej)
	h := s.now.UTC().Hour()
	s.quiet(1)
	sa := s.e.scopes[ipA]
	sa.rateHr[h], sa.rateDay[h] = 10e6, dayOf(s.now)
	for i := 0; i < 120; i++ {
		s.healthy(ipB)
		for _, kh := range []netip.Addr{ipK1, ipK2} {
			s.path(ipA, kh, PathSample{Loss: 0.002, QueueMs: 30, RateBps: 2e6, BusyLinks: 0, PathLimited: -1})
		}
		s.tick()
	}
	s.quiet(24)
	if l := s.closes(ipA); len(l) > 0 && field(l[0], "conf") != "low" {
		t.Fatalf("low demand must stay at low: %s", l[0])
	}
}

// TestStatusPathInputs: the status block shows what path inputs (M7/O7)
// reached each scope, so a lab run can prove PeerLoss / BusyLinks /
// PathLimited arrive with real values. No rule reads the tally.
func TestStatusPathInputs(t *testing.T) {
	resetGlobals()
	defer resetGlobals()
	s := newSim(t, SideKharej)
	s.path(ipA, ipK1, PathSample{Loss: 0.01, PeerLoss: 0.02, BusyLinks: 3, PathLimited: 0.7, RateBps: 1e6})
	s.path(ipA, ipK1, PathSample{Loss: 0.02, BusyLinks: 0, PathLimited: -1})
	s.ev(event{kind: evPath, local: ipK1, p: PathSample{Local: ipK1}}) // no Iran IP: no scope
	setStatus(s.e)
	var st Status
	if err := json.Unmarshal(StatusJSON(), &st); err != nil {
		t.Fatal(err)
	}
	if st.PathEv != [2]uint64{3, 1} || len(st.Paths) != 1 {
		t.Fatalf("path_ev %v paths %q", st.PathEv, st.Paths)
	}
	for _, w := range []string{"n=2", "peer_known=1", "peer_max=0.020", "busy_n=1", "busy_max=3", "lim_known=1", "lim_max=0.70", "last: loss=0.020 peer=-1.000 busy=0 lim=-1.00"} {
		if !strings.Contains(st.Paths[0], w) {
			t.Fatalf("missing %q in %q", w, st.Paths[0])
		}
	}
	if out := Render(StatusJSON()); !strings.Contains(out, "path samples: 3 received, 1 without") || !strings.Contains(out, "path inputs") {
		t.Fatalf("render: %s", out)
	}
}

// slot 4: a blackhole that outlasts the LB's cut keeps its episode open
// while the hooks still show the path dead (sparse probe timeouts, no
// success), and closes after the path recovers.
func TestBlackholeHeldOpenWhileCut(t *testing.T) {
	s := newSim(t, SideKharej)
	s.quiet(3)
	for i := 0; i < 6; i++ { // the cut: dials time out, readings silent
		s.healthy(ipB)
		s.addUsers(50)
		s.dial(ipA, ipK1, DialConnTimeout)
		s.dial(ipA, ipK2, DialConnTimeout)
		s.path(ipA, ipK1, PathSample{Loss: 1, Silent: true})
		s.path(ipA, ipK2, PathSample{Loss: 1, Silent: true})
		s.tick()
	}
	for i := 0; i < 140; i++ { // 11.7 min: the LB backs off, a probe dial a minute
		s.healthy(ipB)
		s.addUsers(50)
		if i%12 == 0 {
			s.dial(ipA, ipK1, DialConnTimeout)
		}
		s.tick()
	}
	if n := len(s.closes(ipA)); n != 0 {
		t.Fatalf("closed while the path is still cut:\n%s", strings.Join(s.lines, "\n"))
	}
	s.dial(ipA, ipK1, DialOK) // recovered
	s.quiet(24)
	s.expect(ipA, "blackhole", confMed)
	cl := s.closes(ipA)[0]
	if field(cl, "cutheld") != "true" {
		t.Fatalf("not held: %s", cl)
	}
	if d, _ := time.ParseDuration(field(cl, "dur")); d < 10*time.Minute {
		t.Fatalf("dur %v: %s", d, cl)
	}
}

// A path that recovers is not held: the episode closes on the quiet rule.
func TestHoldEndsOnRecovery(t *testing.T) {
	s := newSim(t, SideKharej)
	s.quiet(3)
	for i := 0; i < 6; i++ {
		s.healthy(ipB)
		s.dial(ipA, ipK1, DialConnTimeout)
		s.dial(ipA, ipK2, DialConnTimeout)
		s.path(ipA, ipK1, PathSample{Loss: 1, Silent: true})
		s.tick()
	}
	s.quiet(20) // healthy readings (bytes acked) clear the dead state
	if n := len(s.closes(ipA)); n != 1 {
		t.Fatalf("want 1 close after recovery, got %d:\n%s", n, strings.Join(s.lines, "\n"))
	}
}

func massRST(s *sim, ir netip.Addr, n int) {
	for k := 0; k < n; k++ { // injection timing: 5 ms after our send on a 90 ms path
		s.death(ir, ipK1, Death{Class: DeathRST, LifeMs: 600000, Bytes: 1 << 22, HaveTCP: true, LastSentMs: 5, MinRTTUs: 90000})
	}
}

// slot 4: a peer restart under l3mtcp: fast RSTs on every link of every
// Iran IP at once, then the redials come back. Peer restart, not rst-inject.
func TestPeerRestartMassFastRST(t *testing.T) {
	s := newSim(t, SideIran)
	s.quiet(3)
	massRST(s, ipA, 16)
	massRST(s, ipB, 16)
	s.addUsers(50)
	s.tick()
	s.tick()
	for i := 0; i < 3; i++ {
		s.accept(ipA, ipK1, AcceptOK)
		s.accept(ipB, ipK1, AcceptOK)
		s.quiet(1)
	}
	s.quiet(24)
	if s.e.nMass != 1 {
		t.Fatalf("mass restarts %d, want 1:\n%s", s.e.nMass, strings.Join(s.lines, "\n"))
	}
	s.expect(ipA, "peer-restart", confMed)
	s.noCensorship(ipA, confUnknown)
	s.noCensorship(ipB, confUnknown)
}

// Injection on one IP only, even with redials succeeding: not a mass burst.
func TestRSTInjectOneIPNotMass(t *testing.T) {
	s := newSim(t, SideIran)
	s.quiet(3)
	for i := 0; i < 24; i++ {
		s.healthy(ipB)
		s.addUsers(50)
		massRST(s, ipA, 2)
		s.accept(ipA, ipK1, AcceptOK)
		s.tick()
	}
	s.quiet(24)
	if s.e.nMass != 0 {
		t.Fatalf("one-IP injection read as a peer restart")
	}
	s.expect(ipA, "rst-inject", confHigh)
}

// Injection on every IP that keeps resetting the redials: not a restart.
func TestRSTInjectAllIPsContinuingNotMass(t *testing.T) {
	s := newSim(t, SideIran)
	s.quiet(3)
	for i := 0; i < 24; i++ {
		s.addUsers(50)
		massRST(s, ipA, 3)
		massRST(s, ipB, 3)
		s.accept(ipA, ipK1, AcceptOK)
		s.accept(ipB, ipK1, AcceptOK)
		s.tick()
	}
	s.quiet(24)
	if s.e.nMass != 0 {
		t.Fatalf("continuing injection on all IPs read as a peer restart:\n%s", strings.Join(s.lines, "\n"))
	}
	for _, ip := range []netip.Addr{ipA, ipB} {
		for _, l := range s.closes(ip) {
			if field(l, "class") == "peer-restart" {
				t.Fatalf("called peer-restart: %s", l)
			}
		}
	}
}

// slot 5 (e): bufferbloat drops links on every IP in one tick with no fast
// RST (the old peer-restart signature), but the timeouts and the queue go
// on after the drop. Without a clean redial the peer grace must not be set.
func TestOldSignatureNeedsRedials(t *testing.T) {
	s := newSim(t, SideIran)
	s.quiet(3)
	for i := 0; i < 20; i++ {
		s.addUsers(50)
		for _, ir := range []netip.Addr{ipA, ipB} {
			s.death(ir, ipK1, Death{Class: DeathTimeout, LifeMs: 600000, Bytes: 1 << 22})
			s.death(ir, ipK1, Death{Class: DeathEOF, LifeMs: 600000, Bytes: 1 << 22})
			s.accept(ir, ipK1, AcceptOK) // the pool keeps redialing into the queue
			s.path(ir, ipK1, PathSample{Loss: 0.002, QueueMs: 900, RateBps: 2e6, PeerLoss: 0.25})
		}
		s.tick()
	}
	s.quiet(24)
	if s.e.nOld != 0 || s.e.nMass != 0 {
		t.Fatalf("bufferbloat confirmed as a peer restart (old %d mass %d)", s.e.nOld, s.e.nMass)
	}
	for _, ip := range []netip.Addr{ipA, ipB} {
		for _, l := range s.closes(ip) {
			if c := field(l, "class"); c == "peer-restart" || c == "asym-loss" || c == "intl-cut" {
				t.Fatalf("bufferbloat called %s: %s", c, l)
			}
		}
		s.noCensorship(ip, confLow)
	}
}

// slot 5 (e): the old signature with a clean redial on every IP is confirmed.
func TestOldSignatureConfirmedByRedials(t *testing.T) {
	s := newSim(t, SideIran)
	s.quiet(3)
	for _, ir := range []netip.Addr{ipA, ipB} {
		for k := 0; k < 4; k++ {
			s.death(ir, ipK1, Death{Class: DeathEOF, LifeMs: 3600000, Bytes: 1 << 24})
		}
	}
	s.addUsers(50)
	s.tick()
	for i := 0; i < 10; i++ {
		s.accept(ipA, ipK1, AcceptOK)
		s.accept(ipB, ipK1, AcceptOK)
		s.quiet(1)
	}
	if s.e.nOld != 1 || s.e.peerRestart.IsZero() {
		t.Fatalf("old-signature restart not confirmed (old %d)", s.e.nOld)
	}
}

// slot 5 (f): the peer's resends under a standing queue (the sender's
// segments overflow the bottleneck queue) are bufferbloat, not asym-loss;
// nor is one-direction loss during silence (an outage).
func TestAsymLossExcludedByQueueOrSilence(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    PathSample
	}{
		{"queue", PathSample{Loss: 0.002, QueueMs: 400, RateBps: 8e6, PeerLoss: 0.28}},
		{"silence", PathSample{Loss: 0.002, QueueMs: 10, RateBps: 8e6, PeerLoss: 0.28}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSim(t, SideKharej)
			s.quiet(3)
			for i := 0; i < 20; i++ {
				s.healthy(ipB)
				s.path(ipA, ipK1, tc.p)
				if tc.name == "silence" && i%4 == 0 {
					s.path(ipA, ipK2, PathSample{Loss: 1, Silent: true})
				}
				s.tick()
			}
			s.quiet(24)
			for _, l := range s.closes(ipA) {
				if field(l, "class") == "asym-loss" {
					t.Fatalf("called asym-loss: %s", l)
				}
			}
		})
	}
}

// slot 5 (g): heavy one-direction loss on every IP (a few silent readings,
// but the path still moves data) and a deep queue with failing hellos on
// every IP are not an international cut.
func TestIntlCutNeedsRealCut(t *testing.T) {
	t.Run("asymloss", func(t *testing.T) {
		s := newSim(t, SideIran)
		s.quiet(30)
		for i := 0; i < 40; i++ {
			s.addUsers(50)
			for _, ir := range []netip.Addr{ipA, ipB} {
				s.path(ir, ipK1, PathSample{Loss: 0.12, QueueMs: 3, RateBps: 3e6, PeerLoss: 0.004})
				if i%10 == 0 {
					s.path(ir, ipK1, PathSample{Loss: 1, Silent: true})
					s.death(ir, ipK1, Death{Class: DeathTimeout, LifeMs: 600000, Bytes: 1 << 22})
				}
			}
			s.tick()
		}
		s.quiet(24)
		for _, ip := range []netip.Addr{ipA, ipB} {
			s.noCensorship(ip, confLow)
		}
	})
	t.Run("bufferbloat", func(t *testing.T) {
		s := newSim(t, SideIran)
		s.quiet(30)
		for i := 0; i < 40; i++ {
			s.addUsers(50)
			for _, ir := range []netip.Addr{ipA, ipB} {
				s.path(ir, ipK1, PathSample{Loss: 0.007, QueueMs: 2000, RateBps: 1e6, PeerLoss: 0.25})
				s.accept(ir, ipK1, AcceptNoHello)
				if i%10 == 0 {
					s.accept(ir, ipK1, AcceptOK)
				}
			}
			s.tick()
		}
		s.quiet(24)
		for _, ip := range []netip.Addr{ipA, ipB} {
			s.noCensorship(ip, confLow)
		}
	})
}

// slot 6 (i): killing the peer process: FIN on idle links, RST on links
// with unread data (run a: .1 had 4 RST + 12 EOF; here 1 RST + 15 EOF,
// below massPerIP RSTs on that IP). Mass rule over both.
func TestPeerRestartMassRSTAndEOF(t *testing.T) {
	s := newSim(t, SideIran)
	s.quiet(3)
	for _, x := range []struct {
		ir       netip.Addr
		rst, eof int
	}{{ipA, 1, 15}, {ipB, 6, 10}} {
		massRST(s, x.ir, x.rst)
		for k := 0; k < x.eof; k++ {
			s.death(x.ir, ipK1, Death{Class: DeathEOF, LifeMs: 600000, Bytes: 1 << 22})
		}
	}
	s.addUsers(50)
	s.tick()
	s.tick()
	for i := 0; i < 3; i++ {
		s.accept(ipA, ipK1, AcceptOK)
		s.accept(ipB, ipK1, AcceptOK)
		s.quiet(1)
	}
	s.quiet(24)
	if s.e.nMass != 1 {
		t.Fatalf("mass restarts %d, want 1:\n%s", s.e.nMass, strings.Join(s.lines, "\n"))
	}
	s.noCensorship(ipA, confUnknown)
	s.noCensorship(ipB, confUnknown)
}

// slot 6 (i): FIN-only bursts on every IP (graceful retires) never confirm
// the mass rule: a retire resets nothing.
func TestMassNeedsAnRST(t *testing.T) {
	s := newSim(t, SideIran)
	s.quiet(3)
	for _, ir := range []netip.Addr{ipA, ipB} {
		for k := 0; k < 6; k++ {
			s.death(ir, ipK1, Death{Class: DeathEOF, LifeMs: 600000, Bytes: 1 << 22})
		}
	}
	s.tick()
	for i := 0; i < 10; i++ {
		s.accept(ipA, ipK1, AcceptOK)
		s.accept(ipB, ipK1, AcceptOK)
		s.quiet(1)
	}
	if s.e.nMass != 0 {
		t.Fatalf("FIN-only burst confirmed as a mass restart")
	}
}

// slot 6 (i): RST injection on one IP stays rst-inject even while the other
// IP loses links by FIN (retires) and every redial on both IPs succeeds.
func TestRSTInjectOneIPWithRetiresNotMass(t *testing.T) {
	s := newSim(t, SideIran)
	s.quiet(3)
	for i := 0; i < 24; i++ {
		s.addUsers(50)
		massRST(s, ipA, 2)
		s.death(ipB, ipK1, Death{Class: DeathEOF, LifeMs: 600000, Bytes: 1 << 22})
		s.death(ipB, ipK1, Death{Class: DeathEOF, LifeMs: 600000, Bytes: 1 << 22})
		s.accept(ipA, ipK1, AcceptOK)
		s.accept(ipB, ipK1, AcceptOK)
		s.tick()
	}
	s.quiet(24)
	if s.e.nMass != 0 {
		t.Fatalf("one-IP injection + retires read as a peer restart:\n%s", strings.Join(s.lines, "\n"))
	}
	s.expect(ipA, "rst-inject", confHigh)
}

// slot 6 (j): bufferbloat (2 s standing queue): long-lived links die by
// slow RSTs with clustered lifetimes. Not delayed-kill or fingerprint-kill.
func TestKillClassesExcludedByQueueWithoutFastRST(t *testing.T) {
	for _, fast := range []bool{false, true} {
		s := newSim(t, SideIran)
		s.quiet(3)
		for i := 0; i < 20; i++ {
			s.addUsers(50)
			s.healthy(ipB)
			last := uint32(1500) // slow: the RST comes long after our send
			if fast {
				last = 5
			}
			s.death(ipA, ipK1, Death{Class: DeathRST, LifeMs: 130000 + uint32(s.rng.Intn(4000)), Bytes: uint64(1 << (12 + s.rng.Intn(10))), HaveTCP: true, LastSentMs: last, MinRTTUs: 45000})
			s.path(ipA, ipK1, PathSample{Loss: 0.003, QueueMs: 1200, RateBps: 2e6, PeerLoss: -1})
			s.tick()
		}
		s.quiet(24)
		kill := false
		for _, l := range s.closes(ipA) {
			if c := field(l, "class"); c == "delayed-kill" || c == "fingerprint-kill" {
				kill = true
			}
		}
		if !fast && kill {
			t.Fatalf("bufferbloat with slow RSTs called a kill class:\n%s", strings.Join(s.lines, "\n"))
		}
		if !fast {
			s.noCensorship(ipA, confLow)
		}
	}
}

// slot 7 (l): bufferbloat (a 900 ms standing queue): the busy links die by
// peer RSTs and about a third of them land "fast" by chance (a busy link
// always sent recently). With the other IP clean and fresh links dying,
// slot 6 called this rst-inject med. Under the queue a minority fast share
// is explained by the queue. A majority fast share is still injection.
func TestRSTInjectUnderQueueNeedsFastMajority(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fastOf int // fast RSTs per 3
	}{{"bloat-third-fast", 1}, {"inject-all-fast", 3}} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSim(t, SideIran)
			s.quiet(3)
			for i := 0; i < 20; i++ {
				s.addUsers(50)
				s.healthy(ipB)
				for k := 0; k < 12; k++ {
					last := uint32(700) // the peer's RST, not tied to our send
					if k%3 < tc.fastOf {
						last = 5 // our link was sending at that moment
					}
					life, bytes := uint32(600000), uint64(1<<20)
					if k%4 == 0 {
						// redialed links die too (fresh), in the handshake
						// (slot 11 (w): young kills past the handshake
						// bytes at one count are a byte trigger)
						life, bytes = 3000, 2500
					}
					s.death(ipA, ipK1, Death{Class: DeathRST, LifeMs: life, Bytes: bytes, HaveTCP: true, LastSentMs: last, MinRTTUs: 45000})
				}
				s.path(ipA, ipK1, PathSample{Loss: 0.004, QueueMs: 900, RateBps: 2e6, PeerLoss: -1})
				s.accept(ipA, ipK1, AcceptOK)
				s.tick()
			}
			s.quiet(24)
			cls := ""
			for _, l := range s.closes(ipA) {
				if c := field(l, "class"); IsCensorship(classIndex(c)) && field(l, "conf") != "low" && field(l, "conf") != "unknown" {
					cls = c
				}
			}
			switch {
			case tc.fastOf == 1 && cls != "":
				t.Fatalf("bufferbloat called %s med+:\n%s", cls, strings.Join(s.lines, "\n"))
			case tc.fastOf == 3 && cls != "rst-inject":
				t.Fatalf("injection under a queue not called rst-inject med+ (got %q):\n%s", cls, strings.Join(s.lines, "\n"))
			}
		})
	}
}

func classIndex(name string) int {
	for i := 0; i < NClasses; i++ {
		if className[i] == name {
			return i
		}
	}
	return -1
}

// slot 7 (m): the res5 run-a case. An injection burst on IP A (30 fast RSTs
// in one tick) while IP B loses a few links (3 EOF, background churn); the
// injection then stops and every redial on both IPs succeeds. Every IP has
// >= massPerIP deaths, an RST is in the burst and the post window is clean,
// so the slot 6 mass rule confirmed a peer restart (peermass = 330 in the
// lab). The burst is not balanced across the IPs, so it is not a restart.
func TestMassNeedsBalancedBurst(t *testing.T) {
	s := newSim(t, SideIran)
	s.quiet(3)
	massRST(s, ipA, 30)
	for k := 0; k < 3; k++ {
		s.death(ipB, ipK1, Death{Class: DeathEOF, LifeMs: 600000, Bytes: 1 << 22})
	}
	s.addUsers(50)
	s.tick()
	s.tick()
	for i := 0; i < 3; i++ {
		s.accept(ipA, ipK1, AcceptOK)
		s.accept(ipB, ipK1, AcceptOK)
		s.quiet(1)
	}
	s.quiet(24)
	if s.e.nMass != 0 {
		t.Fatalf("one-IP injection burst + EOFs on the other IP confirmed as a peer restart:\n%s", strings.Join(s.lines, "\n"))
	}
	for _, l := range s.closes(ipA) {
		if field(l, "class") == "peer-restart" {
			t.Fatalf("called peer-restart: %s", l)
		}
	}
}

// slot 7: loss on the peer's send direction only (ploss 15 %, our loss
// 0.2 %, our queue 0 ms) is what a one-direction congestion queue looks
// like from this side: the queue that explains the drops is on the peer's
// direction and not reported here. Not asym-loss on this side.
func TestAsymLossPeerDirectionNotJudgedHere(t *testing.T) {
	s := newSim(t, SideIran)
	s.quiet(3)
	for i := 0; i < 20; i++ {
		s.addUsers(50)
		s.healthy(ipB)
		s.path(ipA, ipK1, PathSample{Loss: 0.002, QueueMs: 0, RateBps: 4e6, PeerLoss: 0.15})
		s.tick()
	}
	s.quiet(24)
	for _, l := range s.closes(ipA) {
		if field(l, "class") == "asym-loss" {
			t.Fatalf("peer-direction loss called asym-loss here: %s", l)
		}
	}
}

// slot 8 (n): res6 run b. A killed peer comes back in about 2 s and refills
// its pool at once, so every redial lands in the same 5 s tick as the
// deaths (5 RST + 11 EOF on each IP, 16 accepts each). The arming tick's
// redials count: a confirmed peer restart, not rst-inject.
func TestMassRedialsInBurstTick(t *testing.T) {
	s := newSim(t, SideIran)
	s.quiet(3)
	for _, ir := range []netip.Addr{ipA, ipB} {
		massRST(s, ir, 5)
		for k := 0; k < 11; k++ {
			s.death(ir, ipK1, Death{Class: DeathEOF, LifeMs: 600000, Bytes: 1 << 22})
		}
		for k := 0; k < 16; k++ {
			s.accept(ir, ipK1, AcceptOK)
		}
	}
	s.addUsers(50)
	s.tick()
	s.quiet(30)
	if s.e.nMass != 1 {
		t.Fatalf("mass restarts %d, want 1:\n%s", s.e.nMass, strings.Join(s.lines, "\n"))
	}
	s.noCensorship(ipA, confUnknown)
	s.noCensorship(ipB, confUnknown)
}

// slot 8 (o): res6 run d. An RST injection on A ends, the episode is still
// open, and 40 s later a burst on every IP (16 links each) is confirmed as
// a restart. The restart explains the burst's RSTs only; the injection's
// RSTs stay, so A is still rst-inject, not peer-restart.
func TestMassExplainsOnlyBurstRSTs(t *testing.T) {
	s := newSim(t, SideIran)
	s.quiet(3)
	for i := 0; i < 12; i++ {
		s.healthy(ipB)
		s.addUsers(50)
		massRST(s, ipA, 15)
		s.accept(ipA, ipK1, AcceptOK)
		s.tick()
	}
	s.quiet(8)
	for _, ir := range []netip.Addr{ipA, ipB} {
		for k := 0; k < 16; k++ { // the peer's own resets: not timed to our sends
			s.death(ir, ipK1, Death{Class: DeathRST, LifeMs: 600000, Bytes: 1 << 22, HaveTCP: true, LastSentMs: 900, MinRTTUs: 90000})
		}
	}
	s.addUsers(50)
	s.tick()
	for i := 0; i < 3; i++ {
		s.accept(ipA, ipK1, AcceptOK)
		s.accept(ipB, ipK1, AcceptOK)
		s.quiet(1)
	}
	s.quiet(30)
	if s.e.nMass != 1 {
		t.Fatalf("mass restarts %d, want 1:\n%s", s.e.nMass, strings.Join(s.lines, "\n"))
	}
	cl := s.closes(ipA)
	if len(cl) == 0 || field(cl[0], "class") != "rst-inject" {
		t.Fatalf("injection episode not rst-inject after a later restart burst:\n%s", strings.Join(s.lines, "\n"))
	}
	s.noCensorship(ipB, confUnknown)
}

// slot 9 (s): a delayed kill resets links once they reach an age, not at
// once or after N bytes. Iran side, injection timing on every RST: first
// the old links die, then each redial dies at 30-32 s of age. Delayed-kill,
// not rst-inject (res7: delayedkill read as rst-inject med and high).
func dkRun(t *testing.T, redialLifeMs func(r *rand.Rand) uint32) *sim {
	s := newSim(t, SideIran)
	s.quiet(3)
	for i := 0; i < 24; i++ {
		s.healthy(ipB)
		s.healthy(ipA)
		s.addUsers(50)
		n, life := 3, func() uint32 { return redialLifeMs(s.rng) }
		if i < 2 {
			n, life = 8, func() uint32 { return 600000 + uint32(s.rng.Intn(600000)) } // the old pool
		}
		for k := 0; k < n; k++ {
			s.death(ipA, ipK1, Death{Class: DeathRST, LifeMs: life(), Bytes: uint64(1 << (12 + s.rng.Intn(12))),
				HaveTCP: true, LastSentMs: 5, MinRTTUs: 90000})
		}
		s.accept(ipA, ipK1, AcceptOK)
		s.tick()
	}
	s.quiet(24)
	return s
}

func TestDelayedKillByAgeNotRSTInject(t *testing.T) {
	s := dkRun(t, func(r *rand.Rand) uint32 { return 30000 + uint32(r.Intn(2500)) })
	s.expect(ipA, "delayed-kill", confMed)
	s.noCensorship(ipB, confUnknown)
}

// The same stream with the redials dying young (at once / after N bytes on
// a busy link) stays rst-inject high.
func TestRSTInjectYoungRedialsStaysRSTInject(t *testing.T) {
	s := dkRun(t, func(r *rand.Rand) uint32 { return 300 + uint32(r.Intn(3000)) })
	s.expect(ipA, "rst-inject", confHigh)
}

// slot 9 (q): an RST injection right after a bufferbloat episode keeps the
// episode open (the trigger never lapses), so the queue's mean covers the
// whole run. The RSTs of the clean ticks are not explained by the queue
// (res7 run a: 263 RSTs on a clean path read as bufferbloat low).
func TestRSTAfterQueueNotExplainedByIt(t *testing.T) {
	s := newSim(t, SideIran)
	s.quiet(3)
	for i := 0; i < 10; i++ { // bufferbloat: a standing queue, no RSTs
		s.healthy(ipB)
		s.addUsers(50)
		s.path(ipA, ipK1, PathSample{Loss: 0.002, QueueMs: 2400, RateBps: 2e6})
		s.tick()
	}
	for i := 0; i < 14; i++ { // queue gone, RSTs on every tick (a third fast)
		s.healthy(ipB)
		s.addUsers(50)
		s.path(ipA, ipK1, PathSample{Loss: 0.002, QueueMs: 5, RateBps: 8e6})
		for k := 0; k < 12; k++ {
			last, life, bytes := uint32(700), uint32(600000), uint64(1<<20)
			if k%3 == 0 {
				last = 5
			}
			if k%4 == 0 {
				// redials die young, in the handshake (slot 11 (w): young
				// kills past the handshake bytes are a byte trigger)
				life, bytes = 2000, 2500
			}
			s.death(ipA, ipK1, Death{Class: DeathRST, LifeMs: life, Bytes: bytes, HaveTCP: true, LastSentMs: last, MinRTTUs: 90000})
		}
		s.accept(ipA, ipK1, AcceptOK)
		s.tick()
	}
	s.quiet(24)
	cl := s.closes(ipA)
	// slot 10 (u): the RSTs now open their own episode (the last one).
	if len(cl) == 0 || field(cl[len(cl)-1], "class") != "rst-inject" || field(cl[len(cl)-1], "conf") == "low" || field(cl[len(cl)-1], "conf") == "unknown" {
		t.Fatalf("injection after a queue episode not rst-inject med+:\n%s", strings.Join(s.lines, "\n"))
	}
}

// slot 9 (p): after a restart, our next send on an old link of the dead
// process draws an RST from the peer kernel, at any time. Those leftovers
// (links born before the burst) do not count against massPostMax; resets
// of the redials (born after it) still do, so injection cannot pass.
func massPostRun(t *testing.T, postLifeMs, postRecvMs uint32) *sim {
	s := newSim(t, SideIran)
	s.quiet(3)
	massRST(s, ipA, 6)
	massRST(s, ipB, 4)
	s.addUsers(50)
	s.tick()
	for i := 0; i < 8; i++ {
		s.accept(ipA, ipK1, AcceptOK)
		s.accept(ipB, ipK1, AcceptOK)
		if i >= 2 && i < 6 { // 4 RSTs within 20 s after the burst span
			s.death(ipA, ipK1, Death{Class: DeathRST, LifeMs: postLifeMs, Bytes: 1 << 22, HaveTCP: true, LastSentMs: 5, LastRecvMs: postRecvMs, MinRTTUs: 90000})
		}
		s.quiet(1)
	}
	s.quiet(24)
	return s
}

func TestMassPostLeftoverRSTsDoNotReject(t *testing.T) {
	s := massPostRun(t, 900000, 60000) // old link, nothing received since the restart
	if s.e.nMass != 1 {
		t.Fatalf("mass restarts %d, want 1:\n%s", s.e.nMass, strings.Join(s.lines, "\n"))
	}
	s.noCensorship(ipA, confUnknown)
}

func TestMassPostRedialResetsStillReject(t *testing.T) {
	for _, tc := range []struct {
		name         string
		life, recvMs uint32
	}{{"redial", 2000, 20}, {"live-old-link", 900000, 20}} {
		t.Run(tc.name, func(t *testing.T) {
			s := massPostRun(t, tc.life, tc.recvMs)
			if s.e.nMass != 0 {
				t.Fatalf("resets after the burst passed as a restart (%d):\n%s", s.e.nMass, strings.Join(s.lines, "\n"))
			}
		})
	}
}

// slot 10 (t): a delayed kill whose redials die at one age (30-31 s) with
// byte counts spread inside one log2 bin (4-8 MB): the log2 bins tie (res8
// run b, 98 % / 98 %), the spread in log space does not. Delayed-kill, not
// fingerprint-kill.
func fkRun(t *testing.T, life func(r *rand.Rand) uint32, bytes func(r *rand.Rand) uint64) *sim {
	s := newSim(t, SideIran)
	s.quiet(3)
	for i := 0; i < 24; i++ {
		s.healthy(ipB)
		s.healthy(ipA)
		s.addUsers(50)
		for k := 0; k < 3; k++ {
			s.death(ipA, ipK1, Death{Class: DeathRST, LifeMs: life(s.rng), Bytes: bytes(s.rng),
				HaveTCP: true, LastSentMs: 700, MinRTTUs: 90000})
		}
		s.accept(ipA, ipK1, AcceptOK)
		s.tick()
	}
	s.quiet(24)
	return s
}

func TestDelayedKillAgeTighterNotFingerprintKill(t *testing.T) {
	s := fkRun(t, func(r *rand.Rand) uint32 { return 30000 + uint32(r.Intn(1000)) },
		func(r *rand.Rand) uint64 { return 1<<22 + uint64(r.Intn(1<<22)) })
	s.expect(ipA, "delayed-kill", confMed)
}

// The same mid-age kills (no young ones: the delayed-kill age signature)
// at one byte count with ages spread 12-110 s stay fingerprint-kill.
func TestFingerprintKillBytesTighterMidAge(t *testing.T) {
	s := fkRun(t, func(r *rand.Rand) uint32 { return 12000 + uint32(r.Intn(98000)) },
		func(r *rand.Rand) uint64 { return 20000 + uint64(r.Intn(500)) })
	s.expect(ipA, "fingerprint-kill", confMed)
}

// slot 10 (u): a standing queue, then clean-path RSTs on the same IP with
// no lapse in the trigger (res8 runs c and d): the episode splits where the
// evidence changes kind. The queue part closes as an ordinary class; the
// RSTs open their own episode, rst-inject med+.
func TestQueueThenCleanRSTsSplit(t *testing.T) {
	s := newSim(t, SideIran)
	s.quiet(3)
	for i := 0; i < 10; i++ {
		s.healthy(ipB)
		s.addUsers(50)
		s.path(ipA, ipK1, PathSample{Loss: 0.002, QueueMs: 2400, RateBps: 2e6})
		s.tick()
	}
	for i := 0; i < 14; i++ {
		s.healthy(ipB)
		s.addUsers(50)
		s.path(ipA, ipK1, PathSample{Loss: 0.002, QueueMs: 5, RateBps: 8e6})
		for k := 0; k < 12; k++ {
			s.death(ipA, ipK1, Death{Class: DeathRST, LifeMs: 1500, Bytes: 300, HaveTCP: true, LastSentMs: 5, MinRTTUs: 90000})
		}
		s.accept(ipA, ipK1, AcceptOK)
		s.tick()
	}
	s.quiet(24)
	cl := s.closes(ipA)
	if len(cl) != 2 {
		t.Fatalf("want 2 episodes (queue, then RSTs), got %d:\n%s", len(cl), strings.Join(s.lines, "\n"))
	}
	if c := field(cl[0], "class"); c == "rst-inject" || c == "fingerprint-kill" || c == "delayed-kill" {
		t.Fatalf("queue part read as %s:\n%s", c, strings.Join(s.lines, "\n"))
	}
	// the clean run's kills moved with it: none left on the queue part
	if a := field(cl[0], "alt"); strings.HasPrefix(a, "delayed-kill") || strings.HasPrefix(a, "fingerprint-kill") || strings.HasPrefix(a, "rst-inject") {
		t.Fatalf("queue part kept the RST part's kills (alt %s):\n%s", a, strings.Join(s.lines, "\n"))
	}
	if field(cl[1], "class") != "rst-inject" || field(cl[1], "conf") == "low" || field(cl[1], "conf") == "unknown" {
		t.Fatalf("RST part not rst-inject med+:\n%s", strings.Join(s.lines, "\n"))
	}
}

// A queue that drains for a moment with no RSTs and comes back stays one
// episode (only clean-path RST evidence splits).
func TestQueueDipNoSplit(t *testing.T) {
	s := newSim(t, SideIran)
	s.quiet(3)
	for i := 0; i < 24; i++ {
		s.healthy(ipB)
		s.addUsers(50)
		q := 2400.0
		if i%6 == 5 {
			q = 5
		}
		s.path(ipA, ipK1, PathSample{Loss: 0.002, QueueMs: q, RateBps: 2e6})
		s.tick()
	}
	s.quiet(24)
	if cl := s.closes(ipA); len(cl) != 1 {
		t.Fatalf("want 1 episode, got %d:\n%s", len(cl), strings.Join(s.lines, "\n"))
	}
}

// slot 10 (r): a standing queue on every IP opens episodes, then the peer
// restarts (a confirmed mass burst). The episodes opened before the burst
// get no peer grace (slot 8 (o)) and only the queue is left once the burst
// is taken out: res8 read three restarts as bufferbloat low. The confirmed
// restart takes precedence over the ordinary classes: peer-restart.
func TestRestartUnderQueueIsPeerRestart(t *testing.T) {
	s := newSim(t, SideIran)
	s.quiet(3)
	qtick := func() {
		s.path(ipA, ipK1, PathSample{Loss: 0.002, QueueMs: 600, RateBps: 2e6})
		s.path(ipB, ipK1, PathSample{Loss: 0.002, QueueMs: 600, RateBps: 2e6})
		s.addUsers(50)
		s.tick()
	}
	for i := 0; i < 10; i++ {
		qtick()
	}
	massRST(s, ipA, 16)
	massRST(s, ipB, 16)
	qtick()
	qtick()
	for i := 0; i < 6; i++ {
		s.accept(ipA, ipK1, AcceptOK)
		s.accept(ipB, ipK1, AcceptOK)
		qtick()
	}
	s.quiet(24)
	if s.e.nMass != 1 {
		t.Fatalf("mass restarts %d, want 1:\n%s", s.e.nMass, strings.Join(s.lines, "\n"))
	}
	for _, ip := range []netip.Addr{ipA, ipB} {
		cl := s.closes(ip)
		if len(cl) == 0 || field(cl[len(cl)-1], "class") != "peer-restart" {
			t.Fatalf("%s: restart under a queue not peer-restart:\n%s", ip, strings.Join(s.lines, "\n"))
		}
	}
}

// fpRun (slot 11, w) is res9's fpkill: a reset after 20000 bytes (both
// directions) on busy links. The old pool dies at its next segment, and
// every redial dies 0.5-2 s old at about 20 KB, with injection timing.
//
// With mid, every eighth kill is a 30 s link at 2-6 MB (other trouble on
// the IP; res9 run a had 15 % of the kills outside the 2^14 B bin): the
// log spread of the bytes is then wider than that of the ages, though the
// young kills sit at one byte count.
func fpRun(t *testing.T, side Side, mid bool, bytes func(r *rand.Rand) uint64) *sim {
	s := newSim(t, side)
	s.quiet(3)
	for i := 0; i < 20; i++ {
		s.healthy(ipB)
		s.healthy(ipA)
		s.addUsers(50)
		for k := 0; k < 4; k++ {
			life, b := uint32(500+s.rng.Intn(1500)), bytes(s.rng)
			if i == 0 {
				life, b = 600000+uint32(s.rng.Intn(600000)), uint64(1<<20+s.rng.Intn(1<<22)) // the old pool
			} else if mid && i%2 == 0 && k == 3 {
				life, b = 30000+uint32(s.rng.Intn(5000)), uint64(1<<21+s.rng.Intn(1<<22))
			}
			s.death(ipA, ipK1, Death{Class: DeathRST, LifeMs: life, Bytes: b, HaveTCP: true, LastSentMs: 5, MinRTTUs: 90000})
		}
		s.accept(ipA, ipK1, AcceptOK)
		s.dial(ipA, ipK1, DialOK)
		s.tick()
	}
	s.quiet(24)
	return s
}

// slot 11 (w): young kills past the handshake bytes, clustered at one byte
// count, are fingerprint-kill on both sides, never rst-inject.
func TestFPKillYoungPastHandshakeIsFingerprintKill(t *testing.T) {
	for _, side := range []Side{SideIran, SideKharej} {
		for _, mid := range []bool{false, true} {
			s := fpRun(t, side, mid, func(r *rand.Rand) uint64 { return 19000 + uint64(r.Intn(1500)) })
			s.expect(ipA, "fingerprint-kill", confMed)
			s.noCensorship(ipB, confUnknown)
		}
	}
}

// The same young resets within the handshake bytes (an immediate reset,
// Reality or tls) stay rst-inject high on Iran, med+ on Kharej.
func TestRSTInjectWithinHandshakeBytes(t *testing.T) {
	for _, side := range []Side{SideIran, SideKharej} {
		s := fpRun(t, side, false, func(r *rand.Rand) uint64 { return 300 + uint64(r.Intn(7000)) })
		want := confMed
		if side == SideIran {
			want = confHigh
		}
		s.expect(ipA, "rst-inject", want)
	}
}

// slot 11 (x): res9 run a. A peer restart 40 s after an rst event on A:
// the LB had moved most links to B, so A lost 6 (3 RST, 3 FIN) and B 34
// (8 RST, 26 FIN). The deaths are 34 vs 6, but the resets 8 vs 6: a
// restart, not a one-IP injection.
func TestMassUnevenLinksFINHeavyIsRestart(t *testing.T) {
	s := newSim(t, SideIran)
	s.quiet(3)
	massRST(s, ipA, 3)
	massRST(s, ipB, 8)
	for k := 0; k < 3; k++ {
		s.death(ipA, ipK1, Death{Class: DeathEOF, LifeMs: 600000, Bytes: 1 << 22})
	}
	for k := 0; k < 26; k++ {
		s.death(ipB, ipK1, Death{Class: DeathEOF, LifeMs: 600000, Bytes: 1 << 22})
	}
	for k := 0; k < 8; k++ {
		s.accept(ipA, ipK1, AcceptOK)
		s.accept(ipB, ipK1, AcceptOK)
	}
	s.addUsers(50)
	s.tick()
	s.quiet(30)
	if s.e.nMass != 1 {
		t.Fatalf("mass restarts %d, want 1:\n%s", s.e.nMass, strings.Join(s.lines, "\n"))
	}
	s.noCensorship(ipA, confUnknown)
	s.noCensorship(ipB, confUnknown)
}
