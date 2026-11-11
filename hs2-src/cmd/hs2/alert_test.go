package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var alertT0 = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

func ip(v int) *int { return &v }

// healthy: an active, settled tunnel with a fresh status.
func healthySnap(now time.Time, ls *alertStatus) alertSnap {
	return alertSnap{Now: now, Unit: "hs2", Active: "active", MainPID: 100, NRestarts: 0,
		UptimeS: 3600, Status: ls, PSI60: 1, CertDays: -1}
}

func keys(ms []alertMsg) []string {
	var k []string
	for _, m := range ms {
		k = append(k, m.Sev+":"+m.Key)
	}
	return k
}

func has(ms []alertMsg, sev, key string) bool {
	for _, m := range ms {
		if m.Sev == sev && m.Key == key {
			return true
		}
	}
	return false
}

func TestAlertUnitDownDedupResendResolve(t *testing.T) {
	cfg := defaultAlertCfg()
	st := newAlertState()
	now := alertT0
	evalAlerts(cfg, st, healthySnap(now, nil)) // first check arms the state
	s := healthySnap(now.Add(time.Minute), nil)
	s.Active = "failed"
	ms := evalAlerts(cfg, st, s)
	if !has(ms, sevCrit, "unit") {
		t.Fatalf("failed unit: want crit, got %v", keys(ms))
	}
	for i := 2; i <= 10; i++ {
		s.Now = now.Add(time.Duration(i) * time.Minute)
		if ms := evalAlerts(cfg, st, s); len(ms) != 0 {
			t.Fatalf("minute %d: de-dup broken: %v", i, keys(ms))
		}
	}
	s.Now = now.Add(32 * time.Minute)
	ms = evalAlerts(cfg, st, s)
	if len(ms) != 1 || !strings.Contains(ms[0].Text, "هنوز") {
		t.Fatalf("resend after 30 min: %v", ms)
	}
	ok := healthySnap(now.Add(33*time.Minute), nil)
	ok.MainPID = 200
	ms = evalAlerts(cfg, st, ok)
	if !has(ms, sevOK, "unit") || has(ms, sevWarn, "restart") {
		t.Fatalf("want resolved only (no separate restart line), got %v", keys(ms))
	}
}

func TestAlertRestartTransientActivating(t *testing.T) {
	cfg := defaultAlertCfg()
	st := newAlertState()
	evalAlerts(cfg, st, healthySnap(alertT0, nil))
	s := healthySnap(alertT0.Add(time.Minute), nil)
	s.Active = "activating"
	if ms := evalAlerts(cfg, st, s); has(ms, sevCrit, "unit") {
		t.Fatal("one 'activating' check (a restart) must not alert")
	}
	s.Now = s.Now.Add(time.Minute)
	if ms := evalAlerts(cfg, st, s); !has(ms, sevCrit, "unit") {
		t.Fatal("stuck activating for 2 checks must alert")
	}
}

func TestAlertIdleServerNoLinksAlarm(t *testing.T) {
	cfg := defaultAlertCfg()
	st := newAlertState()
	// night: autopilot shrank to 2, one serving, no users
	ls := &alertStatus{Links: 1, Target: 2, Serving: ip(1), Users: 0, Phase: "steady"}
	for i := 0; i < 10; i++ {
		if ms := evalAlerts(cfg, st, healthySnap(alertT0.Add(time.Duration(i)*time.Minute), ls)); len(ms) != 0 {
			t.Fatalf("idle small pool alerted: %v", keys(ms))
		}
	}
	// zero links, zero users: still nothing (no one is affected)
	ls = &alertStatus{Links: 0, Target: 2, Serving: ip(0)}
	if ms := evalAlerts(cfg, st, healthySnap(alertT0.Add(11*time.Minute), ls)); len(ms) != 0 {
		t.Fatalf("idle zero links: %v", keys(ms))
	}
	// users connected and nothing serving: critical at once
	ls = &alertStatus{Links: 0, Target: 2, Serving: ip(0), Users: 3}
	if ms := evalAlerts(cfg, st, healthySnap(alertT0.Add(12*time.Minute), ls)); !has(ms, sevCrit, "dead") {
		t.Fatal("users without links must alert immediately")
	}
}

