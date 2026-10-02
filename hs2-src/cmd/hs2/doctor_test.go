package main

import (
	"net"
	"strings"
	"testing"
	"time"
)

func TestNormalizeWS(t *testing.T) {
	cases := map[string]string{
		"bbr\n":                 "bbr",
		"4096\t131072\t1048576": "4096 131072 1048576",
		"  4096   131072  ":     "4096 131072",
		"":                      "",
		"\t\n ":                 "",
	}
	for in, want := range cases {
		if got := normalizeWS(in); got != want {
			t.Errorf("normalizeWS(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAddrsContainCIDR(t *testing.T) {
	got := []string{"10.77.0.1/30", "fe80::1/64"}
	if !addrsContainCIDR(got, "10.77.0.1/30") {
		t.Error("expected 10.77.0.1/30 to be found")
	}
	if addrsContainCIDR(got, "10.77.0.2/30") {
		t.Error("10.77.0.2 is not on the interface; should not match")
	}
	// an unparseable config value must not raise a false alarm (returns true)
	if !addrsContainCIDR(got, "not-a-cidr") {
		t.Error("unparseable cidr should return true (no false alarm)")
	}
	// a bare-IP interface address should still match the configured CIDR's IP
	if !addrsContainCIDR([]string{"10.77.0.1"}, "10.77.0.1/30") {
		t.Error("bare-IP interface address should match the CIDR's IP")
	}
}

func TestDoctorReportTally(t *testing.T) {
	d := &doctorReport{}
	d.ok("a", "x")
	d.info("b", "x")
	d.warn("c", "x")
	d.fail("d", "x")
	d.warn("e", "x")
	if d.fails != 1 {
		t.Errorf("fails = %d, want 1", d.fails)
	}
	if d.warns != 2 {
		t.Errorf("warns = %d, want 2", d.warns)
	}
	if len(d.lines) != 5 {
		t.Errorf("lines = %d, want 5", len(d.lines))
	}
}

func TestCheckCert(t *testing.T) {
	cases := []struct {
		name     string
		notAfter time.Time
		wantFail int
		wantWarn int
	}{
		{"valid", time.Now().Add(400 * 24 * time.Hour), 0, 0},
		{"soon", time.Now().Add(4 * 24 * time.Hour), 0, 1},
		{"expired", time.Now().Add(-time.Hour), 1, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cert, key := writeCert(t, c.notAfter)
			d := &doctorReport{}
			// TLS-server side (listener of a TLS carrier) — the cert check runs.
			checkCert(d, fileConfig{Mode: "listen", Carrier: "tls", CertFile: cert, KeyFile: key})
			if d.fails != c.wantFail || d.warns != c.wantWarn {
				t.Errorf("%s: fails=%d warns=%d, want fails=%d warns=%d (line: %q)",
					c.name, d.fails, d.warns, c.wantFail, c.wantWarn, strings.Join(d.lines, " | "))
			}
		})
	}
	// no cert configured -> no check emitted at all
	d := &doctorReport{}
	checkCert(d, fileConfig{Mode: "listen", Carrier: "tls"})
	if len(d.lines) != 0 {
		t.Errorf("no cert_file should emit nothing, got %v", d.lines)
	}
	// a cert_file that does not exist, on the server side -> FAIL (not a silent skip)
	d = &doctorReport{}
	checkCert(d, fileConfig{Mode: "listen", Carrier: "tls", CertFile: "/no/such/cert.pem", KeyFile: "/no/such/key.pem"})
	if d.fails != 1 {
		t.Errorf("missing cert file should FAIL, got %v", d.lines)
	}
	// gating: an EXPIRED cert on the DIALER side must be SKIPPED, not FAIL — the
	// tunnel never uses it there (Finding 2).
	cert, key := writeCert(t, time.Now().Add(-time.Hour))
	d = &doctorReport{}
	checkCert(d, fileConfig{Mode: "dial", Carrier: "tls", CertFile: cert, KeyFile: key})
	if len(d.lines) != 0 {
		t.Errorf("expired cert on the dialer side should be skipped, got %v", d.lines)
	}
	// gating: a cert on a non-TLS carrier (even on the listen side) is skipped.
	d = &doctorReport{}
	checkCert(d, fileConfig{Mode: "listen", Carrier: "dgtun", CertFile: cert, KeyFile: key})
	if len(d.lines) != 0 {
		t.Errorf("cert on a non-TLS carrier should be skipped, got %v", d.lines)
	}
	// reverse edge (mode=dial + reverse) LISTENS, so it legitimately bears the
	// cert and the check must run (expired -> FAIL).
	d = &doctorReport{}
	checkCert(d, fileConfig{Mode: "dial", Reverse: true, Carrier: "tls", CertFile: cert, KeyFile: key})
	if d.fails != 1 {
		t.Errorf("reverse edge is the TLS server; expired cert should FAIL, got %v", d.lines)
	}
}

func TestCheckEndpointGating(t *testing.T) {
	// a reachable TCP listener -> dialer on a TCP carrier passes
	ln, err := net.Listen("tcp", "127.0.0.1:0")
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
			c.Close()
		}
	}()
	addr := ln.Addr().String()

	d := &doctorReport{}
	checkEndpoint(d, fileConfig{Mode: "dial", Carrier: "tls", Addr: addr})
	if d.fails != 0 || len(d.lines) != 1 {
		t.Errorf("reachable tls dialer should pass with one line, got fails=%d lines=%v", d.fails, d.lines)
	}

	// a datagram carrier must be skipped (no TCP meaning), even when dialing
	d = &doctorReport{}
	checkEndpoint(d, fileConfig{Mode: "dial", Carrier: "dgtun", Addr: addr})
	if len(d.lines) != 0 {
		t.Errorf("dgtun endpoint should be skipped, got %v", d.lines)
	}

	// the listener side is skipped (Addr is a local bind, liveness is the running check)
	d = &doctorReport{}
	checkEndpoint(d, fileConfig{Mode: "listen", Carrier: "tls", Addr: addr})
	if len(d.lines) != 0 {
		t.Errorf("listener endpoint should be skipped, got %v", d.lines)
	}

	// an unreachable dialer endpoint -> FAIL
	d = &doctorReport{}
	checkEndpoint(d, fileConfig{Mode: "dial", Carrier: "mtcp", Addr: "127.0.0.1:1"})
	if d.fails != 1 {
		t.Errorf("unreachable endpoint should FAIL, got %v", d.lines)
	}
}
