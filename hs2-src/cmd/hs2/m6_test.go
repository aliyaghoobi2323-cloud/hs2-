package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAlertConfValidation(t *testing.T) {
	good := "bot_token=123456:AAAAAAAAAAAAAAAAAAAAAAAAAAAA\nchat_id=-100123\ndisable=rtt,psi\nquiet_hours=23:30-07:00\nsummary_items=users,cuts\n"
	c, err := parseAlertConf(good)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Disabled["rtt"] || !c.Disabled["psi"] || c.Disabled["unit"] || c.Quiet != "23:30-07:00" || strings.Join(c.SummaryItems, ",") != "users,cuts" {
		t.Fatalf("parsed %+v", c)
	}
	for _, bad := range []string{"disable=rtt,bogus", "quiet_hours=25:00-07:00", "quiet_hours=07:00-07:00", "quiet_hours=night",
		"summary_items=users,weather", "lang=de", "summary_time=24:00", "resend_min=-3"} {
		if _, err := parseAlertConf(bad); err == nil {
			t.Errorf("%q accepted", bad)
		} else if !strings.HasPrefix(err.Error(), "line 1") {
			t.Errorf("%q: error without line: %v", bad, err)
		}
	}
	if c, err := parseAlertConf("quiet_hours=off\n"); err != nil || c.Quiet != "" {
		t.Fatalf("off: %v %q", err, c.Quiet)
	}
}

func TestAlertConfSetAndToken(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "alert.conf")
	if err := confSet(p, []string{"chat_id=123456789", "quiet_hours=22:00-06:30", "lang=en"}); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", fi.Mode().Perm())
	}
	// invalid values: nothing written
	before, _ := os.ReadFile(p)
	for _, bad := range [][]string{{"chat_id=hello"}, {"proxy=ftp://x"}, {"quiet_hours=9-5"}, {"bot_token=1:2"}, {"nosuch=1"}, {"rtt_factor=abc"}, {"disable=everything"}} {
		if err := confSet(p, bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	after, _ := os.ReadFile(p)
	if !bytes.Equal(before, after) {
		t.Fatal("a rejected set changed the file")
	}
	// token from stdin, never on the command line; the file keeps other keys
	tok := "123456789:ABCdefGHIjklMNOpqrSTUvwxYZ012345"
	if err := confToken(p, strings.NewReader(tok+"\n")); err != nil {
		t.Fatal(err)
	}
	if err := confToken(p, strings.NewReader("not-a-token\n")); err == nil {
		t.Fatal("bad token accepted")
	}
	c, _ := loadAlertConf(p)
	if c.BotToken != tok || c.ChatID != "123456789" || c.Lang != "en" {
		t.Fatalf("%+v", c)
	}
	// rule off/on
	if err := ruleSet(p, "rtt", false); err != nil {
		t.Fatal(err)
	}
	if err := ruleSet(p, "psi", false); err != nil {
		t.Fatal(err)
	}
	if err := ruleSet(p, "rtt", true); err != nil {
		t.Fatal(err)
	}
	c, _ = loadAlertConf(p)
	if c.Disabled["rtt"] || !c.Disabled["psi"] {
		t.Fatalf("disabled %v", c.Disabled)
	}
	if ruleSet(p, "nosuch", false) == nil {
		t.Fatal("unknown rule accepted")
	}
	// confShow never prints the token
	var buf bytes.Buffer
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	confShow(p)
	w.Close()
	os.Stdout = old
	_, _ = buf.ReadFrom(r)
	if strings.Contains(buf.String(), "ABCdef") || !strings.Contains(buf.String(), "hidden") {
		t.Fatalf("show: %s", buf.String())
	}
}

func TestQuietHours(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 11, 11, h, m, 0, 0, time.Local) }
	cases := []struct {
		q    string
		h, m int
		want bool
	}{
		{"23:00-07:00", 23, 0, true}, {"23:00-07:00", 2, 0, true}, {"23:00-07:00", 6, 59, true},
		{"23:00-07:00", 7, 0, false}, {"23:00-07:00", 12, 0, false}, {"23:00-07:00", 22, 59, false},
		{"01:00-05:00", 3, 0, true}, {"01:00-05:00", 0, 30, false}, {"01:00-05:00", 5, 0, false},
		{"", 3, 0, false}, {"off", 3, 0, false},
	}
	for _, c := range cases {
		if got := inQuiet(c.q, at(c.h, c.m)); got != c.want {
			t.Errorf("%q at %02d:%02d = %v", c.q, c.h, c.m, got)
		}
	}
	cfg := alertCfg{Quiet: "23:00-07:00"}
	night := at(2, 0)
	if telegramAllowed(cfg, alertMsg{Key: "rtt", Sev: sevWarn}, night) {
		t.Fatal("warn sent in quiet hours")
	}
	if !telegramAllowed(cfg, alertMsg{Key: "unit", Sev: sevOK, Was: sevCrit}, night) || telegramAllowed(cfg, alertMsg{Key: "rtt", Sev: sevOK, Was: sevWarn}, night) {
		t.Fatal("resolved: critical passes, warning held")
	}
	if !telegramAllowed(cfg, alertMsg{Key: "unit", Sev: sevCrit}, night) || !telegramAllowed(cfg, alertMsg{Key: "summary", Sev: sevInfo}, night) {
		t.Fatal("crit/summary held in quiet hours")
	}
	if !telegramAllowed(cfg, alertMsg{Key: "rtt", Sev: sevWarn}, at(12, 0)) {
		t.Fatal("warn held by day")
	}
}

