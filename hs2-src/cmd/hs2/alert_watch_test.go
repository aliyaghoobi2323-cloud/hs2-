package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// b352 watchdog rules: fake snapshots in, messages out (hysteresis, one
// alert per incident, one recovery).

func wSnap(now time.Time, users int) alertSnap {
	s := healthySnap(now, &alertStatus{Links: 40, Target: 40, Users: users, Phase: "steady", Updated: now.Unix()})
	s.W = watchSnap{StatusAgeS: 2, DiskPct: 40, DiskPath: "/", MemPct: 50, SubDied: 0}
	return s
}

// run: n checks one minute apart from t0, mutate edits each snapshot; all msgs.
func wRun(cfg alertCfg, st *alertState, t0 time.Time, n int, mutate func(i int, s *alertSnap)) (time.Time, [][]alertMsg) {
	var all [][]alertMsg
	now := t0
	for i := 0; i < n; i++ {
		s := wSnap(now, 1000)
		if mutate != nil {
			mutate(i, &s)
		}
		all = append(all, evalAlerts(cfg, st, s))
		now = now.Add(time.Minute)
	}
	return now, all
}

func countKey(all [][]alertMsg, sev, key string) int {
	n := 0
	for _, ms := range all {
		for _, m := range ms {
			if m.Sev == sev && m.Key == key {
				n++
			}
		}
	}
	return n
}

// at 12:20 UTC: outside the default 00-06 skip window
var wT0 = time.Date(2026, 11, 18, 12, 20, 0, 0, time.UTC)

func TestWatchStaleStatusHysteresis(t *testing.T) {
	cfg := defaultAlertCfg()
	st := newAlertState()
	_, all := wRun(cfg, st, wT0, 6, func(i int, s *alertSnap) {
		if i >= 2 && i <= 3 {
			s.W.StatusAgeS = 200
		}
	})
	if !has(all[3], sevWarn, "stale") || has(all[2], sevWarn, "stale") {
		t.Fatalf("stale wants 2 checks: %v / %v", keys(all[2]), keys(all[3]))
	}
	if !has(all[4], sevOK, "stale") || countKey(all, sevWarn, "stale") != 1 {
		t.Fatalf("one alert + one recovery: %v", all)
	}
	// missing file while active counts too; during settling it does not
	st = newAlertState()
	_, all = wRun(cfg, st, wT0, 4, func(i int, s *alertSnap) { s.W.StatusAgeS = -2; s.UptimeS = 30 })
	if countKey(all, sevWarn, "stale") != 0 {
		t.Fatal("settling: no stale alert")
	}
}

func TestWatchPathLoss(t *testing.T) {
	cfg := defaultAlertCfg()
	st := newAlertState()
	hi, lo := 9.0, 0.5
	_, all := wRun(cfg, st, wT0, 7, func(i int, s *alertSnap) {
		l := &lo
		if i >= 1 && i <= 4 {
			l = &hi
		}
		s.Status.Paths = []alertPath{{Local: "1.1.1.1", Remote: "2.2.2.2", Target: 10, Links: 10, Serving: 10, State: "ok", LossDown: l}}
	})
	if countKey(all, sevWarn, "loss:1.1.1.1>2.2.2.2") != 1 || !has(all[3], sevWarn, "loss:1.1.1.1>2.2.2.2") {
		t.Fatalf("loss after 3 checks, once: %v", all)
	}
	if countKey(all, sevOK, "loss:1.1.1.1>2.2.2.2") != 1 {
		t.Fatal("loss recovery missing")
	}
}

