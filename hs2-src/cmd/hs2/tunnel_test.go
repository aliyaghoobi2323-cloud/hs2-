package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeHost: no tunnels, every IP local, nothing listening, peer reachable.
func fakeHost(t *testing.T) tunHostEnv {
	t.Helper()
	d := menuEnv(t)
	_ = d
	return tunHostEnv{
		etcDir: hs2CfgDir,
		validate: validateHost{
			localIP:   func(net.IP) bool { return true },
			listening: func() []sockListen { return nil },
			otherCfgs: func(self string) map[string][]byte {
				out := map[string][]byte{}
				ms, _ := filepath.Glob(filepath.Join(hs2CfgDir, "*.json"))
				for _, m := range ms {
					if m != self {
						b, _ := os.ReadFile(m)
						out[m] = b
					}
				}
				return out
			},
		},
		ifaceUsed: func(string) bool { return false },
		ping:      func(src, dst string) (float64, bool) { return 12, true },
		dial:      func(string) error { return nil },
		lookup:    func(string) ([]string, error) { return []string{"203.0.113.5"}, nil },
		exists:    func(string) bool { return true },
		sysctl: func(k string) string {
			if k == "net.core.rmem_max" {
				return "33554432"
			}
			return "bbr"
		},
		unitFile:  func(string) bool { return false },
		publicIPs: func() []string { return []string{"203.0.113.5"} },
	}
}

func kharejSpec() tunnelSpec {
	return tunnelSpec{Role: "kharej", PublicIP: "203.0.113.5", Port: 8443, Cert: "self", Expose: "127.0.0.1:2083", Name: "hs2-a"}
}

func cfgOf(t *testing.T, b []byte) fileConfig {
	t.Helper()
	var fc fileConfig
	if err := json.Unmarshal(b, &fc); err != nil {
		t.Fatal(err)
	}
	return fc
}

func TestCodeRoundTripAndSecrets(t *testing.T) {
	h := fakeHost(t)
	s := kharejSpec()
	if err := normalize(&s, h); err != nil {
		t.Fatal(err)
	}
	code, err := encodeCode(codeFor(s))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(code, "hs2://v2.") {
		t.Fatalf("prefix: %s", code)
	}
	c, err := decodeCode("  " + code + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.V != 2 || c.SharedKey != s.SharedKey || c.Endpoint != "203.0.113.5:8443" || c.Role != "kharej" || c.Subnet != s.Subnet {
		t.Fatalf("decoded %+v", c)
	}
	if len(c.Secrets) != 1 || c.Secrets[0] != "shared_key" {
		t.Fatalf("secrets %v", c.Secrets)
	}
	if m := c.masked(); strings.Contains(m, s.SharedKey) || !strings.Contains(m, "(secret)") {
		t.Fatalf("masked leaks: %s", m)
	}
	if sc := shortCode(c); strings.Contains(sc, s.SharedKey) || !strings.Contains(sc, "hs2-a kharej→iran") {
		t.Fatalf("short %q", sc)
	}
}

func TestCodeVersioning(t *testing.T) {
	newer := "hs2://v3." + base64.RawURLEncoding.EncodeToString([]byte(`{"v":3}`))
	if _, err := decodeCode(newer); !errors.Is(err, errCodeNewer) || !strings.Contains(err.Error(), "upgrade") {
		t.Fatalf("newer: %v", err)
	}
	if _, err := decodeCode("hs2://v2.!!!"); err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Fatalf("damaged: %v", err)
	}
	mism := "hs2://v2." + base64.RawURLEncoding.EncodeToString([]byte(`{"v":1}`))
	if _, err := decodeCode(mism); err == nil {
		t.Fatal("version mismatch accepted")
	}
	// a classic v1 installer link (base64 of 13 '|' fields, reverse: iran made it)
	key := strings.Repeat("ab", 32)
	v1 := "hs2://" + base64.StdEncoding.EncodeToString([]byte("198.51.100.7:443|ex.com|"+key+"|127.0.0.1:443|mtcp|false|tcp|reverse|1380|||hs2-old|10.77.9.4"))
	c, err := decodeCode(v1)
	if err != nil {
		t.Fatal(err)
	}
	if c.V != 1 || c.Role != "iran" || c.Direction != "reverse" || c.SNI != "ex.com" || c.Name != "hs2-old" || c.Subnet != "10.77.9.4" {
		t.Fatalf("v1 %+v", c)
	}
	// a v1 udp link: refused with a pointer to the classic menu
	v1u := base64.StdEncoding.EncodeToString([]byte("198.51.100.7:443|-|" + key + "|-|udp|false|udp|direct"))
	if _, err := decodeCode(v1u); err == nil || !strings.Contains(err.Error(), "classic") {
		t.Fatalf("v1 udp: %v", err)
	}
	if _, err := decodeCode("hs2://garbage"); err == nil {
		t.Fatal("garbage accepted")
	}
	// encode refuses a code that could not be joined
	if _, err := encodeCode(tunnelCode{Role: "kharej", Direction: "direct", Carrier: "mtcp", Endpoint: "x", SharedKey: key, Subnet: "10.77.1.0", Name: "hs2"}); err == nil {
		t.Fatal("bad endpoint encoded")
	}
}

