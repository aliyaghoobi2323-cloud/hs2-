package main

// The live dashboard (menu M2):
//
//	hs2 dashboard [--once] [--lang fa|en] [--width N] [--color auto|on|off] [--ascii]
//	hs2 dashboard --header [--lang fa]   one line: server · tunnels · users · Mbit · health · warnings
//	hs2 dashboard --paths  [--lang fa]   with vs without tunnel ping per LB IP (~3 s)
//
// Read-only: it reads /etc/hs2/*.json, /run/hs2/*.status.json, systemctl,
// /var/lib/hs2/alert-*.json and stats-*.json and changes nothing (it never
// takes the admin lock). On a tty it redraws every 3 s; q / Enter quits,
// l shows logs, d runs the server doctor, p the path check. Not a tty (a
// pipe, a cron mail): one frame, no escape codes.

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
)

type dashEnv struct {
	etcDir, runDir, stateDir string
	host                     string
	now                      time.Time
	// systemctl: ActiveState and NRestarts of a unit ("" = unknown)
	unitInfo func(unit string) (state string, restarts int)
}

type dashTunnel struct {
	Unit, Cfg string
	FC        fileConfig
	Active    string // systemd ActiveState ("" unknown)
	Restarts  int
	St        *liveStatus
	Age       int64 // seconds since the status was written (-1: none)
	Alerts    []string
	Stats     *statsFile
}

type dashData struct {
	Host    string
	Now     time.Time
	Tunnels []dashTunnel
}

type dashOpt struct {
	Width int
	Lang  string
	ASCII bool
	Color bool
}

func realUnitInfo(unit string) (string, int) {
	m := systemctlShow(unit, "ActiveState", "NRestarts")
	n, _ := strconv.Atoi(m["NRestarts"])
	return m["ActiveState"], n
}

func collectDash(env dashEnv) dashData {
	d := dashData{Host: env.host, Now: env.now}
	for _, t := range listTunnelConfigs(env.etcDir) {
		dt := dashTunnel{Unit: t.unit, Cfg: t.cfg, FC: t.fc, Age: -1}
		if env.unitInfo != nil {
			dt.Active, dt.Restarts = env.unitInfo(t.unit)
		}
		if b, err := os.ReadFile(filepath.Join(env.runDir, filepath.Base(statusPath(t.cfg)))); err == nil {
			var ls liveStatus
			if json.Unmarshal(b, &ls) == nil {
				dt.St = &ls
				dt.Age = env.now.Unix() - ls.Updated
			}
		}
		if b, err := os.ReadFile(alertStatePath(env.stateDir, t.unit)); err == nil {
			st := newAlertState()
			if json.Unmarshal(b, st) == nil {
				keys := make([]string, 0, len(st.Active))
				for k := range st.Active {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					a := st.Active[k]
					txt := a.Text
					if txt == "" {
						txt = k
					}
					dt.Alerts = append(dt.Alerts, strings.ToUpper(a.Sev)+" "+txt)
				}
			}
		}
		if _, err := os.Stat(statsPath(env.stateDir, t.unit)); err == nil {
			dt.Stats = loadStats(statsPath(env.stateDir, t.unit))
		}
		d.Tunnels = append(d.Tunnels, dt)
	}
	return d
}

// fresh: the status file exists and was written in the last 30 s by a
// running daemon.
func (t dashTunnel) fresh() bool { return t.St != nil && t.Age >= 0 && t.Age <= 30 }

// state is the one-word state: running | starting | stale | stopped | failed | unknown.
func (t dashTunnel) state() string {
	switch t.Active {
	case "failed":
		return "failed"
	case "inactive", "deactivating":
		return "stopped"
	case "activating":
		return "starting"
	}
	if t.fresh() {
		return "running"
	}
	if t.Active == "active" {
		if t.St == nil {
			return "starting"
		}
		return "stale"
	}
	return "unknown"
}

