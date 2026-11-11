package main

// Menu M6, section 6 "Monitoring & alerts": the settings behind `hs2 alert`
// and the commands the menu screen runs.
//
//	hs2 alert conf show                 settings (the bot token only as set/not set)
//	hs2 alert conf set KEY=VALUE …      validated, written atomically, mode 600
//	hs2 alert conf token                read the bot token from stdin (never echoed or logged)
//	hs2 alert rule NAME on|off          enable/disable one alert kind (all tunnels)
//	hs2 alert history [--days 7] [-u U] alerts of the last days (/var/log/hs2-alert.log)
//	hs2 alert summary-now [-u U]        send the daily summary so far now (every tunnel)
//	hs2 alert telegram-check            can this server reach Telegram (direct / proxy)?
//
// New alert.conf keys: disable=rtt,psi (alert kinds that are never sent),
// quiet_hours=23:30-07:00 (only critical alerts reach Telegram in that window,
// which may cross midnight; everything is still logged), summary_items=
// users,mbit,rtt,cuts,restarts,alerts (what the daily summary shows).

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// alertRuleNames are the alert kinds `disable=` / `hs2 alert rule` accept.
var alertRuleNames = []string{"unit", "restart", "panic", "dead", "links", "ipcut", "flap", "rtt", "psi", "cert", "probe"}

// summaryItemNames are the parts of the daily summary, in order.
var summaryItemNames = []string{"users", "mbit", "rtt", "cuts", "restarts", "alerts"}

// alertConfKeys: what `conf set` may write (bot_token only via `conf token`).
var alertConfKeys = []string{"chat_id", "proxy", "lang", "summary_time", "summary_items", "quiet_hours", "disable",
	"resend_min", "rtt_factor", "rtt_min_add_ms", "cpu_pressure_pct", "cert_days", "flap_cuts", "probe", "host", "api_url", "log_file"}

var (
	reChatID   = regexp.MustCompile(`^(-?[0-9]{3,20}|@[A-Za-z0-9_]{4,64})$`)
	reBotToken = regexp.MustCompile(`^[0-9]{5,15}:[A-Za-z0-9_-]{20,64}$`)
)

// alertRuleOf maps a message key to its alert kind ("ipcut:1.2.3.4" → ipcut).
func alertRuleOf(key string) string {
	k, _, _ := strings.Cut(key, ":")
	return k
}

func validList(v string, allowed []string, what string) ([]string, error) {
	var out []string
	if strings.TrimSpace(v) == "" || v == "none" {
		return nil, nil
	}
	for _, p := range strings.Split(v, ",") {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		ok := false
		for _, a := range allowed {
			ok = ok || a == p
		}
		if !ok {
			return nil, fmt.Errorf("%s: unknown %q (one of %s)", what, p, strings.Join(allowed, ","))
		}
		out = append(out, p)
	}
	return out, nil
}

