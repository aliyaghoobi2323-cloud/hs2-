package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The deep doctor (`hs2 doctor --deep [-c config] [--lang fa]`): a read-only
// look at the whole SERVER an hs2 tunnel runs on, built from problems seen in
// production — a single-queue NIC whose one rx core drowns in softirq, CPU
// pressure from xray/x-ui, a provider path that queues and loses packets with
// or without the tunnel, a Reality "dest" on a multi-tenant CDN that strangers
// use as a clean IP, an x-ui panel churning users and failing node syncs, and
// a congested network at night. Every check only reads (/proc, /sys, the
// status files, journalctl, a few ICMP pings), is bounded in time, and skips
// with a note when a tool or file is missing. It changes nothing.

// finding is one result: severity, what, why it matters, what to do.
type finding struct {
	Sev  string // OK | WARN | BAD | INFO | SKIP
	Area string
	What string
	Why  string
	Do   string
}

// deepLang selects the output language ("en" or "fa").
var deepLang = "en"

// tr returns the text in the selected language.
func tr(en, fa string) string {
	if deepLang == "fa" && fa != "" {
		return fa
	}
	return en
}

// deepEnv holds the paths and hooks the checks read through, so tests can
// point them at fixtures.
type deepEnv struct {
	proc, sys string
	runDir    string
	cfgPath   string
	window    time.Duration
	run       func(timeout time.Duration, name string, args ...string) (string, error)
}

func realRun(timeout time.Duration, name string, args ...string) (string, error) {
	if _, err := exec.LookPath(name); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// deepDoctor runs every deep check and prints the findings. It returns the
// number of BAD findings.
func deepDoctor(cfgPath string) int {
	env := &deepEnv{proc: "/proc", sys: "/sys", runDir: statusRunDir, cfgPath: cfgPath,
		window: 3 * time.Second, run: realRun}
	start := time.Now()
	fs := runDeep(env)
	printFindings(fs, time.Since(start))
	bad := 0
	for _, f := range fs {
		if f.Sev == "BAD" {
			bad++
		}
	}
	return bad
}

func runDeep(env *deepEnv) []finding {
	statuses := readStatuses(env.runDir, env.cfgPath)

	// Snapshot 1 of the sampled counters, then run the slow checks (pings,
	// journal reads) in parallel during the sampling window, then snapshot 2.
	stat1, _ := os.ReadFile(filepath.Join(env.proc, "stat"))
	irq1, _ := os.ReadFile(filepath.Join(env.proc, "interrupts"))
	procs1 := readProcCPU(env.proc)
	t0 := time.Now()

	var wg sync.WaitGroup
	var pathF, xuiF, hs2F, realF []finding
	wg.Add(4)
	go func() { defer wg.Done(); pathF = checkPaths(env, statuses) }()
	go func() { defer wg.Done(); xuiF = checkXUI(env) }()
	go func() { defer wg.Done(); hs2F = checkHS2Health(env, statuses) }()
	go func() { defer wg.Done(); realF = checkReality(env) }()
	if d := env.window - time.Since(t0); d > 0 {
		time.Sleep(d)
	}
	stat2, _ := os.ReadFile(filepath.Join(env.proc, "stat"))
	irq2, _ := os.ReadFile(filepath.Join(env.proc, "interrupts"))
	procs2 := readProcCPU(env.proc)
	elapsed := time.Since(t0)
	wg.Wait()

	var out []finding
	out = append(out, checkNIC(env, stat1, stat2, irq1, irq2)...)
	out = append(out, checkPressure(env, procs1, procs2, elapsed, stat1, stat2)...)
	out = append(out, hs2F...)
	out = append(out, pathF...)
	out = append(out, realF...)
	out = append(out, xuiF...)
	return out
}

func printFindings(fs []finding, took time.Duration) {
	fmt.Println()
	fmt.Println(tr("hs2 server doctor — deep check (read-only)", "hs2 پزشک سرور — بررسی عمیق (فقط خواندنی، هیچ چیزی تغییر نمی‌کند)"))
	nb, nw := 0, 0
	for _, f := range fs {
		fmt.Printf("\n[%-4s] %s: %s\n", f.Sev, f.Area, f.What)
		if f.Why != "" {
			fmt.Printf("       %s %s\n", tr("why:", "چرا مهم است:"), f.Why)
		}
		if f.Do != "" {
			fmt.Printf("       %s %s\n", tr("do: ", "چه کنید:"), f.Do)
		}
		switch f.Sev {
		case "BAD":
			nb++
		case "WARN":
			nw++
		}
	}
	fmt.Println()
	fmt.Printf(tr("summary: %d bad, %d warning(s) — took %.0fs\n", "خلاصه: %d مشکل جدی، %d هشدار — %.0f ثانیه طول کشید\n"), nb, nw, took.Seconds())
}

// ---------------------------------------------------------------- parsers

// coreTicks is one cpuN row of /proc/stat.
type coreTicks struct {
	idle, softirq, total uint64
}

// parsePerCPU parses the per-core rows (cpu0, cpu1, …) of /proc/stat.
func parsePerCPU(b []byte) map[int]coreTicks {
	out := map[int]coreTicks{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 9 || !strings.HasPrefix(f[0], "cpu") || f[0] == "cpu" {
			continue
		}
		n, err := strconv.Atoi(f[0][3:])
		if err != nil {
			continue
		}
		var v [8]uint64
		ok := true
		for i := range v {
			x, err := strconv.ParseUint(f[i+1], 10, 64)
			if err != nil {
				ok = false
				break
			}
			v[i] = x
		}
		if !ok {
			continue
		}
		var tot uint64
		for _, x := range v {
			tot += x
		}
		out[n] = coreTicks{idle: v[3] + v[4], softirq: v[6], total: tot}
	}
	return out
}

// coreShare is a core's softirq and idle share over the window, percent.
type coreShare struct{ Softirq, Idle float64 }

func perCPUDeltas(a, b map[int]coreTicks) map[int]coreShare {
	out := map[int]coreShare{}
	for n, y := range b {
		x, ok := a[n]
		if !ok || y.total <= x.total || y.idle < x.idle || y.softirq < x.softirq {
			continue
		}
		dt := float64(y.total - x.total)
		out[n] = coreShare{Softirq: 100 * float64(y.softirq-x.softirq) / dt, Idle: 100 * float64(y.idle-x.idle) / dt}
	}
	return out
}

// irqLine is one numbered row of /proc/interrupts.
type irqLine struct {
	Counts []uint64
	Name   string // the trailing description (device / queue name)
}

// parseInterrupts parses /proc/interrupts: numbered IRQ rows only.
func parseInterrupts(b []byte) map[string]irqLine {
	out := map[string]irqLine{}
	lines := strings.Split(string(b), "\n")
	if len(lines) == 0 {
		return out
	}
	ncpu := len(strings.Fields(lines[0]))
	for _, line := range lines[1:] {
		f := strings.Fields(line)
		if len(f) < 2 || !strings.HasSuffix(f[0], ":") {
			continue
		}
		id := strings.TrimSuffix(f[0], ":")
		if _, err := strconv.Atoi(id); err != nil {
			continue
		}
		var counts []uint64
		i := 1
		for ; i < len(f) && i <= ncpu; i++ {
			x, err := strconv.ParseUint(f[i], 10, 64)
			if err != nil {
				break
			}
			counts = append(counts, x)
		}
		out[id] = irqLine{Counts: counts, Name: strings.Join(f[i:], " ")}
	}
	return out
}

// nicIRQCPUs sums, per CPU, the rx interrupts of the IRQs that belong to the
// NIC (names that mention the interface or its virtio device), between two
// snapshots. Only "input"/"rx"/"TxRx" queues are counted where the driver
// names them; otherwise every IRQ of the device.
func nicIRQCPUs(a, b map[string]irqLine, names []string) (perCPU []uint64, irqs int) {
	match := func(desc string) bool {
		for _, n := range names {
			if n != "" && strings.Contains(desc, n) {
				return true
			}
		}
		return false
	}
	rxLike := func(desc string) bool {
		d := strings.ToLower(desc)
		return strings.Contains(d, "input") || strings.Contains(d, "rx") || strings.Contains(d, "txrx")
	}
	anyRx := false
	for _, l := range b {
		if match(l.Name) && rxLike(l.Name) {
			anyRx = true
		}
	}
	for id, y := range b {
		if !match(y.Name) || (anyRx && !rxLike(y.Name)) {
			continue
		}
		irqs++
		x := a[id]
		for i, c := range y.Counts {
			var prev uint64
			if i < len(x.Counts) {
				prev = x.Counts[i]
			}
			for len(perCPU) <= i {
				perCPU = append(perCPU, 0)
			}
			if c >= prev {
				perCPU[i] += c - prev
			}
		}
	}
	return perCPU, irqs
}

// parseCPUList parses a kernel CPU list ("0-3,6,8-9").
func parseCPUList(s string) map[int]bool {
	out := map[int]bool{}
	for _, part := range strings.Split(strings.TrimSpace(s), ",") {
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			continue
		}
		bb := a
		if isRange {
			if bb, err = strconv.Atoi(hi); err != nil {
				continue
			}
		}
		for i := a; i <= bb; i++ {
			out[i] = true
		}
	}
	return out
}

