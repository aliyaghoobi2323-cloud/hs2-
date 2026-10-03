package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
)

// Tests for the resource-aware link-pool ceiling: auto (max_links 0) vs fixed,
// the effective ceiling both servers show in direct and reverse, the drift
// notes, the doctor check, and the status file the installer menu reads.

// pinHW makes detectHW report this hardware for the rest of the test.
func pinHW(t *testing.T, ramMB, cpus int) {
	t.Helper()
	old := detectHW
	detectHW = func() (int, int) { return ramMB, cpus }
	t.Cleanup(func() { detectHW = old })
}

const (
	lowRAM, lowCPU   = 1024, 1 // a 1 GB Iran VPS
	medRAM, medCPU   = 2048, 2
	highRAM, highCPU = 8192, 4
)

func TestLinkCeilingAutoFollowsHardware(t *testing.T) {
	for _, c := range []struct {
		ram, cpu int
		want     int
		profile  string
	}{{lowRAM, lowCPU, 32, "low"}, {medRAM, medCPU, 48, "medium"}, {highRAM, highCPU, 170, "high"},
		{17408, 20, 300, "high"}, {22528, 12, 300, "high"}} { // the production pair
		pinHW(t, c.ram, c.cpu)
		for _, cfg := range []string{`{"max_links": 0}`, `{"mode":"dial","max_links":0,"min_links":2}`} {
			var fc fileConfig
			if err := json.Unmarshal([]byte(cfg), &fc); err != nil {
				t.Fatal(err)
			}
			got, mode, prof := linkCeiling(fc)
			if got != c.want || mode != ceilAuto || prof != c.profile {
				t.Errorf("%s on %dMB/%dcpu: got %d %s %s, want %d auto %s", cfg, c.ram, c.cpu, got, mode, prof, c.want, c.profile)
			}
			if _, max, _ := linkEnvelope(fc); max != c.want {
				t.Errorf("%s on %dMB/%dcpu: envelope max %d, want %d", cfg, c.ram, c.cpu, max, c.want)
			}
		}
	}
}

// A positive max_links is the operator's choice: used as is on any hardware,
// never moved by the auto rule.
func TestLinkCeilingFixedIsNeverMoved(t *testing.T) {
	for _, hw := range [][2]int{{lowRAM, lowCPU}, {highRAM, highCPU}} {
		pinHW(t, hw[0], hw[1])
		got, mode, _ := linkCeiling(fileConfig{MaxLinks: 32})
		if got != 32 || mode != ceilFixed {
			t.Fatalf("fixed 32 on %v: got %d %s", hw, got, mode)
		}
	}
}

// A config WITHOUT max_links (written before the auto ceiling, by an older
// installer or by hand) keeps exactly the 32 it always had, on any hardware —
// upgrading the binary changes nothing for it. null counts as absent.
func TestLinkCeilingAbsentKeepsHistorical32(t *testing.T) {
	for _, hw := range [][2]int{{lowRAM, lowCPU}, {medRAM, medCPU}, {highRAM, highCPU}} {
		pinHW(t, hw[0], hw[1])
		for _, cfg := range []string{`{"mode":"dial","carrier":"mtcp","min_links":2,"per_link":8}`, `{"max_links": null}`} {
			var fc fileConfig
			if err := json.Unmarshal([]byte(cfg), &fc); err != nil {
				t.Fatal(err)
			}
			got, mode, _ := linkCeiling(fc)
			if got != 32 || mode != ceilDefault {
				t.Fatalf("%s on %v: got %d %s, want 32 default", cfg, hw, got, mode)
			}
			if _, max, _ := linkEnvelope(fc); max != 32 {
				t.Fatalf("%s on %v: envelope max %d, want 32", cfg, hw, max)
			}
		}
	}
	if !strings.Contains(ceilingLogLine(fileConfig{Carrier: "mtcp"}), "the default (max_links is not set") {
		t.Fatal("the startup line must say the default applies")
	}
}

