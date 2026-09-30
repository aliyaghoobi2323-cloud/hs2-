package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
)

// checkCmd validates a config file without starting anything:
//
//	hs2 check -c /etc/hs2/config.json
//
// Every problem that would stop the tunnel from starting or working is an
// ERROR (exit status 1); things that work but look wrong are WARNings. The
// installer's tunnel manager runs this after each edit so a typo never
// replaces a working config.
func checkCmd(args []string) {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	cfgPath := fs.String("c", "", "config file (JSON)")
	fs.Parse(args)
	if *cfgPath == "" {
		fmt.Println("usage: hs2 check -c config.json")
		os.Exit(2)
	}
	raw, err := os.ReadFile(*cfgPath)
	if err != nil {
		fmt.Printf("ERROR: %v\n", err)
		os.Exit(1)
	}
	errs, warns := checkConfig(raw, isLocalIP, time.Now())
	for _, w := range warns {
		fmt.Println("WARN:  " + w)
	}
	for _, e := range errs {
		fmt.Println("ERROR: " + e)
	}
	if len(errs) > 0 {
		os.Exit(1)
	}
	fmt.Println("config OK")
}

// isLocalIP reports whether ip is assigned to an interface of this machine.
func isLocalIP(ip net.IP) bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return true // cannot tell; do not block on it
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return false
}

var knownCarriers = map[string]bool{
	"mtcp": true, "l3mtcp": true, "l3": true, "tls": true,
	"udp": true, "auto": true, "noise": true, "reality": true,
	"dgtun": true,
}

// ipxHandledProtos mirrors encap.ipxHandledProtos: IP protocol numbers the
// kernel handles or that are reserved, which ipx must avoid.
var ipxHandledProtos = map[int]bool{
	0: true, 1: true, 2: true, 4: true, 6: true, 17: true, 33: true, 41: true,
	43: true, 44: true, 46: true, 47: true, 50: true, 51: true, 58: true, 59: true,
	60: true, 88: true, 89: true, 92: true, 94: true, 97: true, 98: true, 103: true,
	108: true, 112: true, 115: true, 132: true, 136: true, 137: true, 143: true, 255: true,
}

func validIPXProto(p int) bool { return p >= 1 && p <= 254 && !ipxHandledProtos[p] }

// knownEncaps are the datagram-tun encapsulations (carrier "dgtun"). Empty is
// udp. Kept here (not imported from encap) so check builds without cgo/root.
var knownEncaps = map[string]bool{
	"": true, "udp": true, "icmp": true, "gre": true, "ipip": true, "ipx": true,
}

