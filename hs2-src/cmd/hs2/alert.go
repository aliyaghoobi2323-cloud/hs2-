package main

// `hs2 alert -c <config>`: an on-server watcher for one tunnel. Run every
// 60 s by a systemd timer (hs2-alert-<unit>.timer, set up from the menu) or
// with --loop. Read-only toward the system: it reads the status file, asks
// systemctl/journalctl, reads /proc/pressure/cpu and the cert file, and
// writes only its own state (/var/lib/hs2/alert-<unit>.json) and log
// (/var/log/hs2-alert.log). Alerts go to the journal (logger), the log file
// and — when /etc/hs2/alert.conf has bot_token and chat_id — Telegram.

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var (
	alertConfPath = "/etc/hs2/alert.conf"
	alertStateDir = "/var/lib/hs2"
)

// parseAlertConf reads key=value lines (# comments, optional quotes).
func parseAlertConf(text string) (alertCfg, error) {
	c := defaultAlertCfg()
	sc := bufio.NewScanner(strings.NewReader(text))
	ln := 0
	for sc.Scan() {
		ln++
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			return c, fmt.Errorf("line %d: want key=value", ln)
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		num := func() (float64, error) {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || f < 0 {
				return 0, fmt.Errorf("line %d: %s wants a number", ln, k)
			}
			return f, nil
		}
		var err error
		var f float64
		switch k {
		case "bot_token":
			c.BotToken = v
		case "chat_id":
			c.ChatID = v
		case "proxy":
			c.Proxy = v
		case "api_url":
			c.APIURL = v
		case "lang":
			if v != "fa" && v != "en" {
				return c, fmt.Errorf("line %d: lang is fa or en", ln)
			}
			c.Lang = v
		case "summary_time":
			var h, m int
			if v != "off" && v != "" {
				if _, e := fmt.Sscanf(v, "%d:%d", &h, &m); e != nil || h > 23 || m > 59 || h < 0 || m < 0 {
					return c, fmt.Errorf("line %d: summary_time is HH:MM or off", ln)
				}
			}
			c.SummaryAt = v
		case "resend_min":
			f, err = num()
			c.ResendMin = int(f)
		case "rtt_factor":
			f, err = num()
			c.RTTFactor = f
		case "rtt_min_add_ms":
			f, err = num()
			c.RTTMinAddMs = f
		case "cpu_pressure_pct":
			f, err = num()
			c.PSIPct = f
		case "cert_days":
			f, err = num()
			c.CertDays = int(f)
		case "flap_cuts":
			f, err = num()
			c.FlapCuts = int(f)
		case "probe":
			if v != "off" && v != "tcp" && v != "tls" {
				return c, fmt.Errorf("line %d: probe is off, tcp or tls", ln)
			}
			c.Probe = v
		case "log_file":
			c.LogFile = v
		case "host":
			c.Host = v
		case "disable":
			var l []string
			if l, err = validList(v, alertRuleNames, "disable"); err == nil {
				c.Disabled = map[string]bool{}
				for _, n := range l {
					c.Disabled[n] = true
				}
			}
		case "quiet_hours":
			if _, _, _, err = parseQuiet(v); err == nil {
				c.Quiet = v
				if v == "off" {
					c.Quiet = ""
				}
			}
		case "summary_items":
			c.SummaryItems, err = validList(v, summaryItemNames, "summary_items")
		default:
			// unknown keys are ignored (newer menu, older binary)
		}
		if err != nil {
			if strings.HasPrefix(err.Error(), "line ") {
				return c, err
			}
			return c, fmt.Errorf("line %d: %v", ln, err)
		}
	}
	if c.ResendMin < 1 {
		c.ResendMin = 1
	}
	if c.FlapCuts < 1 {
		c.FlapCuts = 1
	}
	if c.RTTFactor < 1 {
		c.RTTFactor = 1
	}
	return c, sc.Err()
}