// autoFC marks fc as carrying an explicit max_links 0 (auto), as a decoded
// config would.
func autoFC(fc fileConfig) fileConfig { fc.MaxLinks, fc.maxLinksSet = 0, true; return fc }

// min_links above the auto ceiling lifts the ceiling (the envelope must hold).
func TestLinkEnvelopeMinAboveAuto(t *testing.T) {
	pinHW(t, lowRAM, lowCPU)
	if _, max, _ := linkEnvelope(autoFC(fileConfig{MinLinks: 40})); max != 40 {
		t.Fatalf("min 40 over auto 32: max %d, want 40", max)
	}
	if !strings.Contains(ceilingLogLine(autoFC(fileConfig{MinLinks: 40, Carrier: "mtcp"})), "raised from 32 to min_links") {
		t.Fatal("the startup line must say the ceiling was lifted to min_links")
	}
}

func TestCeilingLogLine(t *testing.T) {
	pinHW(t, highRAM, highCPU)
	auto := ceilingLogLine(autoFC(fileConfig{Carrier: "mtcp"}))
	if !strings.Contains(auto, "ceiling 170 links") || !strings.Contains(auto, "auto from this server's hardware (8.0 GB RAM, 4 cores: one link per 48 MB of RAM") {
		t.Fatalf("auto startup line: %q", auto)
	}
	fixed := ceilingLogLine(fileConfig{Carrier: "mtcp", MaxLinks: 40})
	if !strings.Contains(fixed, "ceiling 40 links") || !strings.Contains(fixed, "fixed by max_links") {
		t.Fatalf("fixed startup line: %q", fixed)
	}
}

// The effective ceiling, exactly as the engine applies it.
func TestEffectiveCeiling(t *testing.T) {
	cases := []struct {
		name            string
		iran, reverse   bool
		own, peer, want int
		by              string
	}{
		{"direct Iran, peer higher (not applied)", true, false, 32, 64, 32, "iran"},
		{"direct Iran, peer lower (not applied)", true, false, 64, 32, 64, "iran"},
		{"direct Iran, peer unknown", true, false, 48, 0, 48, "iran"},
		{"direct Kharej: the Iran server's", false, false, 32, 64, 64, "iran"},
		{"direct Kharej, Iran unknown", false, false, 32, 0, 0, ""},
		{"reverse Iran, Kharej lower", true, true, 64, 48, 48, "kharej"},
		{"reverse Iran, Iran lower", true, true, 32, 64, 32, "iran"},
		{"reverse Kharej, Iran lower", false, true, 64, 48, 48, "iran"},
		{"reverse Kharej, Kharej lower", false, true, 32, 64, 32, "kharej"},
		{"reverse equal", true, true, 48, 48, 48, "both"},
		{"reverse peer unknown", false, true, 64, 0, 0, ""},
	}
	for _, c := range cases {
		got, by := effectiveCeiling(c.iran, c.reverse, c.own, c.peer)
		if got != c.want || by != c.by {
			t.Errorf("%s: got %d %q, want %d %q", c.name, got, by, c.want, c.by)
		}
	}
}

func withLinks(ls liveStatus, n int) liveStatus { ls.Links = n; return ls }