func TestWatchDiskMemCPU(t *testing.T) {
	cfg := defaultAlertCfg()
	st := newAlertState()
	var cpu cpuTimes
	cpu.cores = 4
	_, all := wRun(cfg, st, wT0, 14, func(i int, s *alertSnap) {
		if i >= 1 {
			s.W.DiskPct = 98 // crit (>= 95)
			s.W.MemPct = 95
		}
		// 97 % busy each minute: idle 3 % < 10 %
		cpu.user += 97 * 60 * 4
		cpu.idle += 3 * 60 * 4
		s.W.CPU = cpu
	})
	if !has(all[2], sevCrit, "disk") || countKey(all, sevCrit, "disk") != 1 {
		t.Fatalf("disk crit after 2 checks: %v", all)
	}
	if !has(all[3], sevWarn, "mem") || countKey(all, sevWarn, "mem") != 1 {
		t.Fatalf("mem after 3: %v", all)
	}
	// first sample only arms; then 10 bad checks -> alert on index 10
	if !has(all[10], sevWarn, "cpuidle") || countKey(all, sevWarn, "cpuidle") != 1 {
		t.Fatalf("cpuidle after 10 checks: %v", all)
	}
	if !strings.Contains(textOf(all[10], "cpuidle"), "4 هسته") {
		t.Fatalf("cpu text: %q", textOf(all[10], "cpuidle"))
	}
	// unknown (-1) never alerts
	st = newAlertState()
	_, all = wRun(cfg, st, wT0, 4, func(i int, s *alertSnap) { s.W.DiskPct, s.W.MemPct = -1, -1 })
	if countKey(all, sevCrit, "disk")+countKey(all, sevWarn, "mem") != 0 {
		t.Fatal("unknown disk/mem alerted")
	}
}

func textOf(ms []alertMsg, key string) string {
	for _, m := range ms {
		if m.Key == key {
			return m.Text
		}
	}
	return ""
}

func TestWatchUsersDrop(t *testing.T) {
	cfg := defaultAlertCfg()
	st := newAlertState()
	users := []int{1000, 1000, 1000, 300, 300, 320, 400, 900, 950, 950}
	_, all := wRun(cfg, st, wT0, len(users), func(i int, s *alertSnap) { s.Status.Users = users[i] })
	if !has(all[4], sevWarn, "usersdrop") || countKey(all, sevWarn, "usersdrop") != 1 {
		t.Fatalf("drop: alert on the 2nd low check, once: %v", all)
	}
	if !strings.Contains(textOf(all[4], "usersdrop"), "1000") {
		t.Fatalf("text keeps where it fell from: %q", textOf(all[4], "usersdrop"))
	}
	if !has(all[7], sevOK, "usersdrop") || countKey(all, sevOK, "usersdrop") != 1 {
		t.Fatalf("recovery at >= 70 %%: %v", all)
	}
	// the hourly xray restart (HH:01 UTC) is not an incident
	st = newAlertState()
	_, all = wRun(cfg, st, time.Date(2026, 11, 18, 13, 0, 0, 0, time.UTC), len(users), func(i int, s *alertSnap) { s.Status.Users = users[i] })
	if countKey(all, sevWarn, "usersdrop") != 0 {
		t.Fatalf("skip window: %v", all)
	}
	// a slow decline is not a drop; small counts are ignored
	st = newAlertState()
	_, all = wRun(cfg, st, wT0, 20, func(i int, s *alertSnap) { s.Status.Users = 1000 - 40*i })
	if countKey(all, sevWarn, "usersdrop") != 0 {
		t.Fatal("slow decline alerted")
	}
	st = newAlertState()
	_, all = wRun(cfg, st, wT0, 6, func(i int, s *alertSnap) { s.Status.Users = []int{40, 40, 2, 2, 2, 2}[i] })
	if countKey(all, sevWarn, "usersdrop") != 0 {
		t.Fatal("tiny count alerted")
	}
}

func TestWatchSubDiedBurst(t *testing.T) {
	cfg := defaultAlertCfg()
	st := newAlertState()
	_, all := wRun(cfg, st, wT0, 18, func(i int, s *alertSnap) {
		s.W.SubDied = 6
		if i >= 12 && i <= 14 {
			s.W.SubDied = 200
		}
	})
	if !has(all[13], sevWarn, "subdied") || countKey(all, sevWarn, "subdied") != 1 || countKey(all, sevOK, "subdied") != 1 {
		t.Fatalf("burst: %v", all)
	}
	if m, _ := median(intsF(st.Watch.SubDied)); m != 6 {
		t.Fatalf("burst raised the baseline: %v", st.Watch.SubDied)
	}
}

