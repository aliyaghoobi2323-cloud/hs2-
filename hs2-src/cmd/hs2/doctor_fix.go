package main

// Menu M6, section 7 "Server doctor": categories, numbered safe fixes and a
// report file on top of the read-only deep check (doctor_deep.go).
//
//	hs2 doctor --deep [--only network|cpu|tunnel|reality|panel] [--fixes] [--report FILE]
//	hs2 doctor --fix N [--yes] [--hours H]     apply fix N of the last run (saved in /run/hs2)
//
// A fix only ever runs an hs2 command that already has its own safety net:
//   rxcore  → hs2 perf cores -u U --keep N   (CPUAffinity drop-in, safe apply + auto-rollback)
//   budget  → hs2 perf kernel --budget 600   (measured before/after, reverted when worse)
//   stats   → hs2 stats timer on             (the 24 h sampler; harmless)
//   park    → hs2 lb park -u U --ip IP --hours H  (restored by a timer, also after reboot)
//   cert    → certbot renew --dry-run        (changes nothing; shows the renewal command)
//   alerts  → the menu opens the alert setup (section 6)
// x-ui / xray / panel findings never get a fix — only advice text.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// doctorCats are the categories the menu can run one by one.
var doctorCats = []string{"network", "cpu", "tunnel", "reality", "panel"}

// fixAction is one safe automatic fix attached to a finding.
type fixAction struct {
	Kind  string   `json:"kind"`
	Units []string `json:"units,omitempty"`
	Arg   string   `json:"arg,omitempty"`
	Title string   `json:"title"`
}

var doctorFixesPath = "/run/hs2/doctor-fixes.json"

// fixWithin: the trial time of a safe-apply fix before its automatic rollback.
var fixWithin = 5 * time.Minute

type savedFixes struct {
	At    int64       `json:"at"`
	Fixes []fixAction `json:"fixes"`
}

// reTunnelExec: a real tunnel unit runs `hs2 run -c /etc/hs2/<name>.json`
// (coreguard, alert, stats, lb-park units also start with hs2- but are not tunnels).
var reTunnelExec = regexp.MustCompile(`\brun\s+(?:[^;]*\s)?-c\s+/etc/hs2/[^\s;/]+\.json\b`)

func isTunnelExec(execStart string) bool { return reTunnelExec.MatchString(execStart) }

// unitFromStatusFile: "etc-hs2-config.json" → hs2, "etc-hs2-de1.json" → hs2-de1.
func unitFromStatusFile(f string) string {
	if !strings.HasPrefix(f, "etc-hs2-") {
		return ""
	}
	return unitForConfig(strings.TrimPrefix(f, "etc-hs2-"))
}

// fixCommand maps a fix to the argv it runs (without the hs2 binary for hs2
// commands; "certbot" commands are external). nil = nothing to run.
func fixCommand(f fixAction, unit string, hours float64) []string {
	switch f.Kind {
	case "rxcore":
		if !hs2UnitOK(unit) {
			return nil
		}
		return []string{"perf", "cores", "-u", unit, "--keep", f.Arg, "--confirm-within", fixWithin.String(), "--yes"}
	case "budget":
		return []string{"perf", "kernel", "--budget", firstNonEmpty(f.Arg, "600"), "--yes"}
	case "stats":
		return []string{"stats", "timer", "on"}
	case "park":
		if !hs2UnitOK(unit) || hours <= 0 {
			return nil
		}
		return []string{"lb", "park", "-u", unit, "--ip", f.Arg, "--hours", strconv.FormatFloat(hours, 'f', -1, 64), "--yes"}
	case "cert":
		return []string{"certbot", "renew", "--dry-run"}
	}
	return nil
}

// isPanelFinding: x-ui / xray / panel areas never get a fix.
func isPanelArea(cat string) bool { return cat == "panel" || cat == "reality" }

// collectFixes numbers the fixes of the findings (1…n), skipping any on a
// panel/xray finding (defence in depth).
func collectFixes(fs []finding) []fixAction {
	var out []fixAction
	for i := range fs {
		if fs[i].Fix == nil || isPanelArea(fs[i].Cat) {
			fs[i].Fix = nil
			continue
		}
		out = append(out, *fs[i].Fix)
		fs[i].FixN = len(out)
	}
	return out
}

func filterCat(fs []finding, cat string) []finding {
	if cat == "" || cat == "all" {
		return fs
	}
	var out []finding
	for _, f := range fs {
		if f.Cat == cat {
			out = append(out, f)
		}
	}
	return out
}

func saveFixes(path string, fixes []fixAction, now time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(savedFixes{At: now.Unix(), Fixes: fixes})
	return os.WriteFile(path, b, 0o600)
}