// parseHHMM: "07:05" → 425 minutes.
func parseHHMM(s string) (int, bool) {
	var h, m int
	if len(s) < 4 || len(s) > 5 {
		return 0, false
	}
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// parseQuiet: "23:00-07:00" → start, end minutes; "" / "off" → ok, no window.
func parseQuiet(v string) (start, end int, on bool, err error) {
	v = strings.TrimSpace(v)
	if v == "" || v == "off" {
		return 0, 0, false, nil
	}
	a, b, ok := strings.Cut(v, "-")
	s, ok1 := parseHHMM(strings.TrimSpace(a))
	e, ok2 := parseHHMM(strings.TrimSpace(b))
	if !ok || !ok1 || !ok2 {
		return 0, 0, false, fmt.Errorf("quiet_hours is HH:MM-HH:MM (e.g. 23:30-07:00) or off")
	}
	if s == e {
		return 0, 0, false, fmt.Errorf("quiet_hours: start and end are the same")
	}
	return s, e, true, nil
}

// inQuiet: is now inside the window (which may cross midnight)?
func inQuiet(quiet string, now time.Time) bool {
	s, e, on, err := parseQuiet(quiet)
	if err != nil || !on {
		return false
	}
	m := now.Hour()*60 + now.Minute()
	if s < e {
		return m >= s && m < e
	}
	return m >= s || m < e
}

// filterDisabled drops messages of disabled alert kinds.
func filterDisabled(cfg alertCfg, msgs []alertMsg) []alertMsg {
	if len(cfg.Disabled) == 0 {
		return msgs
	}
	var out []alertMsg
	for _, m := range msgs {
		if cfg.Disabled[alertRuleOf(m.Key)] {
			continue
		}
		out = append(out, m)
	}
	return out
}

// telegramAllowed: in quiet hours only critical alerts (and the summary at the
// time the operator chose, and a test) reach Telegram; all are still logged.
func telegramAllowed(cfg alertCfg, m alertMsg, now time.Time) bool {
	if m.Key == "summary" || m.Key == "test" || m.Sev == sevCrit || (m.Sev == sevOK && m.Was == sevCrit) {
		return true
	}
	return !inQuiet(cfg.Quiet, now)
}

func validateAlertKV(k, v string) error {
	switch k {
	case "chat_id":
		if v != "" && !reChatID.MatchString(v) {
			return fmt.Errorf("chat_id is a number (123456789, -100…) or @channel")
		}
	case "proxy":
		if v != "" {
			u, err := url.Parse(v)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
				return fmt.Errorf("proxy is http://host:port, https://… or socks5://host:port")
			}
		}
	case "api_url":
		if v != "" {
			if u, err := url.Parse(v); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
				return fmt.Errorf("api_url is http(s)://host[:port]")
			}
		}
	case "host":
		if strings.ContainsAny(v, "\n\r") || len(v) > 64 {
			return fmt.Errorf("host label: one line, at most 64 characters")
		}
	case "log_file":
		if v != "" && v != "off" && !filepath.IsAbs(v) {
			return fmt.Errorf("log_file is an absolute path or off")
		}
	}
	// everything else (numbers, lang, summary_time, lists, quiet hours) is
	// checked by the real parser on the merged file
	return nil
}

// setConfKeys returns the new file text: each key=value replaces that key
// (an empty value removes it), comments and other keys stay.
func setConfKeys(text string, kv map[string]string) string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		l := sc.Text()
		t := strings.TrimSpace(l)
		if k, _, ok := strings.Cut(t, "="); ok && !strings.HasPrefix(t, "#") {
			if _, set := kv[strings.ToLower(strings.TrimSpace(k))]; set {
				continue
			}
		}
		out = append(out, l)
	}
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if kv[k] != "" {
			out = append(out, k+"="+kv[k])
		}
	}
	s := strings.Join(out, "\n")
	if s != "" {
		s += "\n"
	}
	return s
}

// writeAlertConf writes the file atomically with mode 600 (it holds the token).
func writeAlertConf(path, text string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o600); err != nil {
		return err
	}
	_ = os.Chmod(tmp, 0o600)
	return os.Rename(tmp, path)
}

func notRootHint(err error) string {
	if os.IsPermission(err) || os.Geteuid() != 0 {
		return tr(" — run the menu as root (sudo hs2-menu): alert.conf and the timers belong to root",
			" — منو را با root اجرا کنید (sudo hs2-menu): alert.conf و تایمرها متعلق به root هستند")
	}
	return ""
}

// confSet validates and writes KEY=VALUE pairs. Nothing is written on error.
func confSet(path string, pairs []string) error {
	kv := map[string]string{}
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		if !ok {
			return fmt.Errorf("want KEY=VALUE, got %q", p)
		}
		if k == "bot_token" {
			return fmt.Errorf("the bot token is set with `hs2 alert conf token` (read from stdin, never on the command line)")
		}
		known := false
		for _, a := range alertConfKeys {
			known = known || a == k
		}
		if !known {
			return fmt.Errorf("unknown key %q (one of %s)", k, strings.Join(alertConfKeys, ", "))
		}
		if strings.ContainsAny(v, "\n\r") {
			return fmt.Errorf("%s: one line only", k)
		}
		if err := validateAlertKV(k, v); err != nil {
			return err
		}
		kv[k] = v
	}
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	text := setConfKeys(string(old), kv)
	if _, err := parseAlertConf(text); err != nil {
		return err
	}
	return writeAlertConf(path, text)
}

