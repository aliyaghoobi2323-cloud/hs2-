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
	autorenew       string
	http01Port      int // standalone's listen port; 80 when absent
	renewBeforeDays int // 30 when absent or unparseable (certbot's default)
}

// certbotLineage returns the lineage name when certFile lives in
// <leDir>/live/<name>/ (how certbot and the installer lay it out), else "".
func certbotLineage(leDir, certFile string) string {
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
	rc := renewalConf{renewBeforeDays: 30, http01Port: 80}
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
	if daysLeft < rc.renewBeforeDays-2 {
		// certbot renews once fewer than renewBeforeDays remain and its timer
		// runs twice a day; two days past that without a renewal is a failure.
		d.warn(name, fmt.Sprintf("overdue — certbot (%s) should have renewed this at %d days left but has not; see: journalctl -u certbot.service · certbot renew --dry-run",
			rc.authenticator, rc.renewBeforeDays))
		problems++
	}
	portNote := ""
	if rc.authenticator == "standalone" {
		port := rc.http01Port
		if busy, known := env.portBusy(port); known && busy {
			if rc.preHook != "" {
				// The usual "stop nginx, renew, start nginx" setup: certbot
				// frees the port itself, so a busy port now is expected.
				portNote = fmt.Sprintf(" (port %d is in use now; your pre_hook %q is expected to free it at renewal — confirm with: certbot renew --dry-run)", port, rc.preHook)
			} else {
				d.warn(name, fmt.Sprintf("standalone HTTP-01 needs port %d at each renewal, but another program is listening on it now — the next renewal will fail (who: ss -ltnp 'sport = :%d'); free it, or re-issue the certificate with DNS-01", port, port))
				problems++
			}
		} else if known {
			portNote = fmt.Sprintf(" (HTTP-01: port %d is free now; it must also be reachable from the internet at renewal time)", port)
		}
	}
	if sched, known := env.scheduler(); known && sched == "" {
		d.warn(name, "no certbot renewal timer is active (certbot.timer / snap.certbot.renew.timer) — nothing will run the renewal; enable it: systemctl enable --now certbot.timer")
		problems++
	}
	if problems == 0 {
		d.ok(name, fmt.Sprintf("automatic — certbot %s, renews when %d days are left%s", rc.authenticator, rc.renewBeforeDays, portNote))
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

// certbotScheduler names the active certbot renewal timer. Only meaningful
// under systemd (the installer requires it); elsewhere it reports unknown so the
// check stays quiet rather than guessing. Debian's /etc/cron.d/certbot entry is
// deliberately NOT counted: it skips itself whenever systemd is running.
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
	return "", true
}
