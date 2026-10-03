package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testKey = "d0e7d7f85bdd3190377cf4b97c876f52f3ca7227d95356918b6194442af56e54"

func localIs(ips ...string) func(net.IP) bool {
	return func(ip net.IP) bool {
		for _, s := range ips {
			if net.ParseIP(s).Equal(ip) {
				return true
			}
		}
		return false
	}
}

func writeCert(t *testing.T, notAfter time.Time) (string, string) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "t.example"},
		DNSNames: []string{"t.example"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(k)
	d := t.TempDir()
	c, kf := filepath.Join(d, "c.pem"), filepath.Join(d, "k.pem")
	os.WriteFile(c, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600)
	os.WriteFile(kf, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0600)
	return c, kf
}

// Every config shape the installer writes must pass with no errors, or the
// tunnel manager would refuse a perfectly good edit.
func TestCheckAcceptsInstallerConfigs(t *testing.T) {
	c, k := writeCert(t, time.Now().Add(60*24*time.Hour))
	cert := fmt.Sprintf(`"cert_file": %q, "key_file": %q`, c, k)
	cases := map[string]string{
		"kharej direct tcp": `{"mode": "listen", "carrier": "mtcp", "reverse": false, "addr": "0.0.0.0:2096",
			"iface": "hs0", "local_cidr": "10.77.0.2/30", "peer_ip": "10.77.0.1", "mtu": 1380,
			"backend_addr": "builtin", "shared_key": "` + testKey + `", ` + cert + `, "expose": "127.0.0.1:8443"}`,
		"iran direct tcp": `{"mode": "dial", "carrier": "mtcp", "reverse": false, "udp": false,
			"addr": "91.107.166.13:2096", "sni": "t.example", "iface": "hs0", "local_cidr": "10.77.0.1/30",
			"peer_ip": "10.77.0.2", "mtu": 1380, "shared_key": "` + testKey + `", "forward_ports": "8443,443",
			"peer_panel": "127.0.0.1:8443", "min_links": 8, "max_links": 16, "per_link": 8,
			"bind_local_ip": "", "user_listen_ip": ""}`,
		"iran reverse tcp": `{"mode": "dial", "carrier": "mtcp", "reverse": true, "udp": false,
			"addr": "5.57.38.168:2082", "iface": "hs0", "local_cidr": "10.77.0.1/30", "peer_ip": "10.77.0.2",
			"mtu": 1380, "backend_addr": "builtin", "shared_key": "` + testKey + `", ` + cert + `,
			"forward_ports": "8443", "user_listen_ip": ""}`,
		"kharej reverse tcp": `{"mode": "listen", "carrier": "mtcp", "reverse": true, "addr": "5.57.38.168:2082",
			"sni": "t.example", "iface": "hs0", "local_cidr": "10.77.0.2/30", "peer_ip": "10.77.0.1", "mtu": 1380,
			"shared_key": "` + testKey + `", "expose": "127.0.0.1:8443", "min_links": 8, "max_links": 16,
			"per_link": 8, "bind_local_ip": "91.107.166.13"}`,
		// tun over TLS (l3mtcp): the TUN is a routed side channel and the panel
		// inbound is forwarded like tcp — the edge opens forward_ports, the exit
		// delivers them to expose. All four direction/role shapes.
		"kharej direct tun": `{"mode": "listen", "carrier": "l3mtcp", "reverse": false, "addr": "91.107.166.13:2096",
			"iface": "hs0", "local_cidr": "10.77.0.2/30", "peer_ip": "10.77.0.1", "mtu": 1320,
			"backend_addr": "builtin", "shared_key": "` + testKey + `", ` + cert + `, "expose": "127.0.0.1:8443"}`,
		"iran direct tun": `{"mode": "dial", "carrier": "l3mtcp", "reverse": false, "udp": false, "addr": "91.107.166.13:2096",
			"sni": "t.example", "iface": "tun9", "local_cidr": "10.77.0.1/30", "peer_ip": "10.77.0.2", "mtu": 1320,
			"shared_key": "` + testKey + `", "forward_ports": "8443,443", "peer_panel": "127.0.0.1:8443", "user_listen_ip": "",
			"min_links": 8, "max_links": 16, "per_link": 8, "bind_local_ip": ""}`,
		"iran reverse tun": `{"mode": "dial", "carrier": "l3mtcp", "reverse": true, "udp": true, "addr": "5.57.38.168:2082",
			"iface": "hs0", "local_cidr": "10.77.0.1/30", "peer_ip": "10.77.0.2", "mtu": 1320,
			"backend_addr": "builtin", "shared_key": "` + testKey + `", ` + cert + `,
			"forward_ports": "8443,443", "user_listen_ip": "", "min_links": 2, "max_links": 32, "per_link": 8}`,
		"kharej reverse tun": `{"mode": "listen", "carrier": "l3mtcp", "reverse": true, "addr": "5.57.38.168:2082",
			"sni": "t.example", "iface": "hs0", "local_cidr": "10.77.0.2/30", "peer_ip": "10.77.0.1", "mtu": 1320,
			"shared_key": "` + testKey + `", "expose": "127.0.0.1:8443", "min_links": 2, "max_links": 32,
			"per_link": 8, "bind_local_ip": "91.107.166.13"}`,
		// tun -> tcp with the single-link TLS mode (carrier tls).
		"iran reverse tun tls": `{"mode": "dial", "carrier": "tls", "reverse": true, "udp": false, "addr": "5.57.38.168:2082",
			"iface": "hs0", "local_cidr": "10.77.0.1/30", "peer_ip": "10.77.0.2", "mtu": 1320,
			"backend_addr": "builtin", "shared_key": "` + testKey + `", ` + cert + `,
			"forward_ports": "8443", "user_listen_ip": "", "min_links": 2, "max_links": 32, "per_link": 8}`,
		"kharej reverse tun tls": `{"mode": "listen", "carrier": "tls", "reverse": true, "addr": "5.57.38.168:2082",
			"sni": "t.example", "iface": "hs0", "local_cidr": "10.77.0.2/30", "peer_ip": "10.77.0.1", "mtu": 1320,
			"shared_key": "` + testKey + `", "expose": "127.0.0.1:8443", "min_links": 2, "max_links": 32,
			"per_link": 8, "bind_local_ip": "91.107.166.13"}`,
		"kharej udp": `{"mode": "listen", "carrier": "udp", "reverse": false, "addr": "0.0.0.0:2096",
			"iface": "hs0", "local_cidr": "10.77.0.2/30", "peer_ip": "10.77.0.1", "mtu": 1280, "shared_key": "` + testKey + `"}`,
		"iran auto": `{"mode": "dial", "carrier": "auto", "reverse": false, "addr": "91.107.166.13:2096",
			"iface": "hs0", "local_cidr": "10.77.0.1/30", "peer_ip": "10.77.0.2", "mtu": 1280,
			"shared_key": "` + testKey + `", "bind_local_ip": "5.57.38.168"}`,
		"kharej dgtun udp": `{"mode": "listen", "carrier": "dgtun", "encap": "udp", "reverse": false,
			"addr": "0.0.0.0:2096", "iface": "hs0", "local_cidr": "10.77.0.2/30", "peer_ip": "10.77.0.1", "mtu": 1280,
			"shared_key": "` + testKey + `", "expose": "127.0.0.1:8443",
			"min_links": 2, "max_links": 32, "per_link": 8}`,
		"iran dgtun gre": `{"mode": "dial", "carrier": "dgtun", "encap": "gre", "reverse": false,
			"addr": "91.107.166.13:2096", "iface": "hs0", "local_cidr": "10.77.0.1/30", "peer_ip": "10.77.0.2",
			"mtu": 1280, "shared_key": "` + testKey + `", "forward_ports": "8443,443",
			"min_links": 2, "max_links": 32, "per_link": 8, "bind_local_ip": "5.57.38.168"}`,
		"iran dgtun ipx proto": `{"mode": "dial", "carrier": "dgtun", "encap": "ipx", "proto": 200, "reverse": false,
			"addr": "91.107.166.13:2096", "iface": "hs0", "local_cidr": "10.77.0.1/30", "peer_ip": "10.77.0.2",
			"mtu": 1280, "shared_key": "` + testKey + `", "forward_ports": "8443", "min_links": 2, "max_links": 8, "per_link": 8}`,
	}
	local := localIs("5.57.38.168", "91.107.166.13")
	for name, cfg := range cases {
		errs, warns := checkConfig([]byte(cfg), local, time.Now())
		if len(errs) > 0 || len(warns) > 0 {
			t.Errorf("%s: errs=%v warns=%v", name, errs, warns)
		}
	}
}

