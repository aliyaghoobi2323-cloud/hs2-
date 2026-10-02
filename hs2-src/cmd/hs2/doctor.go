package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// doctorCmd implements `hs2 doctor -c config.json`: a read-only, on-box health
// check an operator runs when a tunnel misbehaves. It aggregates what `hs2
// check` (config), `hs2 status` (live pattern) and the installer's peer-ping
// already show, and adds the environment checks none of them cover — endpoint
// reachability, certificate expiry, the TUN device's real state, and whether
// the kernel tuning actually landed. It changes NOTHING: every check only reads
// (files, /proc, interfaces, one TCP connect to our own endpoint), needs no
// root, and touches no wire/data path. Exit status mirrors `hs2 check`: 1 only
// when a hard problem (FAIL) is found; WARN/INFO are advisory and exit 0.
func doctorCmd(args []string) {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	cfgPath := fs.String("c", "", "config file (JSON)")
	fs.Parse(args)
	if *cfgPath == "" {
		fmt.Println("usage: hs2 doctor -c config.json")
		os.Exit(2)
	}

	d := &doctorReport{}
	d.info("binary", versionLine())

	raw, err := os.ReadFile(*cfgPath)
	if err != nil {
		d.fail("config", fmt.Sprintf("cannot read %s: %v", *cfgPath, err))
	} else {
		errs, warns := checkConfig(raw, isLocalIP, time.Now())
		switch {
		case len(errs) > 0:
			d.fail("config", fmt.Sprintf("%d error(s): %s", len(errs), strings.Join(errs, " · ")))
		case len(warns) > 0:
			d.warn("config", fmt.Sprintf("valid, %d warning(s): %s", len(warns), strings.Join(warns, " · ")))
		default:
			d.ok("config", "valid")
		}
		var fc fileConfig
		if json.Unmarshal(raw, &fc) == nil {
			checkRunning(d, *cfgPath)
			checkEndpoint(d, fc)
			checkCert(d, fc)
			checkTun(d, fc)
			checkTuning(d, fc)
		} else {
			d.info("live checks", "skipped — the config does not parse as JSON (see the config error above)")
		}
	}
	// Link auth binds an HMAC to the wall-clock MINUTE, so the two servers'
	// clocks must agree to within ~1 minute or no link authenticates. We cannot
	// see the peer's clock from here; show ours so the operator can compare.
	d.info("clock", fmt.Sprintf("node time %s — both servers must agree within ~1 min (link auth is minute-bound)",
		time.Now().UTC().Format(time.RFC3339)))

	fmt.Printf("hs2 doctor — %s\n", *cfgPath)
	d.print()
	if d.fails > 0 {
		os.Exit(1)
	}
}

// doctorReport collects one line per check and tallies the hard failures and
// warnings so the summary and exit status are consistent.
type doctorReport struct {
	lines        []string
	fails, warns int
}

func (d *doctorReport) add(tag, name, detail string) {
	d.lines = append(d.lines, fmt.Sprintf("  [%s] %s: %s", tag, name, detail))
}
func (d *doctorReport) ok(n, s string)   { d.add(" ok ", n, s) }
func (d *doctorReport) warn(n, s string) { d.warns++; d.add("warn", n, s) }
func (d *doctorReport) fail(n, s string) { d.fails++; d.add("fail", n, s) }
func (d *doctorReport) info(n, s string) { d.add("info", n, s) }

func (d *doctorReport) print() {
	for _, l := range d.lines {
		fmt.Println(l)
	}
	switch {
	case d.fails > 0:
		fmt.Printf("\n%d problem(s), %d warning(s) — the tunnel is likely NOT healthy.\n", d.fails, d.warns)
	case d.warns > 0:
		fmt.Printf("\n0 problems, %d warning(s) — worth a look, not necessarily broken.\n", d.warns)
	default:
		fmt.Println("\nall checks passed.")
	}
}