// defaultIface returns the interface of the IPv4 default route.
func defaultIface(proc string) string {
	b, err := os.ReadFile(filepath.Join(proc, "net", "route"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) > 2 && f[1] == "00000000" {
			return f[0]
		}
	}
	return ""
}

// ---------------------------------------------------------------- 1. NIC

func checkNIC(env *deepEnv, stat1, stat2, irq1, irq2 []byte) []finding {
	area := tr("NIC & softirq", "کارت شبکه و softirq")
	ifc := defaultIface(env.proc)
	if ifc == "" {
		return []finding{{Sev: "SKIP", Area: area, What: tr("no default route found in /proc/net/route", "مسیر پیش‌فرض در /proc/net/route پیدا نشد")}}
	}
	qdir := filepath.Join(env.sys, "class", "net", ifc, "queues")
	rxq, _ := filepath.Glob(filepath.Join(qdir, "rx-*"))
	nq := len(rxq)
	if out, err := env.run(3*time.Second, "ethtool", "-l", ifc); err == nil {
		if c := ethtoolCombined(out); c > 0 {
			nq = c
		}
	}
	rps := ""
	for _, q := range rxq {
		if b, err := os.ReadFile(filepath.Join(q, "rps_cpus")); err == nil {
			rps = strings.TrimSpace(string(b))
			break
		}
	}
	names := []string{ifc}
	if l, err := os.Readlink(filepath.Join(env.sys, "class", "net", ifc, "device")); err == nil {
		names = append(names, filepath.Base(l)+"-") // virtio0-input.0
	}
	perCPU, irqs := nicIRQCPUs(parseInterrupts(irq1), parseInterrupts(irq2), names)
	shares := perCPUDeltas(parsePerCPU(stat1), parsePerCPU(stat2))
	ncores := len(shares)

	hot, sum := -1, uint64(0)
	var hotN uint64
	for i, c := range perCPU {
		sum += c
		if c > hotN {
			hot, hotN = i, c
		}
	}
	// No IRQ seen (e.g. no traffic, or names not matched): fall back to the
	// core with the most softirq.
	if hot < 0 || sum == 0 {
		hot = -1
		best := -1.0
		for n, s := range shares {
			if s.Softirq > best {
				hot, best = n, s.Softirq
			}
		}
	}
	irqPct := 0.0
	if sum > 0 {
		irqPct = 100 * float64(hotN) / float64(sum)
	}
	hs := shares[hot]
	rpsOff := rps == "" || strings.Trim(rps, "0,") == ""
	desc := fmt.Sprintf(tr("%s: %d rx queue(s), %d IRQ(s), RPS mask %s", "%s: %d صف دریافت، %d وقفه (IRQ)، ماسک RPS %s"), ifc, nq, irqs, orDash(rps))
	if hot < 0 || ncores == 0 {
		return []finding{{Sev: "INFO", Area: area, What: desc}}
	}
	concentrated := nq <= 1 || irqPct >= 80
	where := fmt.Sprintf(tr("core %d (%.0f%% softirq, %.0f%% idle", "هستهٔ %d (%.0f٪ softirq، %.0f٪ بیکار"), hot, hs.Softirq, hs.Idle)
	if sum > 0 {
		where += fmt.Sprintf(tr(", %.0f%% of the NIC's interrupts)", "، %.0f٪ وقفه‌های کارت شبکه)"), irqPct)
	} else {
		where += ")"
	}
	// Do hs2 / xray processes still run on the hot core?
	var onHot []string
	self := os.Getpid()
	for _, p := range listProcs(env.proc) {
		if !isTunnelProc(p.comm) || p.pid == self {
			continue
		}
		if allowed := procAllowed(env.proc, p.pid); allowed != nil && allowed[hot] {
			onHot = append(onHot, p.comm)
		}
	}
	onHot = uniq(onHot)
	f := finding{Area: area}
	switch {
	case concentrated && ncores > 1 && (hs.Softirq >= 25 || hs.Idle < 20):
		f.Sev = "WARN"
		if hs.Idle < 5 {
			f.Sev = "BAD"
		}
		f.What = fmt.Sprintf(tr("%s — all incoming packets go through %s", "%s — همهٔ بسته‌های ورودی از %s می‌گذرند"), desc, where)
		f.Why = rxWhy(nq) + tr("; when that one core is full, packets wait or drop before hs2 even sees them: higher RTT and loss for every user, however good the path is",
			"؛ وقتی همین یک هسته پر شود، بسته‌ها قبل از رسیدن به hs2 منتظر می‌مانند یا دور ریخته می‌شوند: RTT و افت بیشتر برای همهٔ کاربران، هر قدر هم مسیر خوب باشد")
		if len(onHot) > 0 {
			f.Do = fmt.Sprintf(tr("keep core %d free of %s: add CPUAffinity=<all cores except %d> to their systemd units (drop-in), restart them; also consider RPS (rps_cpus) to spread the rx work",
				"هستهٔ %d را از %s خالی نگه دارید: در سرویس systemd آن‌ها CPUAffinity=<همهٔ هسته‌ها جز %d> بگذارید (drop-in) و ری‌استارت کنید؛ RPS (rps_cpus) هم برای پخش کار دریافت کمک می‌کند"),
				hot, strings.Join(onHot, ", "), hot)
		} else {
			f.Do = fmt.Sprintf(tr("hs2/xray already avoid core %d; if it is still saturated, spread rx with RPS (rps_cpus) or use a VPS with a multi-queue NIC",
				"hs2/xray از هستهٔ %d دوری می‌کنند؛ اگر باز هم پر است، با RPS (rps_cpus) کار دریافت را پخش کنید یا سروری با کارت شبکهٔ چندصفه بگیرید"), hot)
		}
		if !rpsOff {
			f.Do += tr(" (RPS is already on)", " (RPS از قبل روشن است)")
		}
	case concentrated && ncores > 1:
		f.Sev = "INFO"
		f.What = fmt.Sprintf(tr("%s — incoming packets land on %s; it has room now", "%s — بسته‌های ورودی روی %s می‌نشینند؛ فعلاً جا دارد"), desc, where)
		f.Why = rxWhy(nq) + tr("; fine while it has idle time", "؛ تا وقتی آن هسته بیکاری دارد مشکلی نیست")
		if len(onHot) > 0 {
			f.Do = fmt.Sprintf(tr("at peak, check again; if core %d fills up, move %s off it with CPUAffinity", "در ساعت اوج دوباره بررسی کنید؛ اگر هستهٔ %d پر شد، %s را با CPUAffinity از آن جدا کنید"), hot, strings.Join(onHot, ", "))
		}
	default:
		f.Sev = "OK"
		f.What = fmt.Sprintf(tr("%s — busiest rx core: %s", "%s — شلوغ‌ترین هستهٔ دریافت: %s"), desc, where)
	}
	return []finding{f}
}