// The joiner's config mirrors the creator's, for every direction/role and IPv6.
func TestJoinMirrorsCreator(t *testing.T) {
	cases := []tunnelSpec{
		kharejSpec(),
		{Role: "iran", Direction: "reverse", PublicIP: "203.0.113.9", Port: 9443, Cert: "self", UserPorts: "443,8443", Name: "hs2-r"},
		{Role: "iran", Direction: "direct", Peer: "2001:db8::7", Port: 443, UserPorts: "443", Domain: "ex.com", Name: "hs2-6"},
		{Role: "kharej", Carrier: "dgtun", Encap: "icmp", PublicIP: "203.0.113.5", Port: 1, Name: "hs2-d"},
	}
	for _, s := range cases {
		h := fakeHost(t)
		if err := normalize(&s, h); err != nil {
			t.Fatalf("%s: %v", s.Name, err)
		}
		a, err := buildConfig(s, "00")
		if err != nil {
			t.Fatal(err)
		}
		code, err := encodeCode(codeFor(s))
		if err != nil {
			t.Fatalf("%s: %v", s.Name, err)
		}
		c, _ := decodeCode(code)
		j := specFromCode(c, h)
		if j.Role == "kharej" && j.Expose == "" {
			j.Expose = "127.0.0.1:443"
		}
		if j.Role == "iran" && j.UserPorts == "" {
			j.UserPorts = "443"
		}
		if j.listener() && j.Cert == "" {
			j.Cert = "self"
		}
		if err := normalize(&j, h); err != nil {
			t.Fatalf("%s join: %v", s.Name, err)
		}
		b, _ := buildConfig(j, "00")
		fa, fb := cfgOf(t, a), cfgOf(t, b)
		if fa.SharedKey != fb.SharedKey || fa.Carrier != fb.Carrier || fa.Reverse != fb.Reverse || fa.MTU != fb.MTU || fa.Encap != fb.Encap {
			t.Fatalf("%s: shared fields differ\n%s\n%s", s.Name, a, b)
		}
		if fa.Mode == fb.Mode {
			t.Fatalf("%s: both sides mode %s", s.Name, fa.Mode)
		}
		if fa.PeerIP+"/30" != fb.LocalCIDR || fb.PeerIP+"/30" != fa.LocalCIDR {
			t.Fatalf("%s: tun addresses not mirrored: %s/%s vs %s/%s", s.Name, fa.LocalCIDR, fa.PeerIP, fb.LocalCIDR, fb.PeerIP)
		}
		// exactly one side listens on a wildcard/bind, the other dials the endpoint
		_, pa, _ := net.SplitHostPort(fa.Addr)
		_, pb, _ := net.SplitHostPort(fb.Addr)
		if pa != pb {
			t.Fatalf("%s: ports %s vs %s", s.Name, fa.Addr, fb.Addr)
		}
		if s.Name == "hs2-6" && fa.Addr != "[2001:db8::7]:443" {
			t.Fatalf("ipv6 addr %s", fa.Addr)
		}
		if s.Name == "hs2-r" && (fa.Mode != "dial" || !fa.Reverse || fa.CertFile == "" || fb.Addr != "203.0.113.9:9443") {
			t.Fatalf("reverse: iran must listen with the cert: %s / %s", a, b)
		}
		if errs, _ := checkConfig(b, func(net.IP) bool { return true }, time.Now()); len(filterCert(errs)) > 0 {
			t.Fatalf("%s joined config invalid: %v\n%s", s.Name, errs, b)
		}
	}
}

func filterCert(errs []string) (out []string) {
	for _, e := range errs {
		if !selfCertPending(e) {
			out = append(out, e)
		}
	}
	return
}

