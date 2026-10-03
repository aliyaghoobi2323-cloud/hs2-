package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tune"
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
			if leaf := checkCert(d, fc); leaf != nil {
				checkCertRenewal(d, fc, leaf, hostRenewEnv())
			}
			checkTun(d, fc)
			checkTuning(d, fc)
			checkLinkPool(d, fc, *cfgPath)
			checkManyLinks(d, fc, *cfgPath)
			checkPorts(d, fc, *cfgPath)
			checkTCPMem(d, "/proc")
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
	summary := fmt.Sprintf("%s · %s · %d link(s) up · %s", ls.Role, ls.Carrier, ls.Links, trafficLine(ls))
	if age := time.Now().Unix() - ls.Updated; age > 6 {
		d.warn("running", fmt.Sprintf("status is stale (%ds old) — the tunnel may be down. last seen: %s", age, summary))
		return
	}
	d.ok("running", summary)
	if ls.Refill != "" {
		d.info("refill", ls.Refill)
	}
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
	// 8 s (not a tight 4 s): an Iran↔foreign path is high-latency and often
	// lossy, and a dropped SYN is retransmitted at ~1 s then ~3 s, so a healthy
	// endpoint still completes within 8 s even through a couple of SYN losses —
	// while a filtered or down exit (the failure that matters most here) still
	// fails loudly. The timeout also covers DNS resolution.
	c, err := net.DialTimeout("tcp", fc.Addr, 8*time.Second)
	if err != nil {
		d.fail("endpoint", fmt.Sprintf("cannot TCP-connect to %s: %v (filtered? wrong address? exit down?)", fc.Addr, err))
		return
	}
	_ = c.Close()
	d.ok("endpoint", fmt.Sprintf("TCP connect to %s succeeded", fc.Addr))
}

// certCarrier is the set of carriers that terminate TLS with a real
// certificate. Datagram carriers (udp/auto/dgtun) and noise use their own
// crypto and no cert_file.
var certCarrier = map[string]bool{"mtcp": true, "l3mtcp": true, "l3": true, "tls": true, "reality": true}

