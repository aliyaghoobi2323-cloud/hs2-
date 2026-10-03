package main

import (
	"bufio"
	"crypto/x509"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Certificate RENEWAL health (read-only). checkCert answers "is the cert valid
// now"; this answers "will it still be valid next month". The two differ in a
// way that matters: the client side never verifies the certificate (link auth
// is the shared key bound to the TLS session), so an expired certificate does
// not break the tunnel — users notice nothing — but every probe and browser then
// sees an expired certificate on a live :443 service, which an ordinary site
// does not keep for long. Renewal failures are therefore silent, and this check
// is where they surface.
//
// What it reads: certbot's renewal config for the lineage the config's
// cert_file points into, the kernel's listening sockets (/proc/net/tcp*), and
// whether a certbot renewal timer is active. It never runs certbot (a dry run
// binds port 80 and contacts Let's Encrypt — not read-only); the installer's
// Certificate screen offers that as an explicit action.

// letsencryptDir is where certbot keeps its lineages (a var so tests can point
// it at a temporary tree).
var letsencryptDir = "/etc/letsencrypt"

// renewEnv is everything checkCertRenewal reads from the host, injectable so
// the decision logic is tested without root, certbot or systemd.
type renewEnv struct {
	leDir string
	now   time.Time
	// portBusy: busy is true when something LISTENS on that TCP port; known is
	// false when that cannot be determined here.
	portBusy func(port int) (busy, known bool)
	// scheduler names the active certbot renewal scheduler ("" when none);
	// known is false when this host's init system cannot be queried.
	scheduler func() (name string, known bool)
}

func hostRenewEnv() renewEnv {
	return renewEnv{leDir: letsencryptDir, now: time.Now(), portBusy: portListening, scheduler: certbotScheduler}
}

// renewalConf is the part of a certbot renewal config this check needs.
type renewalConf struct {
	authenticator   string
	manualAuthHook  string
	preHook         string // e.g. "systemctl stop nginx": may free port 80 itself
	directoryHooks  string // "False" turns off renewal-hooks/ for this lineage
	autorenew       string
	http01Port      int // standalone's listen port; 80 when absent
	renewBeforeDays int // explicit renew_before_expiry in days; 0 when absent
}

// renewThresholdDays is when certbot renews this certificate: the explicit
// renew_before_expiry, else certbot's own default — a third of the
// certificate's lifetime (half, under 10 days). For a 90-day Let's Encrypt
// certificate that is 30 days, but a shorter-lived one renews later, and a
// fixed 30 would call it "overdue" while certbot correctly waits.
func renewThresholdDays(rc renewalConf, leaf *x509.Certificate) int {
	if rc.renewBeforeDays > 0 {
		return rc.renewBeforeDays
	}
	life := leaf.NotAfter.Sub(leaf.NotBefore)
	part := life / 3
	if life < 10*24*time.Hour {
		part = life / 2
	}
	if d := int(part.Hours()/24 + 0.5); d > 0 {
		return d
	}
	return 30 // no usable validity period: certbot's historical default
}

// certbotLineage returns the lineage name when certFile lives in
// <leDir>/live/<name>/ (how certbot and the installer lay it out), else "".
// "//" and "/./" are tolerated, but a ".." element never is — it is not
// resolved into some other lineage (the installer's cert_lineage agrees).
func certbotLineage(leDir, certFile string) string {
	if strings.HasSuffix(certFile, string(filepath.Separator)) {
		return ""
	}
	for _, el := range strings.Split(certFile, string(filepath.Separator)) {
		if el == ".." {
			return ""
		}
	}
	live := filepath.Clean(filepath.Join(leDir, "live")) + string(filepath.Separator)
	p := filepath.Clean(certFile)
	if !strings.HasPrefix(p, live) {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(p, live), string(filepath.Separator))
	if len(parts) != 2 || parts[0] == "" || parts[0] == "." || parts[0] == ".." {
		return ""
	}
	return parts[0]
}

// readRenewalConf parses certbot's INI-style renewal config ("key = value"
// lines; sections and comments ignored — the keys read here are unique).
func readRenewalConf(path string) (renewalConf, error) {
	rc := renewalConf{http01Port: 80}
	f, err := os.Open(path)
	if err != nil {
		return rc, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == '[' {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "authenticator":
			rc.authenticator = v
		case "manual_auth_hook":
			rc.manualAuthHook = v
		case "pre_hook":
			rc.preHook = v
		case "directory_hooks":
			rc.directoryHooks = v
		case "http01_port":
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 65536 {
				rc.http01Port = n
			}
		case "autorenew":
			rc.autorenew = v
		case "renew_before_expiry":
			if n := parseRenewBefore(v); n > 0 {
				rc.renewBeforeDays = n
			}
		}
	}
	return rc, sc.Err()
}

// parseRenewBefore reads certbot's "30 days" / "2 weeks" form; 0 if unknown.
func parseRenewBefore(v string) int {
	f := strings.Fields(strings.ToLower(v))
	if len(f) != 2 {
		return 0
	}
	n, err := strconv.Atoi(f[0])
	if err != nil || n <= 0 {
		return 0
	}
	switch strings.TrimSuffix(f[1], "s") {
	case "day":
		return n
	case "week":
		return n * 7
	}
	return 0
}

// knownPreHook describes a certbot pre-hook that runs before every renewal —
// the usual way to free port 80 for standalone ("systemctl stop nginx") — or
// returns "". certbot takes them from three places: the lineage's pre_hook, an
// executable in renewal-hooks/pre/ (run by default, dry runs included; listed
// the way certbot lists them: dotfiles count, "~" backups do not, and the
// whole directory is off with directory_hooks = False / no-directory-hooks),
// and a pre-hook line in cli.ini.
func knownPreHook(leDir string, rc renewalConf) string {
	if rc.preHook != "" {
		return fmt.Sprintf("pre_hook %q", rc.preHook)
	}
	ini := readCliIni(filepath.Join(leDir, "cli.ini"))
	dirHooks := !strings.EqualFold(rc.directoryHooks, "false") && !iniTrue(ini, "no-directory-hooks", "no_directory_hooks")
	if dirHooks {
		dir := filepath.Join(leDir, "renewal-hooks", "pre")
		if ents, err := os.ReadDir(dir); err == nil {
			for _, e := range ents {
				if strings.HasSuffix(e.Name(), "~") {
					continue
				}
				if fi, err := os.Stat(filepath.Join(dir, e.Name())); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
					return "the hook " + filepath.Join(dir, e.Name())
				}
			}
		}
	}
	for _, k := range []string{"pre-hook", "pre_hook"} {
		if v, ok := ini[k]; ok && v != "" {
			return fmt.Sprintf("the cli.ini pre-hook %q", v)
		}
	}
	return ""
}

