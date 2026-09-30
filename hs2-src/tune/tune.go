// Package tune sizes and applies the kernel network tuning hs2 wants, based on
// the server's RAM and CPU cores, every time the daemon starts. It is the single
// source of truth for tuning: nothing is written into a sysctl file behind the
// user's back, so `hs2 tune` always shows exactly what is in effect, and a
// change in RAM (a resized VPS) or in the config is picked up on the next start.
//
// Three things are chosen:
//
//   - a congestion control (default bbr) and a queueing discipline (default
//     fq_codel), both verified against the kernel and falling back cleanly when
//     one is unavailable;
//   - socket-buffer and backlog sysctls scaled to a low / medium / high profile
//     picked from RAM and cores (a 512 MB / 1-core box and a 16 GB / 8-core box
//     must not get the same buffers);
//   - a few fixed correctness/latency sysctls (BBR-friendly, multi-IP-friendly).
//
// Everything is applied best-effort: a value the kernel rejects (an old kernel,
// a restricted container) is noted and skipped, never fatal.
package tune

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Mode is how tuning behaves.
const (
	ModeAuto   = "auto"   // derive everything from RAM/cores (default)
	ModeManual = "manual" // auto values, with the operator's explicit overrides on top
	ModeOff    = "off"    // touch no system sysctls (operator tunes the box themselves)
)

// Config is the "tuning" section of the hs2 config. All fields are optional; an
// empty Config means fully-automatic tuning with the defaults.
type Config struct {
	Mode       string `json:"mode"`       // auto | manual | off  (default auto)
	Congestion string `json:"congestion"` // bbr (default), cubic, …
	Qdisc      string `json:"qdisc"`      // fq_codel (default), fq, cake, …

	// Manual overrides (bytes / counts). Zero means "leave to the profile".
	// Used in manual mode; ignored in off mode.
	RmemMax   int `json:"rmem_max"`
	WmemMax   int `json:"wmem_max"`
	Backlog   int `json:"netdev_backlog"`
	Somaxconn int `json:"somaxconn"`
}

// Plan is what tuning decided, ready to apply and to print.
type Plan struct {
	Mode       string
	Profile    string // low | medium | high | manual | off
	RAMMB      int
	CPUs       int
	Congestion string   // effective, after availability checks
	Qdisc      string   // effective, after availability checks
	Sysctls    []KV     // ordered sysctls to set
	Notes      []string // fallbacks / skips, shown to the operator
}

// KV is one sysctl key/value.
type KV struct{ Key, Val string }

// Detect returns total RAM in MB and the CPU core count.
func Detect() (ramMB, cpus int) {
	return detectRAMMB(), runtime.NumCPU()
}

func detectRAMMB() int {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				if kb, err := strconv.Atoi(f[1]); err == nil {
					return kb / 1024
				}
			}
		}
	}
	return 0
}

// profileFor picks low/medium/high from RAM and cores. RAM dominates (buffers
// live in it); cores nudge a borderline box up.
func profileFor(ramMB, cpus int) string {
	switch {
	case ramMB >= 4096 && cpus >= 4:
		return "high"
	case ramMB >= 4096 || (ramMB >= 2048 && cpus >= 4):
		return "high"
	case ramMB >= 1536:
		return "medium"
	default:
		return "low"
	}
}

// profileValues returns the buffer/backlog/somaxconn values for a profile.
func profileValues(profile string) (rmemMax, wmemMax, backlog, somaxconn int) {
	switch profile {
	case "high":
		return 32 << 20, 32 << 20, 16384, 8192
	case "medium":
		return 16 << 20, 16 << 20, 8192, 4096
	default: // low
		return 8 << 20, 8 << 20, 2048, 1024
	}
}