func TestWatchNewErrorKinds(t *testing.T) {
	cfg := defaultAlertCfg()
	st := newAlertState()
	line := func(ts time.Time, txt string) panicLine { return panicLine{TS: ts.UnixMicro(), Text: txt} }
	now := wT0
	// learning hour: kinds are recorded, nothing is sent
	for i := 0; i < 61; i++ {
		s := wSnap(now, 1000)
		s.W.ErrLines = []panicLine{line(now, "dial 10.0.0.7:443 failed: i/o timeout after 3001 ms")}
		if ms := evalAlerts(cfg, st, s); has(ms, sevInfo, "newerr") {
			t.Fatalf("learning minute %d sent newerr", i)
		}
		now = now.Add(time.Minute)
	}
	s := wSnap(now, 1000)
	s.W.ErrLines = []panicLine{
		line(now, "dial 10.9.9.9:8443 failed: i/o timeout after 12 ms"), // same kind, other numbers
		line(now, "mconn: write record error: broken pipe"),             // new
	}
	ms := evalAlerts(cfg, st, s)
	if !has(ms, sevInfo, "newerr") || strings.Contains(textOf(ms, "newerr"), "dial") {
		t.Fatalf("want one new kind (not the dial one): %v", ms)
	}
	// the same line again (journal window overlap) is not new
	s.Now = now.Add(time.Minute)
	if ms := evalAlerts(cfg, st, s); has(ms, sevInfo, "newerr") {
		t.Fatal("overlap re-reported")
	}
}

func TestWatchJournalParse(t *testing.T) {
	j := "1763467200.100000 host hs2[12]: mconn 5: every sub died\n" +
		"1763467201.200000 host hs2[12]: lb: dial 1.2.3.4:443 failed: refused\n" +
		"1763467202.300000 host hs2[12]: links 40 users 900\n" +
		"1763467203.400000 host hs2[12]: panic: boom\n"
	n, errs := parseJournalWatch(j)
	if n != 1 || len(errs) != 1 || !strings.HasPrefix(errs[0].Text, "lb: dial") {
		t.Fatalf("got %d %v", n, errs)
	}
	if n, _ := parseJournalWatch(""); n != -1 {
		t.Fatal("empty journal is unknown")
	}
	if errSig("dial 1.2.3.4:443 failed after 30 ms") != errSig("dial 5.6.7.8:8443 failed after 7 ms") {
		t.Fatal("signature keeps numbers")
	}
	v := parseMemInfo(strings.NewReader("MemTotal: 1000 kB\nMemFree: 10 kB\nMemAvailable: 250 kB\n"))
	if v != 75 {
		t.Fatalf("mem %v", v)
	}
	var u int64
	if !jsonUnmarshalUpdated([]byte(`{"role":"x","updated": 1763467200,"links":3}`), &u) || u != 1763467200 {
		t.Fatalf("updated %d", u)
	}
}

func TestWatchTunnelProbe(t *testing.T) {
	cfg := defaultAlertCfg()
	cfg.ProbeTarget = "127.0.0.1:1"
	st := newAlertState()
	// 70 min of a 40 ms baseline (one base sample per 5 min -> 14)
	now, all := wRun(cfg, st, wT0, 70, func(i int, s *alertSnap) { s.W.TProbe = []float64{40, 41, 39} })
	if countKey(all, sevWarn, "tprobeslow") != 0 {
		t.Fatal("healthy probe alerted")
	}
	// 90 ms (2.25x): warn after 3, no crit; then 150 ms (3.75x) 5 checks: crit
	_, all = wRun(cfg, st, now, 12, func(i int, s *alertSnap) {
		v := 90.0
		if i >= 4 {
			v = 150
		}
		s.W.TProbe = []float64{v, v, v}
	})
	if countKey(all, sevWarn, "tprobeslow") != 1 || !has(all[2], sevWarn, "tprobeslow") {
		t.Fatalf("slow warn: %v", all)
	}
	if countKey(all, sevCrit, "tprobex3") != 1 {
		t.Fatalf("x3 crit: %v", all)
	}
	if b, _ := median(st.Watch.TProbeBase); b > 41 {
		t.Fatalf("baseline poisoned: %v", b)
	}
	// failures: > 20 % of the last 10 checks -> crit, once; back -> resolved
	st = newAlertState()
	_, all = wRun(cfg, st, wT0, 25, func(i int, s *alertSnap) {
		s.W.TProbe = []float64{40, 40, 40}
		if i >= 5 && i < 12 {
			s.W.TProbe = []float64{-1, -1, -1}
		}
	})
	if countKey(all, sevCrit, "tprobe") != 1 || countKey(all, sevOK, "tprobe") != 1 {
		t.Fatalf("probe loss: %v", all)
	}
}