var dashWords = map[string]string{
	"running": "در حال کار", "starting": "در حال شروع", "stale": "کهنه", "stopped": "متوقف",
	"failed": "خطا", "unknown": "نامعلوم", "Iran": "ایران", "Kharej": "خارج", "direct": "مستقیم", "reverse": "معکوس",
	"OK": "سالم", "WARN": "هشدار", "BAD": "خراب", "users": "کاربر", "tunnels": "تونل", "warnings": "هشدار فعال",
	"health": "سلامت", "down": "دانلود", "up": "آپلود", "links": "لینک", "RTT": "RTT", "restarts": "ری‌استارت",
	"LB paths": "مسیرهای LB", "ok": "سالم", "lowered": "کم‌شده", "cut": "قطع", "no LB": "بدون LB",
	"dialing": "شماره‌گیر", "listening": "شنونده", "no data yet": "هنوز داده‌ای نیست",
	"counted on Iran": "شمارش در ایران", "24h RTT": "RTT ۲۴س", "24h Mbit": "Mbit ۲۴س", "max": "بیشینه",
	"no tunnels installed": "هیچ تونلی نصب نیست", "updated": "به‌روز", "ago": "پیش",
	"keys": "کلیدها", "quit": "خروج", "logs": "لاگ", "doctor": "پزشک", "path check": "بررسی مسیر",
	"status file missing — starting or older hs2": "فایل وضعیت نیست — در حال شروع یا hs2 قدیمی",
	"status is stale": "وضعیت کهنه است",
	"serving/target":  "فعال/هدف",
}

func (o dashOpt) w(s string) string {
	if o.Lang == "fa" {
		if f, ok := dashWords[s]; ok {
			return f
		}
	}
	return s
}

// dispWidth: terminal columns of s (ANSI codes and zero-width marks excluded).
func dispWidth(s string) int {
	n, esc := 0, false
	for _, r := range s {
		switch {
		case esc:
			if r == 'm' {
				esc = false
			}
		case r == 0x1b:
			esc = true
		case r == 0x200c || r == 0x200d || r == 0x200f || r == 0x200e || unicode.Is(unicode.Mn, r):
		default:
			n++
		}
	}
	return n
}

// clip cuts s to w columns (escape codes are dropped from a clipped line).
func clipW(s string, w int) string {
	if w <= 0 || dispWidth(s) <= w {
		return s
	}
	var b strings.Builder
	n := 0
	esc := false
	for _, r := range s {
		if esc {
			if r == 'm' {
				esc = false
			}
			continue
		}
		if r == 0x1b {
			esc = true
			continue
		}
		if n >= w-1 {
			break
		}
		b.WriteRune(r)
		if !(r == 0x200c || r == 0x200d || unicode.Is(unicode.Mn, r)) {
			n++
		}
	}
	return b.String() + "…"
}

// asciiOnly replaces what a non-UTF-8 terminal cannot show.
func asciiOnly(s string) string {
	r := strings.NewReplacer("·", "|", "—", "-", "…", "...", "─", "-", "→", "->", "≥", ">=")
	s = r.Replace(s)
	var b strings.Builder
	for _, c := range s {
		if c < 128 {
			b.WriteRune(c)
		} else {
			b.WriteByte('?')
		}
	}
	return b.String()
}

func sparkline(vals []float64, have []bool, ascii bool) (string, float64) {
	levels := []rune("▁▂▃▄▅▆▇█")
	if ascii {
		levels = []rune("_.-:=+*#")
	}
	mx := 0.0
	for i, v := range vals {
		if have[i] && v > mx {
			mx = v
		}
	}
	var b strings.Builder
	for i, v := range vals {
		if !have[i] {
			b.WriteRune(' ')
			continue
		}
		k := 0
		if mx > 0 {
			k = int(math.Round(v / mx * float64(len(levels)-1)))
		}
		b.WriteRune(levels[k])
	}
	return b.String(), mx
}