func TestAlertDisableAndSummaryItems(t *testing.T) {
	cfg := alertCfg{Disabled: map[string]bool{"ipcut": true, "psi": true}}
	got := filterDisabled(cfg, []alertMsg{{Key: "ipcut:1.2.3.4"}, {Key: "psi"}, {Key: "unit"}, {Key: "summary"}})
	if len(got) != 2 || got[0].Key != "unit" || got[1].Key != "summary" {
		t.Fatalf("%v", got)
	}
	s := alertText("en").summary(alertDay{PeakUsers: 7, Restarts: 2}, "", 0, nil, "users", "restarts")
	if s != "daily summary: peak users 7 · restarts 2" {
		t.Fatalf("%q", s)
	}
	full := alertText("en").summary(alertDay{PeakUsers: 7}, "", 0, nil)
	if !strings.Contains(full, "peak users 7") || !strings.Contains(full, "cuts: none") || !strings.Contains(full, "alerts 0") {
		t.Fatalf("%q", full)
	}
}

func TestAlertHistoryAndTelegramCheck(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.Local)
	log := "2026-11-01 10:00:00 old [h/hs2] x\n2026-11-05 10:00:00 🔴 [h/hs2] down\n2026-11-10 01:00:00 🟠 [h/hs2-de1] rtt\nbroken\n"
	var b bytes.Buffer
	if n := filterHistory(strings.NewReader(log), "", 7, now, &b); n != 2 || strings.Contains(b.String(), "old") {
		t.Fatalf("%d %s", n, b.String())
	}
	b.Reset()
	if n := filterHistory(strings.NewReader(log), "hs2-de1", 7, now, &b); n != 1 {
		t.Fatalf("unit filter %d", n)
	}
	down := func(string) error { return errors.New("timeout") }
	up := func(string) error { return nil }
	if ok, m := telegramCheck(alertCfg{LogFile: "/var/log/hs2-alert.log"}, down); ok || !strings.Contains(m, "proxy") {
		t.Fatalf("iran: %v %s", ok, m)
	}
	if ok, _ := telegramCheck(alertCfg{}, up); !ok {
		t.Fatal("direct")
	}
	if ok, _ := telegramCheck(alertCfg{Proxy: "socks5://127.0.0.1:1080"}, func(a string) error {
		if a == "127.0.0.1:1080" {
			return nil
		}
		return errors.New("blocked")
	}); !ok {
		t.Fatal("proxy")
	}
}

