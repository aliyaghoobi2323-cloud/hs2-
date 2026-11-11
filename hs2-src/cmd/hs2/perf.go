package main

// Menu M4, section 5 "Speed & performance": `hs2 perf …`. The menu renders
// this output. Presets and feature toggles are Environment= lines in ONE
// drop-in per tunnel unit (50-hs2-perf.conf); CPUAffinity is its own drop-in
// (60-hs2-cpu.conf). Both go through the M1 safe apply with the drop-ins
// (diff, health gate, automatic rollback restores the previous drop-ins).
//
//	hs2 perf show    -u U                     effective keys: explicit / default / off
//	hs2 perf presets                          the presets and what each trades
//	hs2 perf preset  -u U NAME [--confirm-within 5m] [--yes] [--dry-run]
//	hs2 perf set     -u U KEY=VALUE [KEY=VALUE…]   (documented keys only)
//	hs2 perf unset   -u U KEY [KEY…]
//	hs2 perf nic                              queues, rx-IRQ core, softirq/idle per core (~2 s)
//	hs2 perf cores   -u U --keep N | --clear  CPUAffinity drop-in for an hs2 unit (never x-ui/xray)
//	hs2 perf kernel  [--budget 600 [--measure 10s]] [--revert-budget]
//	hs2 perf speedtest --port P [--secs 5] [--gap 3]   iperf3 through the tunnel (127.0.0.1:P)

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/envdef"
)

const (
	perfDropin = "50-hs2-perf.conf"
	cpuDropin  = "60-hs2-cpu.conf"
)

var (
	procDir        = "/proc"
	sysDir         = "/sys"
	sysctlDir      = "/etc/sysctl.d"
	budgetConfName = "90-hs2-netdev-budget.conf"
)

// perfKey: one documented key with its one-line plain explanation.
type perfKey struct {
	Name, En, Fa string
	Side         envdef.Side
	Default      string // the production default ("" = off unless set)
	Advanced     bool   // simple mode hides it
}

var perfKeys = func() []perfKey {
	ex := map[string][2]string{
		"HS2_SMUX_AUTOWIN":          {"window of a heavy stream grows while there is no queue (single-connection speed)", "پنجرهٔ جریان سنگین وقتی صف نیست بزرگ می‌شود (سرعت تک‌اتصال)"},
		"HS2_SMUX_AUTOWIN_REL":      {"…and shrinks at once when a light user shares the link", "…و با آمدن کاربر سبک روی همان لینک فوراً کوچک می‌شود"},
		"HS2_SMUX_AUTOWIN_SHARE_MB": {"total budget of all grown windows, MiB (memory vs speed)", "بودجهٔ کل پنجره‌های بزرگ‌شده، مگابایت (حافظه در برابر سرعت)"},
		"HS2_STRIPE_AW_SOLO":        {"Iran: a non-striped connection keeps the adaptive window", "ایران: اتصالِ پخش‌نشده پنجرهٔ تطبیقی را نگه می‌دارد"},
		"HS2_STRIPE_AW_HEAVY":       {"Iran: every stripe of a big download keeps the adaptive window", "ایران: همهٔ تکه‌های یک دانلود بزرگ پنجرهٔ تطبیقی دارند"},
		"HS2_MCONN_STRIPE":          {"spread one big connection over up to N links (0 = off)", "پخش یک اتصال بزرگ روی حداکثر N لینک (۰ = خاموش)"},
		"HS2_MCONN_STRIPE_JUMP_MS":  {"kharej: stripe jump delay, ms (no measurable gain; parity)", "خارج: تأخیر پرش تکه‌ها، میلی‌ثانیه (بی‌اثر محسوس)"},
		"HS2_LB_AUTO2":              {"automatic path judge: lowers or cuts lossy paths", "داور خودکار مسیر: مسیرهای پرافت را کم یا قطع می‌کند"},
		"HS2_LB_FAST_RECOVER":       {"a path clean 3 readings in a row comes back sooner", "مسیری که ۳ بار پشت‌سرهم سالم است زودتر برمی‌گردد"},
		"HS2_LB_START_GRACE_S":      {"no whole-IP cut in the first N s after start", "تا N ثانیه پس از شروع هیچ IPای کامل قطع نمی‌شود"},
		"HS2_LB_FILL_PCT":           {"kharej: healthy paths may fill up to target + N %", "خارج: مسیرهای سالم تا هدف + N٪ پر می‌شوند"},
		"HS2_LB_SPARE_PER_ROUND":    {"kharej: spare links asked per rebalance round", "خارج: لینک یدکی در هر دور توازن"},
		"HS2_LB_REC_CONFIRM":        {"kharej: one bad tiny reading does not restart a cut path's recovery", "خارج: یک خوانش بد کوچک بازگشت مسیر قطع‌شده را از نو شروع نمی‌کند"},
		"HS2_LB_FLAP_PEN":           {"kharej: a flapping path is kept out longer (30 min, up to 2 h)", "خارج: مسیر نوسانی مدت بیشتری بیرون می‌ماند (۳۰ دقیقه تا ۲ ساعت)"},
		"HS2_L3_DUP":                {"duplicate tiny packets of calls/games on a second link", "تکرار بسته‌های کوچک تماس/بازی روی لینک دوم"},
		"HS2_MCONN":                 {"multi-connection mode (lite = production)", "حالت چنداتصالی (lite = پیش‌فرض)"},
	}
	var out []perfKey
	seen := map[string]bool{}
	for _, k := range envdef.Table {
		e, ok := ex[k.Name]
		pk := perfKey{Name: k.Name, Side: k.Side, Default: k.Value, Advanced: !ok}
		if ok {
			pk.En, pk.Fa = e[0], e[1]
		} else {
			pk.En, pk.Fa = "production default (see README)", "پیش‌فرض تولید (README)"
		}
		out = append(out, pk)
		seen[k.Name] = true
	}
	for _, n := range []string{"HS2_MCONN_STRIPE_JUMP_MS"} {
		if !seen[n] {
			e := ex[n]
			out = append(out, perfKey{Name: n, En: e[0], Fa: e[1], Side: envdef.Exit})
		}
	}
	for _, n := range []string{"HS2_LB_FLAP_N", "HS2_LB_FLAP_WIN_S", "HS2_LB_FLAP_HOLD_S", "HS2_LB_FLAP_MAX_S", "HS2_LB_FLAP_PROB_S", "HS2_LB_FLAP_EARLY_DIV"} {
		out = append(out, perfKey{Name: n, En: "flap-penalty tuning (rarely needed)", Fa: "تنظیم جریمهٔ نوسان (به‌ندرت لازم)", Side: envdef.Exit, Advanced: true})
	}
	out = append(out, perfKey{Name: "HS2_DEFAULTS", En: "legacy = all production defaults off", Fa: "legacy = همهٔ پیش‌فرض‌های تولید خاموش", Side: envdef.Both, Advanced: true})
	return out
}()

