package main

import (
	"strings"
	"testing"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
)

// ou180 (prod audit #9): on the Iran side a row lists every Kharej IP
// ("198.51.100.113,203.0.113.67") while the judge names one pair per cut:
// the row sums them (before: always 0 on every IPv4 row).
func TestLBHistCutsCommaRemotes(t *testing.T) {
	deepLang = "en"
	hist := map[string]int{
		"203.0.113.10>198.51.100.113": 2,
		"203.0.113.10>203.0.113.67":   6,
		"2a05::2>2a01::1":             9,
	}
	if n := lbHistCuts(hist, "203.0.113.10", "198.51.100.113,203.0.113.67"); n != 8 {
		t.Fatalf("comma remotes: %d cuts, want 8", n)
	}
	if n := lbHistCuts(hist, "2a05::2", "2a01::1"); n != 9 {
		t.Fatalf("single pair: %d, want 9", n)
	}
	ls := liveStatus{LBPaths: []engine.LBPathView{
		{Local: "203.0.113.10", Remote: "198.51.100.113,203.0.113.67", Target: 1, Links: 1, State: "ok"},
	}}
	out := renderLBTable(ls, hist)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if !strings.HasSuffix(lines[1], "8") || !strings.Contains(lines[0], "subs") {
		t.Fatalf("table:\n%s", out)
	}
}

// ou180: the listening side's header says the mode in effect, taken from the
// dialing side, not its own config's (unset: "fixed").
func TestLBLiveSummaryPeerMode(t *testing.T) {
	deepLang = "en"
	fc := fileConfig{LB: &lbConfig{Enabled: true,
		Iran:   []lbIP{{IP: "1.1.1.1", Weight: 50}, {IP: "1.1.1.2", Weight: 50}},
		Kharej: []lbIP{{IP: "9.9.9.9", Weight: 100}}}}
	got := lbLiveSummary(fc, liveStatus{LBMode: "auto", LBModeFrom: "peer"}, true)
	if !strings.HasPrefix(got, "lb: auto (from the Kharej server) · ") {
		t.Fatalf("peer mode: %q", got)
	}
	if got := lbLiveSummary(fc, liveStatus{}, false); !strings.HasPrefix(got, "lb: fixed · ") {
		t.Fatalf("no status: %q", got)
	}
	if got := lbLiveSummary(fc, liveStatus{LBMode: "fixed"}, true); !strings.HasPrefix(got, "lb: fixed · ") {
		t.Fatalf("own mode: %q", got)
	}
}

// ou180 (prod audit #10): the Iran side's count is streams, said so.
func TestTrafficLineStreams(t *testing.T) {
	ls := liveStatus{Role: "Iran side", Users: 1985, UsersKind: "streams", MConnOpen: 1012, Counted: true, Flowing: 136, Mbit: 104.2}
	if s := trafficLine(ls); !strings.HasPrefix(s, "1985 streams of 1012 user connections (136 active)") {
		t.Fatalf("iran: %q", s)
	}
	ls = liveStatus{Role: "Kharej side", Users: 1014, UsersKind: "connections", Counted: true, Flowing: 100}
	if s := trafficLine(ls); !strings.HasPrefix(s, "1014 connections (100 active)") {
		t.Fatalf("kharej: %q", s)
	}
}
