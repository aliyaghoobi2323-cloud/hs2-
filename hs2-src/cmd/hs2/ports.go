package main

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
)

// Per-port routing, operator side (the model is in engine/routes.go): the Iran
// server opens the user ports (forward_ports); the kharej server decides where
// each one goes (port_map, else expose). `hs2 ports` shows the whole table as
// THIS server knows it — its own half from the config, the other half as the
// other server reported it over a live link — and edits this server's half:
//
//	hs2 ports -c cfg                         the table
//	hs2 ports -c cfg add P[=host:port]       Iran: open user port P
//	                                         Kharej: give P its own target (P alone = 127.0.0.1:P)
//	hs2 ports -c cfg remove P                Iran: close user port P
//	                                         Kharej: P goes back to the default panel
//	hs2 ports -c cfg default host:port|none  Kharej: the default panel (expose)
//	hs2 ports -c cfg udp on|off              Iran: also forward UDP on the user ports
//
// Every change is validated like `hs2 check` and written atomically; it takes
// effect when the tunnel restarts.

// hasUserPorts reports whether the carrier forwards user ports to a panel.
func hasUserPorts(fc fileConfig) bool {
	switch carrierName(fc) {
	case "mtcp", "l3mtcp", "l3", "tls", "dgtun":
		return true
	}
	return false
}

// edgePorts is the Iran server's user ports (forward_ports) as numbers, in the
// configured order.
func edgePorts(fc fileConfig) []int {
	var out []int
	for _, p := range splitComma(fc.ForwardPorts) {
		if n, err := strconv.Atoi(p); err == nil && n > 0 && n <= 65535 {
			out = append(out, n)
		}
	}
	return out
}

// exitTable is the kharej server's routing table from its config.
func exitTable(fc fileConfig) engine.RouteTable {
	m, _ := engine.ParsePortMap(fc.PortMap) // hs2 check rejects a bad one
	return engine.RouteTable{Default: fc.Expose, Ports: m}
}

// peerView is what this server knows of the other server's half of the table,
// from the live status file, plus the context the wording needs.
type peerView struct {
	State   string // "" not reported, "known", "older"
	Tags    bool   // (Iran) the Kharej server routes by port
	Ports   []int  // Iran: the Kharej server's mapped ports; Kharej: the Iran server's user ports
	Default bool   // (Iran) the Kharej server has a default panel
	UDP     bool   // (Kharej) the Iran server forwards UDP
	Cut     bool
	Running bool // the status file is fresh
	Links   int  // links (carriers) up
}

func viewOf(ls liveStatus, running bool) peerView {
	return peerView{State: ls.PeerRoutes, Tags: ls.PeerTags, Ports: ls.PeerPorts, Default: ls.PeerDefault,
		UDP: ls.PeerUDP, Cut: ls.PeerCut, Running: running, Links: ls.Links}
}

// fillPeerRoutes copies the engine's report into the status fields.
func fillPeerRoutes(ls *liveStatus, r *engine.PeerRoutes) {
	if r == nil {
		return
	}
	switch {
	case r.Known:
		ls.PeerRoutes = "known"
	case r.Filtered:
		ls.PeerRoutes = "filtered"
	case r.Older:
		ls.PeerRoutes = "older"
	}
	ls.PeerTags, ls.PeerPorts, ls.PeerDefault, ls.PeerUDP, ls.PeerCut = r.Tags, r.Ports, r.Default, r.UDP, r.Cut
}

func joinPorts(ps []int) string {
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = strconv.Itoa(p)
	}
	return strings.Join(s, ", ")
}

func contains(ps []int, p int) bool {
	for _, x := range ps {
		if x == p {
			return true
		}
	}
	return false
}

func protoLabel(udp bool) string {
	if udp {
		return "tcp+udp"
	}
	return "tcp"
}

// notKnown says why the other server's half is not known.
func notKnown(pv peerView) string {
	switch {
	case !pv.Running:
		return "not known — the tunnel is not running"
	case pv.Links == 0:
		return "not known yet (no link is up)"
	}
	return "not known yet"
}