func loadFixes(path string, now time.Time) ([]fixAction, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s", tr("no saved doctor run — run the doctor first", "اجرای ذخیره‌شده‌ای از پزشک نیست — اول پزشک را اجرا کنید"))
	}
	var s savedFixes
	if json.Unmarshal(b, &s) != nil {
		return nil, fmt.Errorf("%s", tr("the saved doctor run is unreadable — run the doctor again", "اجرای ذخیره‌شدهٔ پزشک خوانا نیست — دوباره اجرا کنید"))
	}
	if now.Unix()-s.At > 1800 {
		return nil, fmt.Errorf("%s", tr("the last doctor run is older than 30 min — run it again first", "آخرین اجرای پزشک بیش از ۳۰ دقیقه پیش بوده — اول دوباره اجرا کنید"))
	}
	return s.Fixes, nil
}

// unitExists: does systemd know this unit (a tunnel deleted since the run is refused)?
func unitExists(run func(time.Duration, string, ...string) (string, error), u string) bool {
	out, _ := run(4*time.Second, "systemctl", "show", "-p", "LoadState", "--value", u+".service")
	return strings.TrimSpace(out) == "loaded"
}

// applyFix runs fix n. It shows the preview first and asks unless yes.
func applyFix(n int, yes bool, hours float64, in io.Reader, out io.Writer,
	run func(time.Duration, string, ...string) (string, error), exe func(name string, args ...string) error) int {
	fixes, err := loadFixes(doctorFixesPath, time.Now())
	if err != nil {
		fmt.Fprintln(out, tr("ERROR: ", "خطا: ")+err.Error())
		return 1
	}
	if n < 1 || n > len(fixes) {
		fmt.Fprintf(out, tr("ERROR: there is no fix %d (the last run has %d)\n", "خطا: اصلاح شمارهٔ %d نیست (آخرین اجرا %d اصلاح دارد)\n"), n, len(fixes))
		return 1
	}
	f := fixes[n-1]
	fmt.Fprintf(out, "%s %d: %s\n", tr("fix", "اصلاح"), n, f.Title)
	if f.Kind == "alerts" {
		fmt.Fprintln(out, tr("open menu → 6) Monitoring & alerts to set up Telegram and turn alerts on", "منو → ۶) پایش و هشدارها را باز کنید تا تلگرام را تنظیم و هشدار را روشن کنید"))
		return 0
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(out, tr("ERROR: fixes change systemd units / sysctl / the LB: run as root (sudo hs2-menu)", "خطا: اصلاح‌ها سرویس systemd / sysctl / لودبالانس را تغییر می‌دهند: با root اجرا کنید (sudo hs2-menu)"))
		return 1
	}
	if f.Kind == "park" && hours <= 0 {
		hours = 6
	}
	units := f.Units
	if len(units) == 0 {
		units = []string{""}
	}
	type step struct {
		name string
		args []string
	}
	var steps []step
	for _, u := range units {
		if u != "" && !unitExists(run, u) {
			fmt.Fprintf(out, tr("  skip: %s does not exist on this server any more\n", "  رد شد: %s دیگر روی این سرور نیست\n"), u)
			continue
		}
		argv := fixCommand(f, u, hours)
		if argv == nil {
			continue
		}
		if argv[0] == "certbot" {
			if _, e := exec.LookPath("certbot"); e != nil {
				fmt.Fprintln(out, tr("  certbot is not installed here; renew with your ACME client (acme.sh --renew -d DOMAIN), then restart the tunnel", "  certbot این‌جا نصب نیست؛ با ابزار ACME خود تمدید کنید (acme.sh --renew -d DOMAIN) و تونل را ری‌استارت کنید"))
				return 0
			}
			steps = append(steps, step{"certbot", argv[1:]})
			continue
		}
		steps = append(steps, step{"hs2", argv})
	}
	if len(steps) == 0 {
		fmt.Fprintln(out, tr("nothing to do: the fix's target is gone — run the doctor again", "کاری نمانده: هدف این اصلاح دیگر وجود ندارد — پزشک را دوباره اجرا کنید"))
		return 1
	}
	fmt.Fprintln(out, tr("preview — will run:", "پیش‌نمایش — اجرا می‌شود:"))
	for _, s := range steps {
		fmt.Fprintf(out, "   %s %s\n", s.name, strings.Join(s.args, " "))
	}
	fmt.Fprintln(out, "   "+fixSafety(f.Kind))
	if !yes {
		fmt.Fprint(out, tr("Apply? [y/N]: ", "اعمال شود؟ [y/N]: "))
		a, _ := bufio.NewReader(in).ReadString('\n')
		if a = strings.TrimSpace(strings.ToLower(a)); a != "y" && a != "yes" {
			fmt.Fprintln(out, tr("Nothing changed.", "تغییری داده نشد."))
			return 0
		}
	}
	rc := 0
	for _, s := range steps {
		if err := exe(s.name, s.args...); err != nil {
			fmt.Fprintf(out, tr("  %s failed: %v\n", "  %s شکست خورد: %v\n"), s.name, err)
			rc = 1
		}
	}
	return rc
}