func TestAlertLinksLowStreakAndResolve(t *testing.T) {
	cfg := defaultAlertCfg()
	st := newAlertState()
	low := &alertStatus{Links: 3, Target: 12, Serving: ip(3), Users: 40, Phase: "steady"}
	var got [][]alertMsg
	for i := 0; i < 3; i++ {
		got = append(got, evalAlerts(cfg, st, healthySnap(alertT0.Add(time.Duration(i)*time.Minute), low)))
	}
	if len(got[0])+len(got[1]) != 0 || !has(got[2], sevWarn, "links") {
		t.Fatalf("want links alert on the 3rd check: %v %v %v", keys(got[0]), keys(got[1]), keys(got[2]))
	}
	good := &alertStatus{Links: 12, Target: 12, Serving: ip(12), Users: 40, Phase: "steady"}
	if ms := evalAlerts(cfg, st, healthySnap(alertT0.Add(4*time.Minute), good)); !has(ms, sevOK, "links") {
		t.Fatalf("want resolve: %v", keys(ms))
	}
}

func TestAlertPostRestartRampAndShrink(t *testing.T) {
	cfg := defaultAlertCfg()
	st := newAlertState()
	ramp := &alertStatus{Links: 1, Target: 16, Serving: ip(1), Users: 5, Phase: "scaling"}
	for i := 0; i < 6; i++ {
		s := healthySnap(alertT0.Add(time.Duration(i)*time.Minute), ramp)
		s.UptimeS = int64(30 + i*20) // just restarted
		if ms := evalAlerts(cfg, st, s); len(ms) != 0 {
			t.Fatalf("post-restart ramp alerted: %v", keys(ms))
		}
	}
	// settled in time but the autopilot is deliberately shrinking
	shrink := &alertStatus{Links: 5, Target: 16, Serving: ip(5), Users: 5, Phase: "shrinking"}
	for i := 0; i < 5; i++ {
		if ms := evalAlerts(cfg, st, healthySnap(alertT0.Add(time.Duration(10+i)*time.Minute), shrink)); len(ms) != 0 {
			t.Fatalf("shrinking alerted: %v", keys(ms))
		}
	}
}

func alertLBStatus(paths ...alertPath) *alertStatus {
	return &alertStatus{Links: 8, Target: 8, Serving: ip(8), Users: 10, Phase: "steady", Paths: paths}
}

func TestAlertLBDrainAndPartialCutAreQuiet(t *testing.T) {
	cfg := defaultAlertCfg()
	st := newAlertState()
	// IP 10.0.0.1 has two paths; the LB drains one and one is cooling: the
	// IP still has... nothing serving? No: draining is intentional, not cut.
	ls := alertLBStatus(
		alertPath{Local: "10.0.0.1", Remote: "20.0.0.1", Target: 4, Links: 4, Serving: 4, State: "ok"},
		alertPath{Local: "10.0.0.1", Remote: "20.0.0.2", Target: 0, Links: 1, State: "draining"},
		alertPath{Local: "2001:db8::1", Remote: "2001:db8::2", Target: 4, Links: 0, State: "cooling", AutoReason: "dial fails"},
		alertPath{Local: "10.0.0.2", Remote: "2001:db8::2", Target: 2, Links: 2, Serving: 2, State: "ok"},
	)
	for i := 0; i < 5; i++ {
		ms := evalAlerts(cfg, st, healthySnap(alertT0.Add(time.Duration(i)*time.Minute), ls))
		for _, m := range ms {
			if m.Key != "ipcut:2001:db8::1" {
				t.Fatalf("partial cut / drain alerted: %v", keys(ms))
			}
		}
	}
	if st.Active["ipcut:2001:db8::1"] == nil {
		t.Fatal("IPv6 local IP whose only path is cut must alert")
	}
	if st.Active["ipcut:2001:db8::2"] != nil {
		t.Fatal("remote v6 still reachable via another path: no alert")
	}
}