// checkRunning reads the live status file the daemon publishes (the same file
// `hs2 status` reads). Absent = not running (or just started); stale = the
// daemon stopped updating it.
func checkRunning(d *doctorReport, cfgPath string) {
	b, err := os.ReadFile(statusPath(cfgPath))
	if err != nil {
		d.info("running", "no live status — tunnel not running, or started in the last few seconds (confirm with: systemctl status hs2…)")
		return
	}
	var ls liveStatus
	if json.Unmarshal(b, &ls) != nil {
		d.warn("running", "the live status file is unreadable")
		return
	}
	summary := fmt.Sprintf("%s · %s · %d link(s) up · %.1f Mbit/s", ls.Role, ls.Carrier, ls.Links, ls.Mbit)
	if age := time.Now().Unix() - ls.Updated; age > 6 {
		d.warn("running", fmt.Sprintf("status is stale (%ds old) — the tunnel may be down. last seen: %s", age, summary))
		return
	}
	d.ok("running", summary)
}

// tcpCarrier is the set of carriers whose carrier link is TCP, so a plain TCP
// connect to the endpoint is a meaningful reachability probe. Datagram carriers
// (udp/auto/dgtun) are excluded — a TCP connect would say nothing true.
var tcpCarrier = map[string]bool{"mtcp": true, "l3mtcp": true, "l3": true, "tls": true, "reality": true}

// checkEndpoint, on the side that DIALS a TCP carrier, confirms the configured
// endpoint accepts a TCP connection (and that its host resolves). This is the
// most common real failure — the edge cannot reach the exit (filtered, wrong
// address, exit down). A success means only that TCP got through; the TLS
// carrier handshake is a separate matter.
func checkEndpoint(d *doctorReport, fc fileConfig) {
	if fc.Addr == "" || !tcpCarrier[fc.Carrier] || !dialing(fc) {
		return
	}
	c, err := net.DialTimeout("tcp", fc.Addr, 4*time.Second)
	if err != nil {
		d.fail("endpoint", fmt.Sprintf("cannot TCP-connect to %s: %v (filtered? wrong address? exit down?)", fc.Addr, err))
		return
	}
	_ = c.Close()
	d.ok("endpoint", fmt.Sprintf("TCP connect to %s succeeded", fc.Addr))
}

// checkCert validates the server certificate (only the side that terminates TLS
// configures one). Expired is fatal; within a week is a warning (certbot renews
// and the daemon hot-reloads, so no restart).
func checkCert(d *doctorReport, fc fileConfig) {
	if fc.CertFile == "" {
		return
	}
	cert, err := tls.LoadX509KeyPair(fc.CertFile, fc.KeyFile)
	if err != nil {
		d.fail("certificate", fmt.Sprintf("cannot load cert/key (%s): %v", fc.CertFile, err))
		return
	}
	if len(cert.Certificate) == 0 {
		d.fail("certificate", "no certificate found in "+fc.CertFile)
		return
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		d.fail("certificate", "cannot parse the leaf certificate: "+err.Error())
		return
	}
	until := leaf.NotAfter.UTC().Format("2006-01-02")
	days := int(time.Until(leaf.NotAfter).Hours() / 24)
	switch {
	case time.Now().After(leaf.NotAfter):
		d.fail("certificate", "EXPIRED on "+until)
	case days <= 7:
		d.warn("certificate", fmt.Sprintf("valid for %d more day(s) (until %s) — certbot should renew it; it hot-reloads, no restart", days, until))
	default:
		d.ok("certificate", fmt.Sprintf("valid for %d more day(s) (until %s)", days, until))
	}
}

