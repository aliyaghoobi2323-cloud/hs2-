package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The status path must match the bash the installer uses to find it:
// /run/hs2/ + abs config path with '/'->'-' and ' '->'_' + .status.json.
func TestStatusPathMatchesInstaller(t *testing.T) {
	cases := map[string]string{
		"/etc/hs2/config.json": "/run/hs2/etc-hs2-config.json.status.json",
		"/tmp/hs2lab/ir.json":  "/run/hs2/tmp-hs2lab-ir.json.status.json",
		"/etc/hs2/my tun.json": "/run/hs2/etc-hs2-my_tun.json.status.json",
	}
	for in, want := range cases {
		if got := statusPath(in); got != want {
			t.Errorf("statusPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// The live status JSON round-trips and renders a sensible pattern line.
func TestLiveStatusRoundTrip(t *testing.T) {
	ls := liveStatus{Role: "Iran side", Dir: "reverse", Transport: "tcp (mtcp)",
		Endpoint: "listens on 0.0.0.0:2082", Links: 8, Target: 10, Min: 2, Max: 32,
		Users: 40, Mbit: 42.5, Phase: "probing", Sat: true}
	b, err := json.Marshal(ls)
	if err != nil {
		t.Fatal(err)
	}
	var got liveStatus
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got != ls {
		t.Fatalf("round-trip mismatch: %+v vs %+v", got, ls)
	}
	line := patternLine(ls)
	for _, want := range []string{"8 up", "10 target", "probing", "2–32"} {
		if !strings.Contains(line, want) {
			t.Errorf("pattern line %q missing %q", line, want)
		}
	}
	// Steady pool at its target shows no arrow.
	if strings.Contains(patternLine(liveStatus{Links: 8, Target: 8, Min: 2, Max: 32, Phase: "steady"}), "target") {
		t.Error("steady pool should not print a target arrow")
	}
}