func TestWatchPeerProbe(t *testing.T) {
	cfg := defaultAlertCfg()
	st := newAlertState()
	bad, good := "connect failed", ""
	_, all := wRun(cfg, st, wT0, 8, func(i int, s *alertSnap) {
		s.W.PeerErr = &good
		if i >= 1 && i <= 4 {
			s.W.PeerErr = &bad
		}
	})
	if !has(all[3], sevCrit, "peer") || countKey(all, sevCrit, "peer") != 1 || countKey(all, sevOK, "peer") != 1 {
		t.Fatalf("peer: %v", all)
	}
}

func TestWatchConfKeys(t *testing.T) {
	c, err := parseAlertConf("probe_target=10.10.0.1:80\npeer_probe=5.6.7.8:443\nusers_drop_skip=58-60\nrollback=dry\ndisk_pct=85\nnew_errors=off\n")
	if err != nil || c.ProbeTarget != "10.10.0.1:80" || c.PeerProbe != "5.6.7.8:443" || c.DropSkip != "58-60" ||
		c.Rollback != "dry" || c.DiskPct != 85 || c.NewErrors {
		t.Fatalf("%v %+v", err, c.watchCfg)
	}
	c, err = parseAlertConf("rollback=yes\nprobe_target=nohost\ndisk_pct=200\nchat_id=123456\n")
	if err == nil || c.Rollback != "off" || c.ProbeTarget != "" || c.DiskPct != 90 || c.ChatID != "123456" {
		t.Fatalf("bad values keep defaults, rest read: %v %+v", err, c.watchCfg)
	}
	if !inSkip(defaultWatchCfg(), time.Date(2026, 1, 1, 7, 3, 0, 0, time.UTC)) || inSkip(defaultWatchCfg(), time.Date(2026, 1, 1, 7, 6, 0, 0, time.UTC)) {
		t.Fatal("skip window")
	}
	// Iran time (+03:30) is judged in UTC minutes
	ir := time.FixedZone("IRST", 3*3600+1800)
	if !inSkip(defaultWatchCfg(), time.Date(2026, 1, 1, 10, 32, 0, 0, ir)) {
		t.Fatal("skip window must use UTC minutes")
	}
}