// checkTun reports the real state of the tunnel's TUN device (tun carriers
// only — mtcp has no iface, so fc.Iface is empty and this is skipped). The
// device only exists while the daemon runs, so absence is a warning, not a
// failure: it is expected when the tunnel is stopped.
func checkTun(d *doctorReport, fc fileConfig) {
	if fc.Iface == "" {
		return
	}
	name := "tun " + fc.Iface
	ifi, err := net.InterfaceByName(fc.Iface)
	if err != nil {
		d.warn(name, "not present — it only exists while the tunnel runs")
		return
	}
	var got []string
	if addrs, err := ifi.Addrs(); err == nil {
		for _, a := range addrs {
			got = append(got, a.String())
		}
	}
	addrText := "no address"
	if len(got) > 0 {
		addrText = "addr " + strings.Join(got, ", ")
	}
	if ifi.Flags&net.FlagUp == 0 {
		d.warn(name, "present but DOWN ("+addrText+")")
		return
	}
	// If the config pins a local CIDR, confirm the device actually carries it.
	if fc.LocalCIDR != "" && len(got) > 0 && !addrsContainCIDR(got, fc.LocalCIDR) {
		d.warn(name, fmt.Sprintf("up, but %s — config expects %s", addrText, fc.LocalCIDR))
		return
	}
	d.ok(name, "up, "+addrText)
}

// addrsContainCIDR reports whether the configured local_cidr appears among the
// interface's addresses. It compares the IP, so "10.77.0.1/30" matches an
// interface address of "10.77.0.1/30" regardless of prefix formatting.
func addrsContainCIDR(got []string, cidr string) bool {
	wantIP, _, err := net.ParseCIDR(cidr)
	if err != nil {
		return true // cannot parse the config value; do not raise a false alarm
	}
	for _, a := range got {
		if ip, _, err := net.ParseCIDR(a); err == nil && ip.Equal(wantIP) {
			return true
		}
		if ip := net.ParseIP(a); ip != nil && ip.Equal(wantIP) {
			return true
		}
	}
	return false
}

// checkTuning compares the kernel-tuning plan this server+config would apply
// against the values actually in /proc/sys. The plan is applied at every
// `hs2 run` as root, so when the tunnel is running they should match except for
// settings a container forbids (skipped silently). A mismatch is a warning, not
// a failure.
func checkTuning(d *doctorReport, fc fileConfig) {
	plan := buildTunePlan(fc)
	if plan.Profile == "off" {
		d.info("kernel tuning", "mode=off — system sysctls intentionally left untouched")
		return
	}
	var mism []string
	checked := 0
	for _, kv := range plan.Sysctls {
		b, err := os.ReadFile("/proc/sys/" + strings.ReplaceAll(kv.Key, ".", "/"))
		if err != nil {
			continue // not readable here (e.g. a restricted container) — not a mismatch
		}
		checked++
		if have, want := normalizeWS(string(b)), normalizeWS(kv.Val); have != want {
			mism = append(mism, fmt.Sprintf("%s want %q have %q", kv.Key, want, have))
		}
	}
	switch {
	case checked == 0:
		d.info("kernel tuning", "cannot read /proc/sys here (restricted environment)")
	case len(mism) == 0:
		d.ok("kernel tuning", fmt.Sprintf("all %d setting(s) match the plan (profile %s, cc %s)", checked, plan.Profile, plan.Congestion))
	default:
		shown := mism
		if len(shown) > 3 {
			shown = shown[:3]
		}
		extra := ""
		if len(mism) > len(shown) {
			extra = fmt.Sprintf(" (+%d more)", len(mism)-len(shown))
		}
		d.warn("kernel tuning", fmt.Sprintf("%d of %d setting(s) differ — applied at each 'hs2 run' as root: %s%s",
			len(mism), checked, strings.Join(shown, " · "), extra))
	}
}

// normalizeWS collapses any run of whitespace to a single space and trims the
// ends, so a multi-field sysctl like net.ipv4.tcp_rmem ("4096\t131072\t...")
// compares equal to the plan's space-separated value.
func normalizeWS(s string) string { return strings.Join(strings.Fields(s), " ") }
