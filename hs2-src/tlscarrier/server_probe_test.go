package tlscarrier

import (
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// A plain-HTTP request on the TLS port must get the exact response a real Go
// HTTPS server sends — not the old silent close that stood out as a fingerprint.
func TestPlainHTTPGetsGoNative400(t *testing.T) {
	key := bytes.Repeat([]byte{0x44}, 32)
	cert := testCert(t, "vpn.example.com")
	be, stopB := backend(t)
	defer stopB()
	addr, stop := startServer(t, key, cert, be, func(c *Carrier) { c.Close() })
	defer stop()

	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.Write([]byte("GET / HTTP/1.1\r\nHost: vpn.example.com\r\n\r\n"))
	raw.SetReadDeadline(time.Now().Add(2 * time.Second))
	out, _ := io.ReadAll(raw)
	if string(out) != httpToHTTPS400 {
		t.Fatalf("plain-HTTP response mismatch:\n got %q\nwant %q", out, httpToHTTPS400)
	}
}

// Pin httpToHTTPS400 to the standard library: whatever a real net/http HTTPS
// server replies to a plain-HTTP request, our constant must equal it byte for
// byte. If a future Go changes the wording, this test fails and tells us to
// update the constant — so "be a Go HTTPS server" never silently drifts.
func TestPinHTTPSResponseMatchesStdlib(t *testing.T) {
	cert := testCert(t, "pin.example.com")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler:   http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
	}
	go srv.ServeTLS(ln, "", "")
	defer srv.Close()
	time.Sleep(100 * time.Millisecond)

	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.Write([]byte("GET / HTTP/1.0\r\n\r\n"))
	raw.SetReadDeadline(time.Now().Add(2 * time.Second))
	out, _ := io.ReadAll(raw)
	if string(out) != httpToHTTPS400 {
		t.Fatalf("our constant drifted from the stdlib:\n stdlib %q\n ours   %q", out, httpToHTTPS400)
	}
}

// An HTTP method Go does NOT recognise (e.g. DELETE) must behave exactly like a
// real Go HTTPS server: it is not answered with the 400; it fails at the TLS
// layer and the connection is closed with no bytes. (Matching Go's exact set is
// what keeps us coherent — we must not be MORE helpful than a real server.)
func TestUnrecognizedMethodClosed(t *testing.T) {
	key := bytes.Repeat([]byte{0x55}, 32)
	cert := testCert(t, "vpn.example.com")
	be, stopB := backend(t)
	defer stopB()
	addr, stop := startServer(t, key, cert, be, func(c *Carrier) { c.Close() })
	defer stop()

	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.Write([]byte("DELETE / HTTP/1.1\r\nHost: x\r\n\r\n"))
	raw.SetReadDeadline(time.Now().Add(2 * time.Second))
	out, _ := io.ReadAll(raw)
	if len(out) != 0 {
		t.Fatalf("unrecognised method should get no response (like a Go HTTPS server), got %q", out)
	}
}

// Non-TLS, non-HTTP garbage must be closed like a TLS server fed a bad
// ClientHello: no 400, no bytes — exactly the old behaviour, preserved.
func TestGarbageClosed(t *testing.T) {
	key := bytes.Repeat([]byte{0x66}, 32)
	cert := testCert(t, "vpn.example.com")
	be, stopB := backend(t)
	defer stopB()
	addr, stop := startServer(t, key, cert, be, func(c *Carrier) { c.Close() })
	defer stop()

	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	// A fixed prefix that is neither a TLS record (0x16) nor a recognised HTTP
	// start, followed by filler — deterministic, unlike random bytes.
	msg := append([]byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00}, bytes.Repeat([]byte{0x41}, 595)...)
	raw.Write(msg)
	raw.SetReadDeadline(time.Now().Add(2 * time.Second))
	out, _ := io.ReadAll(raw)
	if len(out) != 0 {
		t.Fatalf("garbage should get no response, got %q", out)
	}
}