// Typical hand-edit mistakes must be caught with a message that names the field.
func TestCheckCatchesMistakes(t *testing.T) {
	base := `{"mode": "listen", "carrier": "mtcp", "reverse": true, "addr": "5.57.38.168:2082",
		"sni": "t.example", "shared_key": "` + testKey + `", "expose": "127.0.0.1:8443", "bind_local_ip": "91.107.166.13"}`
	local := localIs("91.107.166.13")
	if errs, _ := checkConfig([]byte(base), local, time.Now()); len(errs) != 0 {
		t.Fatalf("base config should pass: %v", errs)
	}
	cases := []struct{ name, from, to, want string }{
		{"missing comma", `"sni": "t.example",`, `"sni": "t.example"`, "line 2"},
		{"quoted bool", `"reverse": true`, `"reverse": "true"`, `"reverse"`},
		{"bad bind ip", `"91.107.166.13"`, `"91.107.166.133"`, "bind_local_ip"},
		{"bind ip typo", `"91.107.166.13"`, `"91.107.166.1333"`, "not a valid IP"},
		{"bad port", `:2082"`, `:99999"`, "port"},
		{"no expose", `"expose": "127.0.0.1:8443"`, `"expose": ""`, "expose"},
		{"key not hex", testKey, "zz" + testKey[2:], "shared_key"},
		{"bad mode", `"mode": "listen"`, `"mode": "server"`, "mode"},
		{"bad carrier", `"carrier": "mtcp"`, `"carrier": "mtpc"`, "carrier"},
	}
	for _, c := range cases {
		cfg := strings.Replace(base, c.from, c.to, 1)
		errs, _ := checkConfig([]byte(cfg), local, time.Now())
		if len(errs) == 0 || !strings.Contains(strings.Join(errs, "|"), c.want) {
			t.Errorf("%s: errs=%v, want one mentioning %q", c.name, errs, c.want)
		}
	}
	// A typo in a key name is a warning (the engine ignores it), not a silent pass.
	_, warns := checkConfig([]byte(strings.Replace(base, `"bind_local_ip"`, `"bind_local_ipp"`, 1)), local, time.Now())
	if !strings.Contains(strings.Join(warns, "|"), "bind_local_ipp") {
		t.Errorf("unknown key not reported: %v", warns)
	}
}