// Build turns a Config plus the detected hardware into a Plan. It is pure (no
// I/O beyond the availability lookups the caller passes in via availCC/availQ),
// so it is fully unit-testable.
func Build(cfg Config, ramMB, cpus int, availCC, availQ func(string) bool) *Plan {
	mode := cfg.Mode
	if mode == "" {
		mode = ModeAuto
	}
	cc := firstNonEmpty(cfg.Congestion, "bbr")
	qd := firstNonEmpty(cfg.Qdisc, "fq_codel")

	p := &Plan{Mode: mode, RAMMB: ramMB, CPUs: cpus}

	// Congestion control: keep the requested one if the kernel has it, else fall
	// back bbr→cubic and note it.
	p.Congestion = pick(cc, []string{cc, "bbr", "cubic"}, availCC, &p.Notes, "congestion control")
	// Qdisc: requested → fq_codel → fq. default_qdisc only affects interfaces
	// created after it is set, which includes the tunnel device.
	p.Qdisc = pick(qd, []string{qd, "fq_codel", "fq"}, availQ, &p.Notes, "qdisc")

	if mode == ModeOff {
		p.Profile = "off"
		p.Notes = append(p.Notes, "mode=off: system sysctls left untouched (only the tunnel's own sockets use the chosen congestion control)")
		// Still record cc/qdisc as the effective per-socket choice, but set no sysctls.
		return p
	}

	profile := profileFor(ramMB, cpus)
	rmemMax, wmemMax, backlog, somaxconn := profileValues(profile)
	if mode == ModeManual {
		profile = "manual"
		if cfg.RmemMax > 0 {
			rmemMax = cfg.RmemMax
		}
		if cfg.WmemMax > 0 {
			wmemMax = cfg.WmemMax
		}
		if cfg.Backlog > 0 {
			backlog = cfg.Backlog
		}
		if cfg.Somaxconn > 0 {
			somaxconn = cfg.Somaxconn
		}
	}
	p.Profile = profile

	add := func(k, v string) { p.Sysctls = append(p.Sysctls, KV{k, v}) }
	if p.Congestion != "" {
		add("net.ipv4.tcp_congestion_control", p.Congestion)
	}
	if p.Qdisc != "" {
		add("net.core.default_qdisc", p.Qdisc)
	}
	add("net.core.rmem_max", itoa(rmemMax))
	add("net.core.wmem_max", itoa(wmemMax))
	add("net.ipv4.tcp_rmem", fmt.Sprintf("4096 131072 %d", rmemMax))
	add("net.ipv4.tcp_wmem", fmt.Sprintf("4096 65536 %d", wmemMax))
	add("net.core.netdev_max_backlog", itoa(backlog))
	add("net.core.somaxconn", itoa(somaxconn))
	add("net.ipv4.tcp_max_syn_backlog", itoa(somaxconn))
	// Fixed correctness/latency knobs, independent of size:
	add("net.ipv4.tcp_notsent_lowat", "131072") // bound bufferbloat for non-tunnel apps
	add("net.ipv4.tcp_slow_start_after_idle", "0")
	add("net.ipv4.tcp_mtu_probing", "1")
	add("net.ipv4.tcp_fin_timeout", "20")
	// ip_local_port_range is deliberately NOT touched: widening it down into
	// 10240+ makes ports that panels commonly bind for inbounds (x-ui, 10000–32767)
	// ephemeral, so a new inbound could fail with "address in use". The kernel
	// default already leaves ~28k outgoing ports — far more than 32 links need.
	// rp_filter=2 (loose): a multi-IP server dialing/listening on a non-default
	// local IP gets return packets strict rp_filter would drop.
	add("net.ipv4.conf.all.rp_filter", "2")
	add("net.ipv4.conf.default.rp_filter", "2")
	return p
}

// pick returns want if avail(want), else the first candidate that is available,
// noting a fallback. If none is available it returns "" and notes it.
func pick(want string, candidates []string, avail func(string) bool, notes *[]string, what string) string {
	if avail == nil || avail(want) {
		return want
	}
	seen := map[string]bool{}
	for _, c := range candidates {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		if avail(c) {
			*notes = append(*notes, fmt.Sprintf("%s %q not available — using %q", what, want, c))
			return c
		}
	}
	*notes = append(*notes, fmt.Sprintf("%s %q not available and no fallback found — leaving the kernel default", what, want))
	return ""
}

