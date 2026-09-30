package tune

import (
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