// confToken reads one line (the token) from r and stores it; empty removes it.
func confToken(path string, r io.Reader) error {
	line, _ := bufio.NewReader(r).ReadString('\n')
	tok := strings.TrimSpace(line)
	if tok != "" && !reBotToken.MatchString(tok) {
		return fmt.Errorf("that does not look like a bot token (123456789:AA…, from @BotFather) — nothing saved")
	}
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	text := setConfKeys(string(old), map[string]string{"bot_token": tok})
	if _, err := parseAlertConf(text); err != nil {
		return err
	}
	return writeAlertConf(path, text)
}

// ruleSet turns one alert kind on or off in the disable= list.
func ruleSet(path, name string, on bool) error {
	if _, err := validList(name, alertRuleNames, "alert"); err != nil {
		return err
	}
	cfg, _ := loadAlertConf(path)
	d := map[string]bool{}
	for k := range cfg.Disabled {
		d[k] = true
	}
	if on {
		delete(d, name)
	} else {
		d[name] = true
	}
	var l []string
	for _, n := range alertRuleNames {
		if d[n] {
			l = append(l, n)
		}
	}
	old, _ := os.ReadFile(path)
	return writeAlertConf(path, setConfKeys(string(old), map[string]string{"disable": strings.Join(l, ",")}))
}

func confShow(path string) {
	cfg, warns := loadAlertConf(path)
	for _, w := range warns {
		fmt.Println("  ! " + w)
	}
	set := tr("not set — alerts go to the journal and the log only", "تنظیم نشده — هشدارها فقط به ژورنال و لاگ می‌روند")
	if cfg.BotToken != "" {
		set = tr("set (hidden)", "تنظیم شده (پنهان)")
	}
	or := func(v, d string) string {
		if v == "" {
			return d
		}
		return v
	}
	fmt.Printf("  %-16s %s\n", tr("bot token:", "توکن ربات:"), set)
	fmt.Printf("  %-16s %s\n", tr("chat id:", "شناسهٔ چت:"), or(cfg.ChatID, "-"))
	fmt.Printf("  %-16s %s\n", tr("proxy:", "پراکسی:"), or(cfg.Proxy, tr("none (direct)", "ندارد (مستقیم)")))
	fmt.Printf("  %-16s %s\n", tr("language:", "زبان:"), cfg.Lang)
	fmt.Printf("  %-16s %s\n", tr("quiet hours:", "ساعت سکوت:"), or(cfg.Quiet, "off"))
	fmt.Printf("  %-16s %s   %s\n", tr("daily summary:", "خلاصهٔ روزانه:"), or(cfg.SummaryAt, "off"), strings.Join(summaryItems(cfg), ","))
	fmt.Println("  " + tr("alert kinds (thresholds):", "انواع هشدار (آستانه‌ها):"))
	th := map[string]string{
		"rtt":   fmt.Sprintf("rtt_factor=%.2g rtt_min_add_ms=%.0f", cfg.RTTFactor, cfg.RTTMinAddMs),
		"psi":   fmt.Sprintf("cpu_pressure_pct=%.0f", cfg.PSIPct),
		"cert":  fmt.Sprintf("cert_days=%d", cfg.CertDays),
		"flap":  fmt.Sprintf("flap_cuts=%d", cfg.FlapCuts),
		"probe": "probe=" + cfg.Probe,
	}
	for i, n := range alertRuleNames {
		st := tr("on ", "روشن ")
		if cfg.Disabled[n] {
			st = tr("OFF", "خاموش")
		}
		fmt.Printf("   %2d) %-8s %-6s %s\n", i+1, n, st, th[n])
	}
	fmt.Printf("  %-16s resend_min=%d\n", tr("repeat:", "تکرار:"), cfg.ResendMin)
}

func summaryItems(cfg alertCfg) []string {
	if len(cfg.SummaryItems) == 0 {
		return summaryItemNames
	}
	return cfg.SummaryItems
}