// portLines renders the routing table from THIS server's point of view. Nil
// when the carrier has no user ports.
func portLines(fc fileConfig, pv peerView) []string {
	if !hasUserPorts(fc) {
		return nil
	}
	if fc.Mode == "dial" {
		return edgePortLines(fc, pv)
	}
	return exitPortLines(fc, pv)
}

func edgePortLines(fc fileConfig, pv peerView) []string {
	own := edgePorts(fc)
	if len(own) == 0 {
		return []string{"none — no user port listens on this server (a pure routed tunnel)"}
	}
	out := []string{fmt.Sprintf("%s (%s) — where each one goes is set on the Kharej server", joinPorts(own), protoLabel(fc.UDP))}
	switch {
	case pv.State == "known" && pv.Tags:
		var refused []int
		for _, p := range own {
			switch {
			case contains(pv.Ports, p):
				out = append(out, fmt.Sprintf("  %d → its own target on the Kharej server (port_map there)", p))
			case pv.Default:
				out = append(out, fmt.Sprintf("  %d → the Kharej server's default panel (expose there)", p))
			default:
				refused = append(refused, p)
				out = append(out, fmt.Sprintf("  %d → NO target on the Kharej server: its connections are refused", p))
			}
		}
		if len(refused) > 0 {
			out = append(out, fmt.Sprintf("  fix: on the Kharej server, Tunnel manager → Ports → add %s (or give it a default panel)", joinPorts(refused)))
		}
		var extra []int
		for _, p := range pv.Ports {
			if !contains(own, p) {
				extra = append(extra, p)
			}
		}
		if len(extra) > 0 {
			out = append(out, fmt.Sprintf("  the Kharej server also has a target for %s — not opened here (add it here to use it)", joinPorts(extra)))
		}
		if pv.Cut {
			out = append(out, "  (the Kharej server's list was too long to report in full)")
		}
	case pv.State == "filtered":
		out = append(out, "  every port → the Kharej server's default panel: per-port routing is OFF — its tun answers on port "+engine.DgTunPort+" but not on "+engine.DgTagPort+", which per-port targets use (a firewall on the Kharej server? allow "+engine.DgTagPort+" on its tun)")
	case pv.State == "known" || pv.State == "older":
		out = append(out, "  every port → the Kharej server's one panel (expose): it runs an older hs2 that does not route by port — upgrade it to give ports their own targets")
	default:
		out = append(out, "  where each goes: "+notKnown(pv))
	}
	return out
}

func exitPortLines(fc fileConfig, pv peerView) []string {
	t := exitTable(fc)
	if t.Default == "" && len(t.Ports) == 0 {
		return []string{"none — no default panel (expose) and no port_map: the Iran server's user ports have nowhere to go here (a pure routed tunnel)"}
	}
	dflt := "refused (no default panel — expose is not set)"
	if t.Default != "" {
		dflt = t.Default + " (default panel, expose)"
	}
	mapped := t.MappedPorts()
	if pv.State != "known" {
		why := notKnown(pv)
		if pv.Running && pv.Links > 0 {
			why = "not reported — if this persists with links up, the Iran server runs an older hs2, which does not say which port a user came in on: then EVERY connection goes to the default panel"
			if carrierName(fc) == "dgtun" {
				why += " (the same happens if this server filters port " + engine.DgTagPort + " on its tun)"
			}
		}
		out := []string{"the Iran server's user ports: " + why}
		for _, p := range mapped {
			out = append(out, fmt.Sprintf("  %d → %s (port_map)", p, t.Ports[p]))
		}
		label := "every other port"
		if len(mapped) == 0 {
			label = "every port"
		}
		return append(out, fmt.Sprintf("  %s → %s", label, dflt))
	}
	if len(pv.Ports) == 0 {
		out := []string{"the Iran server opens no user ports (a pure routed tunnel) — nothing reaches a panel through this tunnel"}
		for _, p := range mapped {
			out = append(out, fmt.Sprintf("  %d → %s (port_map) — unused until the Iran server opens it", p, t.Ports[p]))
		}
		return out
	}
	out := []string{fmt.Sprintf("the Iran server opens %s (%s); each goes to:", joinPorts(pv.Ports), protoLabel(pv.UDP))}
	var refused []int
	for _, p := range pv.Ports {
		switch target, ok := t.Ports[p]; {
		case ok:
			out = append(out, fmt.Sprintf("  %d → %s (port_map)", p, target))
		case t.Default != "":
			out = append(out, fmt.Sprintf("  %d → %s", p, dflt))
		default:
			refused = append(refused, p)
			out = append(out, fmt.Sprintf("  %d → NONE: its connections are refused", p))
		}
	}
	for _, p := range mapped {
		if !contains(pv.Ports, p) {
			out = append(out, fmt.Sprintf("  %d → %s (port_map) — the Iran server does not open this port, so nothing uses it yet", p, t.Ports[p]))
		}
	}
	if len(refused) > 0 {
		out = append(out, fmt.Sprintf("  fix: Tunnel manager → Ports → add %s here (or set a default panel)", joinPorts(refused)))
	}
	if pv.Cut {
		out = append(out, "  (the Iran server's list was too long to report in full)")
	}
	return out
}