// status renders the same exact fact on both servers.
func TestCeilingLineBothServers(t *testing.T) {
	mk := func(role, dir string, own, peer int, auto bool) liveStatus {
		mode := ceilFixed
		if auto {
			mode = ceilAuto
		}
		ls := liveStatus{Role: role, Dir: dir, CfgMax: own, PeerMax: peer, CeilMode: mode, Profile: "high", RAMMB: highRAM, CPUCores: highCPU}
		ls.EffMax, ls.LimitBy = effectiveCeiling(role == "Iran side", dir == "reverse", own, peer)
		return ls
	}
	cases := []struct {
		name string
		ls   liveStatus
		want []string
	}{
		{"reverse Iran, Kharej 48 limits", mk("Iran side", "reverse", 64, 48, true),
			[]string{"48 links", "limited by the Kharej server", "this server: 64 (auto", "the Kharej server: 48"}},
		{"reverse Kharej, same tunnel", mk("Kharej side", "reverse", 48, 64, false),
			[]string{"48 links", "limited by this server", "this server: 48 (fixed by max_links", "the Iran server: 64"}},
		{"reverse equal", mk("Iran side", "reverse", 48, 48, true), []string{"48 links", "both servers allow the same"}},
		{"reverse unknown, no link", mk("Kharej side", "reverse", 64, 0, true), []string{"at most 64 links", "the Iran server's ceiling is not known yet (no link is up)", "the lower of the two applies"}},
		{"reverse unknown, links up", withLinks(mk("Iran side", "reverse", 64, 0, true), 8), []string{"at most 64 links", "the Kharej server does not report its ceiling", "older hs2"}},
		{"direct Iran", mk("Iran side", "direct", 32, 64, false),
			[]string{"32 links", "this (Iran) server alone sets it", "Kharej server's max_links (64) does not apply"}},
		{"direct Kharej", mk("Kharej side", "direct", 64, 32, true),
			[]string{"32 links", "set by the Iran server", "this server's own ceiling, 64 (auto", "does not apply here"}},
		{"direct Kharej unknown", mk("Kharej side", "direct", 64, 0, true), []string{"set by the Iran server", "not known yet (no link is up)"}},
	}
	for _, c := range cases {
		got := ceilingLine(c.ls)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q missing %q", c.name, got, w)
			}
		}
	}
	if ceilingLine(liveStatus{Role: "Iran side"}) != "" {
		t.Error("no pool (or an older status file): no ceiling line")
	}
}

// A ceiling lifted by a higher min_links is shown as exactly that, never as if
// the hardware (or max_links) gave it; drift is judged on the ceiling itself.
func TestCeilingLiftedByMinLinks(t *testing.T) {
	ls := liveStatus{Role: "Iran side", Dir: "direct", CfgMax: 40, CeilRaw: 32, CeilMode: ceilAuto, Profile: "low", RAMMB: lowRAM, CPUCores: 1, RecMax: 32}
	ls.EffMax, ls.LimitBy = effectiveCeiling(true, false, 40, 0)
	if got := ceilingLine(ls); !strings.Contains(got, "40 (raised to min_links; the ceiling itself is 32: auto — 1.0 GB RAM, 1 core: a single core keeps its low-profile 32") {
		t.Fatalf("lifted auto: %q", got)
	}
	fixed := liveStatus{Role: "Iran side", Dir: "direct", CfgMax: 20, CeilRaw: 16, CeilMode: ceilFixed, Profile: "high", RecMax: 64}
	if d := driftLine(fixed); !strings.Contains(d, "max_links is fixed at 16") {
		t.Fatalf("drift must name the configured 16, not the lifted 20: %q", d)
	}
	pinHW(t, lowRAM, lowCPU)
	d := doctorLinkPool(autoFC(fileConfig{Mode: "dial", Carrier: "mtcp", MinLinks: 40}), filepath.Join(t.TempDir(), "x.json"))
	if line := strings.Join(d.lines, ""); !strings.Contains(line, "auto: 32 links from this server (1.0 GB RAM, 1 core") || !strings.Contains(line, "raised to min_links 40") {
		t.Fatalf("doctor, lifted auto: %v", d.lines)
	}
}

func TestDriftLine(t *testing.T) {
	base := liveStatus{Role: "Iran side", Dir: "reverse", Profile: "low", RecMax: 32}
	high := base
	high.CfgMax = 64
	if d := driftLine(high); !strings.Contains(d, "above what this server suggests (32: low profile)") {
		t.Errorf("fixed 64 on a low box: %q", d)
	}
	lowFixed := liveStatus{Role: "Iran side", Dir: "direct", Profile: "high", RecMax: 64, CfgMax: 32}
	if d := driftLine(lowFixed); !strings.Contains(d, "could use up to 64") || !strings.Contains(d, "0 (auto)") {
		t.Errorf("fixed 32 on a high box: %q", d)
	}
	auto := high
	auto.CeilMode = ceilAuto
	if d := driftLine(auto); d != "" {
		t.Errorf("auto never drifts: %q", d)
	}
	def := liveStatus{Role: "Iran side", Dir: "reverse", Profile: "high", RecMax: 64, CfgMax: 32, CeilMode: ceilDefault}
	if d := driftLine(def); !strings.Contains(d, "max_links is not set") || !strings.Contains(d, "could use up to 64") {
		t.Errorf("default 32 on a high box: %q", d)
	}
	dk := lowFixed
	dk.Role = "Kharej side"
	if d := driftLine(dk); d != "" {
		t.Errorf("direct Kharej's ceiling is not applied, no drift note: %q", d)
	}
}

