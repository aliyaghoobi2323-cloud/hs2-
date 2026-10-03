package tune

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// allAvail treats every congestion control / qdisc as present.
func allAvail(string) bool { return true }

// only makes an availability func that accepts just the named items.
func only(names ...string) func(string) bool {
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	return func(s string) bool { return set[s] }
}

func find(p *Plan, key string) (string, bool) {
	for _, kv := range p.Sysctls {
		if kv.Key == key {
			return kv.Val, true
		}
	}
	return "", false
}

// Bigger boxes get bigger buffers; small boxes stay conservative.
func TestProfilesScaleWithHardware(t *testing.T) {
	cases := []struct {
		ram, cpu int
		want     string
		rmem     string
	}{
		{512, 1, "low", "8388608"},
		{2048, 2, "medium", "16777216"},
		{8192, 8, "high", "33554432"},
	}
	for _, c := range cases {
		p := Build(Config{}, c.ram, c.cpu, allAvail, allAvail)
		if p.Profile != c.want {
			t.Errorf("RAM %dMB/%dcpu -> profile %q, want %q", c.ram, c.cpu, p.Profile, c.want)
		}
		if v, _ := find(p, "net.core.rmem_max"); v != c.rmem {
			t.Errorf("RAM %dMB rmem_max=%q, want %q", c.ram, v, c.rmem)
		}
	}
}

// The link-pool ceiling: one link per 48 MB of RAM (worst-case link buffers
// <= 25% of RAM), at most 300, at most 128 below 4 cores, a single core keeps
// its profile value, and never below the profile value a server had before.
// This is the single source of truth the engine, installer and doctor use.
func TestRecommendedMaxLinks(t *testing.T) {
	cases := []struct {
		ram, cpu    int
		wantProfile string
		wantMax     int
	}{
		{512, 1, "low", 32},     // typical tiny VPS
		{1024, 1, "low", 32},    // common 1 GB Iran VPS — stays 32 (no OOM risk)
		{1536, 2, "medium", 48}, // medium threshold
		{2048, 2, "medium", 48},
		{2048, 4, "high", 64}, // cores lift a borderline box; RAM allows 42
		{4096, 1, "high", 64}, // single core keeps its profile value
		{4096, 2, "high", 85}, // 4096/48
		{8192, 8, "high", 170},
		{8192, 2, "high", 128}, // below 4 cores
		{14400, 4, "high", 300},
		{16384, 4, "high", 300},
		{17408, 20, "high", 300}, // the Iran production server
		{22528, 12, "high", 300}, // the Kharej production server
		{65536, 64, "high", 300},
	}
	for _, c := range cases {
		if p := ProfileFor(c.ram, c.cpu); p != c.wantProfile {
			t.Errorf("ProfileFor(%d,%d)=%q, want %q", c.ram, c.cpu, p, c.wantProfile)
		}
		if m := RecommendedMaxLinks(c.ram, c.cpu); m != c.wantMax {
			t.Errorf("RecommendedMaxLinks(%d,%d)=%d, want %d", c.ram, c.cpu, m, c.wantMax)
		}
	}
	old := func(ram, cpu int) int { // the ceiling before the RAM rule
		switch ProfileFor(ram, cpu) {
		case "high":
			return 64
		case "medium":
			return 48
		}
		return 32
	}
	for ram := 128; ram <= 65536; ram += 137 {
		for cpu := 1; cpu <= 32; cpu++ {
			m := RecommendedMaxLinks(ram, cpu)
			switch {
			case m < old(ram, cpu):
				t.Fatalf("RecommendedMaxLinks(%d,%d)=%d below the %d it was", ram, cpu, m, old(ram, cpu))
			case m > MaxLinksCap:
				t.Fatalf("RecommendedMaxLinks(%d,%d)=%d above %d", ram, cpu, m, MaxLinksCap)
			case cpu == 1 && m > 64:
				t.Fatalf("single core RecommendedMaxLinks(%d,1)=%d above 64", ram, m)
			case cpu < 4 && m > 128:
				t.Fatalf("RecommendedMaxLinks(%d,%d)=%d above 128 below 4 cores", ram, cpu, m)
			case m > old(ram, cpu) && m*LinkWorstCaseMiB > ram/4+LinkWorstCaseMiB:
				t.Fatalf("RecommendedMaxLinks(%d,%d)=%d: worst case %d MiB above a quarter of RAM", ram, cpu, m, m*LinkWorstCaseMiB)
			}
		}
	}
}

