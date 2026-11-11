package main

import (
	"encoding/json"
	"flag"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
)

func fp(v float64) *float64 { return &v }

func TestLBTableRendering(t *testing.T) {
	deepLang = "en"
	ls := liveStatus{LBPaths: []engine.LBPathView{
		{Local: "1.1.1.1", Remote: "9.9.9.9", Primary: true, SharePct: 60, EffPct: 70, Target: 12, Links: 12, Users: 30, DownMbit: 120.5, UpMbit: 8, LossDown: fp(0.4), RTTp50: 80, RTTp99: 95, AutoFactor: 1, AutoReason: "best", State: "ok"},
		{Local: "2.2.2.2", Remote: "9.9.9.9", SharePct: 40, EffPct: 10, Target: 2, Links: 2, Users: 0, LossDown: fp(4.2), LossUp: fp(1), RTTp50: 90, RTTp99: 300, AutoFactor: 0.25, AutoReason: "lossy", State: "cooling", CooledS: 20},
	}, LBTotal: &engine.LBPathView{SharePct: 100, Target: 14, Links: 14, Users: 30, DownMbit: 120.5, UpMbit: 8}}
	out := renderLBTable(ls, map[string]int{"2.2.2.2>9.9.9.9": 3})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("want header + 2 paths + total:\n%s", out)
	}
	for _, w := range []string{"share/eff", "cuts 24h", "60%/70%", "12/12", "↓120.5 ↑8.0", "↓0.4%", "80/95", "×1.00 best", "primary"} {
		if !strings.Contains(lines[0]+lines[1], w) {
			t.Fatalf("missing %q in\n%s", w, out)
		}
	}
	if !strings.Contains(lines[2], "×0.25 lossy") || !strings.Contains(lines[2], "cooled 20s") || !strings.HasSuffix(lines[2], "3") || !strings.Contains(lines[2], "↓4.2 ↑1.0%") {
		t.Fatalf("path 2: %q", lines[2])
	}
	// columns aligned: "links" starts at the same display column on every row
	col := func(s, sub string) int { return dispWidth(s[:strings.Index(s, sub)]) }
	if col(lines[0], "links") != col(lines[1], "12/12") || col(lines[1], "12/12") != col(lines[2], "2/2") {
		t.Fatalf("not aligned:\n%s", out)
	}
	if !strings.Contains(renderLBTable(liveStatus{}, nil), "No load-balancing paths") {
		t.Fatal("empty status message")
	}
	deepLang = "fa"
	if !strings.Contains(renderLBTable(ls, nil), "سهم/مؤثر") {
		t.Fatal("fa header")
	}
	deepLang = "en"
}

func TestParseLBEvents(t *testing.T) {
	log := `2026-11-10T12:00:01+0330 host hs2[1]: 12:00:01.000 lb auto2: path 2.2.2.2>9.9.9.9 cut (loss 4.0% vs best 0.3%); recovery after 30s under 1.0%
2026-11-10T12:01:01+0330 host hs2[1]: 12:01:01.000 lb auto2: path 2.2.2.2>9.9.9.9 recovering (loss 0.2% for 30s) — its share comes back step by step
2026-11-10T12:05:00+0330 host hs2[1]: 12:05:00.000 lb auto2: path 2.2.2.2>9.9.9.9 cut (loss 5.0%, as the dialing side judged)
2026-11-10T12:06:00+0330 host hs2[1]: 12:06:00.000 lb auto2: path 1.1.1.1>9.9.9.9 flapping: 4 cuts in 60m — kept out 30m0s (penalty 1 of max 1)
2026-11-10T12:07:00+0330 host hs2[1]: something else entirely
`
	ev := parseLBEvents(log)
	if len(ev) != 4 || ev[0].Kind != "cut" || ev[1].Kind != "recovering" || ev[3].Kind != "flapping" || ev[0].When != "2026-11-10T12:00:01+0330" {
		t.Fatalf("%+v", ev)
	}
	c := cutCounts(ev)
	if c["2.2.2.2>9.9.9.9"] != 2 || c["1.1.1.1>9.9.9.9"] != 0 {
		t.Fatalf("%v", c)
	}
}

func TestRankIPs(t *testing.T) {
	ps := []ipProbe{
		{IP: "a", RTT: 90, Loss: 0, OK: true},
		{IP: "b", RTT: 70, Loss: 3, OK: true},
		{IP: "c", OK: false},
		{IP: "d", RTT: 80, Loss: 0, OK: true},
		{IP: "e", RTT: 80.5, Loss: 0.4, OK: true, Current: true},
	}
	r := rankIPs(ps)
	// the current one is within 1 ms / 1 % of the best (d): it stays first
	if r[0].IP != "e" || r[1].IP != "d" || r[2].IP != "a" || r[3].IP != "b" || r[4].IP != "c" {
		t.Fatalf("%v", r)
	}
	ps[4].RTT = 85 // now clearly worse: d wins
	r = rankIPs(ps)
	if r[0].IP != "d" || r[1].IP != "e" {
		t.Fatalf("%v", r)
	}
	out := renderSuggest(r, []lbIP{{IP: "a", Weight: 100}})
	if !strings.Contains(out, "★ d") || !strings.Contains(out, "Suggested primary: d") || !strings.Contains(out, "in lb") || !strings.Contains(out, "current primary") {
		t.Fatal(out)
	}
	if !strings.Contains(renderSuggest(rankIPs([]ipProbe{{IP: "x"}}), nil), "No local IP got an answer") {
		t.Fatal("no answer case")
	}
}