func TestNormalizeRejectsAndDefaults(t *testing.T) {
	h := fakeHost(t)
	bad := []tunnelSpec{
		{Role: "x", Port: 1},
		{Role: "kharej", Port: 0},
		{Role: "kharej", Port: 1, Front: "reality"},
		{Role: "iran", Port: 1}, // dialer without a peer
		{Role: "kharej", Port: 1, Carrier: "foo"},
		{Role: "iran", Port: 1, Peer: "1.2.3.4", UserPorts: "99999"},
		{Role: "kharej", Port: 1, Name: "nginx"},
		{Role: "kharej", Port: 1, Subnet: "10.77.1.3"},
	}
	for i, s := range bad {
		if err := normalize(&s, h); err == nil {
			t.Fatalf("case %d accepted: %+v", i, s)
		}
	}
	s := tunnelSpec{Role: "kharej", Port: 443, Carrier: "tun+mtcp"}
	if err := normalize(&s, h); err != nil {
		t.Fatal(err)
	}
	if s.Carrier != "l3mtcp" || s.Name != "hs2" || s.Iface != "hs0" || len(s.SharedKey) != 64 || s.MTU != 1380 || s.Expose != "127.0.0.1:443" || s.Cert != "self" {
		t.Fatalf("defaults %+v", s)
	}
}

// Multiple tunnels: names, ifaces, subnets and ports never collide.
func TestMultiTunnelFreeNamesAndPreflight(t *testing.T) {
	h := fakeHost(t)
	s := kharejSpec()
	s.Name = ""
	normalize(&s, h)
	b, _ := buildConfig(s, "00")
	os.WriteFile(configForUnit(s.Name), b, 0o600)
	s2 := kharejSpec()
	s2.Name = ""
	if err := normalize(&s2, h); err != nil {
		t.Fatal(err)
	}
	if s2.Name == s.Name || s2.Iface == s.Iface || s2.Subnet == s.Subnet {
		t.Fatalf("collision: %s/%s/%s vs %s/%s/%s", s.Name, s.Iface, s.Subnet, s2.Name, s2.Iface, s2.Subnet)
	}
	b2, _ := buildConfig(s2, "00") // same port 8443
	pc := preflight(s2, b2, configForUnit(s2.Name), h)
	if !preflightFailed(pc) || !strings.Contains(fmtChecks(pc), "8443") {
		t.Fatalf("port conflict not caught: %s", fmtChecks(pc))
	}
}

func fmtChecks(cs []preCheck) string {
	var b strings.Builder
	for _, c := range cs {
		b.WriteString(c.Status + " " + c.Name + " " + c.Detail + "\n")
	}
	return b.String()
}

func TestPreflightFixtures(t *testing.T) {
	h := fakeHost(t)
	// listener with a domain pointing elsewhere and no cert
	s := kharejSpec()
	s.Domain, s.Cert, s.Key = "ex.com", "/nope.pem", "/nope.key"
	normalize(&s, h)
	h.exists = func(string) bool { return false }
	h.lookup = func(string) ([]string, error) { return []string{"198.51.100.1"}, nil }
	b, _ := buildConfig(s, "00")
	out := fmtChecks(preflight(s, b, configForUnit(s.Name), h))
	if !strings.Contains(out, "warn domain DNS ex.com points to 198.51.100.1") || !strings.Contains(out, "fail certificate") {
		t.Fatalf("%s", out)
	}
	// the listener keeps its domain as its tls name ("both" needs it)
	if fc := cfgOf(t, b); fc.SNI != "ex.com" || !strings.HasPrefix(fc.Addr, "0.0.0.0:") || len(tlsNames(fc)) != 1 {
		t.Fatalf("listener sni %q addr %q", fc.SNI, fc.Addr)
	}
	// no domain: a warning that suggests ACME DNS-01 / Reality, not a failure
	s.Domain, s.Cert = "", "self"
	b, _ = buildConfig(s, "00")
	if fc := cfgOf(t, b); fc.SNI != "" {
		t.Fatalf("sni without a domain: %q", fc.SNI)
	}
	pc := preflight(s, b, configForUnit(s.Name), h)
	if preflightFailed(pc) || !strings.Contains(fmtChecks(pc), "DNS-01") {
		t.Fatalf("%s", fmtChecks(pc))
	}
	// dialer: peer not answering ping, port closed, kernel untuned -> warnings only
	d := tunnelSpec{Role: "iran", Peer: "198.51.100.7", Port: 443, UserPorts: "8443", Name: "hs2-i"}
	normalize(&d, h)
	h.ping = func(string, string) (float64, bool) { return 0, false }
	h.dial = func(string) error { return errors.New("connection refused") }
	h.sysctl = func(string) string { return "0" }
	b, _ = buildConfig(d, "00")
	pc = preflight(d, b, configForUnit(d.Name), h)
	out = fmtChecks(pc)
	if preflightFailed(pc) || !strings.Contains(out, "warn peer ping") || !strings.Contains(out, "warn peer port") || !strings.Contains(out, "warn kernel tuning") {
		t.Fatalf("%s", out)
	}
	// a local IP that does not exist
	h.validate.localIP = func(net.IP) bool { return false }
	d.BindIP = "192.0.2.50"
	b, _ = buildConfig(d, "00")
	if pc := preflight(d, b, configForUnit(d.Name), h); !preflightFailed(pc) {
		t.Fatalf("missing local IP not caught: %s", fmtChecks(pc))
	}
}

