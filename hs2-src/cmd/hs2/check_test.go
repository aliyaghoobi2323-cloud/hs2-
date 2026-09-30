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
		"kharej direct tun": `{"mode": "listen", "carrier": "l3mtcp", "reverse": false, "addr": "91.107.166.13:2096",
			"iface": "hs0", "local_cidr": "10.77.0.2/30", "peer_ip": "10.77.0.1", "mtu": 1320,
			"backend_addr": "builtin", "shared_key": "` + testKey + `", ` + cert + `}`,
		"iran direct tun": `{"mode": "dial", "carrier": "l3mtcp", "reverse": false, "addr": "91.107.166.13:2096",
			"sni": "t.example", "iface": "tun9", "local_cidr": "10.77.0.1/30", "peer_ip": "10.77.0.2", "mtu": 1320,
			"shared_key": "` + testKey + `", "min_links": 8, "max_links": 16, "per_link": 8, "bind_local_ip": ""}`,
		"kharej udp": `{"mode": "listen", "carrier": "udp", "reverse": false, "addr": "0.0.0.0:2096",
			"iface": "hs0", "local_cidr": "10.77.0.2/30", "peer_ip": "10.77.0.1", "mtu": 1280, "shared_key": "` + testKey + `"}`,
		"iran auto": `{"mode": "dial", "carrier": "auto", "reverse": false, "addr": "91.107.166.13:2096",
			"iface": "hs0", "local_cidr": "10.77.0.1/30", "peer_ip": "10.77.0.2", "mtu": 1280,
			"shared_key": "` + testKey + `", "bind_local_ip": "5.57.38.168"}`,
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
