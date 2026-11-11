package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPresetsToEnv(t *testing.T) {
	if p, _ := presetByName("production"); len(presetEnv(p, true)) != 0 || renderPerfDropin(presetEnv(p, true), "production") != "" {
		t.Fatal("production = no drop-in (the code defaults)")
	}
	low, ok := presetByName("low-cpu")
	if !ok {
		t.Fatal("low-cpu")
	}
	edge, exit := presetEnv(low, true), presetEnv(low, false)
	if edge["HS2_MCONN_STRIPE"] != "0" || edge["HS2_STRIPE_AW_HEAVY"] != "0" || edge["HS2_SMUX_AUTOWIN"] != "0" {
		t.Fatalf("edge %v", edge)
	}
	if _, ok := exit["HS2_STRIPE_AW_HEAVY"]; ok { // an Iran-only key never lands on kharej
		t.Fatalf("exit %v", exit)
	}
	mt, _ := presetByName("max-throughput")
	if _, ok := presetEnv(mt, true)["HS2_MCONN_STRIPE_JUMP_MS"]; ok {
		t.Fatal("kharej-only jump key on iran")
	}
	d := renderPerfDropin(presetEnv(mt, false), "max-throughput")
	if !strings.Contains(d, "[Service]\nEnvironment=HS2_MCONN_STRIPE_JUMP_MS=1500\nEnvironment=HS2_SMUX_AUTOWIN_SHARE_MB=64\n") || perfPresetOf(d) != "max-throughput" {
		t.Fatal(d)
	}
	for _, p := range perfPresets {
		if p.En == "" || p.Fa == "" {
			t.Fatalf("%s: every preset says what it trades", p.Name)
		}
		for k := range p.Keys {
			if _, ok := perfKeyByName(k); !ok {
				t.Fatalf("%s: undocumented key %s", p.Name, k)
			}
		}
	}
}

func TestEffectiveKeys(t *testing.T) {
	fc := fileConfig{Mode: "dial", Reverse: true, Carrier: "l3mtcp"}
	di := map[string]string{
		"10-other.conf": "[Service]\nEnvironment=HS2_SMUX_AUTOWIN_SHARE_MB=48\n",
		perfDropin:      renderPerfDropin(map[string]string{"HS2_SMUX_AUTOWIN_SHARE_MB": "16", "HS2_MCONN_STRIPE": "4"}, "min-latency"),
	}
	m := map[string]effKey{}
	for _, e := range effectiveKeys(fc, di) {
		m[e.Key.Name] = e
	}
	if e := m["HS2_SMUX_AUTOWIN_SHARE_MB"]; e.Value != "16" || e.Source != "explicit ("+perfDropin+")" {
		t.Fatalf("last drop-in wins: %+v", e)
	}
	if e := m["HS2_STRIPE_AW_SOLO"]; e.Value != "1" || e.Source != "default" {
		t.Fatalf("edge default: %+v", e)
	}
	if _, ok := m["HS2_LB_FLAP_PEN"]; ok {
		t.Fatal("kharej-only key shown on iran")
	}
	// direct mtcp: no production defaults, everything off unless set
	for _, e := range effectiveKeys(fileConfig{Mode: "listen", Carrier: "mtcp"}, nil) {
		if e.Source != "off" {
			t.Fatalf("%+v", e)
		}
	}
	// HS2_DEFAULTS=legacy turns the defaults off
	for _, e := range effectiveKeys(fc, map[string]string{"x.conf": "[Service]\nEnvironment=HS2_DEFAULTS=legacy\n"}) {
		if e.Source == "default" {
			t.Fatalf("legacy: %+v", e)
		}
	}
	out := renderEffective(effectiveKeys(fc, di), false)
	if !strings.Contains(out, "HS2_SMUX_AUTOWIN ") || strings.Contains(out, "HS2_SHAPE_FAST") {
		t.Fatalf("simple mode hides the undocumented keys:\n%s", out)
	}
	if !strings.Contains(renderEffective(effectiveKeys(fc, di), true), "HS2_SHAPE_FAST") {
		t.Fatal("advanced shows all")
	}
}

