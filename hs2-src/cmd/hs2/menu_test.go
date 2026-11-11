package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// menuEnv points every M1 path into a temp dir.
func menuEnv(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	saved := []*string{&historyRoot, &hs2CfgDir, &systemdDir, &applyStateDir, &auditLogPath, &adminLockPath}
	old := make([]string, len(saved))
	for i, p := range saved {
		old[i] = *p
	}
	historyRoot = filepath.Join(d, "history")
	hs2CfgDir = filepath.Join(d, "etc")
	systemdDir = filepath.Join(d, "systemd")
	applyStateDir = filepath.Join(d, "apply")
	auditLogPath = filepath.Join(d, "changes.log")
	adminLockPath = filepath.Join(d, "run", "admin.lock")
	os.MkdirAll(hs2CfgDir, 0o700)
	ow, os_, og := healthWindow, healthStable, historyKeep
	t.Cleanup(func() {
		for i, p := range saved {
			*p = old[i]
		}
		healthWindow, healthStable, historyKeep = ow, os_, og
	})
	return d
}

func TestHistoryRotationAndDedup(t *testing.T) {
	menuEnv(t)
	historyKeep = 5
	now := time.Date(2026, 11, 11, 1, 0, 0, 0, time.Local)
	for i := 0; i < 8; i++ {
		if _, err := saveVersion("hs2-de1", []byte(`{"mtu":`+string(rune('0'+i))+`}`), nil, "t est/"+string(rune('a'+i)), now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	vs, _ := listVersions("hs2-de1.service")
	if len(vs) != 5 || vs[0].N != 4 || vs[4].N != 8 {
		t.Fatalf("rotation: %+v", vs)
	}
	if strings.ContainsAny(vs[0].Reason, " /") {
		t.Fatalf("unsafe reason kept: %q", vs[0].Reason)
	}
	if _, err := os.Stat(vs[0].dropinPath()); err != nil {
		t.Fatal("drop-in snapshot missing")
	}
	// identical content reuses the newest version
	v, _ := saveVersion("hs2-de1", []byte(`{"mtu":7}`), nil, "again", now.Add(time.Minute))
	if v.N != 8 {
		t.Fatalf("dedup: got v%d", v.N)
	}
	if configForUnit("hs2") != filepath.Join(hs2CfgDir, "config.json") || configForUnit("hs2-de1.service") != filepath.Join(hs2CfgDir, "de1.json") {
		t.Fatal("configForUnit")
	}
}

func TestSemanticDiffBilateral(t *testing.T) {
	old := []byte(`{"mode":"dial","shared_key":"aa","max_links":8,"addr":"1.2.3.4:443","lb":{"enabled":true,"iran":[{"ip":"10.0.0.1","weight":50}]}}`)
	nw := []byte(`{"mode":"dial","shared_key":"bb","max_links":16,"addr":"1.2.3.5:443","lb":{"enabled":true,"iran":[{"ip":"10.0.0.1","weight":60}]},"sni":"x.com"}`)
	odi := map[string]string{"env.conf": "[Service]\nEnvironment=HS2_TUNE_CC=bbr \"GOGC=100\"\n"}
	ndi := map[string]string{"env.conf": "[Service]\nEnvironment=HS2_TUNE_CC=cubic GOGC=100\nNice=-5\n"}
	d, err := semanticDiff(old, nw, odi, ndi)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]diffEntry{}
	for _, e := range d {
		got[e.Path] = e
	}
	for _, k := range []string{"shared_key", "lb.iran.0.weight", "sni"} {
		if !got[k].Bilateral {
			t.Errorf("%s should be bilateral", k)
		}
	}
	if got["max_links"].Bilateral || got["addr"].Bilateral {
		t.Error("max_links / addr (same port) must not be bilateral")
	}
	if _, ok := got["GOGC"]; ok {
		t.Error("unchanged env key reported")
	}
	if e := got["HS2_TUNE_CC"]; e.Old != "bbr" || e.New != "cubic" || e.Source != "dropin:env.conf" {
		t.Errorf("env diff: %+v", e)
	}
	if _, ok := got["[Service]Nice"]; !ok {
		t.Error("drop-in key Nice missing")
	}
	if s := got["shared_key"].String(); strings.Contains(s, "bb") {
		t.Errorf("secret printed: %s", s)
	}
	d2, _ := semanticDiff([]byte(`{"addr":"0.0.0.0:443"}`), []byte(`{"addr":"0.0.0.0:8443"}`), nil, nil)
	if len(d2) != 1 || !d2[0].Bilateral {
		t.Error("addr port change must be bilateral")
	}
	if b := bilateralOf(d); strings.Join(b, ",") != "lb,sni,shared_key" && len(b) != 3 {
		t.Errorf("bilateralOf: %v", b)
	}
}

func TestCrossCheck(t *testing.T) {
	menuEnv(t)
	cert := filepath.Join(t.TempDir(), "c.pem")
	os.WriteFile(cert, []byte("x"), 0o600)
	h := validateHost{
		localIP:   func(ip net.IP) bool { return ip.String() == "10.0.0.1" },
		listening: func() []sockListen { return []sockListen{{net.IPv4zero, 2222, false}, {net.IPv4zero, 8080, false}} },
		otherCfgs: func(string) map[string][]byte {
			return map[string][]byte{"/etc/hs2/b.json": []byte(`{"mode":"dial","forward_ports":"443,9000"}`)}
		},
	}
	raw := []byte(`{"mode":"dial","forward_ports":"443,2222,8080","bind_local_ip":"10.0.0.9","cert_file":"` + filepath.ToSlash(cert) + `","key_file":"/nope/k.pem",
	  "lb":{"enabled":true,"iran":[{"ip":"10.0.0.1","weight":30},{"ip":"10.0.0.7","weight":30}]}}`)
	cur := []byte(`{"mode":"dial","forward_ports":"8080"}`)
	errs, warns := crossCheck(raw, cur, "/etc/hs2/a.json", h)
	all := strings.Join(errs, "\n")
	for _, want := range []string{"port 443 is also used", "port 2222 is already in use", "bind_local_ip: 10.0.0.9", "lb.1: 10.0.0.7", "key_file: cannot read"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
	if strings.Contains(all, "8080") || strings.Contains(all, "cert_file") || strings.Contains(all, "lb.0") {
		t.Errorf("false positive:\n%s", all)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "sum to 60") {
		t.Errorf("weights warn: %v", warns)
	}
}

// fakeOps is a scripted host; the worker runs in its own goroutine like the
// detached systemd-run service (the caller never waits for it).
type fakeOps struct {
	mu        sync.Mutex
	t         time.Time
	restarts  int
	reloads   int
	isActive  func(restarts int) bool
	panic     bool
	link      int
	spawned   []string
	timers    []string
	stopped   []string
	runWorker bool // spawn starts the worker (systemd) — false = it is started later by the test
}

func (f *fakeOps) restart(string) error { f.mu.Lock(); f.restarts++; f.mu.Unlock(); return nil }
func (f *fakeOps) reload(string) error  { f.mu.Lock(); f.reloads++; f.mu.Unlock(); return nil }
func (f *fakeOps) active(string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.isActive == nil || f.isActive(f.restarts)
}
func (f *fakeOps) panicked(string, time.Time) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.panic }
func (f *fakeOps) links(string, time.Time) int     { return f.link }
func (f *fakeOps) daemonReload()                   {}
func (f *fakeOps) spawn(name string, args ...string) error {
	f.spawned = append(f.spawned, name+" "+strings.Join(args, " "))
	return nil
}
func (f *fakeOps) timer(name string, after time.Duration, args ...string) error {
	f.timers = append(f.timers, name)
	return nil
}
func (f *fakeOps) stopTimer(name string) { f.stopped = append(f.stopped, name) }
func (f *fakeOps) now() time.Time        { f.mu.Lock(); defer f.mu.Unlock(); return f.t }
func (f *fakeOps) sleep(d time.Duration) {
	time.Sleep(200 * time.Microsecond) // yield: a concurrent confirm gets in
	f.mu.Lock()
	f.t = f.t.Add(d)
	f.mu.Unlock()
}

func setupApply(t *testing.T) (*fakeOps, string) {
	menuEnv(t)
	healthWindow, healthStable = 20*time.Second, 3*time.Second
	cfg := configForUnit("hs2-lab")
	os.WriteFile(cfg, []byte(`{"mode":"dial","max_links":8}`), 0o600)
	return &fakeOps{t: time.Date(2026, 11, 11, 2, 0, 0, 0, time.Local), link: 1}, cfg
}

func start(t *testing.T, f *fakeOps, cfg string, within time.Duration) applyState {
	st, _, err := startApply(f, applyReq{Unit: "hs2-lab", Cfg: cfg, Reason: "test", New: []byte(`{"mode":"dial","max_links":16}`), Within: within, HealthGate: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.spawned) != 1 || len(f.timers) != 1 || !strings.Contains(f.spawned[0], "--worker") {
		t.Fatalf("not handed over: %v %v", f.spawned, f.timers)
	}
	return st
}

func cfgIs(t *testing.T, cfg, want string) {
	t.Helper()
	b, _ := os.ReadFile(cfg)
	if !strings.Contains(string(b), want) {
		t.Fatalf("config = %s, want %s", b, want)
	}
}

func TestApplyConfirm(t *testing.T) {
	f, cfg := setupApply(t)
	start(t, f, cfg, 30*time.Minute)
	cfgIs(t, cfg, "16")
	// The caller (SSH) is gone; the detached worker runs on its own.
	done := make(chan struct{})
	go func() { runWorker(f, "hs2-lab"); close(done) }()
	for i := 0; i < 2000; i++ {
		if st, _ := loadState("hs2-lab"); st.State == stTrial {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := confirmApply(f, "hs2-lab"); err != nil {
		t.Fatal(err)
	}
	<-done
	st, _ := loadState("hs2-lab")
	if st.State != stConfirmed {
		t.Fatalf("state %s", st.State)
	}
	cfgIs(t, cfg, "16")
	if len(f.stopped) == 0 {
		t.Error("expiry timer not stopped")
	}
	if err := confirmApply(f, "hs2-lab"); err == nil {
		t.Error("double confirm accepted")
	}
	b, _ := os.ReadFile(auditLogPath)
	if !strings.Contains(string(b), "action=confirm") || !strings.Contains(string(b), "max_links") {
		t.Errorf("audit: %s", b)
	}
}

func TestApplyTimeoutRollback(t *testing.T) {
	f, cfg := setupApply(t)
	st := start(t, f, cfg, time.Minute)
	runWorker(f, "hs2-lab") // never confirmed
	s, _ := loadState("hs2-lab")
	if s.State != stRolledBack || !strings.Contains(s.Detail, "not confirmed") {
		t.Fatalf("state %s %s", s.State, s.Detail)
	}
	cfgIs(t, cfg, `"max_links":8`)
	if f.restarts != 2 {
		t.Errorf("restarts %d, want 2 (apply + rollback)", f.restarts)
	}
	vs, _ := listVersions("hs2-lab")
	if len(vs) != 3 || vs[0].N != st.From {
		t.Errorf("history %+v", vs)
	}
}

func TestApplyHealthFailRollback(t *testing.T) {
	f, cfg := setupApply(t)
	f.isActive = func(r int) bool { return r != 1 } // the new config never comes up
	start(t, f, cfg, time.Minute)
	runWorker(f, "hs2-lab")
	s, _ := loadState("hs2-lab")
	if s.State != stRolledBack || !strings.Contains(s.Detail, "health gate") {
		t.Fatalf("state %s %s", s.State, s.Detail)
	}
	cfgIs(t, cfg, `"max_links":8`)

	// panic in the log
	f2, cfg2 := setupApply(t)
	f2.panic = true
	start(t, f2, cfg2, time.Minute)
	runWorker(f2, "hs2-lab")
	if s, _ := loadState("hs2-lab"); !strings.Contains(s.Detail, "panic") {
		t.Fatalf("panic not caught: %s", s.Detail)
	}
	// no serving link with --health-gate
	f3, cfg3 := setupApply(t)
	f3.link = 0
	start(t, f3, cfg3, time.Minute)
	runWorker(f3, "hs2-lab")
	if s, _ := loadState("hs2-lab"); !strings.Contains(s.Detail, "no link") {
		t.Fatalf("links not gated: %s", s.Detail)
	}
}

// The caller dies right after the hand-over and the worker is killed too:
// the independent expiry timer still rolls back.
func TestApplySSHDropSurvival(t *testing.T) {
	f, cfg := setupApply(t)
	start(t, f, cfg, time.Minute)
	if _, _, err := startApply(f, applyReq{Unit: "hs2-lab", Cfg: cfg, New: []byte(`{"mode":"dial","max_links":4}`), Within: time.Minute}); err == nil {
		t.Fatal("second apply accepted while one is live")
	}
	expireApply(f, "hs2-lab") // before the deadline: no-op
	if s, _ := loadState("hs2-lab"); s.State != stRestarting {
		t.Fatalf("early expiry acted: %s", s.State)
	}
	f.sleep(time.Minute + expireGrace)
	expireApply(f, "hs2-lab")
	s, _ := loadState("hs2-lab")
	if s.State != stRolledBack {
		t.Fatalf("state %s", s.State)
	}
	cfgIs(t, cfg, `"max_links":8`)
	runWorker(f, "hs2-lab") // a late worker must not touch a finished apply
	if f.restarts != 1 {
		t.Fatalf("restarts %d", f.restarts)
	}
}

func TestAdminLockStale(t *testing.T) {
	menuEnv(t)
	now := time.Now()
	oldAlive := pidAlive
	defer func() { pidAlive = oldAlive }()
	alive := map[int]bool{1111: true}
	pidAlive = func(p int) bool { return alive[p] }

	rel, err := acquireLock(1111, false, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireLock(2222, false, now); err == nil {
		t.Fatal("live lock not respected")
	}
	if _, err := acquireLock(2222, true, now); err != nil {
		t.Fatal("--force failed")
	}
	if l, _ := readLock(); l.PID != 2222 {
		t.Fatal("force did not take over")
	}
	rel() // 1111's release must not drop 2222's lock
	if _, ok := readLock(); !ok {
		t.Fatal("foreign release removed the lock")
	}
	// holder 2222 dies -> stale
	if _, err := acquireLock(3333, false, now); err != nil {
		t.Fatalf("stale (dead pid) lock not taken over: %v", err)
	}
	// alive but too old -> stale
	alive[3333] = true
	if !lockStale(adminLock{PID: 3333, Time: now.Add(-3 * time.Hour).Unix()}, now) {
		t.Fatal("old lock not stale")
	}
	if lockStale(adminLock{PID: 3333, Time: now.Unix()}, now) {
		t.Fatal("fresh live lock reported stale")
	}
}
