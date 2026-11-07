package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

func serialOf(t *testing.T, c *tls.Certificate) int64 {
	t.Helper()
	if c == nil || c.Leaf == nil {
		t.Fatal("no certificate")
	}
	return c.Leaf.SerialNumber.Int64()
}

// helloOn builds a ClientHelloInfo whose Conn's local address is ip.
type fakeLocalConn struct {
	net.Conn
	local net.Addr
}

func (f fakeLocalConn) LocalAddr() net.Addr { return f.local }

func helloOn(ip, sni string) *tls.ClientHelloInfo {
	h := &tls.ClientHelloInfo{ServerName: sni}
	if ip != "" {
		h.Conn = fakeLocalConn{local: &net.TCPAddr{IP: net.ParseIP(ip), Port: 443}}
	}
	return h
}

func TestCertSelectorByLocalIPAndSNI(t *testing.T) {
	d := t.TempDir()
	mainC, mainK := filepath.Join(d, "m.pem"), filepath.Join(d, "mk.pem")
	bC, bK := filepath.Join(d, "b.pem"), filepath.Join(d, "bk.pem")
	writeCertTo(t, mainC, mainK, 1, time.Now().Add(60*24*time.Hour))
	writeCertTo(t, bC, bK, 2, time.Now().Add(60*24*time.Hour))
	js := `{"mode":"dial","reverse":true,"carrier":"mtcp","addr":"10.1.0.1:443","cert_file":"` + mainC + `","key_file":"` + mainK + `",
	 "lb":{"enabled":true,"iran":[{"ip":"10.1.0.1","weight":50},{"ip":"10.1.0.2","weight":50,"sni":"t2.example.com","cert_file":"` + bC + `","key_file":"` + bK + `"}],
	       "kharej":[{"ip":"10.0.0.1","weight":100}]}}`
	js = strings.ReplaceAll(js, `\`, `/`)
	fc := lbFC(t, js)
	s, err := newCertSelector(fc, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		ip, sni string
		want    int64
	}{
		{"10.1.0.1", "t1.example.com", 1},
		{"10.1.0.2", "", 2},
		{"10.1.0.2", "whatever", 2}, // local address wins
		{"::ffff:10.1.0.2", "", 2},  // v4-mapped
		{"10.9.9.9", "t2.example.com", 2},
		{"10.9.9.9", "T2.Example.com", 2},
		{"10.9.9.9", "other", 1},
		{"", "", 1},
	} {
		got, _ := s.getCertificate(helloOn(c.ip, c.sni))
		if serialOf(t, got) != c.want {
			t.Errorf("ip %q sni %q: serial %d, want %d", c.ip, c.sni, serialOf(t, got), c.want)
		}
	}

	// Renewal of the per-address cert: overwrite, then the SIGHUP path
	// (reloadAllCerts) and the mtime poll both pick it up.
	writeCertTo(t, bC, bK, 3, time.Now().Add(90*24*time.Hour))
	reloadAllCerts("test")
	if got, _ := s.getCertificate(helloOn("10.1.0.2", "")); serialOf(t, got) != 3 {
		t.Fatalf("per-address cert not reloaded: serial %d", serialOf(t, got))
	}
	r := s.byIP["10.1.0.2"]
	writeCertTo(t, bC, bK, 4, time.Now().Add(90*24*time.Hour))
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(bC, future, future); err != nil {
		t.Fatal(err)
	}
	if !r.changedOnDisk() {
		t.Fatal("mtime poll does not see the renewed per-address cert")
	}
	r.reload("file changed on disk")
	if got, _ := s.getCertificate(helloOn("10.1.0.2", "")); serialOf(t, got) != 4 {
		t.Fatalf("per-address cert not reloaded by the poll: serial %d", serialOf(t, got))
	}
	if got, _ := s.getCertificate(helloOn("10.1.0.1", "")); serialOf(t, got) != 1 {
		t.Fatalf("primary cert changed: serial %d", serialOf(t, got))
	}
}

func TestCertSelectorNoLBIsTopLevel(t *testing.T) {
	d := t.TempDir()
	c, k := filepath.Join(d, "c.pem"), filepath.Join(d, "k.pem")
	writeCertTo(t, c, k, 7, time.Now().Add(30*24*time.Hour))
	fc := fileConfig{Mode: "listen", CertFile: c, KeyFile: k}
	s, err := newCertSelector(fc, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []*tls.ClientHelloInfo{nil, helloOn("10.1.0.2", "x")} {
		if got, _ := s.getCertificate(h); serialOf(t, got) != 7 {
			t.Fatal("top-level cert not used")
		}
	}
}

func TestLBSNIForPath(t *testing.T) {
	js := strings.Replace(lbKharejRev, `{"ip":"10.1.0.2","weight":30}`, `{"ip":"10.1.0.2","weight":30,"sni":"t2.example.com"}`, 1)
	js = strings.Replace(js, `"carrier":"mtcp"`, `"carrier":"mtcp","sni":"t1.example.com"`, 1)
	fc := lbFC(t, js)
	if got := lbSNIFor(fc, "10.1.0.2"); got != "t2.example.com" {
		t.Fatalf("extra path sni %q", got)
	}
	if got := lbSNIFor(fc, "10.1.0.1"); got != "t1.example.com" {
		t.Fatalf("primary path sni %q", got)
	}
	fc.LB.Enabled = false
	if got := lbSNIFor(fc, "10.1.0.2"); got != "t1.example.com" {
		t.Fatalf("lb off: sni %q", got)
	}
}

func TestCoverPerAddress(t *testing.T) {
	seed := "00112233445566778899aabbccddeeff"
	now := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	main, _ := buildCover(coverSeedFor(seed, ""), now)
	ref, _ := buildCover(seed, now)
	if !bytes.Equal(main, ref) {
		t.Fatal("the primary address's page changed")
	}
	a, _ := buildCover(coverSeedFor(seed, "t2.example.com"), now)
	b, _ := buildCover(coverSeedFor(seed, "t3.example.com"), now)
	if bytes.Equal(a, main) || bytes.Equal(a, b) {
		t.Fatal("per-address pages are not distinct")
	}
	if coverSeedFor("", "t2.example.com") != "" {
		t.Fatal("a seedless install must keep the legacy page (no seed from a public name)")
	}

	js := `{"mode":"dial","reverse":true,"carrier":"mtcp","addr":"10.1.0.1:443","cover_seed":"` + seed + `",
	 "lb":{"enabled":true,"iran":[{"ip":"10.1.0.1","weight":50},{"ip":"10.1.0.2","weight":50,"sni":"t2.example.com"}],
	       "kharej":[{"ip":"10.0.0.1","weight":100}]}}`
	pick := coverBackends(lbFC(t, js), nil)
	if pick == nil {
		t.Fatal("no per-address cover")
	}
	if pick(&net.TCPAddr{IP: net.ParseIP("10.1.0.1")}, "") != "" {
		t.Fatal("primary address must use the main backend")
	}
	x := pick(&net.TCPAddr{IP: net.ParseIP("10.1.0.2")}, "")
	if x == "" || pick(&net.TCPAddr{IP: net.ParseIP("10.9.9.9")}, "t2.example.com") != x {
		t.Fatal("extra address / its sni not mapped to its own cover")
	}
	if coverBackends(lbFC(t, strings.Replace(js, `"cover_seed"`, `"backend_addr":"127.0.0.1:1","cover_seed"`, 1)), nil) != nil {
		t.Fatal("a custom backend_addr must not be replaced")
	}
}

func TestACMEConfig(t *testing.T) {
	for in, want := range map[string]string{"": "", "off": "", "on": acmeDefaultWebroot, "true": acmeDefaultWebroot, "/srv/acme/": "/srv/acme"} {
		if got := filepath.ToSlash(acmeWebrootOf(fileConfig{ACMEWebroot: in})); got != want && !(want == "/srv/acme" && strings.HasSuffix(got, "/srv/acme")) {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	p, err := acmePortsOf(fileConfig{})
	if err != nil || !p[80] || len(p) != 1 {
		t.Fatalf("default ports %v %v", p, err)
	}
	if _, err := acmePortsOf(fileConfig{ACMEPorts: "80,x"}); err == nil {
		t.Fatal("bad port accepted")
	}
	if acmeResponder(fileConfig{Mode: "dial", ForwardPorts: "80"}, t.Logf) != nil {
		t.Fatal("off by default")
	}
	a := acmeResponder(fileConfig{Mode: "dial", ForwardPorts: "80,443", ACMEWebroot: "on"}, t.Logf)
	if a == nil || !a.On(80) || a.On(443) {
		t.Fatalf("responder %+v", a)
	}
}

func TestCheckLB3Warnings(t *testing.T) {
	has := func(list []string, sub string) bool {
		for _, s := range list {
			if strings.Contains(s, sub) {
				return true
			}
		}
		return false
	}
	anyIP := func(net.IP) bool { return true }
	edge := `{"mode":"dial","carrier":"mtcp","addr":"10.0.0.1:443","sni":"t1.example.com","shared_key":"` + strings.Repeat("ab", 32) + `","forward_ports":"80,443"%s}`
	_, w := checkConfig([]byte(strings.Replace(edge, "%s", "", 1)), anyIP, time.Now())
	if !has(w, `"acme_webroot"`) {
		t.Fatalf("no acme advice for user port 80: %v", w)
	}
	_, w = checkConfig([]byte(strings.Replace(edge, "%s", `,"acme_webroot":"on"`, 1)), anyIP, time.Now())
	if has(w, "acme") {
		t.Fatalf("acme on still warned: %v", w)
	}
	_, w = checkConfig([]byte(strings.Replace(edge, "%s", `,"user_listen_ip":"10.0.0.9"`, 1)), anyIP, time.Now())
	if has(w, "acme_webroot") {
		t.Fatalf("port 80 on one address only: no acme advice expected: %v", w)
	}

	d := t.TempDir()
	mainC, mainK := filepath.Join(d, "m.pem"), filepath.Join(d, "mk.pem")
	bC, bK := filepath.Join(d, "b.pem"), filepath.Join(d, "bk.pem")
	writeCertTo(t, mainC, mainK, 1, time.Now().Add(60*24*time.Hour))
	writeCertTo(t, bC, bK, 2, time.Now().Add(5*24*time.Hour)) // names t.example only
	js := `{"mode":"dial","reverse":true,"carrier":"mtcp","addr":"10.1.0.1:443","cert_file":"` + mainC + `","key_file":"` + mainK + `",
	 "lb":{"enabled":true,"iran":[{"ip":"10.1.0.1","weight":50},{"ip":"10.1.0.2","weight":50,"sni":"t2.example.com","cert_file":"` + bC + `","key_file":"` + bK + `"}],
	       "kharej":[{"ip":"10.0.0.1","weight":100}]}}`
	issues, n := lbCertIssues(lbFC(t, strings.ReplaceAll(js, `\`, `/`)), time.Now())
	var msgs []string
	for _, is := range issues {
		msgs = append(msgs, is.ip+": "+is.msg)
	}
	if n != 1 || !has(msgs, "10.1.0.2: certificate") || !has(msgs, "expires in") || !has(msgs, "not covered") {
		t.Fatalf("issues %v (checked %d)", msgs, n)
	}
	ok := strings.Replace(js, `"sni":"t2.example.com"`, `"sni":"t.example"`, 1)
	writeCertTo(t, bC, bK, 3, time.Now().Add(60*24*time.Hour))
	if issues, _ := lbCertIssues(lbFC(t, strings.ReplaceAll(ok, `\`, `/`)), time.Now()); len(issues) != 0 {
		t.Fatalf("clean config flagged: %+v", issues)
	}
}

// End to end on two loopback addresses: a TLS client reaching 127.0.0.2 gets
// that address's own certificate and its own cover page; 127.0.0.1 gets the
// top-level ones. (Linux routes all of 127/8 to lo.)
func TestPerAddressCertAndCoverEndToEnd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs 127.0.0.2")
	}
	d := t.TempDir()
	mainC, mainK := filepath.Join(d, "m.pem"), filepath.Join(d, "mk.pem")
	bC, bK := filepath.Join(d, "b.pem"), filepath.Join(d, "bk.pem")
	writeCertTo(t, mainC, mainK, 11, time.Now().Add(60*24*time.Hour))
	writeCertTo(t, bC, bK, 12, time.Now().Add(60*24*time.Hour))
	seed := "ffeeddccbbaa99887766554433221100"
	js := `{"mode":"dial","reverse":true,"carrier":"mtcp","addr":"127.0.0.1:443","cover_seed":"` + seed + `","cert_file":"` + mainC + `","key_file":"` + mainK + `",
	 "lb":{"enabled":true,"iran":[{"ip":"127.0.0.1","weight":50},{"ip":"127.0.0.2","weight":50,"sni":"t2.example.com","cert_file":"` + bC + `","key_file":"` + bK + `"}],
	       "kharej":[{"ip":"10.0.0.1","weight":100}]}}`
	fc := lbFC(t, js)
	cs, err := newCertSelector(fc, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := &tlscarrier.Server{SharedKey: make([]byte, 32), GetCertificate: cs.getCertificate,
		BackendAddr: streamBackend(fc), BackendFor: coverBackends(fc, nil)}
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.Handle(context.Background(), c, func(*tlscarrier.Carrier) {})
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	get := func(ip, sni string) (int64, string) {
		c, err := tls.Dial("tcp", net.JoinHostPort(ip, port), &tls.Config{ServerName: sni, InsecureSkipVerify: true})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		serial := c.ConnectionState().PeerCertificates[0].SerialNumber.Int64()
		io.WriteString(c, "GET / HTTP/1.1\r\nHost: "+sni+"\r\nConnection: close\r\n\r\n")
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		b, _ := io.ReadAll(c)
		return serial, string(b)
	}
	s1, p1 := get("127.0.0.1", "t1.example.com")
	s2, p2 := get("127.0.0.2", "t2.example.com")
	if s1 != 11 || s2 != 12 {
		t.Fatalf("certs by address: %d, %d", s1, s2)
	}
	main, _ := buildCover(seed, time.Now())
	if !strings.Contains(p1, string(main[:200])) || strings.Contains(p2, string(main[:200])) || !strings.Contains(p2, "<!doctype html>") {
		t.Fatalf("cover pages not per address:\n%.300s\n---\n%.300s", p1, p2)
	}
}
