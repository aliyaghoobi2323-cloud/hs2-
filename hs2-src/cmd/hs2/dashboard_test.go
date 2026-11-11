package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
)

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// dashFixture: three tunnels — an Iran dialer with LB (fresh status, an
// alert, 24 h history), a Kharej listener whose status file is missing, and
// a stopped one whose status is stale — plus a non-tunnel JSON to ignore.
func dashFixture(t *testing.T, now time.Time) dashEnv {
	dir := t.TempDir()
	etc, run, st := filepath.Join(dir, "etc"), filepath.Join(dir, "run"), filepath.Join(dir, "state")
	for _, d := range []string{etc, run, st} {
		os.MkdirAll(d, 0o755)
	}
	writeJSON(t, filepath.Join(etc, "de1.json"), map[string]any{"mode": "dial", "carrier": "mtcp", "addr": "1.2.3.4:443"})
	writeJSON(t, filepath.Join(etc, "nl.json"), map[string]any{"mode": "listen", "carrier": "l3mtcp", "addr": "0.0.0.0:443", "reverse": true})
	writeJSON(t, filepath.Join(etc, "old.json"), map[string]any{"mode": "listen", "carrier": "tls", "addr": "0.0.0.0:8443"})
	writeJSON(t, filepath.Join(etc, "menu-extra.json"), map[string]any{"lang": "fa"})
	srv := 6
	lossy := 0.4
	ls := liveStatus{Users: 123, Mbit: 48.3, Links: 9, Target: 8, Serving: &srv, Counted: true, Updated: now.Unix() - 2,
		ObsCtrlRTTP50: 41, ObsCtrlRTTP99: 97,
		LBPaths: []engine.LBPathView{
			{Local: "10.0.0.1", Remote: "1.2.3.4", State: "ok"},
			{Local: "10.0.0.2", Remote: "1.2.3.5", State: "ok", AutoFactor: 0.5, LossDown: &lossy},
			{Local: "10.0.0.3", Remote: "1.2.3.6", State: "cooling"},
		},
		LBTotal: &engine.LBPathView{DownMbit: 44.1, UpMbit: 4.2}}
	writeJSON(t, filepath.Join(run, filepath.Base(statusPath(filepath.Join(etc, "de1.json")))), ls)
	writeJSON(t, filepath.Join(run, filepath.Base(statusPath(filepath.Join(etc, "old.json")))), liveStatus{Users: 5, Updated: now.Unix() - 600})
	as := newAlertState()
	as.Active = map[string]*activeAlert{}
	as.Active["rtt"] = &activeAlert{Sev: "warn", Text: "link RTT 250 ms (usual 60)"}
	writeJSON(t, alertStatePath(st, "hs2-de1"), as)
	sf := &statsFile{Unit: "hs2-de1"}
	for h := 30; h >= 0; h-- {
		at := now.Add(-time.Duration(h) * time.Hour)
		for k := 0; k < 3; k++ {
			sf.addSample(&liveStatus{ObsCtrlRTTP50: float64(40 + h), ObsCtrlRTTP99: 90, Mbit: float64(10 * (h % 7)), Users: h}, at)
		}
	}
	saveStats(statsPath(st, "hs2-de1"), sf)
	units := map[string]string{"hs2-de1": "active", "hs2-nl": "active", "hs2-old": "inactive"}
	return dashEnv{etcDir: etc, runDir: run, stateDir: st, host: "ir-edge", now: now,
		unitInfo: func(u string) (string, int) { return units[u], 2 }}
}

func TestDashboardRender(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 30, 0, 0, time.UTC)
	d := collectDash(dashFixture(t, now))
	if len(d.Tunnels) != 3 {
		t.Fatalf("tunnels = %d, want 3 (the non-tunnel JSON is skipped)", len(d.Tunnels))
	}
	for _, lang := range []string{"en", "fa"} {
		for _, w := range []int{50, 60, 80, 120} {
			for _, ascii := range []bool{false, true} {
				o := dashOpt{Width: w, Lang: lang, ASCII: ascii}
				out := renderDash(d, o)
				for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
					if dispWidth(l) > w {
						t.Errorf("%s w=%d ascii=%v: line wider than %d: %q", lang, w, ascii, w, l)
					}
				}
				if ascii {
					for _, r := range out {
						if r > 127 {
							t.Fatalf("ascii output has %q", r)
						}
					}
				}
				if lang == "en" && !ascii && w >= 80 {
					for _, want := range []string{"ir-edge · 3 tunnels · 123 users · 48.3 Mbit · health BAD · 1 warnings",
						"hs2-de1 · Iran · direct · mtcp · dialing", "users 123", "down 44.1", "up 4.2", "links 6/8",
						"RTT p50 41 · p99 97 ms", "LB paths: 1 ok · 1 lowered · 1 cut", "restarts 2",
						"24h RTT", "! WARN link RTT 250 ms", "hs2-nl · Kharej · reverse · l3mtcp · dialing",
						"status file missing", "hs2-old", "stopped", "status is stale (updated 600s ago)"} {
						if !strings.Contains(out, want) {
							t.Errorf("w=%d: missing %q in\n%s", w, want, out)
						}
					}
				}
				if lang == "fa" && !ascii && !strings.Contains(out, "تونل") {
					t.Errorf("fa render has no Persian:\n%s", out)
				}
			}
		}
	}
	t.Logf("\n%s", renderDash(d, dashOpt{Width: 80, Lang: "en"}))
	t.Logf("\n%s", renderDash(d, dashOpt{Width: 60, Lang: "fa"}))
}