// The TLS-server side needs a loadable cert; an expired one is flagged.
func TestCheckCertificate(t *testing.T) {
	cfg := func(c, k string) []byte {
		return []byte(fmt.Sprintf(`{"mode": "dial", "carrier": "mtcp", "reverse": true, "addr": "0.0.0.0:2082",
			"shared_key": %q, "cert_file": %q, "key_file": %q, "forward_ports": "8443"}`, testKey, c, k))
	}
	if errs, _ := checkConfig(cfg("/nope/c.pem", "/nope/k.pem"), localIs(), time.Now()); len(errs) == 0 {
		t.Error("missing cert files not reported")
	}
	c, k := writeCert(t, time.Now().Add(-time.Hour))
	errs, warns := checkConfig(cfg(c, k), localIs(), time.Now())
	if len(errs) != 0 || !strings.Contains(strings.Join(warns, "|"), "expired") {
		t.Errorf("expired cert: errs=%v warns=%v", errs, warns)
	}
	// user port equal to the tunnel port is a conflict
	errs, _ = checkConfig([]byte(strings.Replace(string(cfg(c, k)), `"8443"`, `"2082"`, 1)), localIs(), time.Now())
	if !strings.Contains(strings.Join(errs, "|"), "tunnel port") {
		t.Errorf("port clash not reported: %v", errs)
	}
}