func perfKeyByName(n string) (perfKey, bool) {
	for _, k := range perfKeys {
		if k.Name == n {
			return k, true
		}
	}
	return perfKey{}, false
}

// ---- presets ------------------------------------------------------------------

type perfPreset struct {
	Name, En, Fa string
	Keys         map[string]string
}

var perfPresets = []perfPreset{
	{"production", "recommended: the code defaults (A/B-tested, light users never slower); removes the preset/toggle drop-in",
		"پیشنهادی: پیش‌فرض‌های کد (آزمون A/B، کاربر سبک هرگز کندتر نشد)؛ drop-in پیش‌تنظیم/کلیدها حذف می‌شود", nil},
	{"low-cpu", "less CPU per Mbit: no striping, fixed 2 MiB windows, no call duplication — one connection tops out near ~200 Mbit/s at 80 ms",
		"CPU کمتر به‌ازای هر مگابیت: بدون پخش، پنجرهٔ ثابت ۲ مگ، بدون تکرار تماس — یک اتصال حدود ۲۰۰ مگابیت در ۸۰ میلی‌ثانیه سقف می‌خورد",
		map[string]string{"HS2_MCONN_STRIPE": "0", "HS2_SMUX_AUTOWIN": "0", "HS2_STRIPE_AW_SOLO": "0", "HS2_STRIPE_AW_HEAVY": "0", "HS2_L3_DUP": "0"}},
	{"max-throughput", "EXPERIMENTAL, not recommended — bigger window budget (64 MiB) + stripe jump: more memory; light users may queue a little more under very heavy load (not lab-proven beyond the defaults)",
		"آزمایشی، توصیه نمی‌شود — بودجهٔ پنجرهٔ بزرگ‌تر (۶۴ مگ) + پرش تکه: حافظهٔ بیشتر؛ زیر بار بسیار سنگین کاربر سبک ممکن است کمی بیشتر در صف بماند (فراتر از پیش‌فرض‌ها در آزمایشگاه اثبات نشده)",
		map[string]string{"HS2_SMUX_AUTOWIN_SHARE_MB": "64", "HS2_MCONN_STRIPE_JUMP_MS": "1500"}},
	{"min-latency", "smaller window budget (16 MiB), stripes over 4 links, no heavy-stripe window: big downloads slower, less queueing for calls/games",
		"بودجهٔ پنجرهٔ کوچک‌تر (۱۶ مگ)، پخش روی ۴ لینک، بدون پنجرهٔ تکه‌های سنگین: دانلود بزرگ کندتر، صف کمتر برای تماس/بازی",
		map[string]string{"HS2_SMUX_AUTOWIN_SHARE_MB": "16", "HS2_MCONN_STRIPE": "4", "HS2_STRIPE_AW_HEAVY": "0"}},
}