func TestWatchRollbackPlanAndApply(t *testing.T) {
	now := wT0
	w := defaultWatchCfg()
	st := newAlertState()
	st.fill(now)
	panicMsg := []alertMsg{{Key: "panic", Sev: sevCrit}}
	if do, _ := rollbackPlan(w, st, panicMsg, now.Add(-time.Hour).Unix(), true, now); do {
		t.Fatal("default off must never roll back")
	}
	w.Rollback, w.RollbackDropin = "on", "/x/exec.conf"
	if do, why := rollbackPlan(w, st, panicMsg, now.Add(-time.Hour).Unix(), true, now); !do || why != "panic" {
		t.Fatal("young release + panic: roll back")
	}
	if do, _ := rollbackPlan(w, st, panicMsg, now.Add(-7*time.Hour).Unix(), true, now); do {
		t.Fatal("old release: never")
	}
	if do, _ := rollbackPlan(w, st, panicMsg, now.Add(-time.Hour).Unix(), false, now); do {
		t.Fatal("no .prev: never")
	}
	if do, _ := rollbackPlan(w, st, nil, now.Add(-time.Hour).Unix(), true, now); do {
		t.Fatal("no severe alert: never")
	}
	st.Active["tprobex3"] = &activeAlert{Sev: sevCrit, Since: now.Unix()}
	if do, _ := rollbackPlan(w, st, nil, now.Add(-time.Hour).Unix(), true, now); !do {
		t.Fatal("probe x3: roll back")
	}
	// dry: one message per release, files untouched
	dir := t.TempDir()
	d := filepath.Join(dir, "exec.conf")
	os.WriteFile(d, []byte("new"), 0o644)
	os.WriteFile(d+".prev", []byte("old"), 0o644)
	cfg := defaultAlertCfg()
	cfg.Rollback, cfg.RollbackDropin = "dry", d
	m := applyRollback(cfg, st, "hs2", panicMsg, time.Now(), false)
	if m == nil || m.Key != "rollback" || m.Sev != sevWarn {
		t.Fatalf("dry message: %+v", m)
	}
	if m := applyRollback(cfg, st, "hs2", panicMsg, time.Now(), false); m != nil {
		t.Fatal("dry: once per release")
	}
	if b, _ := os.ReadFile(d); string(b) != "new" {
		t.Fatal("dry changed files")
	}
}

// BenchmarkWatchCheck: the CPU of one full check's rule work on a busy
// server: a 40-path status file decoded, a 2-minute journal window of 3000
// lines parsed, every rule evaluated, the state encoded.
func BenchmarkWatchCheck(b *testing.B) {
	cfg := defaultAlertCfg()
	cfg.ProbeTarget = "x:1"
	st := newAlertState()
	var paths []alertPath
	for i := 0; i < 40; i++ {
		l := 0.3
		paths = append(paths, alertPath{Local: "10.0.0." + string(rune('a'+i%26)), Remote: "5.5.5.5", Target: 10, Links: 10, Serving: 10, State: "ok", LossDown: &l, RTTp50: 90, RTTp99: 140})
	}
	ls := &alertStatus{Links: 400, Target: 400, Users: 15000, Phase: "steady", Paths: paths}
	sb, _ := jsonMarshal(ls)
	var jb strings.Builder
	for i := 0; i < 3000; i++ {
		switch i % 10 {
		case 0:
			jb.WriteString("1763467200.100000 host hs2[12]: mconn 77: every sub died\n")
		case 1:
			jb.WriteString("1763467200.200000 host hs2[12]: lb: dial 1.2.3.4:443 failed: i/o timeout\n")
		default:
			jb.WriteString("1763467200.300000 host hs2[12]: lb: path 1.2.3.4>5.6.7.8 links 10 serving 10 rtt 91 ms\n")
		}
	}
	journal := jb.String()
	now := wT0
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		now = now.Add(time.Minute)
		s := alertSnap{Now: now, Unit: "hs2", Active: "active", MainPID: 1, UptimeS: 3600, PSI60: 1, CertDays: -1}
		s.Panics = parsePanicLines(journal)
		s.Status = parseAlertStatus(sb, now)
		s.Status.Updated = now.Unix()
		s.W = watchSnap{StatusAgeS: 1, DiskPct: 40, MemPct: 50, TProbe: []float64{40, 41, 42}}
		s.W.SubDied, s.W.ErrLines = parseJournalWatch(journal)
		_ = evalAlerts(cfg, st, s)
		_, _ = jsonMarshal(st)
	}
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// wd365 miss: a path drained by the LB to target 0 / links 0 while dark.
func darkPaths(p0 alertPath) []alertPath {
	f := 0.3
	ok := func(l, r string) alertPath {
		return alertPath{Local: l, Remote: r, Target: 40, Links: 40, Serving: 40, State: "ok", AutoReason: "ok", LossDown: &f}
	}
	return []alertPath{p0, ok("192.168.52.1", "192.168.50.2"), ok("192.168.53.1", "192.168.50.2")}
}

const darkKey = "dark:192.168.50.1>192.168.50.2"