func TestAlertIPFullyCutAndFlap(t *testing.T) {
	cfg := defaultAlertCfg()
	st := newAlertState()
	up := alertPath{Local: "10.0.0.1", Remote: "20.0.0.1", Target: 4, Links: 4, Serving: 4, State: "ok"}
	down := up
	down.State, down.Links, down.Serving = "cooling", 0, 0
	other := alertPath{Local: "10.0.0.2", Remote: "20.0.0.2", Target: 4, Links: 4, Serving: 4, State: "ok"}
	var all []alertMsg
	for i := 0; i < 10; i++ {
		p := up
		if i%2 == 1 {
			p = down
		}
		all = append(all, evalAlerts(cfg, st, healthySnap(alertT0.Add(time.Duration(i)*time.Minute), alertLBStatus(p, other)))...)
	}
	if !has(all, sevWarn, "flap:10.0.0.1>20.0.0.1") {
		t.Fatalf("flapping path not reported: %v", keys(all))
	}
	for _, m := range all {
		if strings.HasPrefix(m.Key, "ipcut:") {
			t.Fatalf("1-minute cuts must not count as a full IP cut: %v", keys(all))
		}
	}
	if st.Day.Cuts["10.0.0.1"] != 5 {
		t.Fatalf("daily cuts per IP: %v", st.Day.Cuts)
	}
}

// buildBaseline feeds 5-minute samples so the RTT baseline exists.
func buildBaseline(cfg alertCfg, st *alertState, mk func(rtt float64) *alertStatus, from time.Time, rtt float64) time.Time {
	t := from
	for i := 0; i < rttMinSamples+1; i++ {
		evalAlerts(cfg, st, healthySnap(t, mk(rtt)))
		t = t.Add(rttSampleEvery)
	}
	return t
}

func TestAlertRTTNoLB(t *testing.T) {
	cfg := defaultAlertCfg()
	st := newAlertState()
	mk := func(r float64) *alertStatus {
		return &alertStatus{Links: 4, Target: 4, Users: 3, Phase: "steady", RTTp50: r}
	}
	now := buildBaseline(cfg, st, mk, alertT0, 60)
	// 80 ms: above 1.6x? no (96); and min +30 => 96. Not high.
	for i := 0; i < 8; i++ {
		if ms := evalAlerts(cfg, st, healthySnap(now.Add(time.Duration(i)*time.Minute), mk(80))); len(ms) != 0 {
			t.Fatalf("small rise alerted: %v", keys(ms))
		}
	}
	now = now.Add(10 * time.Minute)
	var fired int
	for i := 0; i < rttSustain; i++ {
		ms := evalAlerts(cfg, st, healthySnap(now.Add(time.Duration(i)*time.Minute), mk(200)))
		if has(ms, sevWarn, "rtt") {
			fired = i + 1
		}
	}
	if fired != rttSustain {
		t.Fatalf("RTT alert should fire on check %d, fired at %d", rttSustain, fired)
	}
}

func TestAlertRTTNetworkWideOneMessage(t *testing.T) {
	cfg := defaultAlertCfg()
	st := newAlertState()
	mk := func(r float64) *alertStatus {
		return alertLBStatus(
			alertPath{Local: "10.0.0.1", Remote: "20.0.0.1", Target: 4, Links: 4, Serving: 4, State: "ok", RTTp50: r},
			alertPath{Local: "10.0.0.2", Remote: "20.0.0.2", Target: 4, Links: 4, Serving: 4, State: "ok", RTTp50: r + 10},
		)
	}
	now := buildBaseline(cfg, st, mk, alertT0, 50)
	var all []alertMsg
	for i := 0; i < rttSustain+2; i++ {
		all = append(all, evalAlerts(cfg, st, healthySnap(now.Add(time.Duration(i)*time.Minute), mk(200)))...)
	}
	if len(all) != 1 || all[0].Key != "rtt:all" || !strings.Contains(all[0].Text, "شبکه") {
		t.Fatalf("want exactly one network-wide message, got %v", keys(all))
	}
	// only one path high -> per-path alert, not network-wide
	st2 := newAlertState()
	now = buildBaseline(cfg, st2, mk, alertT0, 50)
	one := func() *alertStatus {
		s := mk(50)
		s.Paths[0].RTTp50 = 300
		return s
	}
	all = nil
	for i := 0; i < rttSustain; i++ {
		all = append(all, evalAlerts(cfg, st2, healthySnap(now.Add(time.Duration(i)*time.Minute), one()))...)
	}
	if !has(all, sevWarn, "rtt:10.0.0.1>20.0.0.1") || has(all, sevWarn, "rtt:all") {
		t.Fatalf("single path rise: %v", keys(all))
	}
}