// rxWhy explains why the rx work sits on one core: one queue, or several
// queues whose interrupts all land on the same core.
func rxWhy(nq int) string {
	if nq <= 1 {
		return tr("a single-queue NIC puts all receive work on one core", "کارت شبکهٔ تک‌صفه همهٔ کار دریافت را روی یک هسته می‌گذارد")
	}
	return tr("the NIC has several queues but almost all receive interrupts land on one core (the provider steers the traffic to one queue, or IRQ affinity points them all there)",
		"کارت شبکه چند صف دارد ولی تقریباً همهٔ وقفه‌های دریافت روی یک هسته می‌نشینند (دیتاسنتر ترافیک را به یک صف می‌فرستد یا affinity وقفه‌ها همه را آن‌جا گذاشته)")
}

func ethtoolCombined(out string) int {
	// The second "Combined:" is the current setting.
	n := 0
	cur := 0
	for _, l := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), ":")
		if !ok || k != "Combined" {
			continue
		}
		n++
		if x, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n == 2 {
			cur = x
		}
	}
	return cur
}

// ---------------------------------------------------------------- 2. CPU pressure

type procInfo struct {
	pid  int
	comm string
}

func listProcs(proc string) []procInfo {
	ents, _ := os.ReadDir(proc)
	var out []procInfo
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(proc, e.Name(), "comm"))
		if err != nil {
			continue
		}
		out = append(out, procInfo{pid, strings.TrimSpace(string(b))})
	}
	return out
}

func isTunnelProc(comm string) bool {
	c := strings.ToLower(comm)
	if strings.HasSuffix(c, ".test") {
		return false
	}
	return strings.HasPrefix(c, "hs2") || strings.HasPrefix(c, "xray") || c == "x-ui" || strings.HasPrefix(c, "sing-box") || strings.HasPrefix(c, "backhaul")
}

func procAllowed(proc string, pid int) map[int]bool {
	b, err := os.ReadFile(filepath.Join(proc, strconv.Itoa(pid), "status"))
	if err != nil {
		return nil
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "Cpus_allowed_list:"); ok {
			return parseCPUList(v)
		}
	}
	return nil
}

// readProcCPU returns utime+stime (clock ticks) per pid, keyed "pid comm".
func readProcCPU(proc string) map[string]uint64 {
	out := map[string]uint64{}
	for _, p := range listProcs(proc) {
		b, err := os.ReadFile(filepath.Join(proc, strconv.Itoa(p.pid), "stat"))
		if err != nil {
			continue
		}
		if t, ok := parsePidStatTicks(string(b)); ok {
			out[strconv.Itoa(p.pid)+" "+p.comm] = t
		}
	}
	return out
}