func presetByName(n string) (perfPreset, bool) {
	for _, p := range perfPresets {
		if p.Name == n {
			return p, true
		}
	}
	return perfPreset{}, false
}

func sideOK(s envdef.Side, edge bool) bool {
	return s == envdef.Both || (s == envdef.Edge && edge) || (s == envdef.Exit && !edge)
}

// presetEnv: the preset's keys that apply to this side.
func presetEnv(p perfPreset, edge bool) map[string]string {
	out := map[string]string{}
	for k, v := range p.Keys {
		if pk, ok := perfKeyByName(k); ok && !sideOK(pk.Side, edge) {
			continue
		}
		out[k] = v
	}
	return out
}

// renderPerfDropin: the drop-in for env (nil/empty = no file).
func renderPerfDropin(env map[string]string, preset string) string {
	if len(env) == 0 {
		return ""
	}
	ks := make([]string, 0, len(env))
	for k := range env {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var b strings.Builder
	b.WriteString("# hs2 perf (menu 5) — preset: " + preset + "; change with `hs2 perf`, not by hand\n[Service]\n")
	for _, k := range ks {
		fmt.Fprintf(&b, "Environment=%s=%s\n", k, env[k])
	}
	return b.String()
}

func perfPresetOf(content string) string {
	if i := strings.Index(content, "preset: "); i >= 0 {
		s := content[i+8:]
		if j := strings.IndexAny(s, ";\n"); j >= 0 {
			return s[:j]
		}
	}
	return "custom"
}

// ---- effective keys -------------------------------------------------------------

type effKey struct {
	Key    perfKey
	Value  string
	Source string // "explicit (file)", "default", "off"
}

// effectiveKeys: for each documented key, the value the daemon will run with.
func effectiveKeys(fc fileConfig, dropins map[string]string) []effKey {
	explicit := map[string][2]string{}
	names := make([]string, 0, len(dropins))
	for f := range dropins {
		names = append(names, f)
	}
	sort.Strings(names) // systemd applies drop-ins in name order: the last wins
	for _, f := range names {
		for k, v := range envOfDropin(dropins[f]) {
			if strings.HasPrefix(k, "HS2_") {
				explicit[k] = [2]string{v, f}
			}
		}
	}
	edge := fc.Mode == "dial"
	legacy := false
	if v, ok := explicit["HS2_DEFAULTS"]; ok {
		switch strings.ToLower(v[0]) {
		case "legacy", "0", "off", "false", "no":
			legacy = true
		}
	}
	defs := defaultsApply(fc) && !legacy
	var out []effKey
	for _, k := range perfKeys {
		if !sideOK(k.Side, edge) {
			continue
		}
		e := effKey{Key: k, Source: "off"}
		if v, ok := explicit[k.Name]; ok {
			e.Value, e.Source = v[0], "explicit ("+v[1]+")"
		} else if defs && k.Default != "" {
			e.Value, e.Source = k.Default, "default"
		}
		out = append(out, e)
	}
	return out
}

func renderEffective(es []effKey, advanced bool) string {
	var b strings.Builder
	for _, e := range es {
		if e.Key.Advanced && !advanced && !strings.HasPrefix(e.Source, "explicit") {
			continue
		}
		v := e.Value
		if v == "" {
			v = "-"
		}
		src := map[string]string{"default": tr("default", "پیش‌فرض"), "off": tr("off", "خاموش")}[e.Source]
		if src == "" {
			src = tr("set: ", "تنظیم‌شده: ") + strings.TrimSuffix(strings.TrimPrefix(e.Source, "explicit ("), ")")
		}
		fmt.Fprintf(&b, "%-28s %-6s %-26s %s\n", e.Key.Name, v, src, tr(e.Key.En, e.Key.Fa))
	}
	return b.String()
}

// ---- NIC / cores -------------------------------------------------------------------

type nicInfo struct {
	Iface   string
	Queues  int
	RxCPU   int     // -1 unknown
	RxShare float64 // share of the rx interrupts on RxCPU
	NCPU    int
	Cores   map[int]coreShare
}

// detectNIC: queues from sysfs (or ethtool's current "Combined"), the rx-IRQ
// core from two /proc/interrupts samples, softirq/idle per core from two
// /proc/stat samples.
func detectNIC(proc, sys, ethtool string, stat1, stat2, irq1, irq2 []byte) nicInfo {
	n := nicInfo{RxCPU: -1}
	n.Iface = defaultIface(proc)
	if n.Iface == "" {
		return n
	}
	rxq, _ := filepath.Glob(filepath.Join(sys, "class", "net", n.Iface, "queues", "rx-*"))
	n.Queues = len(rxq)
	if c := ethtoolCombined(ethtool); c > 0 {
		n.Queues = c
	}
	names := []string{n.Iface}
	if l, err := os.Readlink(filepath.Join(sys, "class", "net", n.Iface, "device")); err == nil {
		names = append(names, filepath.Base(l)+"-")
	}
	per, _ := nicIRQCPUs(parseInterrupts(irq1), parseInterrupts(irq2), names)
	var sum, hot uint64
	for i, c := range per {
		sum += c
		if c > hot {
			hot, n.RxCPU = c, i
		}
	}
	if sum > 0 {
		n.RxShare = float64(hot) / float64(sum)
	} else {
		n.RxCPU = -1
	}
	n.Cores = perCPUDeltas(parsePerCPU(stat1), parsePerCPU(stat2))
	n.NCPU = len(n.Cores)
	return n
}

func (n nicInfo) singleRx() bool { return n.RxCPU >= 0 && (n.Queues <= 1 || n.RxShare >= 0.9) }

func sampleNIC(window time.Duration) nicInfo {
	r := func(p string) []byte { b, _ := os.ReadFile(filepath.Join(procDir, p)); return b }
	s1, i1 := r("stat"), r("interrupts")
	time.Sleep(window)
	s2, i2 := r("stat"), r("interrupts")
	ifc := defaultIface(procDir)
	et, _ := exec.Command("ethtool", "-l", ifc).Output()
	return detectNIC(procDir, sysDir, string(et), s1, s2, i1, i2)
}

// cpuAffinityFor: every core but keep, as systemd's CPUAffinity list.
func cpuAffinityFor(ncpu, keep int) (string, error) {
	if ncpu < 2 {
		return "", fmt.Errorf(tr("only %d core — nothing to keep free", "فقط %d هسته — چیزی برای آزاد نگه داشتن نیست"), ncpu)
	}
	if keep < 0 || keep >= ncpu {
		return "", fmt.Errorf(tr("core %d does not exist (0..%d)", "هستهٔ %d وجود ندارد (0..%d)"), keep, ncpu-1)
	}
	var s []string
	for i := 0; i < ncpu; i++ {
		if i != keep {
			s = append(s, strconv.Itoa(i))
		}
	}
	return strings.Join(s, " "), nil
}

func cpuDropinText(list string) string {
	return "# hs2 perf cores (menu 5): keep the rx-IRQ core free for the NIC\n[Service]\nCPUAffinity=" + list + "\n"
}

// hs2UnitOK: the cores/preset commands only ever touch hs2 tunnel units.
func hs2UnitOK(u string) bool {
	u = cleanUnit(u)
	if !strings.HasPrefix(u, "hs2") || strings.Contains(u, "/") {
		return false
	}
	for _, bad := range []string{"x-ui", "xray", "xui"} {
		if strings.Contains(u, bad) {
			return false
		}
	}
	return true
}

// ---- kernel ------------------------------------------------------------------------

var kernelKeys = []string{"net.ipv4.tcp_congestion_control", "net.core.default_qdisc", "net.core.rmem_max", "net.core.wmem_max",
	"net.ipv4.tcp_rmem", "net.ipv4.tcp_wmem", "net.core.netdev_budget", "net.core.netdev_budget_usecs", "net.core.netdev_max_backlog"}

func sysctlPath(k string) string {
	return filepath.Join(procDir, "sys", strings.ReplaceAll(k, ".", "/"))
}

func readSysctl(k string) string {
	b, err := os.ReadFile(sysctlPath(k))
	if err != nil {
		return "?"
	}
	return strings.Join(strings.Fields(string(b)), " ")
}

// budgetVerdict: keep the new netdev_budget only if the rx core's idle did
// not drop by more than 3 points (noise) and softirq did not grow by more
// (coreShare is in percent).
func budgetVerdict(before, after coreShare) (bool, string) {
	di := after.Idle - before.Idle
	ds := after.Softirq - before.Softirq
	why := fmt.Sprintf("idle %.0f%% → %.0f%%, softirq %.0f%% → %.0f%%", before.Idle, after.Idle, before.Softirq, after.Softirq)
	if di < -3 || ds > 3 {
		return false, why
	}
	return true, why
}

// ---- speed test ---------------------------------------------------------------------

// iperfMbit: the received rate of an `iperf3 -J` run, Mbit/s.
func iperfMbit(js []byte) (float64, error) {
	var r struct {
		End struct {
			SumReceived struct {
				BPS float64 `json:"bits_per_second"`
			} `json:"sum_received"`
		} `json:"end"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(js, &r); err != nil {
		return 0, err
	}
	if r.Error != "" {
		return 0, fmt.Errorf("%s", r.Error)
	}
	return r.End.SumReceived.BPS / 1e6, nil
}

type speedRun struct {
	Name    string
	Reverse bool
	P       int
}

var speedRuns = []speedRun{{"down1", true, 1}, {"down8", true, 8}, {"up1", false, 1}, {"up8", false, 8}}

func speedArgs(port, secs int, r speedRun) []string {
	a := []string{"-c", "127.0.0.1", "-p", strconv.Itoa(port), "-t", strconv.Itoa(secs), "-P", strconv.Itoa(r.P), "-J"}
	if r.Reverse {
		a = append(a, "-R")
	}
	return a
}

// ---- commands ----------------------------------------------------------------------

func perfCmd(args []string) {
	if len(args) == 0 {
		fmt.Println("usage: hs2 perf show|presets|preset|set|unset|nic|cores|kernel|speedtest …  (hs2 perf --supported)")
		os.Exit(2)
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("perf "+sub, flag.ExitOnError)
	unit := fs.String("u", "hs2", "tunnel")
	lang := langFlag(fs)
	adv := fs.Bool("advanced", false, "show every key")
	within := fs.Duration("confirm-within", 5*time.Minute, "trial time before the automatic rollback")
	yes := fs.Bool("yes", false, "apply without asking")
	dry := fs.Bool("dry-run", false, "show the change only")
	keep := fs.Int("keep", -1, "cores: the core to keep free for the NIC")
	clear := fs.Bool("clear", false, "cores: remove the CPUAffinity drop-in")
	budget := fs.Int("budget", 0, "kernel: set net.core.netdev_budget (e.g. 600)")
	measure := fs.Duration("measure", 10*time.Second, "kernel: measurement window before/after")
	revertB := fs.Bool("revert-budget", false, "kernel: remove the netdev_budget file, back to the previous value")
	port := fs.Int("port", 0, "speedtest: a user port on THIS server whose target is the peer's iperf3 (127.0.0.1:5201 there)")
	secs := fs.Int("secs", 5, "speedtest: seconds per run")
	gap := fs.Duration("gap", 3*time.Second, "speedtest: pause between runs")
	pos := parseInterleaved(fs, rest)
	setLang(*lang)
	switch sub {
	case "--supported":
		fmt.Println("perf-v1")
	case "presets":
		for _, p := range perfPresets {
			fmt.Printf("%-15s %s\n", p.Name, tr(p.En, p.Fa))
		}
	case "show":
		_, fc, _ := loadUnit(*unit)
		di := readDropins(*unit)
		pre := "production"
		if c, ok := di[perfDropin]; ok {
			pre = perfPresetOf(c)
		}
		fmt.Printf(tr("preset: %s · production defaults %s on this carrier/side\n", "پیش‌تنظیم: %s · پیش‌فرض‌های تولید روی این حامل/سمت %s\n"), pre,
			map[bool]string{true: tr("apply", "اعمال می‌شوند"), false: tr("do NOT apply (keys stay off unless set)", "اعمال نمی‌شوند (کلیدها خاموش مگر تنظیم شوند)")}[defaultsApply(fc)])
		fmt.Print(renderEffective(effectiveKeys(fc, di), *adv))
	case "preset", "set", "unset":
		os.Exit(perfChange(sub, *unit, pos, *within, *yes, *dry))
	case "nic":
		n := sampleNIC(2 * time.Second)
		fmt.Print(renderNIC(n))
	case "cores":
		os.Exit(perfCores(*unit, *keep, *clear, *within, *yes, *dry))
	case "kernel":
		os.Exit(perfKernel(*budget, *measure, *revertB, *yes))
	case "speedtest":
		os.Exit(perfSpeed(*port, *secs, *gap))
	default:
		fmt.Println("unknown perf command: " + sub)
		os.Exit(2)
	}
}

func renderNIC(n nicInfo) string {
	var b strings.Builder
	if n.Iface == "" {
		return tr("no default route — cannot tell the NIC\n", "مسیر پیش‌فرض نیست — کارت شبکه معلوم نیست\n")
	}
	fmt.Fprintf(&b, tr("NIC %s: %d queue(s); ", "کارت %s: %d صف؛ "), n.Iface, n.Queues)
	if n.RxCPU >= 0 {
		fmt.Fprintf(&b, tr("rx interrupts on core %d (%.0f%%)\n", "وقفه‌های دریافت روی هستهٔ %d (%.0f٪)\n"), n.RxCPU, 100*n.RxShare)
	} else {
		b.WriteString(tr("no rx interrupts seen in the sample\n", "در نمونه وقفهٔ دریافتی دیده نشد\n"))
	}
	ids := make([]int, 0, len(n.Cores))
	for i := range n.Cores {
		ids = append(ids, i)
	}
	sort.Ints(ids)
	for _, i := range ids {
		c := n.Cores[i]
		fmt.Fprintf(&b, "  cpu%-2d softirq %3.0f%%  idle %3.0f%%\n", i, c.Softirq, c.Idle)
	}
	if n.singleRx() && n.NCPU > 1 {
		fmt.Fprintf(&b, tr("Advice: keep core %d free for the NIC: `hs2 perf cores -u <tunnel> --keep %d` (safe apply, auto-rollback).\n",
			"پیشنهاد: هستهٔ %d را برای کارت شبکه آزاد نگه دارید: `hs2 perf cores -u <tunnel> --keep %d` (اعمال امن، برگشت خودکار).\n"), n.RxCPU, n.RxCPU)
		b.WriteString(tr("A panel's xray is NEVER changed by hs2. If you want, keep it off that core yourself: taskset -a -cp <other cores> $(pidof xray) (lost on its restart).\n",
			"xray پنل هرگز به‌دست hs2 تغییر نمی‌کند. اگر خواستید خودتان آن را از آن هسته دور کنید: taskset -a -cp <هسته‌های دیگر> $(pidof xray) (با ری‌استارتش از بین می‌رود).\n"))
	} else if n.Queues > 1 {
		b.WriteString(tr("Several queues spread the receive work — no core needs to be kept free.\n", "چند صف کار دریافت را پخش می‌کنند — لازم نیست هسته‌ای آزاد بماند.\n"))
	}
	return b.String()
}

func perfChange(sub, unit string, pos []string, within time.Duration, yes, dry bool) int {
	if !hs2UnitOK(unit) {
		fmt.Println(tr("ERROR: only hs2 tunnel units", "خطا: فقط واحدهای تونل hs2"))
		return 1
	}
	path, fc, cur := loadUnit(unit)
	di := readDropins(unit)
	nd := map[string]string{}
	for k, v := range di {
		nd[k] = v
	}
	env := envOfDropin(di[perfDropin])
	for k := range env {
		if strings.HasPrefix(k, "[") {
			delete(env, k)
		}
	}
	preset := perfPresetOf(di[perfDropin])
	if _, ok := di[perfDropin]; !ok {
		preset = "production"
	}
	edge := fc.Mode == "dial"
	switch sub {
	case "preset":
		if len(pos) != 1 {
			fmt.Println("usage: hs2 perf preset -u U production|low-cpu|max-throughput|min-latency")
			return 2
		}
		p, ok := presetByName(pos[0])
		if !ok {
			fmt.Println(tr("unknown preset: ", "پیش‌تنظیم ناشناخته: ") + pos[0])
			return 2
		}
		fmt.Printf("%s: %s\n", p.Name, tr(p.En, p.Fa))
		env, preset = presetEnv(p, edge), p.Name
	case "set":
		if len(pos) == 0 {
			fmt.Println("usage: hs2 perf set -u U KEY=VALUE …")
			return 2
		}
		for _, kv := range pos {
			k, v, ok := strings.Cut(kv, "=")
			pk, known := perfKeyByName(k)
			if !ok || !known || v == "" || strings.ContainsAny(v, " \t\"'\\$`\n") {
				fmt.Printf(tr("ERROR: %q — use KEY=VALUE with a documented key (hs2 perf show --advanced)\n", "خطا: %q — KEY=VALUE با کلید مستند (hs2 perf show --advanced)\n"), kv)
				return 1
			}
			if !sideOK(pk.Side, edge) {
				fmt.Printf(tr("note: %s acts on the %s side only — set it there\n", "توجه: %s فقط روی سمت %s اثر دارد — آن‌جا تنظیم کنید\n"), k, pk.Side)
			}
			env[k] = v
		}
		preset = "custom"
	case "unset":
		for _, k := range pos {
			delete(env, k)
		}
		preset = "custom"
	}
	if t := renderPerfDropin(env, preset); t != "" {
		nd[perfDropin] = t
	} else {
		delete(nd, perfDropin)
	}
	return safeApplyDropins(unit, path, cur, di, nd, "perf-"+sub, within, yes, dry)
}