func loadAlertConf(path string) (alertCfg, []string) {
	var warns []string
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			warns = append(warns, "cannot read "+path+": "+err.Error())
		}
		return defaultAlertCfg(), warns
	}
	if fi, e := os.Stat(path); e == nil && fi.Mode().Perm()&0o077 != 0 {
		warns = append(warns, fmt.Sprintf("%s is mode %o — it holds the bot token; run: chmod 600 %s", path, fi.Mode().Perm(), path))
	}
	c, err := parseAlertConf(string(b))
	if err != nil {
		warns = append(warns, path+": "+err.Error())
	}
	return c, warns
}

// unitForConfig: /etc/hs2/config.json -> hs2, /etc/hs2/<n>.json -> hs2-<n>.
func unitForConfig(cfgPath string) string {
	base := strings.TrimSuffix(filepath.Base(cfgPath), ".json")
	if base == "config" || base == "" {
		return "hs2"
	}
	if strings.HasPrefix(base, "hs2") {
		return base
	}
	return "hs2-" + base
}

func alertCmd(args []string) {
	if alertSubCmd(args) {
		return
	}
	fs := flag.NewFlagSet("alert", flag.ExitOnError)
	cfgPath := fs.String("c", "/etc/hs2/config.json", "tunnel config")
	unit := fs.String("u", "", "systemd unit (default: from the config name)")
	conf := fs.String("conf", alertConfPath, "alert settings (key=value, mode 600)")
	stateDir := fs.String("state", alertStateDir, "state directory")
	loop := fs.Duration("loop", 0, "keep running, one check per interval (0 = one check)")
	test := fs.Bool("test", false, "send a test message and exit")
	dry := fs.Bool("dry", false, "print what would be sent, change no state")
	_ = fs.Parse(args)
	if *unit == "" {
		*unit = unitForConfig(*cfgPath)
	}
	cfg, warns := loadAlertConf(*conf)
	if cfg.Host == "" {
		cfg.Host, _ = os.Hostname()
	}
	for _, w := range warns {
		fmt.Fprintln(os.Stderr, "hs2 alert:", w)
	}
	if *test {
		m := alertMsg{Key: "test", Sev: sevInfo, Text: alertText(cfg.Lang).test()}
		err := deliver(cfg, *unit, []alertMsg{m})
		if err != nil {
			fmt.Fprintln(os.Stderr, "hs2 alert: telegram:", err)
			os.Exit(1)
		}
		if cfg.BotToken == "" || cfg.ChatID == "" {
			fmt.Println("no bot_token/chat_id — written to the journal and", cfg.LogFile, "only")
		} else {
			fmt.Println("test message sent")
		}
		return
	}
	for {
		runAlertOnce(cfg, *cfgPath, *unit, *stateDir, *dry)
		if *loop <= 0 {
			return
		}
		time.Sleep(*loop)
	}
}

func alertStatePath(dir, unit string) string {
	return filepath.Join(dir, "alert-"+unit+".json")
}

func loadAlertState(path string) *alertState {
	st := newAlertState()
	if b, err := os.ReadFile(path); err == nil {
		if json.Unmarshal(b, st) != nil {
			st = newAlertState()
		}
	}
	st.LastNRest = maxInt(st.LastNRest, -1)
	return st
}