// checkCert validates the server certificate. The cert is used ONLY by the side
// that terminates TLS — the side that LISTENS for a TLS carrier (direct: the
// exit; reverse: the edge). On the dialing side, or a non-TLS carrier, a stray
// cert_file is never used by the tunnel, so doctor does not fail on it (that
// would be stricter than the daemon, which only loads the cert on the server
// side). Expired is fatal; within a week is a warning (a renewed cert
// hot-reloads, so no restart). It returns the parsed leaf — even an expired one
// — so checkCertRenewal can say WHY it is not being renewed; nil when there is
// no usable certificate or the check does not apply to this side.
func checkCert(d *doctorReport, fc fileConfig) *x509.Certificate {
	if fc.CertFile == "" || dialing(fc) || !certCarrier[fc.Carrier] {
		return nil
	}
	cert, err := tls.LoadX509KeyPair(fc.CertFile, fc.KeyFile)
	if err != nil {
		d.fail("certificate", fmt.Sprintf("cannot load cert/key (%s): %v", fc.CertFile, err))
		return nil
	}
	if len(cert.Certificate) == 0 {
		d.fail("certificate", "no certificate found in "+fc.CertFile)
		return nil
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		d.fail("certificate", "cannot parse the leaf certificate: "+err.Error())
		return nil
	}
	until := leaf.NotAfter.UTC().Format("2006-01-02")
	days := int(time.Until(leaf.NotAfter).Hours() / 24)
	switch {
	case time.Now().After(leaf.NotAfter):
		d.fail("certificate", "EXPIRED on "+until)
	case days <= 7:
		d.warn("certificate", fmt.Sprintf("valid for %d more day(s) (until %s) — renew it now (see 'cert renewal'); a renewed cert hot-reloads, no restart", days, until))
	default:
		d.ok("certificate", fmt.Sprintf("valid for %d more day(s) (until %s)", days, until))
	}
	return leaf
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
	plan := doctorTunePlan(fc)
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

// checkLinkPool checks this server's link-pool ceiling against its hardware
// NOW, and shows the tunnel's effective ceiling when the daemon has learned the
// other server's:
//
//   - auto (max_links 0): the ceiling follows the hardware at every start. If
//     the hardware changed under a RUNNING daemon (RAM resized live), the
//     running ceiling is out of date until a restart — WARN when it is now too
//     high for the RAM, INFO when more is available;
//   - fixed (max_links > 0), or the historical default 32 (max_links not
//     set): WARN when above what the RAM comfortably holds (it was set for
//     bigger hardware, or by hand), INFO when below what the box could use —
//     both suggest auto;
//   - a direct Kharej's own ceiling is not applied (the Iran server's is), so
//     it gets no drift check, only the fact.
func checkLinkPool(d *doctorReport, fc fileConfig, cfgPath string) {
	const name = "link pool ceiling"
	if !hasLinkPool(fc) {
		d.info(name, fmt.Sprintf("no adaptive link pool on carrier %q (tls is one link; other carriers run one session)", carrierName(fc)))
		return
	}
	_, cfgMax, _ := linkEnvelope(fc)   // what a start now runs with (lifted to min_links)
	rawMax, mode, _ := linkCeiling(fc) // the ceiling itself
	ram, cpus := detectHW()
	recMax := tune.RecommendedMaxLinks(ram, cpus)
	hw := "server (" + tune.MaxLinksReason(ram, cpus) + ")"
	iran := fc.Mode == "dial"

	// The live status (if the daemon runs) knows what it started with and the
	// other server's ceiling; it gives the effective ceiling exactly.
	eff, running := "", 0
	if b, err := os.ReadFile(statusPath(cfgPath)); err == nil {
		var ls liveStatus
		if json.Unmarshal(b, &ls) == nil && time.Now().Unix()-ls.Updated <= 6 {
			running = ls.CfgMax
			if t := ceilingLine(ls); t != "" {
				eff = " · now: " + t
			}
		}
	}

	if !fc.Reverse && !iran {
		d.info(name, "in direct mode the Iran server's ceiling applies; this server's max_links is not applied here"+eff)
		return
	}
	if mode == ceilAuto {
		// cfgMax is what a start NOW would run with (the hardware's ceiling,
		// lifted to min_links if that is higher); compare it, not the raw
		// recommendation, with what the daemon started with.
		lift := ""
		if cfgMax != rawMax {
			lift = fmt.Sprintf(" (raised to min_links %d)", cfgMax)
		}
		switch {
		case running > 0 && cfgMax < running:
			d.warn(name, fmt.Sprintf("auto: running with %d, but a start now would give %d (this %s; the hardware or the config changed since the daemon started) — restart the tunnel to apply the lower ceiling%s", running, cfgMax, hw, eff))
		case running > 0 && cfgMax > running:
			d.info(name, fmt.Sprintf("auto: running with %d; a start now would give %d (this %s; the hardware or the config changed since the daemon started) — restart the tunnel to use it%s", running, cfgMax, hw, eff))
		default:
			if n := dgCapNote(fc); n != "" {
				hw += ", lowered — " + n
			}
			d.ok(name, fmt.Sprintf("auto: %d links from this %s%s — re-derived at every start%s", rawMax, hw, lift, eff))
		}
		return
	}
	how := "fixed by max_links"
	if mode == ceilDefault {
		how = "the default — max_links is not set"
	}
	if cfgMax != rawMax {
		how += fmt.Sprintf(", raised to min_links %d", cfgMax)
	}
	switch {
	case rawMax == recMax:
		d.ok(name, fmt.Sprintf("%d links (%s) — matches this %s%s", rawMax, how, hw, eff))
	case rawMax > recMax:
		d.warn(name, fmt.Sprintf("max_links %d is above what this %s suggests (%d) — more links than its RAM comfortably holds under load; set max_links to 0 (auto) or %d (menu → Link pool)%s", rawMax, hw, recMax, recMax, eff))
	default:
		d.info(name, fmt.Sprintf("%d links (%s); this %s could use up to %d — set max_links to 0 (auto) to follow the hardware (menu → Link pool)%s", rawMax, how, hw, recMax, eff))
	}
}

// normalizeWS collapses any run of whitespace to a single space and trims the
// ends, so a multi-field sysctl like net.ipv4.tcp_rmem ("4096\t131072\t...")
// compares equal to the plan's space-separated value.
func normalizeWS(s string) string { return strings.Join(strings.Fields(s), " ") }

// doctorTunePlan builds the kernel-tuning plan WITHOUT the production
// availability probes' side effects. tune.AvailableCC / tune.AvailableQdisc run
// `modprobe` to try loading a missing module — which a read-only diagnostic
// must not do, and which would need root. Here availability is decided by
// reading /proc only (and the always-built-in qdiscs), so doctor never loads a
// module and never needs root. When the tunnel is running, the modules it needs
// are already loaded, so this read-only view agrees with the plan the daemon
// actually applied; when it is not running, a would-be-loadable congestion
// control simply reads as unavailable, which only softens a WARN.
func doctorTunePlan(fc fileConfig) *tune.Plan {
	var cfg tune.Config
	if fc.Tuning != nil {
		cfg = *fc.Tuning
	}
	ram, cpus := tune.Detect()
	availCC := func(name string) bool {
		return name != "" && procListHas("/proc/sys/net/ipv4/tcp_available_congestion_control", name)
	}
	availQ := func(name string) bool {
		switch name {
		case "fq", "fq_codel", "pfifo_fast", "sfq":
			return true // built in on essentially every kernel — no load needed
		}
		return false // do not modprobe to find out; a custom qdisc may only soften a WARN
	}
	return tune.Build(cfg, ram, cpus, availCC, availQ)
}

// procListHas reports whether name appears as a whitespace-separated field in
// the file at path (a read-only mirror of tune's internal inProcList, kept here
// so doctor never triggers a module load).
func procListHas(path, name string) bool {
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

// manyLinksAt: above this many links the doctor states the visibility
// trade-off of a large pool (the ceiling was 64 before the 300-link rule).
const manyLinksAt = 64

// checkManyLinks states, for a pool allowed more than manyLinksAt links, what
// that looks like from outside — an owner's decision, not a fault — and, in
// reverse, when the other server holds the pool below this one's ceiling.
func checkManyLinks(d *doctorReport, fc fileConfig, cfgPath string) {
	if !hasLinkPool(fc) || (!fc.Reverse && fc.Mode != "dial") {
		return // a direct Kharej does not size the pool
	}
	_, ceil, _ := linkEnvelope(fc)
	eff := ceil
	var ls liveStatus
	if b, err := os.ReadFile(statusPath(cfgPath)); err == nil && json.Unmarshal(b, &ls) == nil && time.Now().Unix()-ls.Updated <= 6 {
		if ls.EffMax > 0 {
			eff = ls.EffMax
		}
		if fc.Reverse && ls.PeerMax > 0 && ls.PeerMax < ls.CfgMax {
			other := "Kharej"
			if fc.Mode != "dial" {
				other = "Iran"
			}
			d.info("link pool (other server)", fmt.Sprintf("the %s server allows at most %d links, this one %d — in reverse the lower applies; to use more, upgrade it and set max_links to 0 (auto) there", other, ls.PeerMax, ls.CfgMax))
		}
	}
	if eff <= manyLinksAt {
		return
	}
	d.info("link count visibility", fmt.Sprintf("this tunnel may open up to %d parallel TLS connections between this server and the other one. "+
		"That many between one fixed pair of IPs is more unusual to an outside observer than a handful: the pool opens them only under load "+
		"(about one per %d active connections), at most ~10 new ones a second, and closes them again in quiet hours — but at peak they are all visible at once. "+
		"Lowering max_links trades peak capacity for a smaller pattern; it is the owner's decision", eff, perLinkOf(fc)))
}

func perLinkOf(fc fileConfig) int {
	_, _, per := linkEnvelope(fc)
	return per
}

// checkTCPMem compares the kernel's TCP buffer memory with its tcp_mem
// thresholds: above the pressure mark every socket's buffers are squeezed,
// above the hard mark the kernel drops packets on all of them. At hundreds of
// links plus thousands of user connections, stalled readers fill buffers here
// first.
func checkTCPMem(d *doctorReport, proc string) {
	sock, err := os.ReadFile(proc + "/net/sockstat")
	if err != nil {
		return
	}
	lim, err := os.ReadFile(proc + "/sys/net/ipv4/tcp_mem")
	if err != nil {
		return
	}
	mem := -1
	for _, line := range strings.Split(string(sock), "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && f[0] == "TCP:" {
			for i := 1; i+1 < len(f); i += 2 {
				if f[i] == "mem" {
					mem, _ = strconv.Atoi(f[i+1])
				}
			}
		}
	}
	th := strings.Fields(string(lim))
	if mem < 0 || len(th) != 3 {
		return
	}
	press, _ := strconv.Atoi(th[1])
	hard, _ := strconv.Atoi(th[2])
	page := os.Getpagesize()
	mb := func(pages int) int { return pages * page >> 20 }
	switch {
	case hard > 0 && mem >= hard:
		d.warn("kernel TCP memory", fmt.Sprintf("%d MB in TCP buffers, at the kernel's hard limit (%d MB, tcp_mem) — packets are being dropped on every TCP socket; look for stalled readers (hs2 logs \"stopped reading\")", mb(mem), mb(hard)))
	case press > 0 && mem >= press:
		d.warn("kernel TCP memory", fmt.Sprintf("%d MB in TCP buffers, above the kernel's pressure mark (%d MB of %d MB, tcp_mem) — socket buffers are being squeezed", mb(mem), mb(press), mb(hard)))
	default:
		d.ok("kernel TCP memory", fmt.Sprintf("%d MB in TCP buffers (pressure at %d MB, limit %d MB)", mb(mem), mb(press), mb(hard)))
	}
}