func TestDoctorFixMapping(t *testing.T) {
	cases := []struct {
		f    fixAction
		unit string
		want string
	}{
		{fixAction{Kind: "rxcore", Arg: "0"}, "hs2-de1", "perf cores -u hs2-de1 --keep 0 --confirm-within 5m0s --yes"},
		{fixAction{Kind: "rxcore", Arg: "0"}, "x-ui", ""},
		{fixAction{Kind: "rxcore", Arg: "0"}, "xray", ""},
		{fixAction{Kind: "budget"}, "", "perf kernel --budget 600 --yes"},
		{fixAction{Kind: "stats"}, "", "stats timer on"},
		{fixAction{Kind: "park", Arg: "10.0.0.2"}, "hs2", "lb park -u hs2 --ip 10.0.0.2 --hours 6 --yes"},
		{fixAction{Kind: "cert"}, "", "certbot renew --dry-run"},
		{fixAction{Kind: "alerts"}, "hs2", ""},
		{fixAction{Kind: "xui-restart"}, "", ""},
	}
	for _, c := range cases {
		if got := strings.Join(fixCommand(c.f, c.unit, 6), " "); got != c.want {
			t.Errorf("%+v %s: %q want %q", c.f, c.unit, got, c.want)
		}
	}
	// panel / reality findings never keep a fix; numbering is 1…n
	fs := []finding{
		{Cat: "network", Fix: &fixAction{Kind: "rxcore"}},
		{Cat: "panel", Fix: &fixAction{Kind: "stats"}},
		{Cat: "reality", Fix: &fixAction{Kind: "stats"}},
		{Cat: "cpu", Fix: &fixAction{Kind: "budget"}},
		{Cat: "tunnel"},
	}
	fl := collectFixes(fs)
	if len(fl) != 2 || fs[0].FixN != 1 || fs[3].FixN != 2 || fs[1].Fix != nil || fs[2].Fix != nil {
		t.Fatalf("%v %+v", fl, fs)
	}
	if len(filterCat(fs, "cpu")) != 1 || len(filterCat(fs, "")) != 5 {
		t.Fatal("filterCat")
	}
}

func TestDoctorApplyFixSafety(t *testing.T) {
	dir := t.TempDir()
	old := doctorFixesPath
	doctorFixesPath = filepath.Join(dir, "fixes.json")
	defer func() { doctorFixesPath = old }()
	var out bytes.Buffer
	none := func(time.Duration, string, ...string) (string, error) { return "", nil }
	ran := 0
	exe := func(string, ...string) error { ran++; return nil }
	if applyFix(1, true, 0, nil, &out, none, exe) != 1 || !strings.Contains(out.String(), "run the doctor first") {
		t.Fatalf("no saved run: %s", out.String())
	}
	_ = saveFixes(doctorFixesPath, []fixAction{{Kind: "alerts", Title: "alerts"}, {Kind: "park", Units: []string{"hs2-gone"}, Arg: "10.0.0.2"}}, time.Now())
	out.Reset()
	if applyFix(1, true, 0, nil, &out, none, exe) != 0 || !strings.Contains(out.String(), "Monitoring") || ran != 0 {
		t.Fatalf("alerts fix: %s", out.String())
	}
	if os.Geteuid() == 0 {
		out.Reset()
		// the unit of the park target no longer exists → refused, nothing runs
		if applyFix(2, true, 2, nil, &out, none, exe) != 1 || ran != 0 || !strings.Contains(out.String(), "does not exist") {
			t.Fatalf("gone unit: %s", out.String())
		}
	}
	out.Reset()
	if applyFix(9, true, 0, nil, &out, none, exe) != 1 {
		t.Fatal("fix 9")
	}
	_ = saveFixes(doctorFixesPath, []fixAction{{Kind: "stats"}}, time.Now().Add(-time.Hour))
	out.Reset()
	if applyFix(1, true, 0, nil, &out, none, exe) != 1 || !strings.Contains(out.String(), "older") {
		t.Fatalf("stale: %s", out.String())
	}
}