func TestMaxLinksReason(t *testing.T) {
	for _, c := range []struct {
		ram, cpu int
		want     string
	}{
		{17408, 20, "17.0 GB RAM, 20 cores: one link per 48 MB of RAM, at most 300"},
		{1024, 1, "1.0 GB RAM, 1 core: a single core keeps its low-profile 32"},
		{2048, 4, "2.0 GB RAM, 4 cores: the high-profile 64 (RAM allows no more)"},
		{8192, 2, "8.0 GB RAM, 2 cores: one link per 48 MB of RAM, at most 128 below 4 cores"},
		{8192, 8, "8.0 GB RAM, 8 cores: one link per 48 MB of RAM, worst-case link buffers <= 25% of RAM"},
	} {
		if got := MaxLinksReason(c.ram, c.cpu); got != c.want {
			t.Errorf("MaxLinksReason(%d,%d)=%q, want %q", c.ram, c.cpu, got, c.want)
		}
	}
}

// Defaults are bbr + fq_codel.
func TestDefaultsAreBBRFqCodel(t *testing.T) {
	p := Build(Config{}, 2048, 2, allAvail, allAvail)
	if p.Congestion != "bbr" || p.Qdisc != "fq_codel" {
		t.Fatalf("defaults: cc=%q qdisc=%q, want bbr/fq_codel", p.Congestion, p.Qdisc)
	}
	if v, _ := find(p, "net.ipv4.tcp_congestion_control"); v != "bbr" {
		t.Errorf("cc sysctl=%q", v)
	}
}

// A requested cc/qdisc the kernel lacks falls back and is noted.
func TestFallbackWhenUnavailable(t *testing.T) {
	p := Build(Config{Congestion: "bbr", Qdisc: "cake"}, 2048, 2, only("cubic", "bbr"), only("fq"))
	if p.Qdisc != "fq" {
		t.Fatalf("qdisc fallback: got %q, want fq", p.Qdisc)
	}
	if len(p.Notes) == 0 || !containsAny(p.Notes, "cake") {
		t.Errorf("expected a note about cake being unavailable, got %v", p.Notes)
	}

	// cc unavailable entirely -> empty, noted, and no cc sysctl emitted.
	p2 := Build(Config{Congestion: "bbr"}, 2048, 2, only("cubic"), allAvail)
	if p2.Congestion == "bbr" {
		t.Fatalf("expected bbr fallback, got %q", p2.Congestion)
	}
}

// Manual mode uses the operator's overrides on top of the profile values.
func TestManualOverrides(t *testing.T) {
	p := Build(Config{Mode: ModeManual, RmemMax: 4 << 20, Somaxconn: 999}, 8192, 8, allAvail, allAvail)
	if p.Profile != "manual" {
		t.Fatalf("mode manual -> profile %q", p.Profile)
	}
	if v, _ := find(p, "net.core.rmem_max"); v != "4194304" {
		t.Errorf("manual rmem_max=%q, want 4194304", v)
	}
	if v, _ := find(p, "net.core.somaxconn"); v != "999" {
		t.Errorf("manual somaxconn=%q, want 999", v)
	}
	// Unset override falls back to the high-profile value.
	if v, _ := find(p, "net.core.wmem_max"); v != "33554432" {
		t.Errorf("manual wmem_max=%q, want high-profile 33554432", v)
	}
}

// off mode emits no system sysctls but still resolves a per-socket cc.
func TestOffModeSetsNoSysctls(t *testing.T) {
	p := Build(Config{Mode: ModeOff}, 8192, 8, allAvail, allAvail)
	if len(p.Sysctls) != 0 {
		t.Fatalf("off mode emitted %d sysctls, want 0", len(p.Sysctls))
	}
	if p.Congestion != "bbr" {
		t.Errorf("off mode should still resolve cc for the tunnel sockets, got %q", p.Congestion)
	}
	if !strings.Contains(p.Summary(), "off") {
		t.Errorf("summary should say off: %q", p.Summary())
	}
}