// parsePidStatTicks returns utime+stime of a /proc/<pid>/stat line.
func parsePidStatTicks(s string) (uint64, bool) {
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(s[i+1:])
	// after ")": state(0) ppid(1) … utime is field 14 overall → index 11 here.
	if len(f) < 13 {
		return 0, false
	}
	u, err1 := strconv.ParseUint(f[11], 10, 64)
	st, err2 := strconv.ParseUint(f[12], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return u + st, true
}

type procUse struct {
	Name string
	Pct  float64 // percent of one core
}

func topProcs(a, b map[string]uint64, elapsed time.Duration, n int) []procUse {
	const hz = 100.0 // USER_HZ on Linux
	// Summed per program name: 30 busy "sh" or a multi-process xray show as
	// one line.
	by := map[string]float64{}
	for k, y := range b {
		x, ok := a[k]
		if !ok || y < x {
			continue
		}
		_, name, _ := strings.Cut(k, " ")
		by[name] += 100 * float64(y-x) / hz / elapsed.Seconds()
	}
	var out []procUse
	for name, pct := range by {
		if pct >= 1 {
			out = append(out, procUse{name, pct})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pct > out[j].Pct })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func checkPressure(env *deepEnv, p1, p2 map[string]uint64, elapsed time.Duration, stat1, stat2 []byte) []finding {
	area := tr("CPU pressure", "فشار پردازنده")
	top := topProcs(p1, p2, elapsed, 5)
	var tl []string
	for _, p := range top {
		tl = append(tl, fmt.Sprintf("%s %.0f%%", p.Name, p.Pct))
	}
	tops := strings.Join(tl, ", ")
	if tops == "" {
		tops = "-"
	}
	a10, a60, ok := readPSI(env.proc)
	if !ok {
		return []finding{{Sev: "INFO", Area: area, What: tr("no PSI on this kernel; top processes (% of one core): ", "این کرنل PSI ندارد؛ پرمصرف‌ترین پردازه‌ها (٪ یک هسته): ") + tops}}
	}
	f := finding{Area: area}
	f.What = fmt.Sprintf(tr("some avg10 %.1f%%, avg60 %.1f%%; top (%% of one core): %s", "some avg10 %.1f٪، avg60 %.1f٪؛ پرمصرف‌ها (٪ یک هسته): %s"), a10, a60, tops)
	switch {
	case a10 > 15 || a60 > 15:
		f.Sev = "WARN"
		if a10 > 40 || a60 > 40 {
			f.Sev = "BAD"
		}
		f.Why = tr("runnable threads are waiting for a CPU: every packet hs2 and xray handle leaves late, which looks like jitter and lag to users",
			"رشته‌های آمادهٔ اجرا منتظر پردازنده‌اند: هر بسته‌ای که hs2 و xray جابه‌جا می‌کنند دیر می‌رود و کاربر آن را کندی و لگ می‌بیند")
		f.Do = tr("find the top consumer above; stop what does not belong here, give xray/x-ui fewer cores (CPUAffinity) or move to a VPS with more CPU",
			"پرمصرف‌ترین پردازهٔ بالا را پیدا کنید؛ چیزهای اضافه را متوقف کنید، به xray/x-ui هستهٔ کمتری بدهید (CPUAffinity) یا سرور قوی‌تری بگیرید")
	default:
		f.Sev = "OK"
	}
	return []finding{f}
}

// ---------------------------------------------------------------- status files

type namedStatus struct {
	file string
	ls   liveStatus
	age  int64
}

func readStatuses(runDir, cfgPath string) []namedStatus {
	var files []string
	if cfgPath != "" {
		files = []string{filepath.Join(runDir, filepath.Base(statusPath(cfgPath)))}
	} else {
		files, _ = filepath.Glob(filepath.Join(runDir, "*.status.json"))
	}
	var out []namedStatus
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var ls liveStatus
		if json.Unmarshal(b, &ls) != nil {
			continue
		}
		out = append(out, namedStatus{strings.TrimSuffix(filepath.Base(f), ".status.json"), ls, time.Now().Unix() - ls.Updated})
	}
	return out
}

// ---------------------------------------------------------------- 3 & 7. paths

type pingResult struct {
	Min, Avg, Max, Loss float64
	Sent                int
	OK                  bool
}

var (
	rePingLoss = regexp.MustCompile(`(\d+) packets transmitted, (\d+) (?:packets )?received.*?([\d.]+)% packet loss`)
	rePingRTT  = regexp.MustCompile(`= ([\d.]+)/([\d.]+)/([\d.]+)`)
)

func parsePing(out string) pingResult {
	var r pingResult
	m := rePingLoss.FindStringSubmatch(out)
	if m == nil {
		return r
	}
	r.Sent, _ = strconv.Atoi(m[1])
	r.Loss, _ = strconv.ParseFloat(m[3], 64)
	r.OK = true
	if m := rePingRTT.FindStringSubmatch(out); m != nil {
		r.Min, _ = strconv.ParseFloat(m[1], 64)
		r.Avg, _ = strconv.ParseFloat(m[2], 64)
		r.Max, _ = strconv.ParseFloat(m[3], 64)
	}
	return r
}

type pathProbe struct {
	local, remote  string
	tunRTT, tunMin float64
	tunLoss        float64
	hasTunLoss     bool
	ping           pingResult
	label          string
}

func collectPaths(env *deepEnv, st []namedStatus) []*pathProbe {
	seen := map[string]bool{}
	var out []*pathProbe
	add := func(local, remote string, v *pathProbe) {
		for _, r := range strings.Split(remote, ",") {
			r = strings.TrimSpace(r)
			if h, _, err := net.SplitHostPort(r); err == nil {
				r = h
			}
			if ip := net.ParseIP(r); ip == nil || ip.IsLoopback() || len(out) >= 8 {
				continue
			}
			k := local + ">" + r
			if seen[k] {
				continue
			}
			seen[k] = true
			p := *v
			p.local, p.remote = local, r
			out = append(out, &p)
		}
	}
	for _, s := range st {
		if s.age > 30 {
			continue
		}
		for _, v := range s.ls.LBPaths {
			p := &pathProbe{tunRTT: v.RTTp50, tunMin: v.MinRTT}
			if v.LossDown != nil {
				p.tunLoss, p.hasTunLoss = *v.LossDown, true
			}
			if v.LossUp != nil && (!p.hasTunLoss || *v.LossUp > p.tunLoss) {
				p.tunLoss, p.hasTunLoss = *v.LossUp, true
			}
			add(v.Local, v.Remote, p)
		}
		// Without LB: the address the dialing side connects to, or on the
		// listening side the peers its links come from.
		if len(s.ls.LBPaths) == 0 && strings.HasPrefix(s.ls.Endpoint, "connects to") {
			for _, w := range strings.Fields(s.ls.Endpoint) {
				add("*", strings.Trim(w, ",;()"), &pathProbe{tunRTT: s.ls.ObsCtrlRTTP50})
			}
		}
		if len(s.ls.LBPaths) == 0 && strings.HasPrefix(s.ls.Endpoint, "listens on") {
			if _, p, err := net.SplitHostPort(strings.TrimSpace(strings.TrimPrefix(s.ls.Endpoint, "listens on"))); err == nil {
				lp, _ := strconv.Atoi(p)
				for _, ip := range linkPeers(env.proc, lp) {
					add("*", ip, &pathProbe{tunRTT: s.ls.ObsCtrlRTTP50})
				}
			}
		}
	}
	// No LB rows: the config's own endpoint.
	if len(out) == 0 && env.cfgPath != "" {
		if raw, err := os.ReadFile(env.cfgPath); err == nil {
			var fc fileConfig
			if json.Unmarshal(raw, &fc) == nil && fc.Mode == "dial" {
				local := fc.BindLocalIP
				if local == "" {
					local = "*"
				}
				add(local, fc.Addr, &pathProbe{})
			}
		}
	}
	return out
}

// linkPeers returns the distinct remote IPs of the established TCP
// connections on a local port (the links' peers, on the listening side).
func linkPeers(proc string, port int) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range []string{"tcp", "tcp6"} {
		b, err := os.ReadFile(filepath.Join(proc, "net", n))
		if err != nil {
			continue
		}
		for _, c := range parseProcNetTCP(b) {
			ip := c.Remote.String()
			if c.LocalPort == port && !c.Remote.IsLoopback() && !seen[ip] {
				seen[ip] = true
				out = append(out, ip)
			}
		}
	}
	return out
}

