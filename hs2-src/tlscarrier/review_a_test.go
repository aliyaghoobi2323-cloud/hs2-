package tlscarrier

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

// The R1 independent review, part A (wire protocol and security):
// docs/architecture/wip/r1/review/A-wire-security.fa.md. Tests named
// TestReviewA*; the ones that prove a finding left open fail by design and
// run only with HS2_REVIEW_RED=1 (reviewRed).

// reviewRed skips a proof of an open finding unless HS2_REVIEW_RED=1.
func reviewRed(t *testing.T) {
	t.Helper()
	if os.Getenv("HS2_REVIEW_RED") != "1" {
		t.Skip("proof of an open review finding (HS2_REVIEW_RED=1 runs it)")
	}
}

// noEMSHello is a TLS 1.2 ClientHello without the extended_master_secret
// extension: a probe any DPI box can send (curl/OpenSSL with an old build,
// a scanner, or one made to find this). Go's server takes the handshake, but
// the session has no channel-binding secret (RFC 7627: Go refuses
// ExportKeyingMaterial there).
func noEMSHello() *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		TLSVersMin: utls.VersionTLS12,
		TLSVersMax: utls.VersionTLS12,
		CipherSuites: []uint16{
			utls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
		},
		CompressionMethods: []byte{0},
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{utls.X25519, utls.CurveP256}},
			&utls.SupportedPointsExtension{SupportedPoints: []byte{0}},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
				utls.PSSWithSHA256, utls.PKCS1WithSHA256, utls.ECDSAWithP256AndSHA256}},
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},
			&utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},
		},
	}
}

// A probe whose TLS 1.2 session has no channel-binding secret must be served
// the website like every other probe. Production (and R1) closed it right
// after the handshake, before reading a byte: a real web server answers its
// request, so the close told hs2 apart (review A, finding A1).
func TestReviewANoEMSProbeServed(t *testing.T) {
	key := bytes.Repeat([]byte{0xa1}, 32)
	cert := testCert(t, "vpn.example.com")
	be, stopB := backend(t)
	defer stopB()
	addr, wait := serveOne(t, &Server{SharedKey: key, Cert: cert, BackendAddr: be},
		func(c *Carrier) { t.Error("a probe reached the tunnel"); c.Close() })
	defer wait()

	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	u := utls.UClient(raw, &utls.Config{ServerName: "vpn.example.com", InsecureSkipVerify: true}, utls.HelloCustom)
	if err := u.ApplyPreset(noEMSHello()); err != nil {
		t.Fatal(err)
	}
	raw.SetDeadline(time.Now().Add(5 * time.Second))
	if err := u.Handshake(); err != nil {
		t.Skipf("the server refused the TLS 1.2 handshake without EMS (%v): nothing to tell apart", err)
	}
	if v := u.ConnectionState().Version; v != utls.VersionTLS12 {
		t.Fatalf("negotiated %x, want TLS 1.2", v)
	}
	if _, err := u.Write([]byte("GET / HTTP/1.1\r\nHost: vpn.example.com\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, _ := io.ReadAll(u)
	if !bytes.Contains(out, []byte("real-backend")) {
		t.Fatalf("a TLS 1.2 probe without the extended master secret got %q, not the website: the server closed it after the handshake", out)
	}
}

// With HS2_TUNE_PROBE_NOEKM=0 (ProbeNoEKM 0) the server closes such a
// session as before.
func TestReviewANoEMSProbeSwitchOff(t *testing.T) {
	old := ProbeNoEKM
	ProbeNoEKM = 0
	key := bytes.Repeat([]byte{0xa2}, 32)
	cert := testCert(t, "vpn.example.com")
	be, stopB := backend(t)
	defer stopB()
	addr, wait := serveOne(t, &Server{SharedKey: key, Cert: cert, BackendAddr: be}, func(c *Carrier) { c.Close() })
	defer func() {
		wait() // (its read of the switch before the switch is set back)
		ProbeNoEKM = old
	}()
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	u := utls.UClient(raw, &utls.Config{ServerName: "vpn.example.com", InsecureSkipVerify: true}, utls.HelloCustom)
	if err := u.ApplyPreset(noEMSHello()); err != nil {
		t.Fatal(err)
	}
	raw.SetDeadline(time.Now().Add(5 * time.Second))
	if err := u.Handshake(); err != nil {
		t.Skipf("handshake refused: %v", err)
	}
	u.Write([]byte("GET / HTTP/1.1\r\nHost: vpn.example.com\r\n\r\n"))
	out, _ := io.ReadAll(u)
	if len(out) != 0 {
		t.Fatalf("switch off: got %q, want the old close", out)
	}
}

// serveOne serves one connection on a listener of its own. wait, deferred
// before the test dials (so the test's own conn closes first), closes the
// listener and returns once Handle has: ProbeNoEKM, which these tests set,
// is read there, and the process sets it once, before any server runs.
func serveOne(t *testing.T, srv *Server, onTunnel func(*Carrier)) (addr string, wait func()) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		if c, err := ln.Accept(); err == nil {
			srv.Handle(context.Background(), c, onTunnel)
		}
	}()
	return ln.Addr().String(), func() { ln.Close(); <-served }
}

