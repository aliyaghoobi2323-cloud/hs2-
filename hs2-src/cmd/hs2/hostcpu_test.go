package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeProc is a /proc tree for hostMeter: stat (aggregate + per-core lines),
// pressure/cpu and net/snmp, rewritten between samples.
type fakeProc struct {
	t   *testing.T
	dir string
	// cumulative ticks: busy (user) and idle, softirq, steal
	busy, idle, softirq, steal uint64
	cores                      int
}

func newFakeProc(t *testing.T, cores int) *fakeProc {
	f := &fakeProc{t: t, dir: t.TempDir(), cores: cores}
	os.MkdirAll(filepath.Join(f.dir, "pressure"), 0o755)
	os.MkdirAll(filepath.Join(f.dir, "net"), 0o755)
	f.write()
	return f
}

// advance moves the counters by one interval with busy% of all cores busy.
func (f *fakeProc) advance(busyPct float64) {
	const tick = 1000
	b := uint64(busyPct * tick / 100)
	f.busy += b
	f.idle += tick - b
	f.write()
}

func (f *fakeProc) write() {
	var b strings.Builder
	// user nice system idle iowait irq softirq steal guest guest_nice
	fmt.Fprintf(&b, "cpu  %d 0 0 %d 0 0 %d %d 0 0\n", f.busy, f.idle, f.softirq, f.steal)
	for i := 0; i < f.cores; i++ {
		fmt.Fprintf(&b, "cpu%d 1 0 0 1 0 0 0 0 0 0\n", i)
	}
	b.WriteString("intr 1 2 3\nctxt 4\n")
	if err := os.WriteFile(filepath.Join(f.dir, "stat"), []byte(b.String()), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeProc) psi(avg10, avg60 float64) {
	s := fmt.Sprintf("some avg10=%.2f avg60=%.2f avg300=0.00 total=123\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n", avg10, avg60)
	os.WriteFile(filepath.Join(f.dir, "pressure", "cpu"), []byte(s), 0o644)
}

func (f *fakeProc) discards(n uint64) {
	s := "Ip: Forwarding DefaultTTL InReceives OutRequests OutDiscards OutNoRoutes\n" +
		fmt.Sprintf("Ip: 1 64 100 200 %d 0\n", n) +
		"Icmp: InMsgs OutMsgs\nIcmp: 1 2\n"
	os.WriteFile(filepath.Join(f.dir, "net", "snmp"), []byte(s), 0o644)
}

// The parsers read the real kernel layouts: the aggregate cpu line and the
// core count, PSI "some" avg10/avg60, and OutDiscards by its header name.
func TestHostProcParsers(t *testing.T) {
	f := newFakeProc(t, 2)
	f.softirq, f.steal = 7, 3
	f.write()
	c, ok := readProcStat(f.dir)
	if !ok || c.cores != 2 || c.softirq != 7 || c.steal != 3 {
		t.Fatalf("stat: %+v ok %v", c, ok)
	}
	if _, _, ok := readPSI(f.dir); ok {
		t.Fatal("no pressure file must read as absent")
	}
	f.psi(12.5, 40.25)
	if a10, a60, ok := readPSI(f.dir); !ok || a10 != 12.5 || a60 != 40.25 {
		t.Fatalf("psi %v %v %v", a10, a60, ok)
	}
	f.discards(77)
	if n, ok := readOutDiscards(f.dir); !ok || n != 77 {
		t.Fatalf("OutDiscards %d %v", n, ok)
	}
	// busy = (total - idle - iowait) / total; softirq and steal count as busy.
	a := cpuTimes{user: 100, idle: 900}
	b := cpuTimes{user: 100 + 600, idle: 900 + 200, softirq: 100, steal: 100}
	busy, si, st, ok := hostShares(a, b)
	if !ok || busy != 80 || si != 10 || st != 10 {
		t.Fatalf("shares %v %v %v %v", busy, si, st, ok)
	}
	if _, _, _, ok := hostShares(b, a); ok {
		t.Fatal("counters that went back must give no reading")
	}
}

// The meter logs once when the server stays saturated (busy or PSI), not on
// one busy sample, once when it has room again, and nothing in between.
func TestHostMeterSaturation(t *testing.T) {
	f := newFakeProc(t, 2)
	var logs []string
	m := &hostMeter{proc: f.dir, logf: func(s string, a ...any) { logs = append(logs, fmt.Sprintf(s, a...)) }}
	if s := m.sample(10); s.ok || s.Cores != 2 {
		t.Fatalf("the first sample has no interval: %+v", s)
	}
	f.advance(97)
	m.sample(140)
	f.advance(60) // one quiet sample resets the run
	m.sample(100)
	for i := 0; i < hostSatRuns-1; i++ {
		f.advance(95)
		if s := m.sample(140); s.Saturated || len(logs) != 0 {
			t.Fatalf("saturated after %d hot samples: %+v %v", i+1, s, logs)
		}
	}
	f.advance(95)
	s := m.sample(140)
	if !s.Saturated || len(logs) != 1 || !strings.Contains(logs[0], "95% busy across 2 core(s)") || !strings.Contains(logs[0], "140% of one core") {
		t.Fatalf("not reported after %d hot samples: %+v %v", hostSatRuns, s, logs)
	}
	f.advance(80) // between the marks: still saturated, no new line
	if s := m.sample(100); !s.Saturated || len(logs) != 1 {
		t.Fatalf("between the marks: %+v %v", s, logs)
	}
	for i := 0; i < hostClearRuns-1; i++ { // a load dipping now and then does not clear it
		f.advance(30)
		if s := m.sample(50); !s.Saturated || len(logs) != 1 {
			t.Fatalf("cleared after %d cool samples: %+v %v", i+1, s, logs)
		}
	}
	f.advance(30)
	if s := m.sample(50); s.Saturated || len(logs) != 2 || !strings.Contains(logs[1], "room again") {
		t.Fatalf("recovery: %+v %v", s, logs)
	}

	// CPU pressure alone (tasks waiting for a core while the busy share is
	// modest: a cgroup quota, or steal) counts too, and holds it saturated.
	f.psi(55, 50)
	for i := 0; i < hostSatRuns; i++ {
		f.advance(50)
		s = m.sample(90)
	}
	if !s.Saturated || len(logs) != 3 || !strings.Contains(logs[2], "tasks waited for a core 55%") {
		t.Fatalf("PSI saturation: %+v %v", s, logs)
	}
	f.psi(30, 50) // below the saturation mark, above the clear one: held
	f.advance(50)
	if s = m.sample(90); !s.Saturated {
		t.Fatalf("PSI between the marks cleared it: %+v", s)
	}
	f.psi(5, 30)
	for i := 0; i < hostClearRuns; i++ {
		f.advance(50)
		s = m.sample(90)
	}
	if s.Saturated || len(logs) != 4 {
		t.Fatalf("PSI recovery: %+v %v", s, logs)
	}

	// A sample that measures nothing (no PSI, the counters went back) leaves
	// the state as it is: it neither clears a saturation nor logs.
	os.Remove(filepath.Join(f.dir, "pressure", "cpu"))
	for i := 0; i < hostSatRuns; i++ {
		f.advance(97)
		s = m.sample(150)
	}
	if !s.Saturated || len(logs) != 5 {
		t.Fatalf("saturation again: %+v %v", s, logs)
	}
	f.steal = 0
	f.busy, f.idle = 1, 1 // the counters went back
	f.write()
	if s = m.sample(150); !s.Saturated || len(logs) != 5 {
		t.Fatalf("an unmeasured sample changed the state: %+v %v", s, logs)
	}
}

// Output discards are counted from the first sample (what came before hs2
// is not its to report), a counter that goes back is a new base, and nothing
// is logged: the count is the whole server's, and on an icmp listener it
// includes the kernel's own replies the echo guard drops on purpose.
func TestHostMeterDiscards(t *testing.T) {
	f := newFakeProc(t, 1)
	var logs []string
	m := &hostMeter{proc: f.dir, logf: func(s string, a ...any) { logs = append(logs, fmt.Sprintf(s, a...)) }}
	f.discards(1000)
	if s := m.sample(0); s.OutDiscards != 0 {
		t.Fatalf("start: %+v", s)
	}
	for _, c := range []struct{ raw, want uint64 }{{1040, 40}, {1100, 100}, {1105, 105}, {3, 105}, {5, 107}} {
		f.discards(c.raw)
		if s := m.sample(0); s.OutDiscards != c.want {
			t.Fatalf("raw %d: %+v, want %d", c.raw, s, c.want)
		}
	}
	if len(logs) != 0 {
		t.Fatalf("output discards were logged: %v", logs)
	}
}

// doctor measures the server over its wait, warns when it is short of CPU
// (busy, or tasks waiting a minute long), and adds up the running tunnels'
// own CPU from their status files.
func TestDoctorCPU(t *testing.T) {
	old := statusRunDir
	statusRunDir = t.TempDir()
	t.Cleanup(func() { statusRunDir = old })
	for i, pct := range []float64{80, 45} {
		b, _ := json.Marshal(liveStatus{CPUPct: pct, Updated: time.Now().Unix()})
		os.WriteFile(filepath.Join(statusRunDir, fmt.Sprintf("t%d.status.json", i)), b, 0o644)
	}
	b, _ := json.Marshal(liveStatus{CPUPct: 999, Updated: time.Now().Unix() - 60}) // stale: not counted
	os.WriteFile(filepath.Join(statusRunDir, "old.status.json"), b, 0o644)
	for _, c := range []struct {
		busy, psi60 float64
		tag, want   string
	}{
		{30, 2, " ok ", "server 30% busy across 2 core(s)"},
		{80, 2, "info", "little room"},
		{96, 2, "warn", "short of CPU"},
		{50, 60, "warn", "(60% over the last minute)"},
	} {
		f := newFakeProc(t, 2)
		f.psi(c.psi60, c.psi60)
		d := &doctorReport{}
		checkCPU(d, f.dir, func() { f.advance(c.busy) })
		out := strings.Join(d.lines, "\n")
		if !strings.Contains(out, "["+c.tag+"] server cpu") || !strings.Contains(out, c.want) || !strings.Contains(out, "2 running hs2 tunnel(s) use 125% of one core in all") {
			t.Errorf("busy %v psi %v: %s", c.busy, c.psi60, out)
		}
	}
	d := &doctorReport{}
	checkCPU(d, t.TempDir(), func() {}) // no /proc/stat: nothing to say
	if len(d.lines) != 0 {
		t.Fatalf("no /proc: %v", d.lines)
	}
}

// hs2 status shows hs2's own CPU next to the server's, the send stage, the
// pool's share and the kernel's discards; an older status file (no host
// fields) keeps the old cpu line.
func TestStatusSendStageAndHostLines(t *testing.T) {
	psi := 81.0
	ls := liveStatus{Carrier: "dgtun", CPUPct: 140, CPUCores: 2, HostCPUPct: 97, HostCores: 2, HostSoftirqPct: 12,
		PSICPU10: &psi, HostSaturated: true, HostOutDiscards: 3210, SendRefused: 4,
		ShareMbit: 12.5, BusyQueueMs: 38, SendMbit: 61.2, SendHeldPct: 54, FQWaitMs: 7.5, WriteUs: 85, PerWrite: 3.2,
		Carriers: "1:up:500/0.1% r20.0/bw25.0 P s15.3 q12/41"}
	if l := cpuLine(ls); l != "hs2 140% of one core (2 core(s)) · server 97% busy across 2 core(s) (softirq 12%), tasks waited for a core 81% of the last 10 s — SATURATED: carriers send late however good the path is (see the log)" {
		t.Errorf("cpu line %q", l)
	}
	if l := cpuLine(liveStatus{CPUPct: 40, CPUCores: 2}); l != "hs2 40% of one core (2 core(s))" {
		t.Errorf("old status file cpu line %q", l)
	}
	if l := sendingLine(ls); l != "pacers sent 61.2 Mbit/s · writers waited for pacer room 54% of the time · fair-queue wait 7.5 ms · socket write 85 µs (3.2 datagram(s) each)" {
		t.Errorf("sending line %q", l)
	}
	if l := poolLine(ls); l != "fair share 12.5 Mbit/s per busy carrier · queue the busy carriers see 38 ms" {
		t.Errorf("pool line %q", l)
	}
	if sendingLine(liveStatus{}) != "" || poolLine(liveStatus{}) != "" {
		t.Error("an idle or older pool must print no send-stage lines")
	}
	path := filepath.Join(t.TempDir(), "s.json")
	ls.Updated = time.Now().Unix()
	writeStatusFile(path, ls)
	out := captureStdout(t, func() { printStatus(path) })
	for _, want := range []string{"refused by the kernel 4", "  sending:    pacers sent 61.2", "  pool:       fair share 12.5",
		"  net:        3210 outgoing IP packet(s) discarded", "an icmp listener", "SATURATED", "sSENT Mbit, qQUEUE/SRTT ms"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	raw, _ := os.ReadFile(path)
	for _, key := range []string{`"send_refused":4`, `"host_cpu_pct":97`, `"psi_cpu10":81`, `"host_saturated":true`, `"host_out_discards":3210`, `"send_held_pct":54`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("status JSON lacks %s", key)
		}
	}
	if strings.Contains(string(raw), "pacer_dropped") {
		t.Error("the dead pacer_dropped counter is still written")
	}
}