func TestWeightsValidation(t *testing.T) {
	cur := []lbIP{{IP: "1.1.1.1", Weight: 50}, {IP: "2.2.2.2", Weight: 50}, {IP: "3.3.3.3"}}
	eq := splitEqual(cur)
	if eq[0].Weight != 34 || eq[1].Weight != 33 || eq[2].Weight != 33 || weightsSum100(eq) != nil {
		t.Fatalf("%v", eq)
	}
	if cur[0].Weight != 50 {
		t.Fatal("splitEqual changed its input")
	}
	ok, err := parseWeights("1.1.1.1=60, 2.2.2.2=30,3.3.3.3=10", cur)
	if err != nil || ok[0].Weight != 60 || ok[2].Weight != 10 {
		t.Fatalf("%v %v", ok, err)
	}
	for spec, want := range map[string]string{
		"1.1.1.1=60,2.2.2.2=30,3.3.3.3=20":           "sum to 110",
		"1.1.1.1=60,2.2.2.2=40":                      "no weight given for 3.3.3.3",
		"1.1.1.1=50,2.2.2.2=25,3.3.3.3=25,4.4.4.4=0": "not in this side's lb list",
		"1.1.1.1=x":   "bad weight",
		"1.1.1.1=101": "bad weight",
		"nope=5":      "not an IP",
	} {
		if _, err := parseWeights(spec, cur); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v (want %q)", spec, err, want)
		}
	}
}

func TestParkUnparkWeights(t *testing.T) {
	l := []lbIP{{IP: "a", Weight: 50}, {IP: "b", Weight: 30}, {IP: "c", Weight: 20}}
	p, err := parkWeights(l, "a")
	if err != nil || p[0].Weight != 0 || weightsSum100(p) != nil || p[1].Weight <= p[2].Weight {
		t.Fatalf("%v %v", p, err)
	}
	if _, err := parkWeights(p, "a"); err == nil {
		t.Fatal("double park allowed")
	}
	if _, err := parkWeights([]lbIP{{IP: "a", Weight: 100}, {IP: "b", Weight: 0}}, "a"); err == nil {
		t.Fatal("parked the only active IP")
	}
	if _, err := parkWeights(l, "zz"); err == nil {
		t.Fatal("unknown IP parked")
	}
	// same set: back to the exact prior weights
	u := unparkWeights(p, l, "a")
	if u[0].Weight != 50 || u[1].Weight != 30 || u[2].Weight != 20 {
		t.Fatalf("%v", u)
	}
	// the set changed meanwhile (d added): a rejoins with an equal share
	cur := append(append([]lbIP(nil), p...), lbIP{IP: "d", Weight: 0})
	u = unparkWeights(cur, l, "a")
	if u[0].Weight != 25 || weightsSum100(u) != nil {
		t.Fatalf("%v", u)
	}
}

func TestParkTimerLogic(t *testing.T) {
	now := time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC)
	ps := []lbPark{{IP: "a", Until: now.Add(-time.Second).Unix()}, {IP: "b", Until: now.Add(time.Hour).Unix()}, {IP: "c", Until: now.Unix()}}
	due, keep := expiredParks(ps, now)
	if len(due) != 2 || due[0].IP != "a" || due[1].IP != "c" || len(keep) != 1 || keep[0].IP != "b" {
		t.Fatalf("%v %v", due, keep)
	}
	// park state survives (a file), the timer is a real unit that survives reboot
	d := t.TempDir()
	old := parkDir
	parkDir = d
	defer func() { parkDir = old }()
	saveParks("hs2-x", ps)
	if got := loadParks("hs2-x.service"); len(got) != 3 || got[1].IP != "b" {
		t.Fatalf("%v", got)
	}
	if !anyParks() {
		t.Fatal("anyParks")
	}
	saveParks("hs2-x", nil)
	if _, err := os.Stat(filepath.Join(d, "lb-park-hs2-x.json")); !os.IsNotExist(err) || anyParks() {
		t.Fatal("empty park list must remove the file")
	}
	svc, tm := parkTimerTexts("/usr/local/bin/hs2")
	if !strings.Contains(svc, "ExecStart=/usr/local/bin/hs2 lb park --sweep") || !strings.Contains(tm, "OnBootSec=") || !strings.Contains(tm, "WantedBy=timers.target") {
		t.Fatal(svc + tm)
	}
}

