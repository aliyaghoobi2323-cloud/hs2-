package main

import (
	"encoding/json"
	"reflect"
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
	if !reflect.DeepEqual(got, ls) {
		t.Fatalf("round-trip mismatch: %+v vs %+v", got, ls)
	}
	line := patternLine(ls)
	for _, want := range []string{"8 up", "target 10", "probing", "2–32"} {
		if !strings.Contains(line, want) {
			t.Errorf("pattern line %q missing %q", line, want)
		}
	}
	// Steady pool at its target shows no arrow.
	if strings.Contains(patternLine(liveStatus{Links: 8, Target: 8, Min: 2, Max: 32, Phase: "steady"}), "target") {
		t.Error("steady pool should not print a target arrow")
	}
}

// A shrinking edge shows serving vs retiring links, why, and what holds them.
func TestLiveStatusShrinkingPool(t *testing.T) {
	five := 5
	ls := liveStatus{Links: 7, Target: 5, Min: 2, Max: 32, Phase: "shrinking", Users: 251, Mbit: 6.1,
		Serving: &five, Retiring: 2, HeldBy: 39, HeldActive: 1, Flowing: 18, Pressed: 1, CapMbit: 2.4,
		Reason: "peak 6.8 Mbit/s needs ~5 links", ExitStats: "ok"}
	b, _ := json.Marshal(ls)
	var got liveStatus
	if err := json.Unmarshal(b, &got); err != nil || !reflect.DeepEqual(got, ls) {
		t.Fatalf("round-trip mismatch (%v): %+v vs %+v", err, got, ls)
	}
	for _, key := range []string{`"serving":5`, `"retiring":2`, `"reason":`, `"exit_stats":"ok"`, `"held_by":39`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("status JSON lacks %s: %s", key, b)
		}
	}
	if l := patternLine(ls); l != "7 up = 5 serving + 2 retiring (shrinking, range 2–32)" {
		t.Errorf("pattern line %q", l)
	}
	if w := whyLine(ls); !strings.Contains(w, "needs ~5 links") || !strings.Contains(w, "held by 39 open, 1 active") {
		t.Errorf("why line %q", w)
	}
	if tr := trafficLine(ls); tr != "251 connections (18 active) · 6.1 Mbit/s · 1 link at its limit (~2.4 Mbit/s each)" {
		t.Errorf("traffic line %q", tr)
	}
	// The exit side (no pool detail) keeps the old, short form.
	b, _ = json.Marshal(liveStatus{Links: 3, Target: 3, Phase: "following"})
	if strings.Contains(string(b), "serving") {
		t.Errorf("exit status carries edge-only keys: %s", b)
	}
}