func TestNICDetectionFixtures(t *testing.T) {
	d := t.TempDir()
	proc, sys := filepath.Join(d, "proc"), filepath.Join(d, "sys")
	os.MkdirAll(filepath.Join(proc, "net"), 0o755)
	os.WriteFile(filepath.Join(proc, "net", "route"), []byte("Iface\tDestination\tGateway\neth0\t00000000\t0101A8C0\n"), 0o644)
	os.MkdirAll(filepath.Join(sys, "class", "net", "eth0", "queues", "rx-0"), 0o755)
	os.Symlink("../../../devices/virtio0", filepath.Join(sys, "class", "net", "eth0", "device"))
	stat1 := "cpu0 100 0 100 800 0 0 0 0\ncpu1 100 0 100 800 0 0 0 0\ncpu2 100 0 100 800 0 0 0 0\ncpu3 100 0 100 800 0 0 0 0\n"
	stat2 := "cpu0 110 0 110 880 0 0 0 0\ncpu1 110 0 110 880 0 0 0 0\ncpu2 110 0 110 880 0 0 0 0\ncpu3 110 0 110 820 0 0 60 0\n"
	n := detectNIC(proc, sys, "", []byte(stat1), []byte(stat2), []byte(fixInterrupts1), []byte(fixInterrupts2))
	if n.Iface != "eth0" || n.Queues != 1 || n.RxCPU != 3 || n.RxShare < 0.99 || n.NCPU != 4 || !n.singleRx() {
		t.Fatalf("%+v", n)
	}
	if n.Cores[3].Softirq < 59 || n.Cores[3].Idle > 21 {
		t.Fatalf("core 3 %+v", n.Cores[3])
	}
	out := renderNIC(n)
	if !strings.Contains(out, "rx interrupts on core 3") || !strings.Contains(out, "--keep 3") || !strings.Contains(out, "NEVER changed by hs2") {
		t.Fatal(out)
	}
	// ethtool's current Combined wins over sysfs; several queues: no advice
	et := "Channel parameters for eth0:\nPre-set maximums:\nCombined:\t4\nCurrent hardware settings:\nCombined:\t4\n"
	n = detectNIC(proc, sys, et, []byte(stat1), []byte(stat2), []byte(strings.Replace(fixInterrupts1, "5000", "100", 1)), []byte(strings.Replace(fixInterrupts2, "9000", "110", 1)))
	if n.Queues != 4 || n.singleRx() {
		t.Fatalf("%+v", n)
	}
	if !strings.Contains(renderNIC(n), "Several queues") {
		t.Fatal(renderNIC(n))
	}
	// no default route
	if n := detectNIC(t.TempDir(), sys, "", nil, nil, nil, nil); n.Iface != "" || n.RxCPU != -1 {
		t.Fatalf("%+v", n)
	}
}

func TestCoresAndUnitGuard(t *testing.T) {
	if l, err := cpuAffinityFor(4, 0); err != nil || l != "1 2 3" {
		t.Fatalf("%q %v", l, err)
	}
	if l, _ := cpuAffinityFor(4, 2); l != "0 1 3" {
		t.Fatal(l)
	}
	for _, c := range [][2]int{{1, 0}, {4, 4}, {4, -1}} {
		if _, err := cpuAffinityFor(c[0], c[1]); err == nil {
			t.Fatalf("%v accepted", c)
		}
	}
	if !strings.Contains(cpuDropinText("1 2 3"), "[Service]\nCPUAffinity=1 2 3\n") {
		t.Fatal(cpuDropinText("1 2 3"))
	}
	for u, ok := range map[string]bool{"hs2": true, "hs2-de1.service": true, "x-ui": false, "xray": false, "hs2-xray": false, "nginx": false, "hs2/../x-ui": false} {
		if hs2UnitOK(u) != ok {
			t.Fatalf("%s: %v", u, !ok)
		}
	}
}