// alertHistory prints the log lines of the last `days` days (optionally of one unit).
func alertHistory(cfg alertCfg, unit string, days int, now time.Time, w io.Writer) int {
	f, err := os.Open(cfg.LogFile)
	if err != nil {
		fmt.Fprintln(w, tr("  (no alerts yet — "+cfg.LogFile+" does not exist)", "  (هنوز هشداری نیست — "+cfg.LogFile+" وجود ندارد)"))
		return 0
	}
	defer f.Close()
	return filterHistory(f, unit, days, now, w)
}

func filterHistory(r io.Reader, unit string, days int, now time.Time, w io.Writer) int {
	from := now.AddDate(0, 0, -days)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	n := 0
	perDay := map[string]int{}
	var order []string
	for sc.Scan() {
		l := sc.Text()
		if len(l) < 19 {
			continue
		}
		ts, err := time.ParseInLocation("2006-01-02 15:04:05", l[:19], now.Location())
		if err != nil || ts.Before(from) {
			continue
		}
		if unit != "" && !strings.Contains(l, "/"+unit+"]") {
			continue
		}
		d := l[:10]
		if perDay[d] == 0 {
			order = append(order, d)
		}
		perDay[d]++
		fmt.Fprintln(w, l)
		n++
	}
	if n == 0 {
		fmt.Fprintf(w, tr("  (no alerts in the last %d days)\n", "  (در %d روز گذشته هشداری نبوده)\n"), days)
		return 0
	}
	var parts []string
	for _, d := range order {
		parts = append(parts, fmt.Sprintf("%s:%d", d[5:], perDay[d]))
	}
	fmt.Fprintf(w, tr("  %d alert line(s); per day: %s\n", "  %d خط هشدار؛ هر روز: %s\n"), n, strings.Join(parts, " "))
	return n
}

// telegramCheck: can this server reach the Telegram API (directly, or the proxy)?
func telegramCheck(cfg alertCfg, dial func(addr string) error) (ok bool, msg string) {
	api, _ := url.Parse(firstNonEmpty(cfg.APIURL, "https://api.telegram.org"))
	host := api.Host
	if api.Port() == "" {
		if api.Scheme == "http" {
			host = net.JoinHostPort(api.Hostname(), "80")
		} else {
			host = net.JoinHostPort(api.Hostname(), "443")
		}
	}
	direct := dial(host)
	if cfg.Proxy == "" {
		if direct == nil {
			return true, tr("Telegram ("+api.Hostname()+") is reachable directly — no proxy needed",
				"تلگرام ("+api.Hostname()+") مستقیم در دسترس است — پراکسی لازم نیست")
		}
		return false, tr("Telegram ("+api.Hostname()+") is NOT reachable from this server (typical on Iran servers): set a proxy (socks5://host:port or http://host:port, e.g. a local xray socks inbound) — until then alerts go to the journal and "+cfg.LogFile+" only",
			"تلگرام ("+api.Hostname()+") از این سرور در دسترس نیست (معمول در سرورهای ایران): یک پراکسی تنظیم کنید (socks5://host:port یا http://host:port، مثلاً یک ورودی socks محلی xray) — تا آن موقع هشدارها فقط به ژورنال و "+cfg.LogFile+" می‌روند")
	}
	pu, err := url.Parse(cfg.Proxy)
	if err != nil || pu.Host == "" {
		return false, tr("the proxy URL is not valid", "آدرس پراکسی معتبر نیست")
	}
	if e := dial(pu.Host); e != nil {
		return false, tr("the proxy "+pu.Host+" does not answer: "+e.Error(), "پراکسی "+pu.Host+" پاسخ نمی‌دهد: "+e.Error())
	}
	m := tr("the proxy "+pu.Host+" answers", "پراکسی "+pu.Host+" پاسخ می‌دهد")
	if direct == nil {
		m += tr(" (Telegram is reachable directly too)", " (تلگرام مستقیم هم در دسترس است)")
	}
	return true, m + tr(" — send a test message to be sure", " — برای اطمینان یک پیام آزمایشی بفرستید")
}

func dialTCP(addr string) error {
	c, err := net.DialTimeout("tcp", addr, 6*time.Second)
	if err == nil {
		c.Close()
	}
	return err
}