// Apply writes the plan's sysctls (best-effort) and loads the modules its
// congestion control and qdisc need. logf receives one summary line plus any
// notes; it never fails the daemon.
func (p *Plan) Apply(logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	// Undo a setting an earlier hs2 build applied and this one no longer does —
	// only when the live value is exactly the one hs2 set, so an operator's own
	// value is never touched.
	if undoLegacyPortRange(readSysctl, writeSysctl) {
		p.Notes = append(p.Notes, "restored net.ipv4.ip_local_port_range to the kernel default 32768 60999 (an earlier hs2 build had widened it)")
	}
	// Load the modules the choices need, so the sysctl writes below succeed.
	if p.Congestion != "" {
		modprobe("tcp_" + p.Congestion)
	}
	if p.Qdisc != "" {
		modprobe("sch_" + p.Qdisc)
	}
	applied, failed := 0, 0
	for i, kv := range p.Sysctls {
		// default_qdisc is the one value the kernel rejects outright when its
		// module is missing; fall back through fq_codel→fq so a kernel without the
		// requested qdisc still gets a good one instead of nothing.
		if kv.Key == "net.core.default_qdisc" {
			if got, ok := writeQdiscWithFallback(kv.Val); ok {
				if got != kv.Val {
					p.Notes = append(p.Notes, fmt.Sprintf("qdisc %q unavailable on this kernel — using %q", kv.Val, got))
					p.Sysctls[i].Val = got
					p.Qdisc = got
				}
				applied++
			} else {
				failed++
				p.Notes = append(p.Notes, "could not set net.core.default_qdisc (leaving the kernel default)")
			}
			continue
		}
		if writeSysctl(kv.Key, kv.Val) {
			applied++
		} else {
			failed++
			p.Notes = append(p.Notes, fmt.Sprintf("could not set %s (kernel rejected it or no permission)", kv.Key))
		}
	}
	logf("tuning: %s", p.Summary())
	if failed > 0 {
		logf("tuning: %d/%d sysctls applied (%d skipped — see notes)", applied, applied+failed, failed)
	}
	for _, n := range p.Notes {
		logf("tuning: note — %s", n)
	}
}

// Summary is a one-line description for the log and `hs2 tune`.
func (p *Plan) Summary() string {
	if p.Mode == ModeOff {
		return fmt.Sprintf("off (RAM %s, %d cpu) — %s qdisc, %s congestion on tunnel sockets only, no system sysctls",
			ramStr(p.RAMMB), p.CPUs, orDefault(p.Qdisc), orDefault(p.Congestion))
	}
	return fmt.Sprintf("profile %s (RAM %s, %d cpu) — %s + %s, %d sysctls",
		p.Profile, ramStr(p.RAMMB), p.CPUs, orDefault(p.Congestion), orDefault(p.Qdisc), len(p.Sysctls))
}

// Report is the multi-line human view for `hs2 tune`.
func (p *Plan) Report() string {
	var b strings.Builder
	fmt.Fprintf(&b, "hs2 kernel tuning\n")
	fmt.Fprintf(&b, "  hardware:    RAM %s, %d CPU core(s)\n", ramStr(p.RAMMB), p.CPUs)
	fmt.Fprintf(&b, "  mode:        %s\n", p.Mode)
	fmt.Fprintf(&b, "  profile:     %s\n", p.Profile)
	fmt.Fprintf(&b, "  congestion:  %s\n", orDefault(p.Congestion))
	fmt.Fprintf(&b, "  qdisc:       %s\n", orDefault(p.Qdisc))
	if len(p.Sysctls) > 0 {
		fmt.Fprintf(&b, "  sysctls:\n")
		kv := append([]KV(nil), p.Sysctls...)
		sort.Slice(kv, func(i, j int) bool { return kv[i].Key < kv[j].Key })
		for _, e := range kv {
			fmt.Fprintf(&b, "    %-34s = %s\n", e.Key, e.Val)
		}
	}
	for _, n := range p.Notes {
		fmt.Fprintf(&b, "  note: %s\n", n)
	}
	return b.String()
}