type fakeCreator struct {
	installed, removed []string
	ok                 bool
}

func (f *fakeCreator) install(s tunnelSpec, p string, b []byte) error {
	f.installed = append(f.installed, s.Name)
	return os.WriteFile(p, b, 0o600)
}
func (f *fakeCreator) healthy(string) bool { return f.ok }
func (f *fakeCreator) remove(u, p string) {
	f.removed = append(f.removed, u)
	os.Remove(p)
}
func (f *fakeCreator) stats(bool) {}

func TestCreateNonInteractive(t *testing.T) {
	h := fakeHost(t)
	certDir = t.TempDir()
	s := kharejSpec()
	// dry run: nothing written
	f := &fakeCreator{ok: true}
	if rc := runCreate(s, createOpts{dryRun: true}, h, f); rc != 0 || len(f.installed) != 0 {
		t.Fatalf("dry run rc=%d %v", rc, f.installed)
	}
	if _, err := os.Stat(configForUnit("hs2-a")); err == nil {
		t.Fatal("dry run wrote the config")
	}
	// without --yes, non-interactive does not create
	if rc := runCreate(s, createOpts{}, h, f); rc != 1 || len(f.installed) != 0 {
		t.Fatalf("no --yes rc=%d", rc)
	}
	// unhealthy: created, then removed again (nothing left behind)
	f = &fakeCreator{ok: false}
	if rc := runCreate(s, createOpts{yes: true}, h, f); rc != 1 || len(f.removed) != 1 {
		t.Fatalf("unhealthy rc=%d removed=%v", rc, f.removed)
	}
	if _, err := os.Stat(configForUnit("hs2-a")); err == nil {
		t.Fatal("unhealthy create left the config")
	}
	// healthy: config + a history version
	f = &fakeCreator{ok: true}
	if rc := runCreate(s, createOpts{yes: true}, h, f); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	if vs, _ := listVersions("hs2-a"); len(vs) != 1 {
		t.Fatalf("versions %v", vs)
	}
	if _, err := os.Stat(filepath.Join(certDir, "hs2-a.crt")); err != nil {
		t.Fatal("self-signed cert not generated")
	}
}

// The wizard aborts on q / EOF at any step, and its answers build a spec.
func TestWizardAbortAndAnswers(t *testing.T) {
	h := fakeHost(t)
	for _, in := range []string{"", "1\n", "1\n2\nq\n", "2\n1\n1\n1\n\n"} {
		var s tunnelSpec
		var sb strings.Builder
		err := wizard(asker{bufio.NewReader(strings.NewReader(in)), &sb}, &s, false, false, h)
		if !errors.Is(err, errAbort) {
			t.Fatalf("input %q: %v", in, err)
		}
	}
	// iran, reverse, tun+mtcp, TLS (Reality first: refused, then TLS), public IP default, port, user ports, domain, cert self, charts yes
	in := "2\n2\n2\n2\n\n9443\n443,8443\n\nself\ny\n"
	var s tunnelSpec
	var sb strings.Builder
	if err := wizard(asker{bufio.NewReader(strings.NewReader(in)), &sb}, &s, false, false, h); err != nil {
		t.Fatalf("%v\n%s", err, sb.String())
	}
	if s.Role != "iran" || s.Direction != "reverse" || s.Carrier != "l3mtcp" || s.PublicIP != "203.0.113.5" || s.Port != 9443 || s.UserPorts != "443,8443" || s.Cert != "self" || !s.Stats {
		t.Fatalf("%+v", s)
	}
	if s.Front != "tls" || !strings.Contains(sb.String(), "Security & camouflage") || strings.Contains(sb.String(), "coming") {
		t.Fatal("reality: create with tls + the per-IP hint")
	}
}