func saveAlertState(path string, st *alertState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(st)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func runAlertOnce(cfg alertCfg, cfgPath, unit, stateDir string, dry bool) {
	now := time.Now()
	snap := collectAlertSnap(cfg, cfgPath, unit, now)
	if snap.Active == "not-found" {
		// the tunnel was deleted; its alert timer is left over — stay quiet
		fmt.Fprintln(os.Stderr, "hs2 alert: unit", unit, "not found — nothing to watch")
		return
	}
	sp := alertStatePath(stateDir, unit)
	st := loadAlertState(sp)
	msgs := filterDisabled(cfg, evalAlerts(cfg, st, snap))
	sortMsgs(msgs)
	if dueSummary(cfg, st, now) {
		msgs = append(msgs, alertMsg{Key: "summary", Sev: sevInfo, Text: takeSummary(cfg, st, now)})
	}
	if dry {
		for _, m := range msgs {
			fmt.Println(formatAlert(cfg.Host, unit, m))
		}
		return
	}
	if err := deliver(cfg, unit, msgs); err != nil {
		writeAlertLog(cfg, unit, "telegram send failed: "+err.Error())
	}
	for _, m := range msgs {
		st.Recent = append(st.Recent, now.Format("01-02 15:04 ")+formatAlert(cfg.Host, unit, m))
	}
	if len(st.Recent) > 20 {
		st.Recent = st.Recent[len(st.Recent)-20:]
	}
	if err := saveAlertState(sp, st); err != nil {
		fmt.Fprintln(os.Stderr, "hs2 alert: state:", err)
	}
	// the dashboard's 24 h history rides on the alert check (stats.go)
	if err := sampleStatsFor(cfgPath, unit, statusRunDir, stateDir, now); err != nil {
		fmt.Fprintln(os.Stderr, "hs2 alert: stats:", err)
	}
}

// deliver writes every message to the journal and the log file, then (if
// configured) sends them to Telegram as one message.
func deliver(cfg alertCfg, unit string, msgs []alertMsg) error {
	if len(msgs) == 0 {
		return nil
	}
	var lines []string
	now := time.Now()
	for _, m := range msgs {
		l := formatAlert(cfg.Host, unit, m)
		if telegramAllowed(cfg, m, now) {
			lines = append(lines, l)
		} else {
			l += " [quiet hours: not sent]"
		}
		writeAlertLog(cfg, unit, l)
		_ = exec.Command("logger", "-t", "hs2-alert", "-p", "daemon."+syslogPrio(m.Sev), "--", l).Run()
	}
	if cfg.BotToken == "" || cfg.ChatID == "" || len(lines) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return sendTelegram(ctx, cfg, strings.Join(lines, "\n"))
}

func syslogPrio(sev string) string {
	switch sev {
	case sevCrit:
		return "crit"
	case sevWarn:
		return "warning"
	}
	return "info"
}

func writeAlertLog(cfg alertCfg, unit, line string) {
	if cfg.LogFile == "" || cfg.LogFile == "off" {
		return
	}
	f, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), redact(line, cfg.BotToken))
}

// ---- collection (read-only) ----

func collectAlertSnap(cfg alertCfg, cfgPath, unit string, now time.Time) alertSnap {
	s := alertSnap{Now: now, Unit: unit, NRestarts: -1, PSI60: -1, CertDays: -1, UptimeS: -1}
	props := systemctlShow(unit, "LoadState", "ActiveState", "MainPID", "NRestarts", "ActiveEnterTimestampMonotonic")
	s.Active = props["ActiveState"]
	if props["LoadState"] == "not-found" {
		s.Active = "not-found"
		return s
	}
	if s.Active == "" {
		out, _ := exec.Command("systemctl", "is-active", unit).Output()
		s.Active = strings.TrimSpace(string(out))
	}
	s.MainPID, _ = strconv.Atoi(props["MainPID"])
	if n, err := strconv.Atoi(props["NRestarts"]); err == nil {
		s.NRestarts = n
	}
	if us, err := strconv.ParseInt(props["ActiveEnterTimestampMonotonic"], 10, 64); err == nil && us > 0 {
		if up := monotonicNowUs(); up > us {
			s.UptimeS = (up - us) / 1e6
		}
	}
	s.Panics = journalPanics(unit)
	s.Status = readAlertStatus(statusPath(cfgPath), now)
	s.PSI60 = readPSI60("/proc/pressure/cpu")
	var fc fileConfig
	if b, err := os.ReadFile(cfgPath); err == nil {
		_ = json.Unmarshal(b, &fc)
	}
	// Only a side that serves TLS has a cert to watch; a missing/unreadable
	// file is "unknown", not an alert (the dialing side has none).
	if fc.CertFile != "" {
		s.CertDays = certFileDays(fc.CertFile, now)
	}
	if s.CertDays < 0 && s.Status != nil && s.Status.CertDays != nil && *s.Status.CertDays >= 0 {
		s.CertDays = *s.Status.CertDays
	}
	if (cfg.Probe == "tcp" || cfg.Probe == "tls") && listensHere(fc) {
		s.ProbeMs, s.ProbeErr = probeOwn(cfg.Probe, fc.Addr)
	}
	return s
}