// summaryNow sends the summary of the day so far for each watched tunnel,
// without resetting the day (the scheduled summary still comes).
func summaryNow(cfg alertCfg, stateDir, unit string, now time.Time) (int, error) {
	files, _ := filepath.Glob(filepath.Join(stateDir, "alert-*.json"))
	sent := 0
	var lastErr error
	for _, f := range files {
		u := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "alert-"), ".json")
		if unit != "" && u != unit {
			continue
		}
		st := loadAlertState(f)
		text := takeSummary(cfg, st, now) // st is a copy: not saved
		if err := deliver(cfg, u, []alertMsg{{Key: "summary", Sev: sevInfo, Text: text}}); err != nil {
			lastErr = err
			continue
		}
		sent++
	}
	return sent, lastErr
}

// alertSubCmd handles the menu-facing subcommands; false = not one of them.
func alertSubCmd(args []string) bool {
	if len(args) == 0 {
		return false
	}
	sub := args[0]
	if sub == "--supported" {
		return true // the menu probes for these subcommands (an older binary would run a check)
	}
	switch sub {
	case "conf", "rule", "history", "summary-now", "telegram-check":
	default:
		return false
	}
	fs := flag.NewFlagSet("alert "+sub, flag.ExitOnError)
	conf := fs.String("conf", alertConfPath, "alert settings")
	stateDir := fs.String("state", alertStateDir, "state directory")
	unit := fs.String("u", "", "tunnel unit (default: all)")
	days := fs.Int("days", 7, "history: days")
	lang := fs.String("lang", "", "fa | en")
	rest := parseInterleaved(fs, args[1:])
	if *lang == "fa" {
		deepLang = "fa"
	}
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, tr("ERROR: ", "خطا: ")+err.Error()+notRootHint(err))
		os.Exit(1)
	}
	switch sub {
	case "conf":
		if len(rest) == 0 || rest[0] == "show" {
			confShow(*conf)
			return true
		}
		switch rest[0] {
		case "set":
			if len(rest) < 2 {
				fail(fmt.Errorf("usage: hs2 alert conf set KEY=VALUE …"))
			}
			if err := confSet(*conf, rest[1:]); err != nil {
				fail(err)
			}
			fmt.Println(tr("saved to ", "ذخیره شد در ") + *conf + " (mode 600)")
		case "token":
			if err := confToken(*conf, os.Stdin); err != nil {
				fail(err)
			}
			fmt.Println(tr("bot token saved (hidden) to ", "توکن ربات (پنهان) ذخیره شد در ") + *conf)
		default:
			fail(fmt.Errorf("usage: hs2 alert conf show|set KEY=VALUE…|token"))
		}
	case "rule":
		if len(rest) != 2 || (rest[1] != "on" && rest[1] != "off") {
			fail(fmt.Errorf("usage: hs2 alert rule NAME on|off  (NAME: %s)", strings.Join(alertRuleNames, ",")))
		}
		if err := ruleSet(*conf, strings.ToLower(rest[0]), rest[1] == "on"); err != nil {
			fail(err)
		}
		fmt.Println(tr("saved: ", "ذخیره شد: ") + rest[0] + " " + rest[1])
	case "history":
		cfg, _ := loadAlertConf(*conf)
		alertHistory(cfg, *unit, *days, time.Now(), os.Stdout)
	case "summary-now":
		cfg, _ := loadAlertConf(*conf)
		if cfg.Host == "" {
			cfg.Host, _ = os.Hostname()
		}
		n, err := summaryNow(cfg, *stateDir, *unit, time.Now())
		if err != nil {
			fail(fmt.Errorf("telegram: %v", err))
		}
		if n == 0 {
			fmt.Println(tr("no watched tunnel yet (turn alerts on for a tunnel first)", "هنوز تونلی پایش نمی‌شود (اول هشدار را برای یک تونل روشن کنید)"))
			return true
		}
		fmt.Printf(tr("summary sent for %d tunnel(s)\n", "خلاصه برای %d تونل فرستاده شد\n"), n)
	case "telegram-check":
		cfg, _ := loadAlertConf(*conf)
		ok, m := telegramCheck(cfg, dialTCP)
		fmt.Println(m)
		if !ok {
			os.Exit(1)
		}
	}
	return true
}