func TestBudgetVerdictAndIperf(t *testing.T) {
	if ok, _ := budgetVerdict(coreShare{Softirq: 70, Idle: 22}, coreShare{Softirq: 60, Idle: 45}); !ok {
		t.Fatal("better must be kept")
	}
	if ok, _ := budgetVerdict(coreShare{Softirq: 50, Idle: 40}, coreShare{Softirq: 51, Idle: 38}); !ok {
		t.Fatal("noise must be kept")
	}
	if ok, why := budgetVerdict(coreShare{Softirq: 50, Idle: 40}, coreShare{Softirq: 60, Idle: 30}); ok || !strings.Contains(why, "idle 40% → 30%") {
		t.Fatal("worse must be reverted: " + why)
	}
	v, err := iperfMbit([]byte(`{"end":{"sum_received":{"bits_per_second":948000000}}}`))
	if err != nil || v != 948 {
		t.Fatalf("%v %v", v, err)
	}
	if _, err := iperfMbit([]byte(`{"error":"unable to connect to server"}`)); err == nil || !strings.Contains(err.Error(), "unable") {
		t.Fatal(err)
	}
	a := strings.Join(speedArgs(15201, 5, speedRuns[1]), " ")
	if a != "-c 127.0.0.1 -p 15201 -t 5 -P 8 -J -R" {
		t.Fatal(a) // only ever the local tunnel port
	}
}

// The drop-in safe apply: preset written, rollback restores the old drop-ins.
func TestApplyWithDropinsRollsBack(t *testing.T) {
	f, cfg := setupApply(t)
	old := map[string]string{"10-x.conf": "[Service]\nEnvironment=HS2_A=1\n"}
	restoreDropins("hs2-lab", old)
	cur, _ := os.ReadFile(cfg)
	nd := map[string]string{"10-x.conf": old["10-x.conf"], perfDropin: renderPerfDropin(map[string]string{"HS2_MCONN_STRIPE": "0"}, "low-cpu")}
	st, d, err := startApply(f, applyReq{Unit: "hs2-lab", Cfg: cfg, Reason: "perf-preset", New: cur, NewDropins: nd, Within: time.Minute, HealthGate: true})
	if err != nil || len(d) != 1 || d[0].Path != "HS2_MCONN_STRIPE" {
		t.Fatalf("%v %+v", err, d)
	}
	if !strings.Contains(readDropins("hs2-lab")[perfDropin], "HS2_MCONN_STRIPE=0") {
		t.Fatal("drop-in not written")
	}
	f.isActive = func(int) bool { return false } // the unit dies → rollback
	runWorker(f, "hs2-lab")
	got, _ := loadState("hs2-lab")
	if got.State != stRolledBack {
		t.Fatalf("state %s (%+v)", got.State, st)
	}
	if di := readDropins("hs2-lab"); len(di) != 1 || di["10-x.conf"] != old["10-x.conf"] {
		t.Fatalf("drop-ins not restored: %v", di)
	}
	// same drop-ins, same config: nothing to apply
	if _, _, err := startApply(f, applyReq{Unit: "hs2-lab", Cfg: cfg, Reason: "x", New: cur, NewDropins: readDropins("hs2-lab"), Within: time.Minute}); err == nil {
		t.Fatal("empty change applied")
	}
}

func TestStatsTimerUnits(t *testing.T) {
	d := t.TempDir()
	var calls []string
	sc := func(a ...string) error { calls = append(calls, strings.Join(a, " ")); return nil }
	if err := statsTimer(true, d, "/usr/local/bin/hs2", sc); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(d, "hs2-stats.service"))
	tm, _ := os.ReadFile(filepath.Join(d, "hs2-stats.timer"))
	if !strings.Contains(string(b), "ExecStart=/usr/local/bin/hs2 stats sample") || !strings.Contains(string(b), "Nice=19") || !strings.Contains(string(tm), "OnUnitActiveSec=5min") {
		t.Fatal(string(b) + string(tm))
	}
	if strings.Join(calls, ";") != "daemon-reload;enable --now hs2-stats.timer" {
		t.Fatal(calls)
	}
	if ch, _ := writeStatsUnits(d, "/usr/local/bin/hs2"); ch {
		t.Fatal("rewrote identical units")
	}
	calls = nil
	statsTimer(false, d, "", sc)
	if _, err := os.Stat(filepath.Join(d, "hs2-stats.timer")); !os.IsNotExist(err) || calls[0] != "disable --now hs2-stats.timer" {
		t.Fatal("off must disable and remove", calls)
	}
}