// writeLive writes a fresh live status for cfgPath into a temp status dir.
func writeLive(t *testing.T, cfgPath string, ls liveStatus) {
	t.Helper()
	ls.Updated = time.Now().Unix()
	b, _ := json.Marshal(ls)
	if err := os.WriteFile(statusPath(cfgPath), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func useTempStatusDir(t *testing.T) {
	t.Helper()
	old := statusRunDir
	statusRunDir = t.TempDir()
	t.Cleanup(func() { statusRunDir = old })
}

func doctorLinkPool(fc fileConfig, cfgPath string) *doctorReport {
	d := &doctorReport{}
	checkLinkPool(d, fc, cfgPath)
	return d
}

func TestDoctorLinkPool(t *testing.T) {
	useTempStatusDir(t)
	cfg := filepath.Join(t.TempDir(), "c.json")
	iranRev := autoFC(fileConfig{Mode: "dial", Reverse: true, Carrier: "mtcp"})

	// auto, daemon not running: ok.
	pinHW(t, highRAM, highCPU)
	if d := doctorLinkPool(iranRev, cfg); d.warns != 0 || !strings.Contains(strings.Join(d.lines, ""), "auto: 170 links") {
		t.Fatalf("auto ok: %v", d.lines)
	}
	// auto, the daemon started on a high box but RAM is now low: restart WARN.
	writeLive(t, cfg, liveStatus{CfgMax: 170})
	pinHW(t, lowRAM, lowCPU)
	if d := doctorLinkPool(iranRev, cfg); d.warns != 1 || !strings.Contains(strings.Join(d.lines, ""), "restart the tunnel to apply the lower ceiling") {
		t.Fatalf("auto shrunk under a running daemon: %v", d.lines)
	}
	// auto, RAM grew: restart INFO, not a warning.
	writeLive(t, cfg, liveStatus{CfgMax: 32})
	pinHW(t, highRAM, highCPU)
	if d := doctorLinkPool(iranRev, cfg); d.warns != 0 || !strings.Contains(strings.Join(d.lines, ""), "restart the tunnel to use it") {
		t.Fatalf("auto grew under a running daemon: %v", d.lines)
	}
	os.Remove(statusPath(cfg))

	// fixed above what the RAM holds: WARN; below what the box allows: INFO.
	pinHW(t, lowRAM, lowCPU)
	fixed64 := iranRev
	fixed64.MaxLinks, fixed64.maxLinksSet = 64, true
	if d := doctorLinkPool(fixed64, cfg); d.warns != 1 || !strings.Contains(strings.Join(d.lines, ""), "above what this server (1.0 GB RAM, 1 core") {
		t.Fatalf("fixed 64 on a low box: %v", d.lines)
	}
	pinHW(t, highRAM, highCPU)
	fixed32 := iranRev
	fixed32.MaxLinks, fixed32.maxLinksSet = 32, true
	if d := doctorLinkPool(fixed32, cfg); d.warns != 0 || !strings.Contains(strings.Join(d.lines, ""), "could use up to 170") {
		t.Fatalf("fixed 32 on a high box: %v", d.lines)
	}

	// direct Kharej: not applied, no drift check even when fixed far off.
	dk := fileConfig{Mode: "listen", Carrier: "dgtun", MaxLinks: 200}
	if d := doctorLinkPool(dk, cfg); d.warns != 0 || !strings.Contains(strings.Join(d.lines, ""), "not applied here") {
		t.Fatalf("direct Kharej: %v", d.lines)
	}
	// effective ceiling from a running daemon's status file is quoted.
	ls := liveStatus{Role: "Iran side", Dir: "reverse", CfgMax: 64, PeerMax: 48, CeilMode: ceilAuto, Profile: "high"}
	ls.EffMax, ls.LimitBy = effectiveCeiling(true, true, 64, 48)
	writeLive(t, cfg, ls)
	if d := doctorLinkPool(iranRev, cfg); !strings.Contains(strings.Join(d.lines, ""), "limited by the Kharej server") {
		t.Fatalf("effective ceiling not shown: %v", d.lines)
	}
	// no pool on this carrier: one info line.
	if d := doctorLinkPool(fileConfig{Carrier: "tls"}, cfg); d.warns != 0 || !strings.Contains(strings.Join(d.lines, ""), "no adaptive link pool") {
		t.Fatalf("tls: %v", d.lines)
	}
}

// The status file the daemon writes carries the exact ceiling fields and the
// rendered line the installer menu prints.
func TestStatusFileCarriesCeiling(t *testing.T) {
	useTempStatusDir(t)
	saved := warmAfter
	warmAfter = 0 // the writer takes it when made: write the warm file at once
	defer func() { warmAfter = saved }()
	pinHW(t, highRAM, highCPU)
	cfg := filepath.Join(t.TempDir(), "c.json")
	fc := autoFC(fileConfig{Mode: "dial", Reverse: true, Carrier: "mtcp"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startStatusWriter(ctx, fc, cfg, func() engine.PoolStats {
		return engine.PoolStats{Links: 3, Target: 3, Min: 2, Max: 64, PeerMax: 48, Phase: "steady"}
	})
	var ls liveStatus
	deadline := time.Now().Add(3 * time.Second)
	for {
		b, err := os.ReadFile(statusPath(cfg))
		if err == nil && json.Unmarshal(b, &ls) == nil && ls.CfgMax > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no status file with the ceiling")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ls.CfgMax != 170 || ls.CeilMode != ceilAuto || ls.PeerMax != 48 || ls.EffMax != 48 || ls.LimitBy != "kharej" {
		t.Fatalf("status ceiling fields: %+v", ls)
	}
	if !strings.Contains(ls.CeilingText, "limited by the Kharej server") {
		t.Fatalf("ceiling_text %q", ls.CeilingText)
	}
	// The edge's pool keeps its target in the warm file, which a restart
	// within warmMaxAge comes back at.
	if n := readWarm(cfg); n != 3 {
		t.Fatalf("warm file: %d, want the target 3", n)
	}
	// A carrier without a pool writes none of it.
	cfg2 := filepath.Join(t.TempDir(), "d.json")
	startStatusWriter(ctx, fileConfig{Mode: "dial", Carrier: "udp"}, cfg2, func() engine.PoolStats { return engine.PoolStats{Links: 1} })
	time.Sleep(200 * time.Millisecond)
	b, _ := os.ReadFile(statusPath(cfg2))
	if strings.Contains(string(b), "cfg_max") || strings.Contains(string(b), "ceiling_text") {
		t.Fatalf("single-session carrier got ceiling fields: %s", b)
	}
}

// `hs2 config set max_links auto` writes 0 (auto); other int keys still take
// only numbers.
func TestConfigSetMaxLinksAuto(t *testing.T) {
	m := map[string]any{}
	if err := setKey(m, configKeys["max_links"], "auto"); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(m["max_links"]); got != "0" {
		t.Fatalf("max_links auto stored as %q, want 0", got)
	}
	if err := setKey(m, configKeys["min_links"], "auto"); err == nil {
		t.Fatal("min_links must not accept 'auto'")
	}
}

// The Iran side's links line says when the Kharej server caps its target
// (reverse only); without a cap, or in direct, it is unchanged.
func TestPatternLineCappedByKharej(t *testing.T) {
	ls := liveStatus{Role: "Iran side", Dir: "reverse", Links: 40, Target: 48, Min: 2, Max: 64, Phase: "steady", CfgMax: 64, PeerMax: 40}
	ls.EffMax, ls.LimitBy = effectiveCeiling(true, true, 64, 40)
	if got := patternLine(ls); !strings.Contains(got, "40 up / target 48, capped at 40 by the Kharej server (steady, range 2–64)") {
		t.Fatalf("capped reverse: %q", got)
	}
	d := ls
	d.Dir = "direct"
	d.EffMax, d.LimitBy = effectiveCeiling(true, false, 64, 40)
	if got := patternLine(d); strings.Contains(got, "capped") {
		t.Fatalf("direct must not claim a Kharej cap: %q", got)
	}
	u := ls
	u.PeerMax, u.EffMax, u.LimitBy = 0, 0, "" // older Kharej: cap unknown
	if got := patternLine(u); strings.Contains(got, "capped") {
		t.Fatalf("unknown cap must not be claimed: %q", got)
	}
}

// The warm file: read back while fresh, ignored when stale or garbled.
func TestWarmFile(t *testing.T) {
	useTempStatusDir(t)
	cfg := filepath.Join(t.TempDir(), "w.json")
	if n := readWarm(cfg); n != 0 {
		t.Fatalf("no file: %d", n)
	}
	// Nothing is written in the first minute of a process (a crash loop would
	// keep renewing the value it started with).
	w := &warmWriter{path: warmPath(cfg), start: time.Now(), after: warmAfter}
	w.note(120)
	if n := readWarm(cfg); n != 0 {
		t.Fatalf("written %d before the process ran a minute", n)
	}
	w.start = time.Now().Add(-warmAfter)
	w.note(120)
	if n := readWarm(cfg); n != 120 {
		t.Fatalf("fresh: %d, want 120", n)
	}
	// It only ever raises the start size: 2 from a quiet night comes up at
	// the default instead.
	saved := configPath
	configPath = cfg
	defer func() { configPath = saved }()
	if n := warmLinks(func(string, ...any) {}, 2, 300); n != 120 {
		t.Fatalf("warm start %d, want 120", n)
	}
	w.last = 0
	w.note(2)
	if n := warmLinks(func(string, ...any) {}, 2, 300); n != 0 {
		t.Fatalf("a warm value below the default start size gave %d, want 0 (the default)", n)
	}
	old := time.Now().Add(-warmMaxAge - time.Minute)
	os.Chtimes(warmPath(cfg), old, old)
	if n := readWarm(cfg); n != 0 {
		t.Fatalf("stale: %d, want 0", n)
	}
	os.WriteFile(warmPath(cfg), []byte("x"), 0o644)
	if n := readWarm(cfg); n != 0 {
		t.Fatalf("garbled: %d, want 0", n)
	}
}

// A pool allowed more than 64 links states the visibility trade-off (the
// owner's call); a small one says nothing. In reverse a lower peer ceiling is
// named.
func TestDoctorManyLinks(t *testing.T) {
	useTempStatusDir(t)
	cfg := filepath.Join(t.TempDir(), "m.json")
	edge := autoFC(fileConfig{Mode: "dial", Reverse: true, Carrier: "mtcp"})
	pinHW(t, 17408, 20)
	d := &doctorReport{}
	checkManyLinks(d, edge, cfg)
	if l := strings.Join(d.lines, ""); !strings.Contains(l, "up to 300 parallel TLS connections") || !strings.Contains(l, "owner's decision") {
		t.Fatalf("300-link edge: %v", d.lines)
	}
	writeLive(t, cfg, liveStatus{CfgMax: 300, PeerMax: 64, EffMax: 64})
	d = &doctorReport{}
	checkManyLinks(d, edge, cfg)
	l := strings.Join(d.lines, "")
	if !strings.Contains(l, "the Kharej server allows at most 64 links, this one 300") || strings.Contains(l, "parallel TLS") {
		t.Fatalf("reverse, Kharej at 64: %v", d.lines)
	}
	pinHW(t, lowRAM, lowCPU)
	os.Remove(statusPath(cfg))
	d = &doctorReport{}
	checkManyLinks(d, edge, cfg)
	if len(d.lines) != 0 {
		t.Fatalf("a 32-link pool: %v", d.lines)
	}
}

func TestDoctorTCPMem(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "net"), 0o755)
	os.MkdirAll(filepath.Join(dir, "sys/net/ipv4"), 0o755)
	os.WriteFile(filepath.Join(dir, "sys/net/ipv4/tcp_mem"), []byte("1000\t2000\t3000\n"), 0o644)
	for _, c := range []struct {
		mem   int
		warns int
		want  string
	}{{500, 0, "in TCP buffers (pressure at"}, {2500, 1, "pressure mark"}, {3100, 1, "hard limit"}} {
		os.WriteFile(filepath.Join(dir, "net/sockstat"), []byte(fmt.Sprintf("sockets: used 1\nTCP: inuse 5 orphan 0 tw 0 alloc 6 mem %d\nUDP: inuse 1 mem 0\n", c.mem)), 0o644)
		d := &doctorReport{}
		checkTCPMem(d, dir)
		if d.warns != c.warns || !strings.Contains(strings.Join(d.lines, ""), c.want) {
			t.Fatalf("mem %d: %v", c.mem, d.lines)
		}
	}
}

// dgtun's auto ceiling is held at 64 over a raw encapsulation and 128 over
// udp for now; explicit numbers and an absent max_links are untouched, and
// the start line, the status and recommend-links say why it is lower.
func TestDgtunAutoCap(t *testing.T) {
	pinHW(t, 17408, 20) // the production Iran server: auto 300
	for _, c := range []struct {
		cfg  string
		want int
		note string
	}{
		{`{"carrier":"dgtun","encap":"gre","max_links":0}`, 64, "dgtun over gre"},
		{`{"carrier":"dgtun","encap":"icmp","max_links":0}`, 64, "dgtun over icmp"},
		{`{"carrier":"dgtun","max_links":0}`, 128, "dgtun over udp"},
		{`{"carrier":"dgtun","encap":"udp","max_links":0}`, 128, "dgtun over udp"},
		{`{"carrier":"dgtun","encap":"gre","max_links":200}`, 200, ""}, // explicit: as written
		{`{"carrier":"dgtun","encap":"gre"}`, 32, ""},                  // absent: the historical 32
		{`{"carrier":"mtcp","max_links":0}`, 300, ""},
	} {
		var fc fileConfig
		if err := json.Unmarshal([]byte(c.cfg), &fc); err != nil {
			t.Fatal(err)
		}
		if got, _, _ := linkCeiling(fc); got != c.want {
			t.Errorf("%s: ceiling %d, want %d", c.cfg, got, c.want)
		}
		note := dgCapNote(fc)
		if (c.note == "") != (note == "") || !strings.Contains(note, c.note) {
			t.Errorf("%s: note %q, want one naming %q", c.cfg, note, c.note)
		}
		if line := ceilingLogLine(fc); c.note != "" && !strings.Contains(line, "lowered — "+c.note) {
			t.Errorf("%s: start line does not say why: %q", c.cfg, line)
		}
	}
	// A small server's auto is below the cap: nothing to say.
	pinHW(t, lowRAM, lowCPU)
	var fc fileConfig
	json.Unmarshal([]byte(`{"carrier":"dgtun","encap":"gre","max_links":0}`), &fc)
	if got, _, _ := linkCeiling(fc); got != 32 || dgCapNote(fc) != "" {
		t.Errorf("1 GB box: ceiling %d, note %q", got, dgCapNote(fc))
	}
	// The status line carries it.
	ls := liveStatus{Role: "Iran side", Dir: "direct", CfgMax: 64, CeilMode: ceilAuto, RAMMB: 17408, CPUCores: 20,
		CeilNote: "dgtun over gre: auto holds at most 64 carriers until its 300-carrier load test", EffMax: 64}
	if s := ceilingLine(ls); !strings.Contains(s, "lowered — dgtun over gre") {
		t.Errorf("status ceiling line: %q", s)
	}
}