func TestAlertRTTHourOfDayBaseline(t *testing.T) {
	cfg := defaultAlertCfg()
	st := newAlertState()
	// evening (20:xx) is always 150 ms on this tunnel; the day is 50 ms
	st.fill(alertT0)
	for i := 0; i < 30; i++ {
		st.RTTHour["rtt|20"] = append(st.RTTHour["rtt|20"], 150)
		st.RTT["rtt"] = append(st.RTT["rtt"], 50)
	}
	evening := time.Date(2026, 11, 11, 20, 10, 0, 0, time.UTC)
	mk := &alertStatus{Links: 4, Target: 4, Users: 3, Phase: "steady", RTTp50: 160}
	for i := 0; i < rttSustain+2; i++ {
		if ms := evalAlerts(cfg, st, healthySnap(evening.Add(time.Duration(i)*time.Minute), mk)); len(ms) != 0 {
			t.Fatalf("usual evening RTT alerted: %v", keys(ms))
		}
	}
}

func TestAlertStaleStatusIsUnknown(t *testing.T) {
	now := alertT0
	b, _ := json.Marshal(map[string]any{"links": 0, "target": 12, "updated": now.Unix() - 60})
	if parseAlertStatus(b, now) != nil {
		t.Fatal("a 60 s old status must read as unknown")
	}
	cfg := defaultAlertCfg()
	st := newAlertState()
	low := &alertStatus{Links: 3, Target: 12, Serving: ip(3), Users: 40, Phase: "steady"}
	for i := 0; i < 3; i++ {
		evalAlerts(cfg, st, healthySnap(now.Add(time.Duration(i)*time.Minute), low))
	}
	if st.Active["links"] == nil {
		t.Fatal("setup: links alert expected")
	}
	// status missing (daemon restarting): neither resolved nor a new alert
	if ms := evalAlerts(cfg, st, healthySnap(now.Add(4*time.Minute), nil)); len(ms) != 0 {
		t.Fatalf("missing status changed alerts: %v", keys(ms))
	}
	if st.Active["links"] == nil {
		t.Fatal("unknown must keep the active alert")
	}
}

func TestAlertTwoTunnelsSeparateState(t *testing.T) {
	dir := t.TempDir()
	a, b := alertStatePath(dir, "hs2"), alertStatePath(dir, "hs2-de1")
	if a == b {
		t.Fatal("state paths collide")
	}
	cfg := defaultAlertCfg()
	sa := loadAlertState(a)
	evalAlerts(cfg, sa, healthySnap(alertT0, nil))
	f := healthySnap(alertT0.Add(time.Minute), nil)
	f.Active = "failed"
	evalAlerts(cfg, sa, f)
	if err := saveAlertState(a, sa); err != nil {
		t.Fatal(err)
	}
	sb := loadAlertState(b)
	if len(sb.Active) != 0 {
		t.Fatal("second tunnel inherited the first one's alerts")
	}
	if got := loadAlertState(a); got.Active["unit"] == nil {
		t.Fatal("state not persisted")
	}
	if unitForConfig("/etc/hs2/config.json") != "hs2" || unitForConfig("/etc/hs2/de1.json") != "hs2-de1" {
		t.Fatal("unitForConfig")
	}
}

