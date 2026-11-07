package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"strconv"
	"time"
)

// Checks for stage 3 of lb (per-address cert/SNI) and the ACME responder,
// shared by `hs2 check` (warnings) and `hs2 doctor` (report lines).

// lbCertIssue is one finding about a per-address certificate.
type lbCertIssue struct {
	ip    string
	fatal bool
	msg   string
}

// lbCertIssues checks every per-address cert of this (listening) side: it
// loads, is not expired or close to it (< 14 days), and covers the address's
// sni. An sni without its own cert must be covered by the top-level cert.
func lbCertIssues(fc fileConfig, now time.Time) (issues []lbCertIssue, checked int) {
	if !lbOn(fc) || dialing(fc) || !certCarrier[fc.Carrier] {
		return nil, 0
	}
	var top *x509.Certificate
	if fc.CertFile != "" {
		if c, err := tls.LoadX509KeyPair(fc.CertFile, fc.KeyFile); err == nil && len(c.Certificate) > 0 {
			top, _ = x509.ParseCertificate(c.Certificate[0])
		}
	}
	for _, e := range lbLocalList(fc) {
		ip := e.IP
		if (e.CertFile == "") != (e.KeyFile == "") {
			issues = append(issues, lbCertIssue{ip, true, `"cert_file" and "key_file" must be set together`})
			continue
		}
		leaf := top
		what := "the top-level certificate"
		if e.CertFile != "" {
			checked++
			c, err := tls.LoadX509KeyPair(e.CertFile, e.KeyFile)
			if err != nil || len(c.Certificate) == 0 {
				issues = append(issues, lbCertIssue{ip, true, fmt.Sprintf("cannot load cert/key (%s): %v", e.CertFile, err)})
				continue
			}
			if leaf, err = x509.ParseCertificate(c.Certificate[0]); err != nil {
				issues = append(issues, lbCertIssue{ip, true, "cannot parse the certificate: " + err.Error()})
				continue
			}
			what = e.CertFile
			switch left := leaf.NotAfter.Sub(now); {
			case left <= 0:
				issues = append(issues, lbCertIssue{ip, false, fmt.Sprintf("certificate %s EXPIRED on %s — renew it (hs2 reloads it within a minute)", e.CertFile, leaf.NotAfter.Format("2006-01-02"))})
			case left < 14*24*time.Hour:
				issues = append(issues, lbCertIssue{ip, false, fmt.Sprintf("certificate %s expires in %d days (%s)", e.CertFile, int(left.Hours()/24), leaf.NotAfter.Format("2006-01-02"))})
			}
		}
		if e.SNI != "" && leaf != nil {
			if err := leaf.VerifyHostname(e.SNI); err != nil {
				issues = append(issues, lbCertIssue{ip, false, fmt.Sprintf(`"sni" %s is not covered by %s (names: %v) — a mismatch a DPI box can see; get a certificate for %s`, e.SNI, what, leaf.DNSNames, e.SNI)})
			}
		}
	}
	return issues, checked
}

// acmeIssues: advice about the ACME responder. holds80: the user ports hs2
// opens on every address include an ACME port, so certbot --standalone cannot
// bind it.
func acmeIssues(fc fileConfig) (bad, warn []string) {
	root := acmeWebrootOf(fc)
	ports, err := acmePortsOf(fc)
	if err != nil {
		if root != "" {
			bad = append(bad, err.Error())
		}
		return
	}
	edge := fc.Mode == "dial"
	userPorts := map[int]bool{}
	for _, p := range splitComma(fc.ForwardPorts) {
		if n, err := strconv.Atoi(p); err == nil {
			userPorts[n] = true
		}
	}
	allAddrs := fc.UserListenIP == "" || net.ParseIP(fc.UserListenIP).IsUnspecified()
	switch {
	case root == "" && edge && userPorts[80] && allAddrs:
		warn = append(warn, `user port 80 is held by hs2 on every address, so certbot --standalone cannot get or renew a certificate on this server — set "acme_webroot": "on" (hs2 then answers the ACME challenge itself) and use certbot --webroot -w `+acmeDefaultWebroot)
	case root != "" && !edge:
		warn = append(warn, `"acme_webroot" has no effect on this side: it answers challenges on the Iran server's user ports`)
	case root != "":
		hit := false
		for p := range ports {
			if userPorts[p] {
				hit = true
			}
		}
		if !hit {
			warn = append(warn, `"acme_webroot" is on but none of its ports (acme_ports, default 80) is in "forward_ports": nothing answers the challenge`)
		}
	}
	return
}

// checkLBCerts is doctor's per-address certificate report.
func checkLBCerts(d *doctorReport, fc fileConfig) {
	issues, checked := lbCertIssues(fc, time.Now())
	for _, is := range issues {
		if is.fatal {
			d.fail("lb cert "+is.ip, is.msg)
		} else {
			d.warn("lb cert "+is.ip, is.msg)
		}
	}
	if len(issues) == 0 && checked > 0 {
		d.ok("lb certs", fmt.Sprintf("%d per-address certificate(s) load, are valid for 14+ days and match their sni", checked))
	}
}

// checkACME is doctor's report on the ACME responder.
func checkACME(d *doctorReport, fc fileConfig) {
	bad, warn := acmeIssues(fc)
	for _, m := range bad {
		d.fail("acme", m)
	}
	for _, m := range warn {
		d.warn("acme", m)
	}
	if len(bad)+len(warn) == 0 && acmeWebrootOf(fc) != "" {
		d.ok("acme", "HTTP-01 challenges are answered on the user ports from "+acmeWebrootOf(fc)+" (certbot --webroot)")
	}
}