// safeApplyDropins: diff of the drop-ins, then M1's safe apply.
func safeApplyDropins(unit, path string, cur []byte, oldDI, newDI map[string]string, reason string, within time.Duration, yes, dry bool) int {
	d, err := semanticDiff(cur, cur, oldDI, newDI)
	if err != nil {
		fmt.Println("ERROR: " + err.Error())
		return 1
	}
	if len(d) == 0 {
		fmt.Println(tr("no changes.", "تغییری نیست."))
		return 0
	}
	printDiff(d)
	if dry {
		fmt.Println(tr("dry run — nothing was changed.", "اجرای آزمایشی — هیچ تغییری داده نشد."))
		return 0
	}
	rel, err := acquireLock(os.Getpid(), false, time.Now())
	if err != nil {
		fmt.Println("ERROR: " + err.Error())
		return 1
	}
	defer rel()
	if !yes {
		in := asker{bufioReader(), os.Stdout}
		v, err := in.ask(tr("Apply now (auto-rollback unless confirmed)? [y/N]", "اکنون اعمال شود (بدون تأیید خودکار برمی‌گردد)؟ [y/N]"), "n")
		if err != nil || (v != "y" && v != "Y") {
			fmt.Println(tr("Not applied.", "اعمال نشد."))
			return 3
		}
	}
	st, _, err := startApply(realOps{}, applyReq{Unit: unit, Cfg: path, Reason: reason, New: cur, NewDropins: newDI, Within: within, HealthGate: true})
	if err != nil {
		fmt.Println("ERROR: " + err.Error())
		return 1
	}
	printState(st)
	fmt.Printf(tr("Confirm when it works: hs2 apply --confirm -u %s  (otherwise it rolls back by itself in %s)\n", "وقتی کار کرد تأیید کنید: hs2 apply --confirm -u %s  (وگرنه پس از %s خودش برمی‌گردد)\n"), cleanUnit(unit), within)
	return 0
}