func TestAlertPanicDedupAndPSIAndCert(t *testing.T) {
	cfg := defaultAlertCfg()
	st := newAlertState()
	evalAlerts(cfg, st, healthySnap(alertT0, nil))
	out := "1762862400.123456 h hs2[12]: panic: runtime error: index out of range\n1762862400.2 h hs2[12]: goroutine 1 [running]:\n"
	pl := parsePanicLines(out)
	if len(pl) != 1 || !strings.HasPrefix(pl[0].Text, "panic:") {
		t.Fatalf("parsePanicLines: %+v", pl)
	}
	s := healthySnap(alertT0.Add(time.Minute), nil)
	s.Panics = pl
	if ms := evalAlerts(cfg, st, s); !has(ms, sevCrit, "panic") {
		t.Fatal("panic not reported")
	}
	s.Now = s.Now.Add(time.Minute) // same line still in the 2-min window
	if ms := evalAlerts(cfg, st, s); has(ms, sevCrit, "panic") {
		t.Fatal("panic reported twice")
	}
	if v := parsePSI60("some avg10=40.00 avg60=31.50 avg300=10.00 total=1\nfull avg10=0 avg60=0 avg300=0 total=0\n"); v != 31.5 {
		t.Fatalf("psi %v", v)
	}
	for i := 0; i < 3; i++ {
		s := healthySnap(alertT0.Add(time.Duration(5+i)*time.Minute), nil)
		s.PSI60 = 31.5
		s.CertDays = 6
		ms := evalAlerts(cfg, st, s)
		if i == 0 && !has(ms, sevWarn, "cert") {
			t.Fatal("cert < 10 days not reported")
		}
		if (i == 2) != has(ms, sevWarn, "psi") {
			t.Fatalf("psi needs 3 checks: i=%d %v", i, keys(ms))
		}
	}
	// cert unknown (dialing side, no file) keeps state, never alerts
	s = healthySnap(alertT0.Add(9*time.Minute), nil)
	s.PSI60 = 31.5
	if ms := evalAlerts(cfg, st, s); len(ms) != 0 {
		t.Fatalf("unknown cert changed state: %v", keys(ms))
	}
}

func TestAlertDailySummary(t *testing.T) {
	cfg := defaultAlertCfg()
	st := newAlertState()
	morning := time.Date(2026, 11, 11, 8, 0, 0, 0, time.Local)
	if dueSummary(cfg, st, morning) {
		t.Fatal("first run must only arm")
	}
	st.fill(morning)
	st.Day.PeakUsers, st.Day.PeakMbit, st.Day.Restarts = 120, 340, 1
	st.Day.HourSum["21"], st.Day.HourN["21"] = 300, 2
	st.Day.Cuts["10.0.0.1"] = 3
	if !dueSummary(cfg, st, morning.Add(65*time.Minute)) {
		t.Fatal("summary due at 09:05")
	}
	txt := takeSummary(cfg, st, morning.Add(65*time.Minute))
	for _, w := range []string{"120", "340", "21:00", "150", "10.0.0.1×3"} {
		if !strings.Contains(txt, w) {
			t.Fatalf("summary %q lacks %q", txt, w)
		}
	}
	if dueSummary(cfg, st, morning.Add(3*time.Hour)) {
		t.Fatal("summary twice a day")
	}
}