func TestWatchDarkDrainedPath(t *testing.T) {
	cfg := defaultAlertCfg()
	st := newAlertState()
	hi, lo := 99.92, 0.2
	// 0-1 serving; 2-6 drained (target 0, links 0) at 99.92 %; 7 back with links
	_, all := wRun(cfg, st, wT0, 9, func(i int, s *alertSnap) {
		p := alertPath{Local: "192.168.50.1", Remote: "192.168.50.2", Target: 40, Links: 40, Serving: 40, State: "ok", AutoReason: "ok", LossDown: &lo}
		if i >= 2 && i <= 6 {
			p.Target, p.Links, p.Serving, p.LossDown, p.AutoReason = 0, 0, 0, &hi, "lower: loss 99.9%"
			if i >= 3 {
				p.Remote = "" // the listening side: no links, no remote (lab wd365b)
			}
		}
		s.Status.Paths = darkPaths(p)
	})
	if countKey(all, sevCrit, darkKey) != 1 || !has(all[4], sevCrit, darkKey) {
		t.Fatalf("dark wants 3 drained+lossy checks, once: %v", all)
	}
	if !has(all[7], sevOK, darkKey) || countKey(all, sevOK, darkKey) != 1 {
		t.Fatalf("dark must resolve when links come back: %v", all)
	}
	for _, ms := range all {
		for _, m := range ms {
			if strings.HasPrefix(m.Key, "loss:") || strings.HasPrefix(m.Key, "ipcut:") {
				t.Fatalf("unexpected %s", m.Key)
			}
		}
	}
	if m := all[4][0].Text; !strings.Contains(m, "192.168.50.1>192.168.50.2") || !strings.Contains(m, "100%") {
		t.Fatalf("text: %q", m)
	}
}

// lab wd365b: links stay 0 while the target flips 0/5/10/41 (cut() on and
// off), the loss figure frozen at 99.53 — the dark streak must still count.
func TestWatchDarkTargetFlips(t *testing.T) {
	cfg := defaultAlertCfg()
	st := newAlertState()
	hi, lo := 99.53, 0.1
	tg := []int{40, 5, 0, 10, 41, 0, 0, 40}
	_, all := wRun(cfg, st, wT0, len(tg), func(i int, s *alertSnap) {
		p := alertPath{Local: "192.168.50.1", Remote: "", Target: tg[i], Links: 0, State: "ok", LossDown: &hi}
		if i == 0 || i == 7 {
			p.Remote, p.Links, p.LossDown = "192.168.50.2", 40, &lo
		}
		s.Status.Paths = darkPaths(p)
	})
	if !has(all[3], sevCrit, darkKey) || countKey(all, sevCrit, darkKey) != 1 || !has(all[7], sevOK, darkKey) {
		t.Fatalf("dark with a flipping target: %v", all)
	}
}

func TestWatchDarkResolveHysteresis(t *testing.T) {
	cfg := defaultAlertCfg()
	st := newAlertState()
	// drained the whole time; loss 90 -> 30 (still dark: holds above 20) -> 5 (resolves)
	vals := []float64{0.2, 90, 90, 90, 30, 30, 5, 5}
	_, all := wRun(cfg, st, wT0, len(vals), func(i int, s *alertSnap) {
		l := vals[i]
		p := alertPath{Local: "192.168.50.1", Remote: "192.168.50.2", State: "ok", LossDown: &l}
		if i == 0 {
			p.Target, p.Links = 40, 40
		}
		s.Status.Paths = darkPaths(p)
	})
	if !has(all[3], sevCrit, darkKey) || has(all[4], sevOK, darkKey) || has(all[5], sevOK, darkKey) || !has(all[6], sevOK, darkKey) {
		t.Fatalf("dark hysteresis: %v", all)
	}
}