func TestDoctorCoreguardNotATunnel(t *testing.T) {
	if !isTunnelExec("{ path=/usr/local/bin/hs2 ; argv[]=/usr/local/bin/hs2 run -c /etc/hs2/config.json ; ignore_errors=no }") ||
		!isTunnelExec("/usr/local/bin/hs2 run -c /etc/hs2/de1.json") {
		t.Fatal("tunnel not recognised")
	}
	for _, ex := range []string{
		"{ path=/usr/local/bin/hs2-coreguard ; argv[]=/usr/local/bin/hs2-coreguard --watch ; }",
		"{ argv[]=/usr/local/bin/hs2 alert -c /etc/hs2/config.json -u hs2 ; }",
		"{ argv[]=/usr/local/bin/hs2 stats sample ; }",
		"{ argv[]=/usr/local/bin/hs2 lb park --sweep ; }",
		"",
	} {
		if isTunnelExec(ex) {
			t.Errorf("%q counted as a tunnel", ex)
		}
	}
	env := &deepEnv{runDir: t.TempDir(), run: func(_ time.Duration, name string, args ...string) (string, error) {
		a := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(a, "list-units"):
			return "hs2.service loaded active running hs2\nhs2-coreguard.service loaded active running guard\nhs2-alert-hs2.service loaded inactive dead alert\n", nil
		case strings.Contains(a, "ExecStart") && strings.HasSuffix(a, " hs2.service"):
			return "{ argv[]=/usr/local/bin/hs2 run -c /etc/hs2/config.json ; }", nil
		case strings.Contains(a, "ExecStart") && strings.HasSuffix(a, "hs2-coreguard.service"):
			return "{ argv[]=/usr/local/sbin/hs2-coreguard ; }", nil
		case strings.Contains(a, "ExecStart"):
			return "{ argv[]=/usr/local/bin/hs2 alert -c /etc/hs2/config.json -u hs2 ; }", nil
		case strings.Contains(a, "NRestarts"):
			return "5", nil
		}
		return "", nil
	}}
	var units []string
	for _, f := range checkHS2Health(env, nil) {
		if strings.Contains(f.What, "restart(s)") {
			units = append(units, f.Area)
		}
	}
	if len(units) != 1 || !strings.HasSuffix(units[0], "hs2.service") {
		t.Fatalf("restart lines for %v", units)
	}
}

func TestDoctorMonitoringAndBudget(t *testing.T) {
	env := &deepEnv{run: func(_ time.Duration, name string, args ...string) (string, error) {
		if args[len(args)-1] == "hs2-alert-hs2.timer" {
			return "enabled\n", nil
		}
		return "disabled\n", nil
	}}
	fs := checkMonitoring(env, []string{"hs2", "hs2-de1"})
	if len(fs) != 2 || fs[0].Fix.Kind != "stats" || fs[1].Fix.Kind != "alerts" || strings.Join(fs[1].Fix.Units, ",") != "hs2-de1" {
		t.Fatalf("%+v", fs)
	}
	if len(checkMonitoring(env, nil)) != 0 {
		t.Fatal("no tunnel: no monitoring finding")
	}
	if parseSoftnetSqueeze([]byte("0000a 00000000 00000003 0\n0000b 00000000 00000010 0\n")) != 0x13 {
		t.Fatal("softnet parse")
	}
	proc := t.TempDir()
	_ = os.MkdirAll(filepath.Join(proc, "sys/net/core"), 0o755)
	_ = os.WriteFile(filepath.Join(proc, "sys/net/core/netdev_budget"), []byte("300\n"), 0o644)
	env.proc = proc
	f := checkBudget(env, []byte("1 0 00000001\n"), []byte("1 0 00000009\n"))
	if len(f) != 1 || f[0].Sev != "WARN" || f[0].Fix == nil || f[0].Fix.Kind != "budget" {
		t.Fatalf("%+v", f)
	}
	f = checkBudget(env, []byte("1 0 00000009\n"), []byte("1 0 00000009\n"))
	if f[0].Sev != "OK" || f[0].Fix != nil {
		t.Fatalf("no squeeze: %+v", f)
	}
}

func TestUnitFromStatusFile(t *testing.T) {
	for in, want := range map[string]string{"etc-hs2-config.json": "hs2", "etc-hs2-de1.json": "hs2-de1", "etc-hs2-hs2-x3.json": "hs2-x3", "tmp-x.json": ""} {
		if got := unitFromStatusFile(in); got != want {
			t.Errorf("%s → %q want %q", in, got, want)
		}
	}
}