func TestAlertConfParse(t *testing.T) {
	c, err := parseAlertConf(`# hs2 alerts
bot_token = "123:ABC"
chat_id=-100200
proxy=socks5://127.0.0.1:1080
lang=en
summary_time=off
resend_min=10
rtt_factor=2
probe=tcp
unknown_key=x
`)
	if err != nil {
		t.Fatal(err)
	}
	if c.BotToken != "123:ABC" || c.ChatID != "-100200" || c.Proxy != "socks5://127.0.0.1:1080" || c.Lang != "en" ||
		c.SummaryAt != "off" || c.ResendMin != 10 || c.RTTFactor != 2 || c.Probe != "tcp" || c.PSIPct != 25 {
		t.Fatalf("parsed %+v", c)
	}
	for _, bad := range []string{"lang=de", "summary_time=25:00", "resend_min=x", "probe=icmp", "nokey"} {
		if _, err := parseAlertConf(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestAlertFormatFa(t *testing.T) {
	tx := alertText("fa")
	m := alertMsg{Key: "links", Sev: sevWarn, Text: tx.linksLow(3, 12)}
	got := formatAlert("ir1", "hs2", m)
	if got != "🟠 [ir1/hs2] لینک‌های فعال کم است: 3 از 12" {
		t.Fatalf("format: %q", got)
	}
	r := tx.resolved(tx.unitDown("hs2", "failed"), 12*time.Minute)
	if !strings.HasPrefix(r, "برطرف شد (12 دقیقه)") {
		t.Fatalf("resolved: %q", r)
	}
	if en := alertText("en").linksLow(3, 12); en != "serving links low: 3 of 12" {
		t.Fatal(en)
	}
}

func TestAlertTelegramFakeServer(t *testing.T) {
	const token = "987654:SECRET-token"
	var gotPath, gotChat, gotText string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		v, _ := url.ParseQuery(string(b))
		gotChat, gotText = v.Get("chat_id"), v.Get("text")
		if gotChat == "bad" {
			w.WriteHeader(401)
			w.Write([]byte(`{"ok":false,"description":"Unauthorized for ` + token + `"}`))
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	cfg := defaultAlertCfg()
	cfg.APIURL, cfg.BotToken, cfg.ChatID = srv.URL, token, "42"
	cfg.LogFile = filepath.Join(t.TempDir(), "alert.log")
	if err := sendTelegram(context.Background(), cfg, "سلام"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/bot"+token+"/sendMessage" || gotChat != "42" || gotText != "سلام" {
		t.Fatalf("server saw %q %q %q", gotPath, gotChat, gotText)
	}
	cfg.ChatID = "bad"
	err := sendTelegram(context.Background(), cfg, "x")
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("error must exist and hide the token: %v", err)
	}
	// unreachable endpoint: net/http puts the URL (with the token) in the error
	cfg.APIURL, cfg.ChatID = "http://127.0.0.1:1", "42"
	err = sendTelegram(context.Background(), cfg, "x")
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("transport error leaks the token: %v", err)
	}
	// via an HTTP proxy setting (the fake server acts as a forward proxy)
	cfg.APIURL, cfg.Proxy = "http://api.telegram.invalid", srv.URL
	if err := sendTelegram(context.Background(), cfg, "via proxy"); err != nil {
		t.Fatal(err)
	}
	if gotText != "via proxy" {
		t.Fatalf("proxy path not used: %q", gotText)
	}
}

func TestAlertAutoRestartBetweenChecks(t *testing.T) {
	cfg := defaultAlertCfg()
	st := newAlertState()
	evalAlerts(cfg, st, healthySnap(alertT0, nil))
	s := healthySnap(alertT0.Add(time.Minute), nil)
	s.MainPID, s.NRestarts, s.UptimeS = 101, 1, 20 // systemd restarted it between two checks
	ms := evalAlerts(cfg, st, s)
	if !has(ms, sevWarn, "restart") || !strings.Contains(ms[0].Text, "1") {
		t.Fatalf("auto-restart not reported: %v", ms)
	}
	if st.Day.Restarts != 1 {
		t.Fatal("daily restart count")
	}
}

// rev53c: a failed Telegram send (no route from Iran, proxy down, 429) used to
// drop the lines — a critical alert or its recovery was simply lost. Now they
// wait in the state and go first, stamped, at the next check.
func TestAlertTelegramFailureIsQueued(t *testing.T) {
	var mu sync.Mutex
	var got []string
	status := 429
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		v, _ := url.ParseQuery(string(b))
		mu.Lock()
		defer mu.Unlock()
		if status != 200 {
			w.WriteHeader(status)
			w.Write([]byte(`{"ok":false,"description":"Too Many Requests: retry after 5"}`))
			return
		}
		got = append(got, v.Get("text"))
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	cfg := defaultAlertCfg()
	cfg.APIURL, cfg.BotToken, cfg.ChatID = srv.URL, "T", "42"
	cfg.LogFile = filepath.Join(t.TempDir(), "alert.log")
	st := newAlertState()
	t1 := time.Date(2026, 11, 11, 3, 4, 0, 0, time.Local)
	if err := deliverQueued(cfg, "hs2", []alertMsg{{Key: "down", Sev: sevCrit, Text: "tunnel down"}}, st, t1); err == nil {
		t.Fatal("429 must be reported")
	}
	if len(st.Pending) != 1 || !strings.HasPrefix(st.Pending[0], "03:04 ") || !strings.Contains(st.Pending[0], "tunnel down") {
		t.Fatalf("failed line not kept with its time: %q", st.Pending)
	}
	// still failing next minute: both lines kept, the old one not stamped twice
	deliverQueued(cfg, "hs2", []alertMsg{{Key: "down", Sev: sevOK, Text: "tunnel back"}}, st, t1.Add(time.Minute))
	if len(st.Pending) != 2 || strings.HasPrefix(st.Pending[0], "03:04 03:04") || !strings.HasPrefix(st.Pending[1], "03:05 ") {
		t.Fatalf("pending after 2nd failure: %q", st.Pending)
	}
	mu.Lock()
	status = 200
	mu.Unlock()
	if err := deliverQueued(cfg, "hs2", nil, st, t1.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(st.Pending) != 0 || len(got) != 1 || !strings.Contains(got[0], "tunnel down") || !strings.Contains(got[0], "tunnel back") {
		t.Fatalf("queued lines not delivered once recovered: pending %q got %q", st.Pending, got)
	}
	// the queue is capped (Telegram blocked for days)
	mu.Lock()
	status = 502
	mu.Unlock()
	for i := 0; i < 100; i++ {
		deliverQueued(cfg, "hs2", []alertMsg{{Key: "x", Sev: sevWarn, Text: fmt.Sprint("flap ", i)}}, st, t1)
	}
	if len(st.Pending) != alertPendingMax || !strings.Contains(st.Pending[alertPendingMax-1], "flap 99") {
		t.Fatalf("queue not capped to the newest %d: %d", alertPendingMax, len(st.Pending))
	}
}

// rev53c: many messages at once (a storm on a server with 7 paths) went out as
// one text over Telegram's 4096-character limit and were refused (HTTP 400).
func TestAlertLongMessageIsSplit(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		v, _ := url.ParseQuery(string(b))
		if n := len([]rune(v.Get("text"))); n > 4096 {
			w.WriteHeader(400)
			w.Write([]byte(`{"ok":false,"description":"Bad Request: message is too long"}`))
			return
		}
		got = append(got, v.Get("text"))
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	cfg := defaultAlertCfg()
	cfg.APIURL, cfg.BotToken, cfg.ChatID = srv.URL, "T", "42"
	cfg.LogFile = filepath.Join(t.TempDir(), "alert.log")
	var ms []alertMsg
	for i := 0; i < 60; i++ {
		ms = append(ms, alertMsg{Key: fmt.Sprint(i), Sev: sevWarn, Text: fmt.Sprintf("مسیر %d قطع شد — %s", i, strings.Repeat("x", 120))})
	}
	ms = append(ms, alertMsg{Key: "huge", Sev: sevWarn, Text: strings.Repeat("y", 9000)})
	if err := deliver(cfg, "hs2", ms); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(got, "\n")
	if len(got) < 3 || !strings.Contains(all, "مسیر 0 ") || !strings.Contains(all, "مسیر 59 ") || !strings.Contains(all, "yyy…") {
		t.Fatalf("split lost lines: %d chunks", len(got))
	}
}

// rev53c: the alert log had no rotation and grew for ever.
func TestAlertLogRotates(t *testing.T) {
	old := alertLogMax
	alertLogMax = 1000
	defer func() { alertLogMax = old }()
	cfg := defaultAlertCfg()
	cfg.LogFile = filepath.Join(t.TempDir(), "alert.log")
	for i := 0; i < 200; i++ {
		writeAlertLog(cfg, "hs2", strings.Repeat("z", 50))
	}
	a, err1 := os.Stat(cfg.LogFile)
	b, err2 := os.Stat(cfg.LogFile + ".1")
	if err1 != nil || err2 != nil || a.Size() > 1100 || b.Size() > 1100 {
		t.Fatalf("log not rotated: %v %v", err1, err2)
	}
}