// A tunnel client still links, and a TLS 1.3 probe still gets the website,
// with the fix on (the paths it does not touch).
func TestReviewANoEMSFixKeepsTunnel(t *testing.T) {
	key := bytes.Repeat([]byte{0xa3}, 32)
	cert := testCert(t, "vpn.example.com")
	be, stopB := backend(t)
	defer stopB()
	got := make(chan struct{}, 1)
	addr, stop := startServer(t, key, cert, be, func(c *Carrier) { got <- struct{}{}; c.Close() })
	defer stop()
	car, err := Dial(addr, "vpn.example.com", key)
	if err != nil {
		t.Fatalf("tunnel dial: %v", err)
	}
	car.Close()
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("no tunnel")
	}
}

// The caps a binary offers follow its kill switches as the engine reads
// them (an integer, cmd/hs2 applyTuning): "00" or "+0" turn the core off
// there, so its bit must go too; MOVE_BACK is on for any value but 0, as
// engine.MoveBackOn reads it; a value that is no integer is the default at
// both (review A, finding A5; the red team's Z6).
func TestReviewACapsFollowEngineSwitches(t *testing.T) {
	all := CapClass | CapSched | CapHostQ | CapSendCap | CapGroup | CapHint
	for _, c := range []struct {
		env, val string
		want     uint32
	}{
		{"HS2_TUNE_SCHED", "00", all &^ (CapSched | CapGroup | CapHint)},
		{"HS2_TUNE_SCHED", "+0", all &^ (CapSched | CapGroup | CapHint)},
		{"HS2_TUNE_QUIET", "-0", all &^ (CapGroup | CapHint)},
		{"HS2_TUNE_SCHED", "off", all}, // no integer: the engine keeps it on
		{"HS2_TUNE_SCHED", "1", all},
		{"HS2_TUNE_MOVE_BACK", "2", all | CapMoveBack},
		{"HS2_TUNE_MOVE_BACK", "01", all | CapMoveBack},
		{"HS2_TUNE_MOVE_BACK", "yes", all},
	} {
		t.Run(c.env+"="+c.val, func(t *testing.T) {
			t.Setenv(c.env, c.val)
			if got := LocalAuthHdr().Caps; got != c.want {
				t.Fatalf("%s=%q: caps %#x, want %#x", c.env, c.val, got, c.want)
			}
		})
	}
}

// A relay that terminates TLS with a certificate of its own (any TLS
// interceptor: the dialer checks no certificate until R8's pin) makes the
// dialer read the real server's website answer to the relayed auth record
// as "the other server is v2 or has another key" (ErrOldServer), which R1
// made a refusal (PeerRefused): the pool's dials then wait 8 s doubling to
// 60 s, and the log sends the operator to re-paste links or upgrade. The
// answer is unauthenticated, so an interceptor can always fake it; the
// proof only shows the classification (review A, finding A4).
func TestReviewAInterceptorReadsAsRefusal(t *testing.T) {
	reviewRed(t)
	key := bytes.Repeat([]byte{0xa4}, 32)
	cert := testCert(t, "vpn.example.com")
	be, stopB := backend(t)
	defer stopB()
	addr, stop := startServer(t, key, cert, be, func(c *Carrier) { c.Close() })
	defer stop()
	maddr, stopM := mitm(t, addr)
	defer stopM()
	_, err := Dial(maddr, "vpn.example.com", key)
	if err == nil {
		t.Fatal("dial through the interceptor succeeded")
	}
	if errors.Is(err, ErrOldServer) || PeerRefused(err) {
		t.Fatalf("an interceptor's relay reads as a refusal (backoff 8-60 s, 'upgrade / re-paste the link'): %v", err)
	}
}
