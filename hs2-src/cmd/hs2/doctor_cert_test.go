package main

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCertbotLineage(t *testing.T) {
	le := "/etc/letsencrypt"
	cases := map[string]string{
		"/etc/letsencrypt/live/vpn.example.com/fullchain.pem":      "vpn.example.com",
		"/etc/letsencrypt/live/vpn.example.com-0001/fullchain.pem": "vpn.example.com-0001",
		"/etc/letsencrypt/live/x/../../../etc/passwd":              "", // traversal is not a lineage
		"/etc/letsencrypt/live/x/../y/fullchain.pem":               "", // ".." is never resolved into another lineage
		"/etc/letsencrypt/live//x/fullchain.pem":                   "x",
		"/etc/letsencrypt/live/x/./fullchain.pem":                  "x",
		"//etc/letsencrypt/live/x/fullchain.pem":                   "x",
		"/etc/letsencrypt/live/fullchain.pem":                      "",
		"/etc/letsencrypt/live/a/b/fullchain.pem":                  "",
		"/etc/letsencrypt/archive/a/fullchain1.pem":                "",
		"/etc/letsencryptX/live/a/fullchain.pem":                   "",
		"/root/certs/fullchain.pem":                                "",
		"":                                                         "",
	}
	for in, want := range cases {
		if got := certbotLineage(le, in); got != want {
			t.Errorf("certbotLineage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReadRenewalConf(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.conf")
	os.WriteFile(p, []byte(`renew_before_expiry = 2 weeks
# renew_before_expiry = 99 days
version = 2.9.0
archive_dir = /etc/letsencrypt/archive/x

[renewalparams]
authenticator = manual
manual_auth_hook = /usr/local/bin/dns-hook.sh
renew_hook = pkill -HUP -x hs2
autorenew = True
`), 0o600)
	rc, err := readRenewalConf(p)
	if err != nil {
		t.Fatal(err)
	}
	if rc.authenticator != "manual" || rc.manualAuthHook != "/usr/local/bin/dns-hook.sh" || rc.autorenew != "True" || rc.renewBeforeDays != 14 || rc.http01Port != 80 {
		t.Fatalf("parsed %+v", rc)
	}
	for in, want := range map[string]int{"30 days": 30, "1 day": 1, "3 Weeks": 21, "30": 0, "x days": 0, "-5 days": 0, "10 hours": 0} {
		if got := parseRenewBefore(in); got != want {
			t.Errorf("parseRenewBefore(%q) = %d, want %d", in, got, want)
		}
	}
	if _, err := readRenewalConf(filepath.Join(t.TempDir(), "missing.conf")); err == nil {
		t.Error("a missing renewal conf must be an error")
	}
}

// One fake certbot tree + a leaf with a chosen expiry per scenario; asserts the
// verdict (ok/warn/info counts) and a phrase the operator must see.
func TestCheckCertRenewal(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	const lineage = "vpn.example.com"
	type sc struct {
		name      string
		certFile  string // "" = the certbot lineage path
		conf      string // "" = no renewal conf written
		daysLeft  int
		port80    bool
		sched     string
		schedKnow bool
		wantWarns int
		wantOK    int
		wantInfo  int
		wantText  string
	}
	standalone := "[renewalparams]\nauthenticator = standalone\n"
	cases := []sc{
		{"DNS-01 manual never auto-renews", "", "[renewalparams]\nauthenticator = manual\n", 80, false, "certbot.timer", true, 1, 0, 0, "does NOT renew automatically"},
		{"manual WITH auth hook is automatic", "", "[renewalparams]\nauthenticator = manual\nmanual_auth_hook = /x.sh\n", 80, false, "certbot.timer", true, 0, 1, 0, "automatic"},
		{"standalone, port 80 free", "", standalone, 80, false, "certbot.timer", true, 0, 1, 0, "port 80 is free now"},
		{"standalone, port 80 taken", "", standalone, 80, true, "certbot.timer", true, 1, 0, 0, "no certbot pre-hook frees it"},
		{"standalone, port 80 taken but a pre_hook frees it", "", standalone + "pre_hook = systemctl stop nginx\n", 80, true, "certbot.timer", true, 0, 1, 0, "expected to free it"},
		{"standalone on a custom http01_port is checked there", "", standalone + "http01_port = 8888\n", 80, false, "certbot.timer", true, 0, 1, 0, "port 8888 is free now"},
		{"overdue renewal", "", standalone, 20, false, "certbot.timer", true, 1, 0, 0, "overdue"},
		{"inside the 2-day grace is not overdue", "", standalone, 29, false, "certbot.timer", true, 0, 1, 0, "automatic"},
		{"custom renew_before_expiry respected", "", "renew_before_expiry = 10 days\n" + standalone, 20, false, "certbot.timer", true, 0, 1, 0, "10 days are left"},
		{"overdue AND port 80 taken: both reported", "", standalone, 5, true, "certbot.timer", true, 2, 0, 0, "overdue"},
		{"expired + manual says why", "", "[renewalparams]\nauthenticator = manual\n", -3, false, "certbot.timer", true, 1, 0, 0, "by hand"},
		{"no renewal timer active", "", standalone, 80, false, "", true, 1, 0, 0, "no certbot renewal timer or cron job"},
		{"timer state unknown stays quiet", "", standalone, 80, false, "", false, 0, 1, 0, "automatic"},
		{"autorenew disabled", "", "[renewalparams]\nauthenticator = standalone\nautorenew = False\n", 80, false, "certbot.timer", true, 1, 0, 0, "switched off"},
		{"certbot lineage without a renewal conf", "", "", 80, false, "certbot.timer", true, 1, 0, 0, "will NOT be renewed"},
		{"own certificate, far from expiry", "/root/own/fullchain.pem", "", 200, false, "", true, 0, 0, 1, "you renew it yourself"},
		{"own certificate, close to expiry", "/root/own/fullchain.pem", "", 12, false, "", true, 1, 0, 0, "renew before"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			le := t.TempDir()
			cf := c.certFile
			if cf == "" {
				cf = filepath.Join(le, "live", lineage, "fullchain.pem")
			}
			if c.conf != "" {
				os.MkdirAll(filepath.Join(le, "renewal"), 0o700)
				os.WriteFile(filepath.Join(le, "renewal", lineage+".conf"), []byte(c.conf), 0o600)
			}
			na := now.Add(time.Duration(c.daysLeft)*24*time.Hour + time.Hour)
			leaf := &x509.Certificate{NotBefore: na.Add(-90 * 24 * time.Hour), NotAfter: na} // a 90-day Let's Encrypt cert
			env := renewEnv{
				leDir: le,
				now:   now,
				portBusy: func(port int) (bool, bool) {
					if strings.Contains(c.conf, "http01_port = 8888") && port != 8888 {
						t.Errorf("checked port %d, want the configured 8888", port)
					}
					return c.port80, true
				},
				scheduler: func() (string, bool) { return c.sched, c.schedKnow },
			}
			d := &doctorReport{}
			checkCertRenewal(d, fileConfig{CertFile: cf}, leaf, env)
			all := strings.Join(d.lines, " | ")
			oks := strings.Count(all, "[ ok ]")
			infos := strings.Count(all, "[info]")
			if d.warns != c.wantWarns || oks != c.wantOK || infos != c.wantInfo || d.fails != 0 {
				t.Fatalf("warns=%d ok=%d info=%d fails=%d, want warns=%d ok=%d info=%d:\n%s",
					d.warns, oks, infos, d.fails, c.wantWarns, c.wantOK, c.wantInfo, all)
			}
			if !strings.Contains(all, c.wantText) {
				t.Fatalf("missing %q in:\n%s", c.wantText, all)
			}
		})
	}
}

// The renewal check runs from doctor only where checkCert ran: the TLS-server
// side with a parseable certificate. checkCert hands back the leaf even when it
// is EXPIRED, so the renewal check can explain why.
func TestCheckCertReturnsLeafForRenewal(t *testing.T) {
	cert, key := writeCert(t, time.Now().Add(-time.Hour))
	d := &doctorReport{}
	if leaf := checkCert(d, fileConfig{Mode: "listen", Carrier: "tls", CertFile: cert, KeyFile: key}); leaf == nil {
		t.Fatal("an expired but parseable cert must still be returned for the renewal diagnosis")
	}
	if leaf := checkCert(&doctorReport{}, fileConfig{Mode: "dial", Carrier: "tls", CertFile: cert, KeyFile: key}); leaf != nil {
		t.Fatal("the dialer side never uses the cert: no leaf, so no renewal check")
	}
	if leaf := checkCert(&doctorReport{}, fileConfig{Mode: "listen", Carrier: "tls", CertFile: "/no/such.pem", KeyFile: "/no/such.key"}); leaf != nil {
		t.Fatal("an unreadable cert yields no leaf")
	}
}

func TestListeningOnPort(t *testing.T) {
	dir := t.TempDir()
	hdr := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	v4 := filepath.Join(dir, "tcp")
	v6 := filepath.Join(dir, "tcp6")
	// :0050 ESTABLISHED (01) and :1F90 (8080) LISTEN must NOT count; only a :0050 LISTEN does.
	os.WriteFile(v4, []byte(hdr+
		"   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0000000000000000 100 0 0 10 0\n"+
		"   1: 0100007F:0050 0200007F:A1B2 01 00000000:00000000 00:00000000 00000000     0        0 2 1 0000000000000000 100 0 0 10 0\n"), 0o600)
	os.WriteFile(v6, []byte(hdr), 0o600)
	if busy, known := listeningOnPort(80, v4, v6); busy || !known {
		t.Fatalf("no :80 listener: busy=%v known=%v", busy, known)
	}
	os.WriteFile(v6, []byte(hdr+
		"   0: 00000000000000000000000000000000:0050 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 3 1 0000000000000000 100 0 0 10 0\n"), 0o600)
	if busy, known := listeningOnPort(80, v4, v6); !busy || !known {
		t.Fatalf("[::]:80 LISTEN must count: busy=%v known=%v", busy, known)
	}
	if _, known := listeningOnPort(80, filepath.Join(dir, "nope"), filepath.Join(dir, "nope6")); known {
		t.Fatal("unreadable tables must report unknown, not 'free'")
	}
}

// Pre-hooks free the HTTP-01 port from any of the three places certbot reads
// them, and the default renewal threshold follows the certificate's lifetime
// (certbot's own rule), not a fixed 30 days.
func TestCheckCertRenewalHooksAndLifetime(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	const lineage = "vpn.example.com"
	standalone := "[renewalparams]\nauthenticator = standalone\n"
	cases := []struct {
		name      string
		setup     func(le string)
		lifeDays  int
		daysLeft  int
		busy      bool
		wantWarns int
		wantText  string
	}{
		{"executable hook in renewal-hooks/pre frees the port", func(le string) {
			d := filepath.Join(le, "renewal-hooks", "pre")
			os.MkdirAll(d, 0o700)
			os.WriteFile(filepath.Join(d, "stop-nginx.sh"), []byte("#!/bin/sh\nsystemctl stop nginx\n"), 0o755)
		}, 90, 80, true, 0, "stop-nginx.sh is expected to free it"},
		{"a NON-executable file there is not a hook", func(le string) {
			d := filepath.Join(le, "renewal-hooks", "pre")
			os.MkdirAll(d, 0o700)
			os.WriteFile(filepath.Join(d, "README"), []byte("notes"), 0o644)
		}, 90, 80, true, 1, "no certbot pre-hook frees it"},
		{"cli.ini pre-hook frees the port", func(le string) {
			os.WriteFile(filepath.Join(le, "cli.ini"), []byte("# defaults\npre-hook = systemctl stop nginx\n"), 0o644)
		}, 90, 80, true, 0, "cli.ini pre-hook"},
		{"a commented cli.ini pre-hook does not count", func(le string) {
			os.WriteFile(filepath.Join(le, "cli.ini"), []byte("# pre-hook = systemctl stop nginx\n"), 0o644)
		}, 90, 80, true, 1, "no certbot pre-hook frees it"},
		{"45-day cert, 25 left, no explicit threshold: not overdue (renews at 15)", nil, 45, 25, false, 0, "renews when 15 days are left"},
		{"45-day cert, 10 left: overdue", nil, 45, 10, false, 1, "renewed this at 15 days left"},
		{"6-day cert renews at half its life", nil, 6, 4, false, 0, "renews when 3 days are left"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			le := t.TempDir()
			os.MkdirAll(filepath.Join(le, "renewal"), 0o700)
			os.WriteFile(filepath.Join(le, "renewal", lineage+".conf"), []byte(standalone), 0o600)
			if c.setup != nil {
				c.setup(le)
			}
			na := now.Add(time.Duration(c.daysLeft)*24*time.Hour + time.Hour)
			leaf := &x509.Certificate{NotBefore: na.Add(-time.Duration(c.lifeDays) * 24 * time.Hour), NotAfter: na}
			env := renewEnv{leDir: le, now: now,
				portBusy:  func(int) (bool, bool) { return c.busy, true },
				scheduler: func() (string, bool) { return "certbot.timer", true }}
			d := &doctorReport{}
			checkCertRenewal(d, fileConfig{CertFile: filepath.Join(le, "live", lineage, "fullchain.pem")}, leaf, env)
			all := strings.Join(d.lines, " | ")
			if d.warns != c.wantWarns || !strings.Contains(all, c.wantText) {
				t.Fatalf("warns=%d (want %d), want %q in:\n%s", d.warns, c.wantWarns, c.wantText, all)
			}
		})
	}
}