// checkConfig returns the errors and warnings for one config. localIP decides
// whether an address belongs to this machine; now is used for cert expiry.
func checkConfig(raw []byte, localIP func(net.IP) bool, now time.Time) (errs, warns []string) {
	bad := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }
	warn := func(f string, a ...any) { warns = append(warns, fmt.Sprintf(f, a...)) }

	var fc fileConfig
	if err := json.Unmarshal(raw, &fc); err != nil {
		var se *json.SyntaxError
		var te *json.UnmarshalTypeError
		switch {
		case errors.As(err, &se):
			line, col := lineCol(raw, se.Offset)
			bad("not valid JSON at line %d, column %d: %v — usually a missing comma or quote just before this point (or at the end of the previous line)", line, col, se)
		case errors.As(err, &te):
			bad("field %q has the wrong type: got %s, want %s (e.g. true/false without quotes, numbers without quotes)", te.Field, te.Value, te.Type)
		default:
			bad("cannot read config: %v", err)
		}
		return
	}

	// Unknown keys are ignored by the engine: almost always a typo that
	// silently drops a setting (bind_local_ipp, reverce, ...).
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) == nil {
		known := jsonKeys(reflect.TypeOf(fc))
		var unknown []string
		for k := range m {
			if !known[k] {
				unknown = append(unknown, k)
			}
		}
		sort.Strings(unknown)
		for _, k := range unknown {
			warn("unknown key %q is ignored — a typo?", k)
		}
	}

	if fc.Mode != "dial" && fc.Mode != "listen" {
		bad(`"mode" must be "dial" (iran) or "listen" (kharej), got %q`, fc.Mode)
		return
	}
	carrier := fc.Carrier
	if carrier == "" {
		carrier = "noise"
	}
	if !knownCarriers[carrier] {
		bad(`unknown "carrier" %q (mtcp, l3mtcp, tls, udp, auto, dgtun)`, fc.Carrier)
		return
	}
	stream := carrier == "mtcp" || carrier == "l3mtcp" || carrier == "l3" || carrier == "tls"
	dgtun := carrier == "dgtun"
	withTUN := carrier != "mtcp"
	dials := dialing(fc)
	edge := fc.Mode == "dial"

	// addr: where this side listens, or the peer it dials.
	host, port, err := net.SplitHostPort(fc.Addr)
	if err != nil {
		bad(`"addr" must be IP:PORT, got %q`, fc.Addr)
	} else {
		if p, perr := strconv.Atoi(port); perr != nil || p < 1 || p > 65535 {
			bad(`"addr" port %q is not a valid port`, port)
		}
		if !dials {
			if ip := net.ParseIP(host); host != "" && ip == nil {
				bad(`"addr" %q: listen address must be an IP (or 0.0.0.0)`, fc.Addr)
			} else if ip != nil && !ip.IsUnspecified() && !localIP(ip) {
				bad(`"addr": %s is not an IP of this server, so it cannot listen on it (use one of this server's IPs, or 0.0.0.0)`, host)
			}
		} else if host == "" || net.ParseIP(host) != nil && net.ParseIP(host).IsUnspecified() {
			bad(`"addr" %q: this side connects out, so it needs the OTHER server's IP`, fc.Addr)
		}
	}

	if fc.BindLocalIP != "" {
		ip := net.ParseIP(fc.BindLocalIP)
		switch {
		case ip == nil:
			bad(`"bind_local_ip" %q is not a valid IP address`, fc.BindLocalIP)
		case !localIP(ip):
			bad(`"bind_local_ip" %s is not an IP of this server — connections from it would fail`, fc.BindLocalIP)
		case !dials:
			warn(`"bind_local_ip" has no effect here: this side listens (set the listen IP in "addr")`)
		}
	}
	if fc.UserListenIP != "" {
		ip := net.ParseIP(fc.UserListenIP)
		if ip == nil {
			bad(`"user_listen_ip" %q is not a valid IP address`, fc.UserListenIP)
		} else if !ip.IsUnspecified() && !localIP(ip) {
			bad(`"user_listen_ip" %s is not an IP of this server`, fc.UserListenIP)
		}
	}

	// Keys.
	switch carrier {
	case "noise":
		if fc.SharedKey == "" && (fc.LocalPriv == "" || fc.RemoteStatic == "") {
			bad(`"shared_key" is missing`)
		}
	default:
		if fc.SharedKey == "" {
			bad(`"shared_key" is missing (it must be the same on both servers)`)
		} else if b, err := hex.DecodeString(fc.SharedKey); err != nil {
			bad(`"shared_key" is not hex (only 0-9 and a-f): copy it again from the other server`)
		} else if len(b) != 32 {
			warn(`"shared_key" is %d bytes; the installer makes 32 (64 hex characters) — make sure both servers have the exact same key`, len(b))
		}
	}

	// Stream roles.
	if stream {
		if edge {
			ports := splitComma(fc.ForwardPorts)
			if len(ports) == 0 && carrier == "mtcp" {
				bad(`"forward_ports" is empty: mtcp needs at least one user port (e.g. "8443")`)
			} else if len(ports) == 0 {
				// l3mtcp/tls also carry a TUN, so no user ports is a valid pure
				// routed tunnel — but then nothing listens for users here, which
				// is almost never what an edge in front of a panel wants.
				warn(`"forward_ports" is empty: no user port listens on this server, so users cannot reach the panel through it (pure routed tunnel — route traffic toward the peer's tun IP yourself, or set the user ports, e.g. "8443,443")`)
			}
			seen := map[string]bool{}
			for _, p := range ports {
				n, err := strconv.Atoi(p)
				if err != nil || n < 1 || n > 65535 {
					bad(`"forward_ports": %q is not a valid port`, p)
					continue
				}
				if seen[p] {
					warn(`"forward_ports": port %s is listed twice`, p)
				}
				seen[p] = true
				if !dials && p == port {
					bad(`"forward_ports": %s is also the tunnel port in "addr" — pick a different user port`, p)
				}
			}
			if fc.MinLinks < 0 || fc.MaxLinks < 0 || fc.PerLink < 0 {
				bad(`"min_links", "max_links" and "per_link" cannot be negative`)
			} else if fc.MinLinks > 0 && fc.MaxLinks > 0 && fc.MinLinks > fc.MaxLinks {
				bad(`"min_links" (%d) is larger than "max_links" (%d)`, fc.MinLinks, fc.MaxLinks)
			}
			if d := fc.DrainIdleSec; d != nil {
				switch {
				case *d < 0:
					bad(`"drain_idle_sec" cannot be negative (0 = never close idle connections on retiring links)`)
				case *d > 0 && *d < 300:
					warn(`"drain_idle_sec" %d is below xray's default connIdle (300 s): connections the panel would still keep may be closed when the pool shrinks`, *d)
				}
			}
		} else if carrier == "mtcp" || carrier == "tls" {
			if _, _, err := net.SplitHostPort(fc.Expose); err != nil {
				bad(`"expose" must be the panel address IP:PORT (e.g. "127.0.0.1:8443"), got %q`, fc.Expose)
			}
		} else if fc.Expose == "" {
			// l3mtcp exit without a panel: a pure routed tunnel works, but any
			// user port the edge opens would reach nothing on this server.
			warn(`"expose" is empty: connections from the edge's user ports have no panel to go to on this server (set it to the panel inbound, e.g. "127.0.0.1:8443")`)
		} else if _, _, err := net.SplitHostPort(fc.Expose); err != nil {
			bad(`"expose" must be the panel address IP:PORT (e.g. "127.0.0.1:8443"), got %q`, fc.Expose)
		}
		if dials && fc.SNI == "" {
			warn(`"sni" is empty: the TLS handshake goes out without a domain name, which stands out to DPI`)
		}
	}

	// The TLS server side needs a real certificate.
	if (stream || carrier == "reality") && !dials {
		if fc.CertFile == "" || fc.KeyFile == "" {
			bad(`"cert_file" and "key_file" are required on this side (it is the TLS server)`)
		} else if cert, err := tls.LoadX509KeyPair(fc.CertFile, fc.KeyFile); err != nil {
			bad(`certificate: %v`, err)
		} else if leaf, err := x509.ParseCertificate(cert.Certificate[0]); err == nil {
			switch left := leaf.NotAfter.Sub(now); {
			case left <= 0:
				warn("certificate expired on %s — renew it (certbot renew), then restart", leaf.NotAfter.Format("2006-01-02"))
			case left < 14*24*time.Hour:
				warn("certificate expires in %d days (%s)", int(left.Hours()/24), leaf.NotAfter.Format("2006-01-02"))
			}
		}
	}

	// Datagram tun (carrier "dgtun"): a routed TUN over a pool of datagram
	// carriers. Validate the encapsulation, the pool envelope, and — when the
	// userspace port forwarder is used — the ports and the panel.
	if dgtun {
		if !knownEncaps[fc.Encap] {
			bad(`unknown "encap" %q (udp, icmp, gre, ipip, ipx)`, fc.Encap)
		}
		if fc.Encap == "ipx" {
			if p := fc.Proto; p != 0 && !validIPXProto(p) {
				bad(`"proto" %d cannot be used for ipx: it is a reserved number or one the kernel already handles (ICMP/IGMP/IPIP/TCP/UDP/GRE/ESP/AH/OSPF/SCTP/MPLS/…). Pick an unassigned number; 253 (the default) is safe`, p)
			}
		} else if fc.Proto != 0 {
			warn(`"proto" only applies to the ipx encapsulation; it is ignored for %q`, fc.Encap)
		}
		if fc.Encap == "icmp" && !dials && !haveEchoGuardTool() {
			warn(`encap icmp: neither nft nor iptables is installed, so to keep the kernel from answering the tunnel's echo requests hs2 must turn off ALL ping replies on this server while it runs (public IP and tun IP) — install nftables to keep normal ping working`)
		}
		if fc.MinLinks < 0 || fc.MaxLinks < 0 || fc.PerLink < 0 {
			bad(`"min_links", "max_links" and "per_link" cannot be negative`)
		} else if fc.MinLinks > 0 && fc.MaxLinks > 0 && fc.MinLinks > fc.MaxLinks {
			bad(`"min_links" (%d) is larger than "max_links" (%d)`, fc.MinLinks, fc.MaxLinks)
		}
		ports := splitComma(fc.ForwardPorts)
		seen := map[string]bool{}
		for _, pt := range ports {
			n, err := strconv.Atoi(pt)
			if err != nil || n < 1 || n > 65535 {
				bad(`"forward_ports": %q is not a valid port`, pt)
				continue
			}
			if seen[pt] {
				warn(`"forward_ports": port %s is listed twice`, pt)
			}
			seen[pt] = true
		}
		// The exit hands everything arriving on the tun forwarder port to the
		// panel; the user ports live only on the edge (forward_ports is ignored
		// here). No panel is a valid pure routed tunnel, but never silently.
		if !edge {
			if fc.Expose == "" {
				warn(`"expose" is empty: connections from the edge's user ports have no panel to go to on this server (set it to the panel inbound, e.g. "127.0.0.1:8443")`)
			} else if _, pport, err := net.SplitHostPort(fc.Expose); err != nil {
				bad(`"expose" must be the panel address IP:PORT (e.g. "127.0.0.1:8443"), got %q`, fc.Expose)
			} else if pport == engine.DgTunPort {
				warn(`"expose" uses port %s, which the tunnel's own forwarder listens on (on the tun IP): a panel bound on 0.0.0.0:%s stops the forwarder from starting — move the panel to another port`, pport, pport)
			}
		}
	}

	if withTUN {
		if len(fc.Iface) > 15 {
			bad(`"iface" %q is longer than 15 characters (Linux limit)`, fc.Iface)
		}
		if _, _, err := net.ParseCIDR(fc.LocalCIDR); fc.LocalCIDR != "" && err != nil {
			bad(`"local_cidr" %q is not a valid CIDR (e.g. "10.77.0.1/30")`, fc.LocalCIDR)
		}
		if fc.PeerIP != "" && net.ParseIP(fc.PeerIP) == nil {
			bad(`"peer_ip" %q is not a valid IP`, fc.PeerIP)
		}
	}
	if fc.MTU != 0 && (fc.MTU < 576 || fc.MTU > 9000) {
		warn(`"mtu" %d is unusual (normal range 1200-1500)`, fc.MTU)
	}

	// Optional tuning section. Auto is the default; validate what is set so a
	// typo does not silently disable tuning at the next restart.
	if fc.Tuning != nil {
		t := fc.Tuning
		switch t.Mode {
		case "", "auto", "manual", "off":
		default:
			bad(`"tuning.mode" must be "auto", "manual" or "off", got %q`, t.Mode)
		}
		if t.RmemMax < 0 || t.WmemMax < 0 || t.Backlog < 0 || t.Somaxconn < 0 {
			bad(`"tuning" buffer/backlog values cannot be negative`)
		}
		if (t.RmemMax > 0 || t.WmemMax > 0 || t.Backlog > 0 || t.Somaxconn > 0) && t.Mode != "manual" {
			warn(`"tuning" has manual values but "mode" is not "manual" — they are ignored (set "mode":"manual" to use them)`)
		}
		if t.RmemMax > 0 && t.RmemMax < 65536 {
			warn(`"tuning.rmem_max" %d is very small (bytes); a normal value is a few MB, e.g. 16777216`, t.RmemMax)
		}
	}
	return
}

// jsonKeys lists the JSON field names of a struct type.
func jsonKeys(t reflect.Type) map[string]bool {
	out := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		name := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			out[name] = true
		}
	}
	return out
}

// lineCol turns a byte offset into a 1-based line and column.
func lineCol(b []byte, off int64) (int, int) {
	line, col := 1, 1
	for i := int64(0); i < off && i < int64(len(b)); i++ {
		if b[i] == '\n' {
			line, col = line+1, 1
		} else {
			col++
		}
	}
	return line, col
}

// haveEchoGuardTool reports whether nft or iptables is present, which an icmp
// listener uses to drop only the kernel's replies to the tunnel's own echo
// requests (encap/echoguard_linux.go). Without either it must silence all ping.
// A variable so tests can pin it.
var haveEchoGuardTool = func() bool {
	for _, t := range []string{"nft", "iptables"} {
		if _, err := exec.LookPath(t); err == nil {
			return true
		}
	}
	return false
}