func checkPaths(env *deepEnv, st []namedStatus) []finding {
	area := tr("path quality", "کیفیت مسیر")
	paths := collectPaths(env, st)
	if len(paths) == 0 {
		return []finding{{Sev: "SKIP", Area: area, What: tr("no tunnel path with a peer IP found (no running LB tunnel / no dial address)", "مسیری با IP طرف مقابل پیدا نشد (تونل LB فعال یا آدرس dial نیست)")}}
	}
	var wg sync.WaitGroup
	for _, p := range paths {
		wg.Add(1)
		go func(p *pathProbe) {
			defer wg.Done()
			args := []string{"-n", "-q", "-c", "10", "-i", "0.2", "-W", "1"}
			if p.local != "" && p.local != "*" {
				args = append(args, "-I", p.local)
			}
			out, _ := env.run(6*time.Second, "ping", append(args, p.remote)...)
			p.ping = parsePing(out)
		}(p)
	}
	wg.Wait()
	return judgePaths(paths)
}

// judgePaths turns the ping results and the tunnel's own figures into
// findings (split from checkPaths for tests).
func judgePaths(paths []*pathProbe) []finding {
	area := tr("path quality", "کیفیت مسیر")
	var out []finding
	congested, judged := 0, 0
	for _, p := range paths {
		name := p.remote
		if p.local != "*" && p.local != "" {
			name = p.local + " → " + p.remote
		}
		f := finding{Area: area + " " + name}
		pg := p.ping
		if !pg.OK {
			f.Sev, f.What = "SKIP", tr("ping did not run (ICMP blocked, or no ping)", "ping اجرا نشد (ICMP بسته است یا ping نیست)")
			out = append(out, f)
			continue
		}
		if pg.Loss >= 100 {
			f.Sev = "INFO"
			f.What = tr("no ICMP reply — the peer or a firewall drops ping; cannot judge this path without the tunnel", "پاسخ ICMP نیامد — طرف مقابل یا فایروال ping را می‌بندد؛ بدون تونل نمی‌توان این مسیر را سنجید")
			out = append(out, f)
			continue
		}
		queue := pg.Avg - pg.Min
		base := pg.Min
		if p.tunMin > 0 && p.tunMin < base {
			base = p.tunMin
		}
		tun := ""
		if p.tunRTT > 0 {
			tun = fmt.Sprintf(tr("; tunnel links: RTT %.1f ms", "؛ لینک‌های تونل: RTT %.1f ms"), p.tunRTT)
			if p.hasTunLoss {
				tun += fmt.Sprintf(tr(", loss %.1f%%", "، افت %.1f٪"), p.tunLoss)
			}
		}
		f.What = fmt.Sprintf(tr("plain ping: %.1f/%.1f/%.1f ms min/avg/max, %.0f%% loss%s", "ping ساده: %.1f/%.1f/%.1f ms (کمینه/میانگین/بیشینه)، %.0f٪ افت%s"), pg.Min, pg.Avg, pg.Max, pg.Loss, tun)
		judged++
		if base > 0 && pg.Avg > base+math.Max(30, 0.5*base) {
			congested++
		}
		switch {
		case pg.Loss >= 1 || queue >= 30 || (base > 0 && pg.Avg-base >= 40):
			f.Sev = "WARN"
			if pg.Loss >= 5 {
				f.Sev = "BAD"
			}
			extra := math.Max(queue, pg.Avg-base)
			f.Why = fmt.Sprintf(tr("this IP has +%.0f ms queue and %.1f%% loss even WITHOUT the tunnel → a network/provider problem, not hs2",
				"این IP حتی بدون تونل +%.0f ms صف و %.1f٪ افت دارد → مشکل از شبکه/دیتاسنتر است، نه hs2"), extra, pg.Loss)
			f.Do = tr("lower this IP's share (or remove it) in load balancing, or ask the provider; LB auto mode already moves load away from a bad path",
				"سهم این IP را در لودبالانس کم کنید (یا حذفش کنید) یا به دیتاسنتر گزارش دهید؛ حالت auto خودش بار را از مسیر بد دور می‌کند")
		case p.tunRTT > 0 && p.tunRTT > pg.Avg+40 || p.hasTunLoss && p.tunLoss >= 3:
			f.Sev = "WARN"
			f.Why = tr("the bare path is clean but the tunnel's links are slower/lossier: the delay is added on this server or the peer (CPU, a full NIC core, queues in xray)",
				"مسیر خام تمیز است ولی لینک‌های تونل کندتر/پرافت‌ترند: تأخیر روی همین سرور یا سرور مقابل اضافه می‌شود (پردازنده، هستهٔ پر کارت شبکه، صف در xray)")
			f.Do = tr("look at the CPU/NIC findings here and run the deep doctor on the other server too", "یافته‌های پردازنده/کارت شبکه را ببینید و پزشک سرور را روی سرور مقابل هم اجرا کنید")
		default:
			f.Sev = "OK"
		}
		out = append(out, f)
	}
	if judged > 0 && congested == judged {
		out = append(out, finding{Sev: "WARN", Area: tr("whole network", "کل شبکه"),
			What: fmt.Sprintf(tr("all %d path(s) ping far above their lowest RTT right now", "همهٔ %d مسیر الان خیلی بالاتر از کمترین RTT خودشان ping می‌دهند"), judged),
			Why:  tr("the whole network between the two servers is congested now (typical at night peak); nothing in hs2 can fix this", "کل شبکهٔ بین دو سرور الان شلوغ است (معمولاً اوج شب)؛ هیچ تنظیمی در hs2 این را درست نمی‌کند"),
			Do:   tr("wait for off-peak, or add a path through another provider", "تا بعد از ساعت اوج صبر کنید یا مسیری از دیتاسنتر دیگری اضافه کنید")})
	}
	return out
}