func perfCores(unit string, keep int, clear bool, within time.Duration, yes, dry bool) int {
	if !hs2UnitOK(unit) {
		fmt.Println(tr("ERROR: only hs2 tunnel units are changed (never x-ui / xray)", "خطا: فقط واحدهای تونل hs2 تغییر می‌کنند (هرگز x-ui / xray)"))
		return 1
	}
	path, _, cur := loadUnit(unit)
	di := readDropins(unit)
	nd := map[string]string{}
	for k, v := range di {
		nd[k] = v
	}
	if clear {
		delete(nd, cpuDropin)
	} else {
		if keep < 0 {
			n := sampleNIC(2 * time.Second)
			fmt.Print(renderNIC(n))
			if !n.singleRx() {
				fmt.Println(tr("Nothing to do: the receive work is not on one core.", "کاری لازم نیست: کار دریافت روی یک هسته نیست."))
				return 0
			}
			keep = n.RxCPU
		}
		list, err := cpuAffinityFor(runtime.NumCPU(), keep)
		if err != nil {
			fmt.Println("ERROR: " + err.Error())
			return 1
		}
		nd[cpuDropin] = cpuDropinText(list)
	}
	return safeApplyDropins(unit, path, cur, di, nd, "perf-cores", within, yes, dry)
}