func (o dashOpt) col(code, s string) string {
	if !o.Color {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

func stateColor(s string) string {
	switch s {
	case "running":
		return "1;32"
	case "starting", "stale", "unknown":
		return "1;33"
	}
	return "1;31"
}

func lbCounts(paths []engine.LBPathView) (ok, low, cut int) {
	for _, p := range paths {
		switch {
		case p.State == "cooling" || p.State == "no-peer" || p.State == "draining" || (p.AutoFactor > 0 && p.AutoFactor < 0.05):
			cut++
		case p.State == "probing" || p.Degraded > 0 || (p.AutoFactor > 0 && p.AutoFactor < 0.95):
			low++
		default:
			ok++
		}
	}
	return
}

// health of the server: OK, WARN (something starting/stale or a warning
// alert) or BAD (a tunnel down or a bad alert); and the active alert count.
func (d dashData) health() (string, int) {
	h, n := "OK", 0
	for _, t := range d.Tunnels {
		n += len(t.Alerts)
		switch t.state() {
		case "failed", "stopped":
			h = "BAD"
		case "starting", "stale", "unknown":
			if h == "OK" {
				h = "WARN"
			}
		}
		for _, a := range t.Alerts {
			if strings.HasPrefix(a, "CRIT") || strings.HasPrefix(a, "BAD") {
				h = "BAD"
			} else if h == "OK" {
				h = "WARN"
			}
		}
	}
	return h, n
}

func (d dashData) totals() (users int, mbit float64) {
	for _, t := range d.Tunnels {
		if t.fresh() {
			users += t.St.Users
			mbit += t.St.Mbit
		}
	}
	return
}

func renderHeader(d dashData, o dashOpt) string {
	users, mbit := d.totals()
	h, n := d.health()
	hc := "1;32"
	if h == "WARN" {
		hc = "1;33"
	} else if h == "BAD" {
		hc = "1;31"
	}
	s := fmt.Sprintf("%s · %d %s · %d %s · %.1f Mbit · %s %s · %d %s",
		d.Host, len(d.Tunnels), o.w("tunnels"), users, o.w("users"), mbit, o.w("health"), o.col(hc, o.w(h)), n, o.w("warnings"))
	return o.fit(s)
}

func renderDash(d dashData, o dashOpt) string {
	if o.ASCII {
		o.Lang = "en" // Persian needs UTF-8
	}
	if o.Width <= 0 {
		o.Width = 80
	}
	narrow := o.Width < 60
	var lines []string
	add := func(s string) { lines = append(lines, o.fit(s)) }
	add(renderHeader(d, o))
	rule := strings.Repeat("─", o.Width)
	if len(d.Tunnels) == 0 {
		add(rule)
		add(" " + o.w("no tunnels installed"))
	}
	for _, t := range d.Tunnels {
		role, side := "Kharej", "listening"
		if t.FC.Mode == "dial" {
			role = "Iran"
		}
		if dialing(t.FC) {
			side = "dialing"
		}
		st := t.state()
		title := fmt.Sprintf("── %s · %s · %s · %s · %s ", t.Unit, o.w(role), o.w(direction(t.FC)), carrierName(t.FC), o.w(side))
		stTxt := " " + o.col(stateColor(st), o.w(st)) + " ──"
		pad := o.Width - dispWidth(title) - dispWidth(stTxt)
		if pad < 1 || narrow {
			add(clipW(title, o.Width))
			add("   " + o.col(stateColor(st), o.w(st)))
		} else {
			add(title + strings.Repeat("─", pad) + stTxt)
		}
		var items []string
		if t.St == nil {
			items = append(items, o.w("status file missing — starting or older hs2"))
		} else {
			ls := t.St
			if !t.fresh() {
				items = append(items, fmt.Sprintf("%s (%s %ds %s)", o.w("status is stale"), o.w("updated"), t.Age, o.w("ago")))
			}
			users := strconv.Itoa(ls.Users)
			if !ls.Counted && t.FC.Mode != "dial" {
				users = o.w("counted on Iran")
			}
			down, up := "—", "—"
			if ls.LBTotal != nil {
				down, up = fmt.Sprintf("%.1f", ls.LBTotal.DownMbit), fmt.Sprintf("%.1f", ls.LBTotal.UpMbit)
			}
			items = append(items,
				fmt.Sprintf("%s %s", o.w("users"), users),
				fmt.Sprintf("Mbit %.1f (%s %s · %s %s)", ls.Mbit, o.w("down"), down, o.w("up"), up))
			serving := ls.Links
			if ls.Serving != nil {
				serving = *ls.Serving
			}
			if ls.Target > 0 {
				items = append(items, fmt.Sprintf("%s %d/%d (%s)", o.w("links"), serving, ls.Target, o.w("serving/target")))
			} else {
				items = append(items, fmt.Sprintf("%s %d", o.w("links"), ls.Links))
			}
			if p50, p99 := statusRTT(ls); p50 > 0 {
				items = append(items, fmt.Sprintf("RTT p50 %s · p99 %s ms", fmtMs(p50), fmtMs(p99)))
			} else {
				items = append(items, "RTT —")
			}
			if len(ls.LBPaths) > 0 {
				k, l, c := lbCounts(ls.LBPaths)
				items = append(items, fmt.Sprintf("%s: %d %s · %d %s · %d %s", o.w("LB paths"), k, o.w("ok"), l, o.w("lowered"), c, o.w("cut")))
			} else {
				items = append(items, o.w("no LB"))
			}
		}
		items = append(items, fmt.Sprintf("%s %d", o.w("restarts"), t.Restarts))
		// pack items into lines of the width
		cur := " "
		for _, it := range items {
			if narrow {
				add(clipW("  "+it, o.Width))
				continue
			}
			if dispWidth(cur)+dispWidth(it)+3 > o.Width && strings.TrimSpace(cur) != "" {
				add(cur)
				cur = " "
			}
			if strings.TrimSpace(cur) != "" {
				cur += " · "
			}
			cur += it
		}
		if !narrow && strings.TrimSpace(cur) != "" {
			add(clipW(cur, o.Width))
		}
		// 24 h sparklines
		slots := t.Stats.last24(d.Now)
		rtt, mb := make([]float64, 24), make([]float64, 24)
		hv, hm := make([]bool, 24), make([]bool, 24)
		any := false
		for i, s := range slots {
			if s != nil {
				any = true
				rtt[i], hv[i] = s.rttAvg(), s.RTTN > 0
				mb[i], hm[i] = s.mbitAvg(), s.N > 0
			}
		}
		lbl := 10
		if !any {
			add("  " + o.w("24h RTT") + "/" + o.w("24h Mbit") + ": " + o.w("no data yet"))
		} else {
			span := 24
			if room := o.Width - lbl - 16; room < span {
				span = maxInt(room, 4)
			}
			for _, row := range []struct {
				name string
				v    []float64
				h    []bool
				unit string
			}{{"24h RTT", rtt, hv, "ms"}, {"24h Mbit", mb, hm, ""}} {
				sp, mx := sparkline(row.v[24-span:], row.h[24-span:], o.ASCII)
				name := o.w(row.name)
				name += strings.Repeat(" ", maxInt(lbl-dispWidth(name), 1))
				tail := o.w("max") + " " + fmtMs(mx) + " " + row.unit
				if !anyTrue(row.h[24-span:]) {
					tail = "—" // e.g. the listening side reports no link RTT
				}
				add(clipW(strings.TrimRight(fmt.Sprintf("  %s|%s| %s", name, sp, tail), " "), o.Width))
			}
		}
		for _, a := range t.Alerts {
			add(clipW("  "+o.col("1;33", "! "+a), o.Width))
		}
	}
	out := strings.Join(lines, "\n") + "\n"
	return out
}

// utf8Locale: the locale can show UTF-8 (else the dashboard falls back to
// ASCII and English).
func utf8Locale() bool {
	for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := os.Getenv(k); v != "" {
			v = strings.ToLower(v)
			return strings.Contains(v, "utf-8") || strings.Contains(v, "utf8")
		}
	}
	return false
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func termWidth() int {
	if c, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && c > 0 {
		return c
	}
	cmd := exec.Command("stty", "size")
	cmd.Stdin = os.Stdin
	if out, err := cmd.Output(); err == nil {
		if f := strings.Fields(string(out)); len(f) == 2 {
			if c, err := strconv.Atoi(f[1]); err == nil && c > 0 {
				return c
			}
		}
	}
	return 80
}

func stty(args ...string) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = os.Stdin
	_ = cmd.Run()
}

// renderPaths: the on-demand "with vs without tunnel" check.
func renderPaths(runDir string, o dashOpt) string {
	env := &deepEnv{proc: "/proc", sys: "/sys", runDir: runDir, window: 3 * time.Second, run: realRun}
	paths := collectPaths(env, readStatuses(runDir, ""))
	var b strings.Builder
	if len(paths) == 0 {
		b.WriteString(tr("no tunnel path with a peer IP found (no running LB tunnel / no dial address)\n", "مسیری با IP طرف مقابل پیدا نشد\n"))
		return b.String()
	}
	pingPaths(env, paths)
	fs := judgePaths(paths)
	fmt.Fprintf(&b, "%-34s %-22s %-14s %s\n", tr("path", "مسیر"), tr("without tunnel (ping)", "بدون تونل (ping)"), tr("tunnel RTT", "RTT تونل"), tr("verdict", "نتیجه"))
	for i, p := range paths {
		name := p.remote
		if p.local != "*" && p.local != "" {
			name = p.local + " → " + p.remote
		}
		plain := "—"
		if p.ping.OK && p.ping.Loss < 100 {
			plain = fmt.Sprintf("%.0f ms, %.0f%% loss", p.ping.Avg, p.ping.Loss)
		} else if p.ping.OK {
			plain = tr("no ICMP reply", "بدون پاسخ ICMP")
		}
		tun := "—"
		if p.tunRTT > 0 {
			tun = fmt.Sprintf("%.0f ms", p.tunRTT)
		}
		sev := ""
		if i < len(fs) {
			sev = fs[i].Sev
		}
		fmt.Fprintf(&b, "%-34s %-22s %-14s %s\n", name, plain, tun, sev)
		if i < len(fs) && fs[i].Why != "" {
			b.WriteString("   " + fs[i].Why + "\n")
		}
	}
	for _, f := range fs[len(paths):] {
		b.WriteString(f.Sev + " " + f.What + " — " + f.Why + "\n")
	}
	s := b.String()
	if o.ASCII {
		s = asciiOnly(s)
	}
	return s
}

func dashboardCmd(args []string) {
	fs := flag.NewFlagSet("dashboard", flag.ExitOnError)
	once := fs.Bool("once", false, "print one frame and exit")
	header := fs.Bool("header", false, "print only the one-line header")
	pathsF := fs.Bool("paths", false, "run the with/without tunnel path check and exit")
	lang := fs.String("lang", "en", "en | fa")
	width := fs.Int("width", 0, "columns (default: the terminal's)")
	color := fs.String("color", "auto", "auto | on | off")
	ascii := fs.Bool("ascii", false, "ASCII only (default when the locale is not UTF-8)")
	every := fs.Duration("interval", 3*time.Second, "refresh interval")
	etc := fs.String("etc", "/etc/hs2", "tunnel configs")
	run := fs.String("run", statusRunDir, "live status directory")
	state := fs.String("state", alertStateDir, "alert/stats state directory")
	fs.Parse(args)
	tty := isTTY(os.Stdout)
	o := dashOpt{Width: *width, Lang: *lang, ASCII: *ascii || !utf8Locale()}
	if o.ASCII {
		o.Lang = "en" // Persian needs UTF-8
	}
	o.Color = *color == "on" || (*color == "auto" && tty && os.Getenv("NO_COLOR") == "")
	if o.Width <= 0 {
		o.Width = 80
		if tty {
			o.Width = termWidth()
		}
	}
	if o.Lang == "fa" {
		deepLang = "fa"
	}
	host, _ := os.Hostname()
	env := dashEnv{etcDir: *etc, runDir: *run, stateDir: *state, host: host, unitInfo: realUnitInfo}
	frame := func() dashData { env.now = time.Now(); return collectDash(env) }
	switch {
	case *header:
		fmt.Println(renderHeader(frame(), o))
		return
	case *pathsF:
		fmt.Print(renderPaths(*run, o))
		return
	case *once || !tty || !isTTY(os.Stdin):
		fmt.Print(renderDash(frame(), o))
		return
	}
	dashLoop(frame, o, *every, *run)
}

func dashLoop(frame func() dashData, o dashOpt, every time.Duration, runDir string) {
	stty("-icanon", "-echo", "min", "1")
	restore := func() { stty("sane") }
	defer restore()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	keys := make(chan byte, 4)
	go func() {
		r := bufio.NewReader(os.Stdin)
		for {
			c, err := r.ReadByte()
			if err != nil {
				close(keys)
				return
			}
			keys <- c
		}
	}()
	foot := fmt.Sprintf("%s: q/Enter %s · l %s · d %s · p %s", o.w("keys"), o.w("quit"), o.w("logs"), o.w("doctor"), o.w("path check"))
	if o.ASCII {
		foot = asciiOnly(foot)
	}
	self, _ := os.Executable()
	wait := func() {
		fmt.Print(tr("\n-- press any key --", "\n-- یک کلید بزنید --"))
		<-keys
	}
	for {
		d := frame()
		fmt.Print("\033[H\033[2J" + renderDash(d, o) + "\n" + clipW(foot, o.Width) + "\n")
		select {
		case <-sig:
			return
		case <-time.After(every):
		case c, ok := <-keys:
			if !ok {
				return
			}
			switch c {
			case 'q', 'Q', '\n', '\r', '0', 0x1b:
				return
			case 'l', 'L':
				fmt.Print("\033[H\033[2J")
				for _, t := range d.Tunnels {
					fmt.Println("== " + t.Unit)
					cmd := exec.Command("journalctl", "-u", t.Unit, "-n", "15", "--no-pager", "-o", "short-iso")
					cmd.Stdout, cmd.Stderr = os.Stdout, os.Stdout
					_ = cmd.Run()
				}
				wait()
			case 'd', 'D':
				fmt.Print("\033[H\033[2J" + tr("server doctor (~10 s, read-only)…\n", "پزشک سرور (~۱۰ ثانیه، فقط خواندنی)…\n"))
				cmd := exec.Command(self, "doctor", "--deep", "--lang", o.Lang)
				cmd.Stdout, cmd.Stderr = os.Stdout, os.Stdout
				_ = cmd.Run()
				wait()
			case 'p', 'P':
				fmt.Print("\033[H\033[2J" + tr("pinging each path without the tunnel (~3 s)…\n", "ping هر مسیر بدون تونل (~۳ ثانیه)…\n"))
				fmt.Print(renderPaths(runDir, o))
				wait()
			}
		}
	}
}

// fit makes one output line safe for the terminal: ASCII-only when asked,
// then cut to the width (the ellipsis is "~" in ASCII mode).
func (o dashOpt) fit(s string) string {
	if o.ASCII {
		s = asciiOnly(s)
	}
	if o.Width > 0 {
		s = clipW(s, o.Width)
	}
	if o.ASCII {
		s = strings.ReplaceAll(s, "…", "~")
	}
	return s
}

// fmtMs: one decimal below 10 (a LAN or loopback RTT), whole numbers above.
func fmtMs(v float64) string {
	if v < 10 {
		return strconv.FormatFloat(v, 'f', 1, 64)
	}
	return strconv.FormatFloat(v, 'f', 0, 64)
}

func anyTrue(b []bool) bool {
	for _, v := range b {
		if v {
			return true
		}
	}
	return false
}