// --- system helpers (best-effort, linux) -------------------------------------

// AvailableCC reports whether the kernel offers a congestion control now.
func AvailableCC(name string) bool {
	if name == "" {
		return false
	}
	if inProcList("/proc/sys/net/ipv4/tcp_available_congestion_control", name) {
		return true
	}
	modprobe("tcp_" + name)
	return inProcList("/proc/sys/net/ipv4/tcp_available_congestion_control", name)
}

// AvailableQdisc reports whether a qdisc module can be loaded. There is no
// kernel list of available qdiscs, so we try to load the module and trust it;
// the real confirmation is whether writing default_qdisc later succeeds.
func AvailableQdisc(name string) bool {
	if name == "" {
		return false
	}
	// A handful ship built-in on essentially every kernel.
	switch name {
	case "fq", "fq_codel", "pfifo_fast", "sfq":
		return true
	}
	modprobe("sch_" + name)
	// Best-effort: assume loadable. writeSysctl(default_qdisc) will reject it
	// otherwise and Build's fallback chain covers that at apply time.
	return true
}

func inProcList(path, name string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, f := range strings.Fields(string(b)) {
		if f == name {
			return true
		}
	}
	return false
}

// writeQdiscWithFallback tries to set net.core.default_qdisc to want, then to
// fq_codel, then fq, loading each module first. It returns the qdisc that took.
func writeQdiscWithFallback(want string) (string, bool) {
	seen := map[string]bool{}
	for _, q := range []string{want, "fq_codel", "fq"} {
		if q == "" || seen[q] {
			continue
		}
		seen[q] = true
		modprobe("sch_" + q)
		if writeSysctl("net.core.default_qdisc", q) {
			return q, true
		}
	}
	return "", false
}

// legacyPortRange is the value an earlier hs2 build wrote; defaultPortRange is
// the Linux default it replaced.
const (
	legacyPortRange  = "10240 65535"
	defaultPortRange = "32768 60999"
)

// undoLegacyPortRange restores the default ephemeral port range if, and only if,
// the live value is exactly the one an earlier hs2 build set. read/write are
// injected for tests.
func undoLegacyPortRange(read func(string) (string, bool), write func(string, string) bool) bool {
	cur, ok := read("net.ipv4.ip_local_port_range")
	if !ok || strings.Join(strings.Fields(cur), " ") != legacyPortRange {
		return false
	}
	return write("net.ipv4.ip_local_port_range", defaultPortRange)
}

func readSysctl(key string) (string, bool) {
	b, err := os.ReadFile("/proc/sys/" + strings.ReplaceAll(key, ".", "/"))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

func writeSysctl(key, val string) bool {
	path := "/proc/sys/" + strings.ReplaceAll(key, ".", "/")
	// tcp_rmem etc. take tab/space-separated values; the kernel accepts spaces.
	if err := os.WriteFile(path, []byte(val), 0o644); err != nil {
		return false
	}
	return true
}

func modprobe(mod string) {
	if _, err := os.Stat("/sbin/modprobe"); err == nil {
		_ = exec.Command("/sbin/modprobe", mod).Run()
		return
	}
	if p, err := exec.LookPath("modprobe"); err == nil {
		_ = exec.Command(p, mod).Run()
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
func orDefault(s string) string {
	if s == "" {
		return "(kernel default)"
	}
	return s
}
func itoa(i int) string { return strconv.Itoa(i) }
func ramStr(mb int) string {
	if mb <= 0 {
		return "unknown"
	}
	if mb >= 1024 {
		return fmt.Sprintf("%.1f GB", float64(mb)/1024)
	}
	return fmt.Sprintf("%d MB", mb)
}