// ---------------------------------------------------------------- 4. Reality dest

var xrayConfigPaths = []string{
	"/usr/local/x-ui/bin/config.json",
	"/usr/local/etc/xray/config.json",
	"/etc/xray/config.json",
	"/usr/local/etc/v2ray/config.json",
}

// realityInbound is what the doctor needs from one Reality inbound (never
// keys, ids or passwords).
type realityInbound struct {
	Tag, Dest       string
	Port            int
	LimitedFallback bool
}

func parseRealityInbounds(b []byte) []realityInbound {
	var cfg struct {
		Inbounds []struct {
			Tag            string          `json:"tag"`
			Port           json.RawMessage `json:"port"`
			StreamSettings struct {
				Security        string `json:"security"`
				RealitySettings struct {
					Dest    json.RawMessage `json:"dest"`
					Target  json.RawMessage `json:"target"`
					LimitUp *struct {
						BytesPerSec int64 `json:"bytesPerSec"`
					} `json:"limitFallbackUpload"`
					LimitDown *struct {
						BytesPerSec int64 `json:"bytesPerSec"`
					} `json:"limitFallbackDownload"`
				} `json:"realitySettings"`
			} `json:"streamSettings"`
		} `json:"inbounds"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return nil
	}
	var out []realityInbound
	for _, in := range cfg.Inbounds {
		rs := in.StreamSettings.RealitySettings
		if in.StreamSettings.Security != "reality" {
			continue
		}
		dest := rawStr(rs.Dest)
		if dest == "" {
			dest = rawStr(rs.Target)
		}
		if dest == "" {
			continue
		}
		port, _ := strconv.Atoi(strings.Trim(string(in.Port), `"`))
		lim := (rs.LimitUp != nil && rs.LimitUp.BytesPerSec > 0) || (rs.LimitDown != nil && rs.LimitDown.BytesPerSec > 0)
		out = append(out, realityInbound{Tag: in.Tag, Dest: dest, Port: port, LimitedFallback: lim})
	}
	return out
}

func rawStr(r json.RawMessage) string {
	if len(r) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(r, &s) == nil {
		return s
	}
	var n int
	if json.Unmarshal(r, &n) == nil {
		return strconv.Itoa(n)
	}
	return ""
}

// multiTenantCDN reports the CDN when the dest host is on a shared CDN whose
// edge serves any customer's domain (so the inbound works as a clean front).
func multiTenantCDN(host string) string {
	h := strings.ToLower(host)
	for _, c := range []struct{ key, name string }{
		{"cloudflare", "Cloudflare"}, {"fastly", "Fastly"}, {"akamai", "Akamai"}, {"edgekey", "Akamai"},
		{"cloudfront", "CloudFront"}, {"google", "Google"}, {"gstatic", "Google"}, {"youtube", "Google"},
		{"azureedge", "Azure CDN"}, {"cdn77", "CDN77"}, {"gcore", "Gcore"}, {"bunny", "Bunny"},
		{"discord", "Cloudflare"}, {"speedtest", "Cloudflare"},
	} {
		if strings.Contains(h, c.key) {
			return c.name
		}
	}
	return ""
}

var cloudflareNets = []string{"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22", "141.101.64.0/18",
	"108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20", "197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15",
	"104.16.0.0/13", "104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22"}

// procNetConn is an established TCP connection from /proc/net/tcp{,6}.
type procNetConn struct {
	Remote    net.IP
	Port      int
	LocalPort int
	Inode     string
}

// parseProcNetTCP returns the ESTABLISHED rows of /proc/net/tcp or tcp6.
func parseProcNetTCP(b []byte) []procNetConn {
	var out []procNetConn
	for _, line := range strings.Split(string(b), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 10 || f[3] != "01" {
			continue
		}
		ip, port, ok := parseHexAddr(f[2])
		if !ok {
			continue
		}
		_, lport, _ := parseHexAddr(f[1])
		out = append(out, procNetConn{ip, port, lport, f[9]})
	}
	return out
}

func parseHexAddr(s string) (net.IP, int, bool) {
	h, p, ok := strings.Cut(s, ":")
	if !ok {
		return nil, 0, false
	}
	port, err := strconv.ParseUint(p, 16, 16)
	if err != nil {
		return nil, 0, false
	}
	raw := make([]byte, len(h)/2)
	for i := range raw {
		x, err := strconv.ParseUint(h[2*i:2*i+2], 16, 8)
		if err != nil {
			return nil, 0, false
		}
		raw[i] = byte(x)
	}
	// /proc stores each 32-bit word in host (little-endian) order.
	for i := 0; i+4 <= len(raw); i += 4 {
		raw[i], raw[i+1], raw[i+2], raw[i+3] = raw[i+3], raw[i+2], raw[i+1], raw[i]
	}
	ip := net.IP(raw)
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip, int(port), true
}

func procSocketInodes(proc string, pid int) map[string]bool {
	out := map[string]bool{}
	dir := filepath.Join(proc, strconv.Itoa(pid), "fd")
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		l, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err == nil && strings.HasPrefix(l, "socket:[") {
			out[strings.TrimSuffix(strings.TrimPrefix(l, "socket:["), "]")] = true
		}
	}
	return out
}