// readLive reads the live status file; running is true when it is fresh.
func readLive(cfgPath string) (ls liveStatus, running bool) {
	b, err := os.ReadFile(statusPath(cfgPath))
	if err != nil || json.Unmarshal(b, &ls) != nil {
		return liveStatus{}, false
	}
	return ls, time.Now().Unix()-ls.Updated <= 7
}

func portsCmd(args []string) {
	fs := flag.NewFlagSet("ports", flag.ExitOnError)
	cfgPath := fs.String("c", "", "config file (JSON)")
	fs.Parse(args)
	rest := fs.Args()
	if *cfgPath == "" {
		fmt.Println("usage: hs2 ports -c config.json [add P[=host:port] | remove P | default host:port|none | udp on|off]")
		os.Exit(2)
	}
	raw, err := os.ReadFile(*cfgPath)
	if err != nil {
		fmt.Printf("ERROR: %v\n", err)
		os.Exit(1)
	}
	var fc fileConfig
	if err := json.Unmarshal(raw, &fc); err != nil {
		fmt.Printf("ERROR: the config is not valid JSON: %v\n", err)
		os.Exit(1)
	}
	if !hasUserPorts(fc) {
		fmt.Printf("carrier %q is an IP tunnel: it has no user ports (only tcp and tun transports forward user ports)\n", carrierName(fc))
		if len(rest) > 0 {
			os.Exit(1)
		}
		return
	}
	if len(rest) == 0 {
		ls, running := readLive(*cfgPath)
		for _, l := range portLines(fc, viewOf(ls, running)) {
			fmt.Println(l)
		}
		return
	}
	msg, err := editPorts(*cfgPath, fc, rest)
	if err != nil {
		fmt.Printf("ERROR: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(msg)
}

// editPorts applies one `hs2 ports` change to the config file.
func editPorts(cfgPath string, fc fileConfig, args []string) (string, error) {
	edge := fc.Mode == "dial"
	op, arg := args[0], ""
	if len(args) > 1 {
		arg = strings.TrimSpace(args[1])
	}
	if len(args) != 2 {
		return "", fmt.Errorf("%s needs one value", op)
	}
	if strings.Contains(arg, ",") {
		return "", fmt.Errorf("one port per %s (got %q)", op, arg)
	}
	if !edge {
		// Never rewrite a table this command cannot read: the entries it
		// could not parse would be silently dropped.
		if _, err := engine.ParsePortMap(fc.PortMap); err != nil {
			return "", fmt.Errorf("the port_map in this config is invalid (%v) — fix it first (hs2 check -c %s)", err, cfgPath)
		}
	}
	m, err := readConfigMap(cfgPath)
	if err != nil {
		return "", err
	}
	var msg string
	switch {
	case op == "add" && edge:
		p, err := portArg(arg)
		if err != nil {
			if strings.Contains(arg, "=") {
				return "", fmt.Errorf("the target of a user port is set on the Kharej server, not here — add just the port here (%s)", strings.SplitN(arg, "=", 2)[0])
			}
			return "", err
		}
		ports := edgePorts(fc)
		if contains(ports, p) {
			return "", fmt.Errorf("user port %d is already open", p)
		}
		ports = append(ports, p)
		m["forward_ports"] = joinPortsCSV(ports)
		msg = fmt.Sprintf("user port %d added (forward_ports = %s)", p, m["forward_ports"])
	case op == "remove" && edge:
		p, err := portArg(arg)
		if err != nil {
			return "", err
		}
		ports := edgePorts(fc)
		if !contains(ports, p) {
			return "", fmt.Errorf("user port %d is not open here (open: %s)", p, joinPorts(ports))
		}
		var keep []int
		for _, x := range ports {
			if x != p {
				keep = append(keep, x)
			}
		}
		m["forward_ports"] = joinPortsCSV(keep)
		msg = fmt.Sprintf("user port %d removed (forward_ports = %s)", p, m["forward_ports"])
	case op == "udp" && edge:
		switch strings.ToLower(arg) {
		case "on", "true", "yes":
			m["udp"] = true
		case "off", "false", "no":
			m["udp"] = false
		default:
			return "", fmt.Errorf("udp takes on or off, got %q", arg)
		}
		msg = fmt.Sprintf("UDP forwarding on the user ports: %s", map[bool]string{true: "on", false: "off"}[m["udp"] == true])
	case op == "add":
		ps, target, hasTarget := strings.Cut(arg, "=")
		p, err := portArg(strings.TrimSpace(ps))
		if err != nil {
			return "", err
		}
		entry := strconv.Itoa(p)
		if hasTarget {
			entry += "=" + strings.TrimSpace(target)
		}
		one, err := engine.ParsePortMap(entry)
		if err != nil {
			return "", err
		}
		table := exitTable(fc).Ports
		if table == nil {
			table = map[int]string{}
		}
		old, had := table[p]
		table[p] = one[p]
		m["port_map"] = engine.FormatPortMap(table)
		if had {
			msg = fmt.Sprintf("user port %d now goes to %s (was %s)", p, one[p], old)
		} else {
			msg = fmt.Sprintf("user port %d now goes to %s", p, one[p])
		}
	case op == "remove":
		p, err := portArg(arg)
		if err != nil {
			return "", err
		}
		table := exitTable(fc).Ports
		if _, ok := table[p]; !ok {
			return "", fmt.Errorf("user port %d has no own target here (port_map: %s)", p, orDash(fc.PortMap))
		}
		delete(table, p)
		if len(table) == 0 {
			delete(m, "port_map")
		} else {
			m["port_map"] = engine.FormatPortMap(table)
		}
		dflt := "is refused (no default panel)"
		if fc.Expose != "" {
			dflt = "goes to the default panel " + fc.Expose
		}
		msg = fmt.Sprintf("user port %d has no own target any more: it %s", p, dflt)
	case op == "default" && !edge:
		if strings.EqualFold(arg, "none") || arg == "" {
			m["expose"] = ""
			msg = "no default panel: a user port without its own target is refused"
		} else {
			if _, _, err := net.SplitHostPort(arg); err != nil {
				return "", fmt.Errorf("the default panel must be host:port (e.g. 127.0.0.1:8443), got %q", arg)
			}
			m["expose"] = arg
			msg = "default panel (expose) = " + arg
		}
	default:
		side := "Kharej"
		if edge {
			side = "Iran"
		}
		return "", fmt.Errorf("%q is not a change the %s server makes here (Iran: add/remove a user port, udp on|off · Kharej: add/remove a port's own target, default host:port|none)", op, side)
	}
	out, err := marshalConfig(m)
	if err != nil {
		return "", err
	}
	if errs, _ := checkConfig(out, isLocalIP, time.Now()); len(errs) > 0 {
		return "", fmt.Errorf("the change would make the config invalid, so it was NOT saved: %s", strings.Join(errs, " · "))
	}
	if err := writeFileAtomic(cfgPath, out); err != nil {
		return "", err
	}
	return msg + " — restart the tunnel to apply it", nil
}

func portArg(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 || s != strconv.Itoa(n) {
		return 0, fmt.Errorf("%q is not a port (1-65535)", s)
	}
	return n, nil
}

func joinPortsCSV(ps []int) string {
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = strconv.Itoa(p)
	}
	return strings.Join(s, ",")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ---- doctor ----------------------------------------------------------------

// checkPorts reports the routing table as doctor sees it: on the Iran server,
// user ports the Kharej server has no target for; on the Kharej server, the
// same, plus targets on this server that nothing listens on.
func checkPorts(d *doctorReport, fc fileConfig, cfgPath string) {
	const name = "user ports"
	if !hasUserPorts(fc) {
		return
	}
	ls, running := readLive(cfgPath)
	pv := viewOf(ls, running)
	if fc.Mode == "dial" {
		own := edgePorts(fc)
		switch {
		case len(own) == 0:
			d.info(name, "none — a pure routed tunnel (no user port listens here)")
		case pv.State == "known" && pv.Tags:
			var refused, extra []int
			for _, p := range own {
				if !contains(pv.Ports, p) && !pv.Default {
					refused = append(refused, p)
				}
			}
			for _, p := range pv.Ports {
				if !contains(own, p) {
					extra = append(extra, p)
				}
			}
			if len(refused) > 0 {
				d.warn(name, fmt.Sprintf("the Kharej server has no target for %s — those connections are refused: on the Kharej server, Tunnel manager → Ports → add %s (or give it a default panel)", joinPorts(refused), joinPorts(refused)))
			} else {
				d.ok(name, fmt.Sprintf("%s — every one has a target on the Kharej server", joinPorts(own)))
			}
			if len(extra) > 0 {
				d.info(name, fmt.Sprintf("the Kharej server also has a target for %s, which is not opened here", joinPorts(extra)))
			}
		case pv.State == "filtered":
			d.warn(name, fmt.Sprintf("per-port routing is OFF: the Kharej server's tun answers on port %s but not on %s — allow %s on its tun (a firewall there?); until then %s all reach its default panel", engine.DgTunPort, engine.DgTagPort, engine.DgTagPort, joinPorts(own)))
		case pv.State != "":
			if len(own) > 1 {
				d.info(name, fmt.Sprintf("%s all reach the Kharej server's one panel: it runs an older hs2 that does not route by port (upgrade it to give ports their own targets)", joinPorts(own)))
			} else {
				d.ok(name, fmt.Sprintf("%d → the Kharej server's panel (an older hs2 there: no per-port targets)", own[0]))
			}
		default:
			d.info(name, fmt.Sprintf("%s — where each goes is %s", joinPorts(own), notKnown(pv)))
		}
		return
	}
	t := exitTable(fc)
	listening := listeningSockets()
	var dead []string
	check := func(target, what string) {
		if ok, checked := localListening(target, listening); checked && !ok {
			dead = append(dead, fmt.Sprintf("%s (%s)", target, what))
		}
	}
	if t.Default != "" {
		check(t.Default, "default panel")
	}
	for _, p := range t.MappedPorts() {
		check(t.Ports[p], "port "+strconv.Itoa(p))
	}
	if len(dead) > 0 {
		d.warn(name, "nothing listens (TCP or UDP) on "+strings.Join(dead, ", ")+" on this server — connections sent there fail; check the panel inbound's port")
	}
	if pv.State == "known" {
		var refused []int
		for _, p := range pv.Ports {
			if _, ok := t.Target(p); !ok {
				refused = append(refused, p)
			}
		}
		if len(refused) > 0 {
			d.warn(name, fmt.Sprintf("the Iran server opens %s, which has no target here (no port_map entry, no default panel) — refused: Tunnel manager → Ports → add %s", joinPorts(refused), joinPorts(refused)))
		} else if len(dead) == 0 {
			d.ok(name, fmt.Sprintf("the Iran server opens %s — each has a target here", joinPorts(pv.Ports)))
		}
		return
	}
	if len(dead) == 0 {
		why := notKnown(pv)
		if pv.Running && pv.Links > 0 {
			why = "not reported — if this persists with links up, the Iran server runs an older hs2: every connection then goes to the default panel and port_map is not used"
			if carrierName(fc) == "dgtun" {
				why += " (the same if this server filters port " + engine.DgTagPort + " on its tun)"
			}
		}
		d.info(name, fmt.Sprintf("%d port(s) with their own target, default panel %s; the Iran server's ports: %s", len(t.Ports), orDash(t.Default), why))
	}
}

// sockListen is one listening socket: TCP in LISTEN, or UDP bound and not
// connected — its local IP and port.
type sockListen struct {
	ip   net.IP
	port int
	udp  bool
}

// listeningSockets reads the listening TCP and bound UDP sockets from
// /proc/net/{tcp,tcp6,udp,udp6} (read only: no connection is made, no packet
// sent). nil when /proc is not readable; empty (not nil) when it is and
// nothing listens.
func listeningSockets() []sockListen {
	out := []sockListen{}
	read := false
	for _, f := range []struct {
		path, state string
		udp         bool
	}{{"/proc/net/tcp", "0A", false}, {"/proc/net/tcp6", "0A", false}, {"/proc/net/udp", "07", true}, {"/proc/net/udp6", "07", true}} {
		fh, err := os.Open(f.path)
		if err != nil {
			continue
		}
		read = true
		sc := bufio.NewScanner(fh)
		for sc.Scan() {
			fld := strings.Fields(sc.Text())
			if len(fld) < 4 || fld[3] != f.state { // TCP 0A = LISTEN, UDP 07 = unconnected
				continue
			}
			addr, port, ok := strings.Cut(fld[1], ":")
			if !ok {
				continue
			}
			pn, err := strconv.ParseUint(port, 16, 16)
			if err != nil {
				continue
			}
			if ip := procIP(addr); ip != nil {
				out = append(out, sockListen{ip, int(pn), f.udp})
			}
		}
		fh.Close()
	}
	if !read {
		return nil
	}
	return out
}

// procIP decodes /proc/net/tcp's address: 32-bit words in host (little-endian)
// byte order.
func procIP(h string) net.IP {
	b, err := hex.DecodeString(h)
	if err != nil || len(b)%4 != 0 {
		return nil
	}
	for i := 0; i+4 <= len(b); i += 4 {
		b[i], b[i+1], b[i+2], b[i+3] = b[i+3], b[i+2], b[i+1], b[i]
	}
	return net.IP(b)
}

// localListening reports whether something listens for target on this server
// — a TCP listener or a bound UDP socket (a UDP-only inbound counts).
// checked is false when it cannot tell (another host, a name, no /proc).
func localListening(target string, ls []sockListen) (ok, checked bool) {
	host, port, err := net.SplitHostPort(target)
	if err != nil || ls == nil {
		return false, false
	}
	pn, err := strconv.Atoi(port)
	if err != nil {
		return false, false
	}
	var ips []net.IP
	if host == "localhost" {
		ips = []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	} else if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsUnspecified() || isLocalIP(ip)) {
		ips = []net.IP{ip}
	} else {
		return false, false
	}
	for _, l := range ls {
		if l.port != pn {
			continue
		}
		for _, ip := range ips {
			if l.ip.IsUnspecified() || l.ip.Equal(ip) || ip.IsUnspecified() {
				return true, true
			}
		}
	}
	return false, true
}