func fixSafety(kind string) string {
	switch kind {
	case "rxcore":
		return tr("safe apply: a trial with automatic rollback unless you confirm; x-ui/xray are never touched", "اعمال امن: آزمایشی با برگشت خودکار مگر تأیید کنید؛ x-ui/xray هرگز تغییر نمی‌کنند")
	case "budget":
		return tr("measured before/after on the rx core; reverted automatically when not better", "قبل/بعد روی هستهٔ دریافت اندازه‌گیری می‌شود؛ اگر بهتر نشد خودکار برمی‌گردد")
	case "park":
		return tr("the IP's share goes to 0; a timer restores it when the time is up (also after a reboot)", "سهم این IP صفر می‌شود؛ زمان‌سنج پس از پایان مدت برش می‌گرداند (حتی پس از ری‌بوت)")
	case "stats":
		return tr("adds hs2-stats.timer (one sample every 5 min, a few ms); off again in menu 9", "hs2-stats.timer اضافه می‌شود (هر ۵ دقیقه یک نمونه، چند میلی‌ثانیه)؛ در منوی ۹ خاموش می‌شود")
	case "cert":
		return tr("dry run: changes nothing; the real renewal is `certbot renew` then restart the tunnel", "اجرای آزمایشی: چیزی تغییر نمی‌کند؛ تمدید واقعی `certbot renew` و سپس ری‌استارت تونل است")
	}
	return ""
}

func execPassthrough(name string, args ...string) error {
	if name == "hs2" {
		self, err := os.Executable()
		if err != nil {
			return err
		}
		name = self
	}
	c := exec.Command(name, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

// ---- extra deep checks (M6): netdev_budget, monitoring, certificates ----

// parseSoftnetSqueeze sums column 3 (time_squeeze) of /proc/net/softnet_stat.
func parseSoftnetSqueeze(b []byte) uint64 {
	var sum uint64
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) < 3 {
			continue
		}
		v, err := strconv.ParseUint(f[2], 16, 64)
		if err == nil {
			sum += v
		}
	}
	return sum
}

// checkBudget: the kernel ran out of netdev_budget (time_squeeze grew in the
// sampling window) with a budget below 600 → offer the measured M4 change.
func checkBudget(env *deepEnv, sq1, sq2 []byte) []finding {
	area := tr("kernel netdev_budget", "netdev_budget کرنل")
	bb, err := os.ReadFile(filepath.Join(env.proc, "sys/net/core/netdev_budget"))
	if err != nil || len(sq1) == 0 || len(sq2) == 0 {
		return nil
	}
	budget, _ := strconv.Atoi(strings.TrimSpace(string(bb)))
	d := parseSoftnetSqueeze(sq2) - parseSoftnetSqueeze(sq1)
	if parseSoftnetSqueeze(sq2) < parseSoftnetSqueeze(sq1) {
		d = 0
	}
	f := finding{Area: area, Cat: "cpu", What: fmt.Sprintf(tr("netdev_budget=%d, %d time_squeeze(s) in the window", "netdev_budget=%d، %d بار time_squeeze در بازهٔ نمونه"), budget, d)}
	if d > 0 && budget > 0 && budget < 600 {
		f.Sev = "WARN"
		f.Why = tr("the kernel stopped receiving packets before the queue was empty: bursts wait for the next round", "کرنل پیش از خالی شدن صف دریافت متوقف شد: بسته‌های انفجاری منتظر دور بعد می‌مانند")
		f.Do = tr("try netdev_budget=600 (measured, reverted when not better)", "netdev_budget=600 را امتحان کنید (اندازه‌گیری‌شده؛ اگر بهتر نشد برمی‌گردد)")
		f.Fix = &fixAction{Kind: "budget", Arg: "600", Title: tr("netdev_budget=600 with before/after measure", "netdev_budget=600 با اندازه‌گیری قبل/بعد")}
	} else {
		f.Sev = "OK"
	}
	return []finding{f}
}