// readCliIni reads certbot's cli.ini: "key = value" lines and bare flags
// ("no-directory-hooks", value ""). Comments and sections are skipped.
func readCliIni(path string) map[string]string {
	m := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return m
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || l[0] == '#' || l[0] == ';' || l[0] == '[' {
			continue
		}
		k, v, _ := strings.Cut(l, "=")
		m[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return m
}

// iniTrue: one of the flags is set (bare, or = true/yes/1).
func iniTrue(ini map[string]string, keys ...string) bool {
	for _, k := range keys {
		if v, ok := ini[k]; ok {
			switch strings.ToLower(v) {
			case "", "true", "yes", "1", "on":
				return true
			}
		}
	}
	return false
}

// checkCertRenewal reports how leaf (the cert checkCert just loaded, for a side
// that terminates TLS) gets renewed, and whether that is going to work.
func checkCertRenewal(d *doctorReport, fc fileConfig, leaf *x509.Certificate, env renewEnv) {
	const name = "cert renewal"
	until := leaf.NotAfter.UTC().Format("2006-01-02")
	daysLeft := int(leaf.NotAfter.Sub(env.now).Hours() / 24)

	lineage := certbotLineage(env.leDir, fc.CertFile)
	if lineage == "" {
		msg := "not managed by certbot — you renew it yourself; replace " + fc.CertFile + " and hs2 reloads it within a minute"
		if daysLeft < 30 {
			d.warn(name, msg+" (renew before "+until+")")
		} else {
			d.info(name, msg+" (expires "+until+")")
		}
		return
	}
	confPath := filepath.Join(env.leDir, "renewal", lineage+".conf")
	rc, err := readRenewalConf(confPath)
	if err != nil {
		d.warn(name, "certbot has no readable renewal config for "+lineage+" ("+confPath+") — it will NOT be renewed; renew before "+until)
		return
	}
	if strings.EqualFold(rc.autorenew, "false") {
		d.warn(name, "automatic renewal is switched off for "+lineage+" (autorenew = False) — renew it by hand before "+until)
		return
	}
	if rc.authenticator == "manual" && rc.manualAuthHook == "" {
		// certbot cannot renew a --manual certificate unattended: someone must
		// publish a fresh DNS TXT record each time.
		d.warn(name, "DNS-01 (manual) — does NOT renew automatically; renew it by hand before "+until+" (installer: this tunnel → c) Certificate)")
		return
	}

	problems := 0
	threshold := renewThresholdDays(rc, leaf)
	if daysLeft < threshold-2 {
		// certbot renews once fewer than threshold days remain and its timer
		// runs twice a day; two days past that without a renewal is a failure.
		d.warn(name, fmt.Sprintf("overdue — certbot (%s) should have renewed this at %d days left but has not; see: journalctl -u certbot.service · certbot renew --dry-run",
			rc.authenticator, threshold))
		problems++
	}
	portNote := ""
	if rc.authenticator == "standalone" {
		port := rc.http01Port
		if busy, known := env.portBusy(port); known && busy {
			if hook := knownPreHook(env.leDir, rc); hook != "" {
				// The usual "stop nginx, renew, start nginx" setup: certbot
				// frees the port itself, so a busy port now is expected.
				portNote = fmt.Sprintf(" (port %d is in use now; %s is expected to free it at renewal — confirm with: certbot renew --dry-run)", port, hook)
			} else {
				d.warn(name, fmt.Sprintf("standalone HTTP-01 needs port %d at each renewal, but another program is listening on it now and no certbot pre-hook frees it — the next renewal will likely fail (who: ss -ltnp 'sport = :%d'; test: certbot renew --dry-run); free it, or re-issue the certificate with DNS-01", port, port))
				problems++
			}
		} else if known {
			portNote = fmt.Sprintf(" (HTTP-01: port %d is free now; it must also be reachable from the internet at renewal time)", port)
		}
	}
	if sched, known := env.scheduler(); known && sched == "" {
		d.warn(name, "no certbot renewal timer or cron job found (certbot.timer / snap.certbot.renew.timer / a crontab entry) — nothing will run the renewal; enable it: systemctl enable --now certbot.timer")
		problems++
	}
	if problems == 0 {
		d.ok(name, fmt.Sprintf("automatic — certbot %s, renews when %d days are left%s", rc.authenticator, threshold, portNote))
	}
}

// portListening reports whether any socket LISTENs on the TCP port, from the
// kernel's own tables — read-only and needs no root (unlike binding the port).
// certbot's standalone server binds the wildcard address, which fails if ANY
// address already listens on that port, so any listener counts.
func portListening(port int) (busy, known bool) {
	return listeningOnPort(port, "/proc/net/tcp", "/proc/net/tcp6")
}

func listeningOnPort(port int, tables ...string) (busy, known bool) {
	want := fmt.Sprintf(":%04X", port)
	for _, t := range tables {
		f, err := os.Open(t)
		if err != nil {
			continue
		}
		known = true
		sc := bufio.NewScanner(f)
		sc.Scan() // header
		for sc.Scan() {
			fl := strings.Fields(sc.Text())
			// sl local_address rem_address st ... ; st 0A = TCP_LISTEN
			if len(fl) > 3 && fl[3] == "0A" && strings.HasSuffix(strings.ToUpper(fl[1]), want) {
				f.Close()
				return true, true
			}
		}
		f.Close()
	}
	return false, known
}

// certbotScheduler names what runs certbot's renewal: an active systemd timer,
// or a cron job (a pip/venv certbot is usually scheduled from a crontab). Only
// meaningful under systemd (the installer requires it); elsewhere it reports
// unknown so the check stays quiet rather than guessing.
func certbotScheduler() (string, bool) {
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return "", false
	}
	sc, err := exec.LookPath("systemctl")
	if err != nil {
		return "", false
	}
	for _, t := range []string{"certbot.timer", "snap.certbot.renew.timer"} {
		if exec.Command(sc, "is-active", "--quiet", t).Run() == nil {
			return t, true
		}
	}
	cronUp := false
	for _, d := range []string{"cron", "crond", "cronie"} {
		if exec.Command(sc, "is-active", "--quiet", d).Run() == nil {
			cronUp = true
			break
		}
	}
	if !cronUp {
		return "", true // a crontab entry with no cron daemon runs nothing
	}
	crons := []string{"/etc/crontab", "/var/spool/cron/crontabs/root", "/var/spool/cron/root"}
	if ents, err := os.ReadDir("/etc/cron.d"); err == nil {
		for _, e := range ents {
			if cronDName(e.Name()) {
				crons = append(crons, filepath.Join("/etc/cron.d", e.Name()))
			}
		}
	}
	if f := cronRunsCertbot(crons...); f != "" {
		return "cron (" + f + ")", true
	}
	return "", true
}