func checkReality(env *deepEnv) []finding {
	area := tr("Reality dest", "مقصد Reality")
	var raw []byte
	var from string
	for _, p := range xrayConfigPaths {
		if b, err := os.ReadFile(p); err == nil {
			raw, from = b, p
			break
		}
	}
	if raw == nil {
		return []finding{{Sev: "SKIP", Area: area, What: tr("no xray/x-ui config on this server", "کانفیگ xray/x-ui روی این سرور نیست")}}
	}
	ins := parseRealityInbounds(raw)
	if len(ins) == 0 {
		return []finding{{Sev: "OK", Area: area, What: tr("no Reality inbound in ", "اینباند Reality در این فایل نیست: ") + from}}
	}
	// xray's established connections.
	inodes := map[string]bool{}
	for _, p := range listProcs(env.proc) {
		if strings.HasPrefix(strings.ToLower(p.comm), "xray") {
			for k := range procSocketInodes(env.proc, p.pid) {
				inodes[k] = true
			}
		}
	}
	var conns []procNetConn
	for _, n := range []string{"tcp", "tcp6"} {
		if b, err := os.ReadFile(filepath.Join(env.proc, "net", n)); err == nil {
			for _, c := range parseProcNetTCP(b) {
				if inodes[c.Inode] {
					conns = append(conns, c)
				}
			}
		}
	}
	var out []finding
	for _, in := range ins {
		host, port, err := net.SplitHostPort(in.Dest)
		if err != nil {
			host, port = in.Dest, "443"
		}
		pn, _ := strconv.Atoi(port)
		f := finding{Area: area + " " + orDash(in.Tag)}
		cdn := multiTenantCDN(host)
		if cdn == "" {
			f.Sev, f.What = "OK", fmt.Sprintf(tr("dest %s (not a known multi-tenant CDN)", "مقصد %s (CDN اشتراکی شناخته‌شده نیست)"), in.Dest)
			out = append(out, f)
			continue
		}
		ips := map[string]bool{}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		addrs, _ := net.DefaultResolver.LookupIPAddr(ctx, host)
		cancel()
		for _, a := range addrs {
			ips[a.IP.String()] = true
		}
		var cfNets []*net.IPNet
		if cdn == "Cloudflare" {
			for _, s := range cloudflareNets {
				_, n, _ := net.ParseCIDR(s)
				cfNets = append(cfNets, n)
			}
		}
		toDest := 0
		for _, c := range conns {
			if c.Port != pn {
				continue
			}
			hit := ips[c.Remote.String()]
			for _, n := range cfNets {
				if n.Contains(c.Remote) {
					hit = true
				}
			}
			if hit {
				toDest++
			}
		}
		f.What = fmt.Sprintf(tr("dest %s is on %s (shared by every customer); %d of xray's %d established connections go to it",
			"مقصد %s روی %s است (مشترک بین همهٔ مشتری‌ها)؛ %d از %d اتصال برقرار xray به آن می‌رود"), in.Dest, cdn, toDest, len(conns))
		if in.LimitedFallback {
			f.What += tr("; fallback is rate-limited", "؛ fallback محدودسرعت دارد")
		}
		switch {
		case len(inodes) == 0:
			f.Sev = "INFO"
			f.What += tr(" (xray process not found — counted nothing)", " (پردازهٔ xray پیدا نشد — شمارشی انجام نشد)")
		case toDest >= 20 || (len(conns) >= 20 && toDest*10 >= len(conns)):
			f.Sev = "WARN"
			if !in.LimitedFallback && toDest >= 100 {
				f.Sev = "BAD"
			}
		default:
			f.Sev = "INFO"
		}
		if f.Sev == "WARN" || f.Sev == "BAD" || !in.LimitedFallback {
			f.Why = fmt.Sprintf(tr("anything that is not your client is proxied to %s: strangers can use your IP as a \"clean IP\" for their own %s proxies and eat your bandwidth and CPU",
				"هر چیزی که کلاینت شما نیست به %s فرستاده می‌شود: غریبه‌ها می‌توانند IP شما را «آی‌پی تمیز» برای پروکسی‌های %s خودشان کنند و پهنای باند و پردازندهٔ شما را بخورند"), host, cdn)
			f.Do = tr("set limitFallbackUpload/limitFallbackDownload (e.g. afterBytes 1 MB, bytesPerSec 50 KB) on this inbound, or pick a single-tenant dest (a site not behind a shared CDN)",
				"روی این اینباند limitFallbackUpload/limitFallbackDownload بگذارید (مثلاً afterBytes یک مگابایت، bytesPerSec پنجاه کیلوبایت) یا dest تک‌مستأجر انتخاب کنید (سایتی که پشت CDN اشتراکی نیست)")
		}
		out = append(out, f)
	}
	return out
}

// ---------------------------------------------------------------- 5. x-ui

// xuiCounts counts the panel's user removals and node-sync errors in a
// journal dump.
func xuiCounts(log string) (removes, syncErrs int) {
	sc := bufio.NewScanner(strings.NewReader(log))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		l := sc.Text()
		ll := strings.ToLower(l)
		if strings.Contains(ll, "remove inbound user") {
			removes++
		}
		if (strings.Contains(ll, "reconcile") && strings.Contains(ll, "fail")) || strings.Contains(ll, "context deadline exceeded") {
			syncErrs++
		}
	}
	return
}

func checkXUI(env *deepEnv) []finding {
	area := tr("x-ui panel", "پنل x-ui")
	if _, err := os.Stat("/usr/local/x-ui"); err != nil {
		if out, err := env.run(3*time.Second, "systemctl", "cat", "x-ui"); err != nil || out == "" {
			return []finding{{Sev: "SKIP", Area: area, What: tr("x-ui is not installed here", "x-ui روی این سرور نصب نیست")}}
		}
	}
	out, err := env.run(10*time.Second, "journalctl", "-u", "x-ui", "--since", "-1h", "--no-pager", "-o", "cat", "-q")
	if err != nil && out == "" {
		return []finding{{Sev: "SKIP", Area: area, What: tr("cannot read the x-ui journal (journalctl missing or no permission)", "ژورنال x-ui خوانده نشد (journalctl نیست یا دسترسی نیست)")}}
	}
	rm, se := xuiCounts(out)
	var fs []finding
	f := finding{Area: area, What: fmt.Sprintf(tr("last hour: %d \"Remove Inbound User\" line(s), %d node-sync error(s)", "ساعت گذشته: %d خط «Remove Inbound User» و %d خطای همگام‌سازی نود"), rm, se)}
	switch {
	case rm >= 60:
		f.Sev = "WARN"
		f.Why = tr("the panel is constantly removing users (expired/over-quota clients) and rewriting xray's config: each change costs CPU and can stall live users",
			"پنل مدام کاربر حذف می‌کند (کاربران منقضی/پرشده) و کانفیگ xray را بازنویسی می‌کند: هر تغییر پردازنده مصرف می‌کند و ممکن است کاربران فعال را لحظه‌ای متوقف کند")
		f.Do = tr("delete expired / depleted clients from the panel instead of letting it remove them every cycle", "کاربران منقضی و تمام‌شده را خودتان از پنل پاک کنید تا پنل هر دوره آن‌ها را حذف نکند")
	default:
		f.Sev = "OK"
	}
	fs = append(fs, f)
	if se >= 5 {
		fs = append(fs, finding{Sev: "WARN", Area: area,
			What: fmt.Sprintf(tr("%d node-sync errors (reconcile failed / context deadline exceeded) in the last hour", "%d خطای همگام‌سازی نود (reconcile failed / context deadline exceeded) در ساعت گذشته"), se),
			Why:  tr("the panel cannot reach a node in time: user changes do not land there and the panel retries in a loop", "پنل به‌موقع به نود نمی‌رسد: تغییرات کاربران آن‌جا اعمال نمی‌شود و پنل پشت‌سرهم تلاش می‌کند"),
			Do:   tr("check the node is up and its API port is reachable from the panel (firewall, wrong port, the tunnel to it)", "بررسی کنید نود روشن است و پورت API آن از پنل در دسترس است (فایروال، پورت اشتباه، تونل تا آن)")})
	}
	return fs
}