func TestWithOwnListSides(t *testing.T) {
	// iran edge, reverse: iran listens on addr; the own list is "iran"
	raw := []byte(`{"mode":"dial","reverse":true,"carrier":"l3mtcp","addr":"1.1.1.1:443","shared_key":"k"}`)
	var fc fileConfig
	json.Unmarshal(raw, &fc)
	if own, peers := lbSide(fc); own != "iran" || len(peers) != 0 {
		t.Fatalf("%s %v", own, peers)
	}
	nb, err := withOwnList(raw, fc, func(l []lbIP) ([]lbIP, error) {
		return splitEqual(append(l, lbIP{IP: "1.1.1.1"}, lbIP{IP: "2.2.2.2"})), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var fc2 fileConfig
	if err := json.Unmarshal(nb, &fc2); err != nil || fc2.LB == nil || !fc2.LB.Enabled || len(fc2.LB.Iran) != 2 || fc2.LB.Iran[0].Weight != 50 || fc2.LB.Kharej == nil {
		t.Fatalf("%s %v", nb, err)
	}
	if !strings.Contains(string(nb), `"shared_key": "k"`) {
		t.Fatal("other keys lost")
	}
	// kharej dialing in reverse: own list kharej, peers = iran's addr + its lb list
	raw = []byte(`{"mode":"listen","reverse":true,"carrier":"l3mtcp","addr":"5.5.5.5:443","bind_local_ip":"9.9.9.9","lb":{"enabled":true,"mode":"auto","iran":[{"ip":"5.5.5.5","weight":50},{"ip":"6.6.6.6","weight":50}],"kharej":[]}}`)
	json.Unmarshal(raw, &fc)
	if own, peers := lbSide(fc); own != "kharej" || strings.Join(peers, ",") != "5.5.5.5,6.6.6.6" {
		t.Fatalf("%s %v", own, peers)
	}
}

func TestFirstOther(t *testing.T) {
	if firstOther([]string{"1.1.1.1", "2.2.2.2"}, "1.1.1.1") != "2.2.2.2" || firstOther([]string{"2001:db8::1", "3.3.3.3"}, "1.1.1.1") != "3.3.3.3" || firstOther(nil, "1.1.1.1") != "" {
		t.Fatal("firstOther")
	}
}

// The validator's "own lb list" in a reverse tunnel: Iran (mode dial) owns
// lb.iran, kharej (mode listen + reverse, it dials) owns lb.kharej.
func TestValidateOwnLBListReverse(t *testing.T) {
	h := validateHost{localIP: func(ip net.IP) bool { return ip.String() == "9.9.9.9" }, listening: func() []sockListen { return nil }, otherCfgs: func(string) map[string][]byte { return nil }}
	kh := []byte(`{"mode":"listen","reverse":true,"carrier":"l3mtcp","addr":"5.5.5.5:443","lb":{"enabled":true,"mode":"auto","iran":[{"ip":"5.5.5.5","weight":50},{"ip":"6.6.6.6","weight":50}],"kharej":[]}}`)
	errs, _ := crossCheck(kh, nil, "", h)
	for _, e := range errs {
		if strings.Contains(e, "not an address of this server") {
			t.Fatalf("kharej took the Iran list for its own: %v", errs)
		}
	}
	ir := []byte(`{"mode":"dial","reverse":true,"carrier":"l3mtcp","addr":"0.0.0.0:443","forward_ports":"8443","lb":{"enabled":true,"mode":"auto","iran":[{"ip":"9.9.9.9","weight":50},{"ip":"6.6.6.6","weight":50}],"kharej":[]}}`)
	errs, _ = crossCheck(ir, nil, "", h)
	if len(errs) != 1 || !strings.Contains(errs[0], "6.6.6.6 is not an address") {
		t.Fatalf("iran must check its own list: %v", errs)
	}
	var fc fileConfig
	json.Unmarshal(ir, &fc)
	if ps := hostPorts(fc); len(ps) < 2 {
		t.Fatalf("iran reverse listens on addr AND the user ports: %v", ps)
	}
}

func TestParseInterleaved(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	u := fs.String("u", "", "")
	y := fs.Bool("yes", false, "")
	pos := parseInterleaved(fs, []string{"-u", "hs2-a", "1.1.1.1=70,2.2.2.2=30", "--yes"})
	if *u != "hs2-a" || !*y || len(pos) != 1 || pos[0] != "1.1.1.1=70,2.2.2.2=30" {
		t.Fatalf("%v %v %v", *u, *y, pos)
	}
}

// A park whose tunnel was deleted is dropped by the sweep, and the timer
// removes itself once nothing is parked.
func TestParkSweepDeletedTunnel(t *testing.T) {
	menuEnv(t)
	old, oldSC := parkDir, parkSystemctl
	parkDir = t.TempDir()
	var calls []string
	parkSystemctl = func(a ...string) error { calls = append(calls, strings.Join(a, " ")); return nil }
	defer func() { parkDir, parkSystemctl = old, oldSC }()
	saveParks("hs2-gone", []lbPark{{Unit: "hs2-gone", IP: "1.1.1.1", Until: 1}})
	parkSweep(time.Now())
	if anyParks() {
		t.Fatal("park of a deleted tunnel kept")
	}
	if len(calls) == 0 || calls[0] != "disable --now "+lbParkTimer {
		t.Fatalf("timer not removed: %v", calls)
	}
}