func TestDashboardEmptyAndStates(t *testing.T) {
	env := dashEnv{etcDir: t.TempDir(), runDir: t.TempDir(), stateDir: t.TempDir(), host: "h", now: time.Now()}
	d := collectDash(env)
	out := renderDash(d, dashOpt{Width: 80, Lang: "en"})
	if !strings.Contains(out, "no tunnels installed") || !strings.Contains(out, "0 tunnels") || !strings.Contains(out, "health OK") {
		t.Fatalf("empty: %s", out)
	}
	cases := []struct {
		active string
		st     *liveStatus
		age    int64
		want   string
	}{
		{"active", nil, -1, "starting"},
		{"active", &liveStatus{}, 100, "stale"},
		{"active", &liveStatus{}, 1, "running"},
		{"failed", &liveStatus{}, 1, "failed"},
		{"activating", nil, -1, "starting"},
		{"", nil, -1, "unknown"},
		{"", &liveStatus{}, 3, "running"},
	}
	for _, c := range cases {
		if got := (dashTunnel{Active: c.active, St: c.st, Age: c.age}).state(); got != c.want {
			t.Errorf("state(%q, age %d) = %s, want %s", c.active, c.age, got, c.want)
		}
	}
}

func TestStatsAggregation(t *testing.T) {
	base := time.Date(2026, 11, 1, 0, 10, 0, 0, time.UTC)
	sf := &statsFile{}
	sf.addSample(&liveStatus{ObsCtrlRTTP50: 40, ObsCtrlRTTP99: 80, Mbit: 10, Users: 3}, base)
	sf.addSample(&liveStatus{ObsCtrlRTTP50: 60, ObsCtrlRTTP99: 120, Mbit: 30, Users: 7}, base.Add(20*time.Minute))
	sf.addSample(&liveStatus{Mbit: 5}, base.Add(30*time.Minute)) // no RTT reading
	if len(sf.Hours) != 1 {
		t.Fatalf("hours = %d", len(sf.Hours))
	}
	h := sf.Hours[0]
	if h.N != 3 || h.RTTN != 2 || h.rttAvg() != 50 || h.RTTMax != 120 || h.mbitAvg() != 15 || h.MbitMax != 30 || h.UsersMax != 7 {
		t.Fatalf("bucket %+v", h)
	}
	// LB total RTT when there is no control RTT
	sf.addSample(&liveStatus{LBTotal: &engine.LBPathView{RTTp50: 70, RTTp99: 140}}, base.Add(time.Hour))
	if sf.Hours[1].rttAvg() != 70 {
		t.Fatalf("lb rtt %+v", sf.Hours[1])
	}
	// 8 days later only 7 days are kept
	for i := 0; i < 9*24; i++ {
		sf.addSample(&liveStatus{Mbit: 1}, base.Add(time.Duration(i)*time.Hour))
	}
	if len(sf.Hours) != statsKeep {
		t.Fatalf("kept %d hours, want %d", len(sf.Hours), statsKeep)
	}
	// clock going back merges into the right hour, order kept
	sf.addSample(&liveStatus{Mbit: 1}, base.Add(time.Duration(9*24-5)*time.Hour))
	for i := 1; i < len(sf.Hours); i++ {
		if sf.Hours[i].H <= sf.Hours[i-1].H {
			t.Fatal("hours out of order")
		}
	}
	last := base.Add(time.Duration(9*24-1) * time.Hour)
	s := sf.last24(last)
	if s[23] == nil || s[0] == nil || s[23].H != last.Truncate(time.Hour).Unix() {
		t.Fatal("last24 slots")
	}
	if (*statsFile)(nil).last24(last)[0] != nil {
		t.Fatal("nil stats")
	}
}

func TestStatsSampleFile(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "x.json")
	writeJSON(t, cfg, map[string]any{"mode": "dial"})
	run := filepath.Join(dir, "run")
	os.MkdirAll(run, 0o755)
	sp := filepath.Join(run, filepath.Base(statusPath(cfg)))
	writeJSON(t, sp, liveStatus{Mbit: 12, Updated: now.Unix() - 100})
	if err := sampleStatsFor(cfg, "hs2-x", run, dir, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(statsPath(dir, "hs2-x")); err == nil {
		t.Fatal("a stale status must not be sampled")
	}
	writeJSON(t, sp, liveStatus{Mbit: 12, Updated: now.Unix()})
	sampleStatsFor(cfg, "hs2-x", run, dir, now)
	sf := loadStats(statsPath(dir, "hs2-x"))
	if len(sf.Hours) != 1 || sf.Hours[0].MbitMax != 12 {
		t.Fatalf("%+v", sf)
	}
}

func TestSparklineAndWidth(t *testing.T) {
	s, mx := sparkline([]float64{0, 5, 10}, []bool{true, false, true}, false)
	if s != "▁ █" || mx != 10 {
		t.Fatalf("%q %v", s, mx)
	}
	s, _ = sparkline([]float64{0, 10}, []bool{true, true}, true)
	if s != "_#" {
		t.Fatalf("%q", s)
	}
	if dispWidth("\033[1;32mok\033[0m") != 2 || dispWidth("می‌شود") != 5 {
		t.Fatal("dispWidth")
	}
	if c := clipW("abcdefghij", 5); dispWidth(c) != 5 {
		t.Fatalf("clip %q", c)
	}
}