// cronDName: the file names Debian's cron reads from /etc/cron.d (letters,
// digits, '_' and '-' only — "x.dpkg-old" or "x~" are ignored by cron).
func cronDName(n string) bool {
	if n == "" {
		return false
	}
	for _, r := range n {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// cronRunsCertbot returns the first crontab file with an active line that runs
// certbot's renewal, or "". A line counts only when a command word is certbot
// (any path) and a later word is exactly "renew", not as a --dry-run; so a
// MAILTO=certbot-renew@… variable, a check-certbot-renewal script or
// "certbot certificates" do not. Debian's packaged /etc/cron.d/certbot entry
// does not count either: it tests for /run/systemd/system and skips itself
// under systemd (the timer is its replacement there).
func cronRunsCertbot(files ...string) string {
	for _, p := range files {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if cronLineRunsCertbotRenew(sc.Text()) {
				f.Close()
				return p
			}
		}
		f.Close()
	}
	return ""
}

func cronLineRunsCertbotRenew(line string) bool {
	l := strings.TrimSpace(line)
	if l == "" || l[0] == '#' || strings.Contains(l, "/run/systemd/system") {
		return false
	}
	if i := strings.Index(l, " #"); i >= 0 { // a shell comment ends the command
		l = l[:i]
	}
	words := strings.FieldsFunc(l, func(r rune) bool {
		return r == ' ' || r == '\t' || strings.ContainsRune(";&|()`'\"", r)
	})
	certbotAt := -1
	for i, w := range words {
		if w == "--dry-run" {
			return false
		}
		if certbotAt < 0 && (w == "certbot" || strings.HasSuffix(w, "/certbot")) {
			certbotAt = i
		}
	}
	if certbotAt < 0 {
		return false
	}
	for _, w := range words[certbotAt+1:] {
		if w == "renew" {
			return true
		}
	}
	return false
}