// drain_idle_sec: accepted when unset, 0 (never) or >= 300; negative is an
// error; below xray's 300 s connIdle is a warning.
func TestCheckDrainIdle(t *testing.T) {
	cfg := func(extra string) []byte {
		return []byte(`{"mode": "dial", "carrier": "mtcp", "reverse": false, "addr": "91.107.166.13:2096",
			"sni": "t.example", "shared_key": "` + testKey + `", "forward_ports": "8443"` + extra + `}`)
	}
	for _, ok := range []string{``, `, "drain_idle_sec": 0`, `, "drain_idle_sec": 310`, `, "drain_idle_sec": 3600`} {
		if errs, warns := checkConfig(cfg(ok), localIs(), time.Now()); len(errs)+len(warns) != 0 {
			t.Errorf("%q: errs=%v warns=%v", ok, errs, warns)
		}
	}
	if errs, _ := checkConfig(cfg(`, "drain_idle_sec": -1`), localIs(), time.Now()); !strings.Contains(strings.Join(errs, "|"), "drain_idle_sec") {
		t.Errorf("negative drain_idle_sec not rejected: %v", errs)
	}
	if _, warns := checkConfig(cfg(`, "drain_idle_sec": 60`), localIs(), time.Now()); !strings.Contains(strings.Join(warns, "|"), "connIdle") {
		t.Errorf("short drain_idle_sec not warned: %v", warns)
	}
	for in, want := range map[string]time.Duration{``: 0, `, "drain_idle_sec": 0`: -1, `, "drain_idle_sec": 400`: 400 * time.Second} {
		var fc fileConfig
		if err := json.Unmarshal(cfg(in), &fc); err != nil {
			t.Fatal(err)
		}
		if got := drainIdle(fc); got != want {
			t.Errorf("drainIdle(%q) = %s, want %s", in, got, want)
		}
	}
}

// A tun over TLS (l3mtcp) without user ports / panel is a valid pure routed
// tunnel, but it is exactly the "users cannot reach the panel" setup, so it
// must not pass silently: the edge warns about forward_ports, the exit about
// expose. A malformed panel address is an error. mtcp keeps requiring ports.
func TestCheckL3TunPortsAndPanel(t *testing.T) {
	c, k := writeCert(t, time.Now().Add(60*24*time.Hour))
	cert := fmt.Sprintf(`"cert_file": %q, "key_file": %q`, c, k)
	local := localIs("5.57.38.168", "91.107.166.13")
	edge := `{"mode": "dial", "carrier": "l3mtcp", "reverse": true, "addr": "5.57.38.168:2082",
		"iface": "hs0", "local_cidr": "10.77.0.1/30", "peer_ip": "10.77.0.2", "mtu": 1320,
		"backend_addr": "builtin", "shared_key": "` + testKey + `", ` + cert + `}`
	exit := func(extra string) []byte {
		return []byte(`{"mode": "listen", "carrier": "l3mtcp", "reverse": true, "addr": "5.57.38.168:2082",
			"sni": "t.example", "iface": "hs0", "local_cidr": "10.77.0.2/30", "peer_ip": "10.77.0.1", "mtu": 1320,
			"shared_key": "` + testKey + `"` + extra + `}`)
	}

	errs, warns := checkConfig([]byte(edge), local, time.Now())
	if len(errs) != 0 {
		t.Errorf("pure routed l3mtcp edge must stay valid: errs=%v", errs)
	}
	if !strings.Contains(strings.Join(warns, "|"), `"forward_ports" is empty`) {
		t.Errorf("l3mtcp edge without user ports not warned: warns=%v", warns)
	}

	errs, warns = checkConfig(exit(""), local, time.Now())
	if len(errs) != 0 {
		t.Errorf("pure routed l3mtcp exit must stay valid: errs=%v", errs)
	}
	if !strings.Contains(strings.Join(warns, "|"), `"expose" is empty`) {
		t.Errorf("l3mtcp exit without a panel not warned: warns=%v", warns)
	}

	if errs, _ := checkConfig(exit(`, "expose": "8443"`), local, time.Now()); !strings.Contains(strings.Join(errs, "|"), "expose") {
		t.Errorf("l3mtcp exit with a malformed panel address not rejected: errs=%v", errs)
	}
	if errs, warns := checkConfig(exit(`, "expose": "127.0.0.1:8443"`), local, time.Now()); len(errs)+len(warns) != 0 {
		t.Errorf("l3mtcp exit with a panel: errs=%v warns=%v", errs, warns)
	}

	mtcpEdge := strings.Replace(edge, `"carrier": "l3mtcp"`, `"carrier": "mtcp"`, 1)
	if errs, _ := checkConfig([]byte(mtcpEdge), local, time.Now()); !strings.Contains(strings.Join(errs, "|"), "forward_ports") {
		t.Errorf("mtcp edge without user ports must still be an error: errs=%v", errs)
	}
}