// checkMonitoring: the 24 h sampler and per-tunnel alert timers.
func checkMonitoring(env *deepEnv, units []string) []finding {
	if len(units) == 0 {
		return nil
	}
	area := tr("monitoring", "پایش")
	var out []finding
	st, _ := env.run(3*time.Second, "systemctl", "is-enabled", "hs2-stats.timer")
	if strings.TrimSpace(st) != "enabled" {
		out = append(out, finding{Sev: "INFO", Area: area, Cat: "tunnel", What: tr("24 h history sampler (hs2-stats.timer) is off", "نمونه‌گیر تاریخچهٔ ۲۴ ساعته (hs2-stats.timer) خاموش است"),
			Why: tr("the dashboard has no history to show what happened at night", "داشبورد تاریخچه‌ای ندارد تا نشان دهد شب چه گذشت"),
			Fix: &fixAction{Kind: "stats", Title: tr("turn the 24 h history sampler on", "روشن کردن نمونه‌گیر تاریخچهٔ ۲۴ ساعته")}})
	}
	var off []string
	for _, u := range units {
		s, _ := env.run(3*time.Second, "systemctl", "is-enabled", "hs2-alert-"+u+".timer")
		if strings.TrimSpace(s) != "enabled" {
			off = append(off, u)
		}
	}
	if len(off) > 0 {
		out = append(out, finding{Sev: "INFO", Area: area, Cat: "tunnel", What: tr("alerts are off for: ", "هشدار خاموش است برای: ") + strings.Join(off, ", "),
			Why: tr("nobody is told when the tunnel goes down or an IP is cut", "وقتی تونل قطع شود یا IPای قطع شود کسی خبردار نمی‌شود"),
			Fix: &fixAction{Kind: "alerts", Units: off, Title: tr("open the alert setup (menu 6)", "باز کردن تنظیم هشدار (منوی ۶)")}})
	}
	return out
}

// checkCerts: tunnel certificates that expire soon (/etc/hs2/*.json cert_file).
func checkCerts(env *deepEnv, now time.Time) []finding {
	if env.etc == "" {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(env.etc, "*.json"))
	var out []finding
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var fc struct {
			CertFile string `json:"cert_file"`
		}
		if json.Unmarshal(raw, &fc) != nil || fc.CertFile == "" {
			continue
		}
		days := certFileDays(fc.CertFile, now)
		if days < 0 {
			continue
		}
		u := unitForConfig(f)
		fi := finding{Area: tr("certificate ", "گواهی ") + u, Cat: "tunnel", What: fmt.Sprintf(tr("%s expires in %d day(s)", "%s تا %d روز دیگر منقضی می‌شود"), fc.CertFile, days)}
		if days < 21 {
			fi.Sev = "WARN"
			if days < 3 {
				fi.Sev = "BAD"
			}
			fi.Why = tr("an expired certificate makes the TLS front look broken to a probe", "گواهی منقضی‌شده جلوهٔ TLS را برای کاوشگر خراب نشان می‌دهد")
			fi.Do = tr("renew: certbot renew (or acme.sh --renew -d DOMAIN), then restart "+u, "تمدید: certbot renew (یا acme.sh --renew -d DOMAIN) و سپس ری‌استارت "+u)
			fi.Fix = &fixAction{Kind: "cert", Title: tr("certbot renew --dry-run (checks renewal, changes nothing)", "certbot renew --dry-run (تمدید را می‌سنجد، چیزی تغییر نمی‌کند)")}
		} else {
			fi.Sev = "OK"
		}
		out = append(out, fi)
	}
	return out
}

// writeReport saves the findings as plain text for support (no keys: the
// findings never carry secrets).
func writeReport(path string, fs []finding, took time.Duration) error {
	var b strings.Builder
	host, _ := os.Hostname()
	fmt.Fprintf(&b, "hs2 doctor report — %s — host %s — %s\n", time.Now().Format("2006-01-02 15:04:05 MST"), host, versionLine())
	printFindingsTo(&b, fs, took)
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

func hasHS2(comms []string) bool {
	for _, c := range comms {
		if strings.HasPrefix(strings.ToLower(c), "hs2") {
			return true
		}
	}
	return false
}

// leftoverStatus: a status file whose tunnel config is not in env.etc any
// more (a deleted tunnel), or of a config outside it (a test run) — reported
// as INFO, not as a stopped tunnel. Without env.etc (tests): never.
func leftoverStatus(env *deepEnv, file string) bool {
	if env.etc == "" {
		return false
	}
	if !strings.HasPrefix(file, "etc-hs2-") {
		return true
	}
	_, err := os.Stat(filepath.Join(env.etc, strings.TrimPrefix(file, "etc-hs2-")))
	return os.IsNotExist(err)
}