func rxCoreShare(core int, window time.Duration) (coreShare, bool) {
	s1, _ := os.ReadFile(filepath.Join(procDir, "stat"))
	time.Sleep(window)
	s2, _ := os.ReadFile(filepath.Join(procDir, "stat"))
	c, ok := perCPUDeltas(parsePerCPU(s1), parsePerCPU(s2))[core]
	return c, ok
}

func perfKernel(budget int, measure time.Duration, revert, yes bool) int {
	for _, k := range kernelKeys {
		fmt.Printf("%-34s %s\n", k, readSysctl(k))
	}
	conf := filepath.Join(sysctlDir, budgetConfName)
	if revert {
		b, err := os.ReadFile(conf)
		if err != nil {
			fmt.Println(tr("no netdev_budget file of hs2 perf — nothing to revert.", "فایل netdev_budget از hs2 perf نیست — چیزی برای برگرداندن نیست."))
			return 0
		}
		prev := "300"
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "# previous: ") {
				prev = strings.TrimSpace(strings.TrimPrefix(l, "# previous: "))
			}
		}
		os.Remove(conf)
		exec.Command("sysctl", "-w", "net.core.netdev_budget="+prev).Run()
		fmt.Println(tr("netdev_budget back to ", "netdev_budget برگشت به ") + prev)
		return 0
	}
	if budget == 0 {
		return 0
	}
	if budget < 300 || budget > 2000 {
		fmt.Println(tr("ERROR: --budget 300..2000 (600 is the value that helped)", "خطا: --budget بین ۳۰۰ و ۲۰۰۰ (۶۰۰ مقداری است که کمک کرد)"))
		return 1
	}
	if !yes {
		in := asker{bufioReader(), os.Stdout}
		v, err := in.ask(fmt.Sprintf(tr("Set netdev_budget=%d, measure %s before and after, revert if worse? [y/N]", "netdev_budget=%d تنظیم، %s قبل و بعد اندازه‌گیری، و اگر بدتر شد برگردد؟ [y/N]"), budget, measure), "n")
		if err != nil || (v != "y" && v != "Y") {
			fmt.Println(tr("Not applied.", "اعمال نشد."))
			return 3
		}
	}
	n := sampleNIC(2 * time.Second)
	core := n.RxCPU
	if core < 0 {
		core = 0
	}
	prev := readSysctl("net.core.netdev_budget")
	before, _ := rxCoreShare(core, measure)
	body := fmt.Sprintf("# hs2 perf kernel (menu 5): larger softirq budget per poll\n# previous: %s\nnet.core.netdev_budget = %d\n", prev, budget)
	os.MkdirAll(sysctlDir, 0o755)
	if err := writeFileAtomic(conf, []byte(body)); err != nil {
		fmt.Println("ERROR: " + err.Error())
		return 1
	}
	os.Chmod(conf, 0o644)
	if err := exec.Command("sysctl", "-w", fmt.Sprintf("net.core.netdev_budget=%d", budget)).Run(); err != nil {
		os.Remove(conf)
		fmt.Println("ERROR: sysctl: " + err.Error())
		return 1
	}
	after, _ := rxCoreShare(core, measure)
	keepIt, why := budgetVerdict(before, after)
	fmt.Printf(tr("rx core %d: %s\n", "هستهٔ دریافت %d: %s\n"), core, why)
	if !keepIt {
		os.Remove(conf)
		exec.Command("sysctl", "-w", "net.core.netdev_budget="+prev).Run()
		fmt.Println(tr("Worse — reverted to ", "بدتر شد — برگشت به ") + prev)
		return 1
	}
	fmt.Printf(tr("Kept: %s (survives reboot). Undo: hs2 perf kernel --revert-budget\n", "ماند: %s (پس از ری‌بوت هم). برگرداندن: hs2 perf kernel --revert-budget\n"), conf)
	return 0
}