// ---------------------------------------------------------------- 6. hs2 health

// countPanics counts Go panics and fatal errors in a journal dump.
func countPanics(log string) int {
	n := 0
	for _, l := range strings.Split(log, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "panic:") || strings.Contains(l, "fatal error:") {
			n++
		}
	}
	return n
}

func checkHS2Health(env *deepEnv, st []namedStatus) []finding {
	area := tr("hs2", "hs2")
	var out []finding
	if len(st) == 0 {
		out = append(out, finding{Sev: "INFO", Area: area, What: tr("no hs2 status file in "+env.runDir+" — no tunnel running here", "فایل وضعیت hs2 در "+env.runDir+" نیست — تونلی این‌جا اجرا نمی‌شود")})
	}
	for _, s := range st {
		ls := s.ls
		f := finding{Area: area + " " + s.file}
		serving := ls.Links
		if ls.Serving != nil {
			serving = *ls.Serving
		}
		f.What = fmt.Sprintf(tr("%s %s: %d/%d link(s) serving, %d user(s), %.0f Mbit/s", "%s %s: %d از %d لینک فعال، %d کاربر، %.0f مگابیت"), ls.Role, ls.Carrier, serving, ls.Target, ls.Users, ls.Mbit)
		if ls.Target == 0 {
			f.What = fmt.Sprintf(tr("%s %s: %d link(s) up, %d user(s), %.0f Mbit/s", "%s %s: %d لینک بالا، %d کاربر، %.0f مگابیت"), ls.Role, ls.Carrier, ls.Links, ls.Users, ls.Mbit)
		}
		switch {
		case s.age > 30:
			f.Sev = "BAD"
			f.What = fmt.Sprintf(tr("status is %ds old — the tunnel is probably stopped", "وضعیت %d ثانیه قدیمی است — احتمالاً تونل متوقف شده"), s.age)
			f.Do = tr("systemctl status hs2…; look at the log for why it stopped", "systemctl status hs2… را ببینید و در لاگ دلیل توقف را پیدا کنید")
		case ls.Target > 0 && serving == 0:
			f.Sev = "BAD"
			f.Why = tr("no link carries users: the tunnel is down", "هیچ لینکی کاربر را حمل نمی‌کند: تونل قطع است")
			f.Do = tr("check the other server is up and reachable (path findings below), then the log", "بررسی کنید سرور مقابل روشن و در دسترس است (یافته‌های مسیر در پایین) و بعد لاگ را ببینید")
		case ls.Target > 0 && serving*2 < ls.Target:
			f.Sev = "WARN"
			f.Why = tr("fewer than half the wanted links are up: dials fail or links die", "کمتر از نصف لینک‌های خواسته‌شده بالا هستند: اتصال‌ها شکست می‌خورند یا لینک‌ها می‌میرند")
			f.Do = tr("see the path findings below and `hs2 status`", "یافته‌های مسیر در پایین و `hs2 status` را ببینید")
		default:
			f.Sev = "OK"
		}
		out = append(out, f)
		for _, v := range ls.LBPaths {
			if v.AutoReason == "" && (v.State == "ok" || v.State == "") {
				continue
			}
			if v.AutoFactor >= 1 && (v.State == "ok" || v.State == "") {
				continue
			}
			out = append(out, finding{Sev: "INFO", Area: tr("LB path ", "مسیر LB ") + v.Local + " → " + v.Remote,
				What: fmt.Sprintf(tr("state %s, factor %.2f %s", "وضعیت %s، ضریب %.2f %s"), orDash(v.State), v.AutoFactor, v.AutoReason),
				Why:  tr("hs2 lowered or cut this path on its own because of its measured quality", "hs2 خودش به‌خاطر کیفیت اندازه‌گیری‌شده سهم این مسیر را کم یا قطع کرده است")})
		}
	}
	// Restarts and panics of the hs2 units.
	units, err := env.run(4*time.Second, "systemctl", "list-units", "hs2*", "--all", "--no-legend", "--plain")
	if err != nil && units == "" {
		return out
	}
	for _, l := range strings.Split(units, "\n") {
		f := strings.Fields(l)
		if len(f) == 0 || !strings.HasSuffix(f[0], ".service") {
			continue
		}
		u := f[0]
		nr := 0
		if s, err := env.run(3*time.Second, "systemctl", "show", "-p", "NRestarts", "--value", u); err == nil {
			nr, _ = strconv.Atoi(strings.TrimSpace(s))
		}
		jl, _ := env.run(8*time.Second, "journalctl", "-u", u, "--since", "-1h", "--no-pager", "-o", "cat", "-q")
		np := countPanics(jl)
		fi := finding{Area: area + " " + u, What: fmt.Sprintf(tr("%d restart(s) since boot, %d panic(s) in the last hour", "%d ری‌استارت از زمان بوت، %d panic در ساعت گذشته"), nr, np)}
		switch {
		case np > 0:
			fi.Sev = "BAD"
			fi.Why = tr("the program crashed: every user was cut at that moment", "برنامه کرش کرده: همهٔ کاربران در آن لحظه قطع شده‌اند")
			fi.Do = tr("send the panic lines (journalctl -u "+u+" | grep -A30 panic) to the developer", "خطوط panic را (journalctl -u "+u+" | grep -A30 panic) برای توسعه‌دهنده بفرستید")
		case nr >= 3:
			fi.Sev = "WARN"
			fi.Why = tr("the unit keeps restarting: users are cut each time", "سرویس مدام ری‌استارت می‌شود: هر بار کاربران قطع می‌شوند")
			fi.Do = tr("look in the log for why it exits (journalctl -u "+u+")", "در لاگ دلیل خروج را پیدا کنید (journalctl -u "+u+")")
		default:
			fi.Sev = "OK"
		}
		out = append(out, fi)
	}
	return out
}

// ---------------------------------------------------------------- helpers

func uniq(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range s {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