func TestRenewThresholdDays(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	leaf := func(days int) *x509.Certificate {
		return &x509.Certificate{NotBefore: at, NotAfter: at.Add(time.Duration(days)*24*time.Hour - time.Second)}
	}
	for _, c := range []struct {
		rc   renewalConf
		life int
		want int
	}{
		{renewalConf{}, 90, 30}, {renewalConf{}, 45, 15}, {renewalConf{}, 6, 3},
		{renewalConf{renewBeforeDays: 21}, 90, 21}, {renewalConf{}, 0, 30},
	} {
		if got := renewThresholdDays(c.rc, leaf(c.life)); got != c.want {
			t.Errorf("life %d days, conf %+v: threshold %d, want %d", c.life, c.rc, got, c.want)
		}
	}
}

// A pip/venv certbot is scheduled from a crontab, not a timer; Debian's
// packaged cron entry skips itself under systemd and must not count.
func TestCronRunsCertbot(t *testing.T) {
	dir := t.TempDir()
	w := func(name, body string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o644)
		return p
	}
	deb := w("certbot-deb", "0 */12 * * * root test -x /usr/bin/certbot -a \\! -d /run/systemd/system && perl -e 'sleep int(rand(43200))' && certbot -q renew\n")
	pip := w("crontab-pip", "SHELL=/bin/sh\n0 0,12 * * * root /opt/certbot/bin/python -c 'import random; import time; time.sleep(random.random() * 3600)' && certbot renew -q\n")
	off := w("crontab-off", "# 0 3 * * * root certbot renew\n")
	if got := cronRunsCertbot(deb, off); got != "" {
		t.Fatalf("Debian's self-skipping entry / a comment counted as a scheduler: %q", got)
	}
	if got := cronRunsCertbot(deb, pip); got != pip {
		t.Fatalf("pip-style crontab not found: %q", got)
	}
}