func perfSpeed(port, secs int, gap time.Duration) int {
	if port <= 0 {
		fmt.Println(tr(`Speed test through the tunnel — prerequisites:
  1) iperf3 on both servers; on the KHAREJ server: iperf3 -s -B 127.0.0.1 -D
  2) on THIS (Iran) server a user port whose target is 127.0.0.1:5201 on kharej
     (per-port target, menu 2 → edit → network), e.g. port 15201
  3) hs2 perf speedtest --port 15201
Only 127.0.0.1:<port> is contacted (the tunnel); 4 short runs, spaced.`,
			`آزمون سرعت از داخل تونل — پیش‌نیازها:
  ۱) iperf3 روی هر دو سرور؛ روی سرور خارج: iperf3 -s -B 127.0.0.1 -D
  ۲) روی همین سرور (ایران) یک پورت کاربر با مقصد 127.0.0.1:5201 در خارج
     (مقصد هر پورت، منو ۲ ← ویرایش ← شبکه)، مثلاً پورت 15201
  ۳) hs2 perf speedtest --port 15201
فقط 127.0.0.1:<port> (یعنی تونل) تماس گرفته می‌شود؛ ۴ اجرای کوتاه با فاصله.`))
		return 2
	}
	if secs < 1 || secs > 15 {
		secs = 5
	}
	if _, err := exec.LookPath("iperf3"); err != nil {
		fmt.Println(tr("ERROR: iperf3 is not installed here (apt install iperf3)", "خطا: iperf3 این‌جا نصب نیست (apt install iperf3)"))
		return 1
	}
	rc := 0
	for i, r := range speedRuns {
		if i > 0 {
			time.Sleep(gap)
		}
		out, _ := exec.Command("iperf3", speedArgs(port, secs, r)...).Output()
		v, err := iperfMbit(out)
		if err != nil {
			fmt.Printf("%-6s %s: %v\n", r.Name, tr("failed", "ناموفق"), err)
			rc = 1
			continue
		}
		fmt.Printf("%-6s %8.0f Mbit/s\n", r.Name, v)
	}
	return rc
}

func bufioReader() *bufio.Reader { return bufio.NewReader(os.Stdin) }