func TestSSHRidesTunnel(t *testing.T) {
	fc := fileConfig{ForwardPorts: "443,2222", Iface: "hs3"}
	if w := sshRidesTunnel("198.51.100.1 5555 10.0.0.1 2222", fc, nil); !strings.Contains(w, "2222") {
		t.Fatalf("port: %q", w)
	}
	if w := sshRidesTunnel("10.77.1.1 5555 10.0.0.1 22", fc, func(string) string { return "hs3" }); !strings.Contains(w, "hs3") {
		t.Fatalf("route: %q", w)
	}
	if w := sshRidesTunnel("198.51.100.1 5555 10.0.0.1 22", fc, func(string) string { return "eth0" }); w != "" {
		t.Fatalf("false alarm: %q", w)
	}
	if sshRidesTunnel("", fc, nil) != "" {
		t.Fatal("no ssh")
	}
}

func TestBestLocalIP(t *testing.T) {
	rtt := map[string]float64{"203.0.113.1": 40, "203.0.113.2": 22, "203.0.113.3": 22.5}
	ping := func(src, dst string) (float64, bool) { v, ok := rtt[src]; return v, ok }
	ips := []string{"203.0.113.1", "203.0.113.2", "203.0.113.3", "203.0.113.4", "2001:db8::1"}
	if b, lines := bestLocalIP("198.51.100.7", ips, ping, "203.0.113.1"); b != "203.0.113.2" || len(lines) != 4 {
		t.Fatalf("%s %v", b, lines)
	}
	if b, _ := bestLocalIP("198.51.100.7", ips, ping, "203.0.113.3"); b != "203.0.113.3" {
		t.Fatalf("within 1 ms should keep current, got %s", b)
	}
}

func TestTunnelListRender(t *testing.T) {
	h := fakeHost(t)
	if !strings.Contains(renderTunnelList(nil), "hs2 tunnel create") {
		t.Fatal("empty list hint")
	}
	s := kharejSpec()
	normalize(&s, h)
	b, _ := buildConfig(s, "00")
	os.WriteFile(configForUnit(s.Name), b, 0o600)
	rows := tunnelRows(hs2CfgDir, func(string) (string, int) { return "active", 0 })
	out := renderTunnelList(rows)
	if len(rows) != 1 || !strings.Contains(out, "hs2-a") || !strings.Contains(out, "active") || !strings.Contains(out, "→ 127.0.0.1:2083") {
		t.Fatalf("%s", out)
	}
}

// M7: both sides default to the name "hs2", so joining the other server's code
// on a server that already runs a tunnel failed at the very last step.
func TestJoinFreeName(t *testing.T) {
	h := fakeHost(t)
	h.unitFile = func(n string) bool { return n == "hs2" }
	s := tunnelSpec{Name: "hs2"}
	if old := joinFreeName(&s, h); old != "hs2" || s.Name == "hs2" || s.Name == "" {
		t.Fatalf("%q %q", old, s.Name)
	}
	s = tunnelSpec{Name: "edge"}
	if old := joinFreeName(&s, h); old != "" || s.Name != "edge" {
		t.Fatalf("%q %q", old, s.Name)
	}
}

// M7: a non-interactive join on the listening side with no domain got
// cert "auto" = /etc/letsencrypt/live//fullchain.pem and failed hs2 check.
func TestNormalizeListenerNoDomainSelfCert(t *testing.T) {
	h := fakeHost(t)
	s := tunnelSpec{Role: "iran", Direction: "reverse", Carrier: "l3mtcp", Port: 9443, UserPorts: "8443", PublicIP: "203.0.113.5"}
	if err := normalize(&s, h); err != nil {
		t.Fatal(err)
	}
	if s.Cert != "self" {
		t.Fatalf("cert %q", s.Cert)
	}
	s = tunnelSpec{Role: "iran", Direction: "reverse", Carrier: "l3mtcp", Port: 9443, UserPorts: "8443", PublicIP: "203.0.113.5", Domain: "a.example.com"}
	if err := normalize(&s, h); err != nil || s.Cert != "auto" {
		t.Fatalf("cert %q %v", s.Cert, err)
	}
}