func systemctlShow(unit string, props ...string) map[string]string {
	out := map[string]string{}
	b, err := exec.Command("systemctl", "show", unit, "-p", strings.Join(props, ",")).Output()
	if err != nil {
		return out
	}
	for _, l := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
			out[k] = v
		}
	}
	return out
}

func monotonicNowUs() int64 {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	f, err := strconv.ParseFloat(strings.Fields(string(b) + " 0")[0], 64)
	if err != nil {
		return 0
	}
	return int64(f * 1e6)
}

// journalPanics: panic/fatal lines of the last 2 minutes.
func journalPanics(unit string) []panicLine {
	b, err := exec.Command("journalctl", "-u", unit, "--since", "-2min", "-o", "short-unix", "--no-pager", "-q").Output()
	if err != nil {
		return nil
	}
	return parsePanicLines(string(b))
}

func parsePanicLines(out string) []panicLine {
	var ps []panicLine
	for _, l := range strings.Split(out, "\n") {
		if !strings.Contains(l, "panic:") && !strings.Contains(l, "fatal error:") {
			continue
		}
		ts, rest, _ := strings.Cut(l, " ")
		f, err := strconv.ParseFloat(ts, 64)
		if err != nil {
			continue
		}
		if i := strings.Index(rest, ": "); i >= 0 { // drop "host hs2[pid]"
			rest = rest[i+2:]
		}
		ps = append(ps, panicLine{TS: int64(f * 1e6), Text: rest})
	}
	return ps
}

// readAlertStatus returns nil for a missing, unreadable or stale (> 30 s)
// status file: "unknown", never a failure by itself.
func readAlertStatus(path string, now time.Time) *alertStatus {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return parseAlertStatus(b, now)
}

func parseAlertStatus(b []byte, now time.Time) *alertStatus {
	var ls alertStatus
	if json.Unmarshal(b, &ls) != nil {
		return nil
	}
	if ls.Updated > 0 && now.Unix()-ls.Updated > 30 {
		return nil
	}
	return &ls
}

func readPSI60(path string) float64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	return parsePSI60(string(b))
}

func parsePSI60(text string) float64 {
	for _, l := range strings.Split(text, "\n") {
		if !strings.HasPrefix(l, "some ") {
			continue
		}
		for _, f := range strings.Fields(l) {
			if v, ok := strings.CutPrefix(f, "avg60="); ok {
				if x, err := strconv.ParseFloat(v, 64); err == nil {
					return x
				}
			}
		}
	}
	return -1
}

func certFileDays(path string, now time.Time) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return -1
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return -1
	}
	d := int(c.NotAfter.Sub(now).Hours() / 24)
	if d < 0 {
		d = 0
	}
	return d
}

// listensHere: this side runs the listener (direct exit, or reverse edge).
func listensHere(fc fileConfig) bool {
	return fc.Addr != "" && ((fc.Mode == "listen") != fc.Reverse)
}

// probeOwn: TCP connect (+ TLS handshake) to the tunnel's own listen
// address on loopback. Cheap; one connection per check.
func probeOwn(kind, addr string) (float64, string) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, ""
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	t0 := time.Now()
	c, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 5*time.Second)
	if err != nil {
		return 0, clip(err.Error(), 80)
	}
	defer c.Close()
	if kind == "tls" {
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		tc := tls.Client(c, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // reachability probe only
		if err := tc.Handshake(); err != nil {
			return 0, clip(err.Error(), 80)
		}
	}
	return float64(time.Since(t0).Microseconds()) / 1000, ""
}