func TestWatchDarkQuiet(t *testing.T) {
	hi, lo := 99.0, 0.4
	cases := []struct {
		name string
		pre  bool // the path served links before
		p    alertPath
	}{
		// drained but its (fresh) loss is low: a healthy drain / share set to 0
		{"lowloss", true, alertPath{Target: 0, Links: 0, State: "ok", AutoReason: "ok", LossDown: &lo}},
		// never had links (config spare), even with a high figure
		{"neverused", false, alertPath{Target: 0, Links: 0, State: "ok", LossDown: &hi}},
		// no loss figure and no verdict: no evidence
		{"noloss", true, alertPath{Target: 0, Links: 0, State: "ok"}},
	}
	for _, c := range cases {
		cfg := defaultAlertCfg()
		st := newAlertState()
		_, all := wRun(cfg, st, wT0, 8, func(i int, s *alertSnap) {
			p := c.p
			p.Local, p.Remote = "192.168.50.1", "192.168.50.2"
			if c.pre && i == 0 {
				p.Target, p.Links, p.LossDown = 40, 40, &lo
			}
			s.Status.Paths = darkPaths(p)
		})
		for _, ms := range all {
			if len(ms) > 0 {
				t.Fatalf("%s: unexpected %v", c.name, ms)
			}
		}
	}
}

func TestWatchDarkCutVerdictAndStale(t *testing.T) {
	cfg := defaultAlertCfg()
	st := newAlertState()
	lo := 0.4
	// drained with a "cut" verdict (smoothed loss not high yet) fires; then the
	// status goes stale for 3 checks (no judging: neither resolves nor repeats)
	// while the path shows links again; fresh again with links -> resolves.
	_, all := wRun(cfg, st, wT0, 9, func(i int, s *alertSnap) {
		p := alertPath{Local: "192.168.50.1", Remote: "192.168.50.2", Target: 40, Links: 40, State: "ok", LossDown: &lo}
		switch {
		case i >= 1 && i <= 3:
			p.Target, p.Links, p.AutoReason = 1, 0, "cut: loss 100.0%"
		case i >= 4 && i <= 6:
			s.W.StatusAgeS = 300
		}
		s.Status.Paths = darkPaths(p)
	})
	if !has(all[3], sevCrit, darkKey) {
		t.Fatalf("cut verdict on a drained path: %v", all)
	}
	for i := 4; i <= 6; i++ {
		if has(all[i], sevOK, darkKey) {
			t.Fatalf("stale status resolved the dark alert at %d: %v", i, all[i])
		}
	}
	if !has(all[7], sevOK, darkKey) {
		t.Fatalf("resolve after fresh status: %v", all)
	}
}

func TestWatchDarkPathRemovedAndIPCut(t *testing.T) {
	cfg := defaultAlertCfg()
	st := newAlertState()
	hi, lo := 99.0, 0.4
	// dark alert fires, then the path leaves the config: resolves as gone
	_, all := wRun(cfg, st, wT0, 6, func(i int, s *alertSnap) {
		p := alertPath{Local: "192.168.50.1", Remote: "192.168.50.2", Target: 40, Links: 40, State: "ok", LossDown: &lo}
		if i >= 1 {
			p.Target, p.Links, p.LossDown = 0, 0, &hi
		}
		ps := darkPaths(p)
		if i >= 5 {
			ps = ps[1:]
		}
		s.Status.Paths = ps
	})
	if !has(all[3], sevCrit, darkKey) || !has(all[5], sevOK, darkKey) {
		t.Fatalf("removed path: %v", all)
	}
	if _, ok := st.Watch.PathUsed["192.168.50.1>192.168.50.2"]; ok {
		t.Fatal("removed path still remembered")
	}
	// every path of the IP cut (cooling): ipcut reports it, dark stays quiet
	st = newAlertState()
	_, all = wRun(cfg, st, wT0, 6, func(i int, s *alertSnap) {
		p := alertPath{Local: "10.0.0.1", Remote: "10.0.0.2", Target: 40, Links: 40, State: "ok", LossDown: &lo}
		if i >= 1 {
			p.Links, p.State, p.LossDown = 0, "cooling", &hi
		}
		s.Status.Paths = []alertPath{p}
	})
	if countKey(all, sevCrit, "dark:10.0.0.1>10.0.0.2") != 0 || countKey(all, sevCrit, "ipcut:10.0.0.1") != 1 {
		t.Fatalf("ipcut owns a fully cut IP: %v", all)
	}
}