func containsAny(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// The ephemeral port range must never be changed: it would let outgoing
// connections occupy ports a panel binds for inbounds.
func TestNeverTouchesLocalPortRange(t *testing.T) {
	for _, prof := range []struct{ ram, cpu int }{{512, 1}, {2048, 2}, {16384, 8}} {
		p := Build(Config{}, prof.ram, prof.cpu, allAvail, allAvail)
		if _, ok := find(p, "net.ipv4.ip_local_port_range"); ok {
			t.Fatalf("plan for %dMB/%dcpu changes ip_local_port_range", prof.ram, prof.cpu)
		}
	}
}

// Only our own earlier value is reverted; any other value is left alone.
func TestUndoLegacyPortRange(t *testing.T) {
	cases := []struct {
		cur     string
		undone  bool
		written string
	}{
		{"10240\t65535", true, "32768 60999"}, // what the earlier build set (kernel prints a tab)
		{"32768\t60999", false, ""},           // kernel default: untouched
		{"20000\t40000", false, ""},           // operator's own value: untouched
	}
	for _, c := range cases {
		var wrote string
		read := func(string) (string, bool) { return c.cur, true }
		write := func(_, v string) bool { wrote = v; return true }
		got := undoLegacyPortRange(read, write)
		if got != c.undone || wrote != c.written {
			t.Errorf("cur=%q: undone=%v wrote=%q, want %v %q", c.cur, got, wrote, c.undone, c.written)
		}
	}
}

// Outgoing connections may reuse TIME_WAIT ports (the dgtun forwarders and a
// non-loopback panel open one per user).
func TestBuildSetsTimeWaitReuse(t *testing.T) {
	p := Build(Config{}, 8192, 4, nil, nil)
	for _, kv := range p.Sysctls {
		if kv.Key == "net.ipv4.tcp_tw_reuse" {
			if kv.Val != "1" {
				t.Fatalf("tcp_tw_reuse=%s, want 1", kv.Val)
			}
			return
		}
	}
	t.Fatal("tcp_tw_reuse not in the plan")
}

// A cgroup's memory limit lowers the RAM the sizing sees (v2: the lowest
// memory.max on the way up; v1: the hierarchical limit); "max" is no limit.
func TestCgroupMemLimit(t *testing.T) {
	root := t.TempDir()
	self := filepath.Join(t.TempDir(), "cgroup")
	write := func(p, v string) {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(v), 0o644)
	}
	// v2, a systemd unit under a slice that has the lower limit
	write(self, "0::/system.slice/hs2.service\n")
	write(filepath.Join(root, "system.slice/hs2.service/memory.max"), "max\n")
	write(filepath.Join(root, "system.slice/memory.max"), "2147483648\n")
	if got := cgroupMemLimitMB(self, root); got != 2048 {
		t.Fatalf("v2 slice limit: %d MB, want 2048", got)
	}
	// v2, no limit anywhere
	os.WriteFile(filepath.Join(root, "system.slice/memory.max"), []byte("max\n"), 0o644)
	if got := cgroupMemLimitMB(self, root); got != 0 {
		t.Fatalf("v2 no limit: %d", got)
	}
	// v1: the hierarchical limit from memory.stat
	root1 := t.TempDir()
	write(self, "4:memory:/docker/abc\n0::/\n")
	write(filepath.Join(root1, "memory/docker/abc/memory.stat"), "cache 0\nhierarchical_memory_limit 1073741824\n")
	write(filepath.Join(root1, "memory/docker/abc/memory.limit_in_bytes"), "9223372036854771712\n")
	if got := cgroupMemLimitMB(self, root1); got != 1024 {
		t.Fatalf("v1: %d MB, want 1024", got)
	}
	if got := cgroupMemLimitMB(filepath.Join(t.TempDir(), "none"), root1); got != 0 {
		t.Fatalf("unreadable: %d", got)
	}
}