// dgtun (datagram tun) config checks.
func TestCheckDgTun(t *testing.T) {
	local := localIs("5.57.38.168", "91.107.166.13")
	base := func(extra string) []byte {
		return []byte(`{"mode": "dial", "carrier": "dgtun", "encap": "udp", "reverse": false,
			"addr": "91.107.166.13:2096", "iface": "hs0", "local_cidr": "10.77.0.1/30", "peer_ip": "10.77.0.2",
			"mtu": 1280, "shared_key": "` + testKey + `", "forward_ports": "8443"` + extra + `}`)
	}
	if errs, warns := checkConfig(base(""), local, time.Now()); len(errs) != 0 || len(warns) != 0 {
		t.Fatalf("base dgtun should pass: errs=%v warns=%v", errs, warns)
	}
	// bad encap
	if errs, _ := checkConfig([]byte(strings.Replace(string(base("")), `"encap": "udp"`, `"encap": "wireguard"`, 1)), local, time.Now()); !strings.Contains(strings.Join(errs, "|"), "encap") {
		t.Errorf("bad encap not caught: %v", errs)
	}
	// ipx with a reserved proto
	if errs, _ := checkConfig([]byte(strings.Replace(string(base(`, "proto": 47`)), `"encap": "udp"`, `"encap": "ipx"`, 1)), local, time.Now()); !strings.Contains(strings.Join(errs, "|"), "proto") {
		t.Errorf("reserved ipx proto not caught: %v", errs)
	}
	// min>max
	if errs, _ := checkConfig(base(`, "min_links": 20, "max_links": 4`), local, time.Now()); !strings.Contains(strings.Join(errs, "|"), "max_links") {
		t.Errorf("min>max not caught: %v", errs)
	}
	// The raw encaps have no ports: addr is a bare IP (what the installer writes
	// now) or an older IP:PORT whose port is ignored. udp still needs its port.
	withAddr := func(encap, addr string) []byte {
		s := strings.Replace(string(base("")), `"encap": "udp"`, `"encap": "`+encap+`"`, 1)
		return []byte(strings.Replace(s, `"91.107.166.13:2096"`, `"`+addr+`"`, 1))
	}
	for _, e := range []string{"icmp", "gre", "ipip", "ipx"} {
		for _, a := range []string{"91.107.166.13", "91.107.166.13:2096"} {
			if errs, warns := checkConfig(withAddr(e, a), local, time.Now()); len(errs)+len(warns) != 0 {
				t.Errorf("encap %s addr %s: errs=%v warns=%v", e, a, errs, warns)
			}
		}
	}
	if errs, _ := checkConfig(withAddr("udp", "91.107.166.13"), local, time.Now()); !strings.Contains(strings.Join(errs, "|"), "IP:PORT") {
		t.Errorf("udp encap without a port not rejected: %v", errs)
	}
	if errs, _ := checkConfig(withAddr("gre", "0.0.0.0"), local, time.Now()); !strings.Contains(strings.Join(errs, "|"), "OTHER server") {
		t.Errorf("gre dialer to 0.0.0.0 not rejected: %v", errs)
	}
	rawExit := []byte(`{"mode": "listen", "carrier": "dgtun", "encap": "gre", "addr": "0.0.0.0",
		"iface": "hs0", "local_cidr": "10.77.0.2/30", "peer_ip": "10.77.0.1", "mtu": 1280,
		"shared_key": "` + testKey + `", "expose": "127.0.0.1:8443"}`)
	if errs, warns := checkConfig(rawExit, local, time.Now()); len(errs)+len(warns) != 0 {
		t.Errorf("gre listener on a bare 0.0.0.0: errs=%v warns=%v", errs, warns)
	}
	// The icmp listener keeps normal ping working when nft or iptables exists (it
	// drops only the kernel's replies to the tunnel's packets); without either it
	// has to silence all ping on the server, and check says so.
	icmpExit := []byte(`{"mode": "listen", "carrier": "dgtun", "encap": "icmp", "addr": "0.0.0.0:2096",
		"iface": "hs0", "local_cidr": "10.77.0.2/30", "peer_ip": "10.77.0.1", "mtu": 1280,
		"shared_key": "` + testKey + `", "expose": "127.0.0.1:8443"}`)
	defer func(f func() bool) { haveEchoGuardTool = f }(haveEchoGuardTool)
	haveEchoGuardTool = func() bool { return false }
	if _, warns := checkConfig(icmpExit, local, time.Now()); !strings.Contains(strings.Join(warns, "|"), "ping") {
		t.Errorf("icmp without nft/iptables: ping warning missing: %v", warns)
	}
	haveEchoGuardTool = func() bool { return true }
	if errs, warns := checkConfig(icmpExit, local, time.Now()); len(errs)+len(warns) != 0 {
		t.Errorf("icmp with nft/iptables must pass clean: errs=%v warns=%v", errs, warns)
	}
	// The exit needs only the panel: the user ports live on the edge. No panel is
	// a valid pure routed tunnel but warns; a malformed one is an error; a panel on
	// the forwarder's own tun port warns; a legacy exit that still carries
	// forward_ports (older installers wrote it) passes clean.
	exit := func(extra string) []byte {
		return []byte(`{"mode": "listen", "carrier": "dgtun", "encap": "udp", "addr": "0.0.0.0:2096",
			"iface": "hs0", "local_cidr": "10.77.0.2/30", "peer_ip": "10.77.0.1", "mtu": 1280,
			"shared_key": "` + testKey + `"` + extra + `}`)
	}
	if errs, warns := checkConfig(exit(""), local, time.Now()); len(errs) != 0 || !strings.Contains(strings.Join(warns, "|"), `"expose" is empty`) {
		t.Errorf("exit without a panel: want only an expose warning, got errs=%v warns=%v", errs, warns)
	}
	if errs, _ := checkConfig(exit(`, "expose": "8443"`), local, time.Now()); !strings.Contains(strings.Join(errs, "|"), "expose") {
		t.Errorf("exit with a malformed panel not rejected: %v", errs)
	}
	if _, warns := checkConfig(exit(`, "expose": "127.0.0.1:28443"`), local, time.Now()); !strings.Contains(strings.Join(warns, "|"), "28443") {
		t.Errorf("panel on the forwarder's own tun port not warned: %v", warns)
	}
	for _, extra := range []string{`, "expose": "127.0.0.1:8443"`, `, "expose": "127.0.0.1:8443", "forward_ports": "8443"`} {
		if errs, warns := checkConfig(exit(extra), local, time.Now()); len(errs)+len(warns) != 0 {
			t.Errorf("exit %s: errs=%v warns=%v", extra, errs, warns)
		}
	}
}

// Pool bounds are checked for every pool role (the reverse exit runs its pool
// with them): negative or past the wire's u16 is an error; a very large count
// is a warning, never a block (an existing explicit number keeps working).
func TestCheckPoolBounds(t *testing.T) {
	base := `{"mode": "listen", "carrier": "mtcp", "reverse": true, "addr": "5.57.38.168:2082",
		"sni": "t.example", "shared_key": "` + testKey + `", "expose": "127.0.0.1:8443", "max_links": MAX}`
	local := localIs("5.57.38.168")
	for _, c := range []struct {
		max         string
		errs, warns bool
	}{{"300", false, false}, {"0", false, false}, {"2000", false, true}, {"70000", true, false}, {"-1", true, false}} {
		errs, warns := checkConfig([]byte(strings.Replace(base, "MAX", c.max, 1)), local, time.Now())
		if (len(errs) > 0) != c.errs || (len(warns) > 0) != c.warns {
			t.Errorf("max_links %s on a reverse exit: errs=%v warns=%v", c.max, errs, warns)
		}
	}
}
