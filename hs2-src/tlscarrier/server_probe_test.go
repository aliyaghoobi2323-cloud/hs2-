package tlscarrier

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
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

// Regression: Carrier.TCPConn() must reach the kernel *net.TCPConn on the
// TLS-server side too. A wrapper once sat between tls.Conn and the socket (the
// old probe peek), a single-level unwrap returned nil, and TCP_INFO
// (retransmits/rwnd) went silently dark for the autopilot on that side.
func TestServerCarrierTCPConn(t *testing.T) {
	key := bytes.Repeat([]byte{0x77}, 32)
	cert := testCert(t, "vpn.example.com")
	be, stopB := backend(t)
	defer stopB()
	got := make(chan bool, 1)
	addr, stop := startServer(t, key, cert, be, func(c *Carrier) {
		got <- (c.TCPConn() != nil) // server-side carrier: tls over prefixConn over TCP
		c.Close()
	})
	defer stop()

	car, err := Dial(addr, "vpn.example.com", key)
	if err != nil {
		t.Fatal(err)
	}
	defer car.Close()
	if car.TCPConn() == nil {
		t.Fatal("client carrier TCPConn() is nil")
	}
	car.SendFrame(1, []byte("x"))
	select {
	case ok := <-got:
		if !ok {
			t.Fatal("server carrier TCPConn() is nil after the peek — TCP_INFO would be dead")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client never reached the tunnel")
	}
}

// probeOutcome is what a prober observes from one connection: the exact bytes
// the server sent, and HOW the connection ended — a clean FIN (EOF) or an RST
// (ECONNRESET, which a real Go server produces when it closes with unread data
// in its socket buffer). Both are on the wire, so both must match.
func probeOutcome(t *testing.T, addr string, probe []byte, halfClose bool) string {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write(probe); err != nil {
		t.Fatal(err)
	}
	if halfClose {
		c.(*net.TCPConn).CloseWrite()
	}
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	var got []byte
	buf := make([]byte, 4096)
	for {
		n, err := c.Read(buf)
		got = append(got, buf[:n]...)
		if err == nil {
			continue
		}
		end := "error:" + err.Error()
		switch {
		case errors.Is(err, io.EOF):
			end = "FIN"
		case errors.Is(err, syscall.ECONNRESET):
			end = "RST"
		case os.IsTimeout(err):
			end = "TIMEOUT"
		}
		return fmt.Sprintf("%s %q", end, got)
	}
}

// Every unauthenticated probe must end EXACTLY as it does against a real Go
// net/http HTTPS server — same bytes AND same FIN-vs-RST — differentially, probe
// by probe, against the real thing (not against our idea of it). The old 5-byte
// peek read less of the socket than crypto/tls does, so a DELETE, an HTTP/2
// preface or random bytes were left unread and the close became an RST where
// Go's is a FIN — a live-observed tell. Large probes RST on both (Go's first
// read does not consume them either), and that must match too.
func TestProbeCloseMatchesStdlib(t *testing.T) {
	cert := testCert(t, "vpn.example.com")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ref := &http.Server{
		Handler:   http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		ErrorLog:  log.New(io.Discard, "", 0),
	}
	go ref.ServeTLS(ln, "", "")
	defer ref.Close()

	be, stopB := backend(t)
	defer stopB()
	ours, stop := startServer(t, bytes.Repeat([]byte{0x88}, 32), cert, be, func(c *Carrier) { c.Close() })
	defer stop()
	time.Sleep(100 * time.Millisecond)

	rnd := rand.New(rand.NewSource(1))
	garbage := func(n int) []byte {
		b := make([]byte, n)
		rnd.Read(b)
		b[0] = 0xDE // never a TLS record type, never an HTTP start
		return b
	}
	bigGET := []byte("GET / HTTP/1.1\r\nHost: x\r\nX-Pad: " + strings.Repeat("a", 3000) + "\r\n\r\n")
	probes := []struct {
		name      string
		data      []byte
		halfClose bool
	}{
		{"plain GET", []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"), false},
		{"plain HEAD", []byte("HEAD / HTTP/1.1\r\nHost: x\r\n\r\n"), false},
		{"plain GET, 3KB of headers", bigGET, false},
		{"DELETE (not a Go-recognised start)", []byte("DELETE / HTTP/1.1\r\nHost: x\r\n\r\n"), false},
		{"HTTP/2 cleartext preface", []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"), false},
		{"100 random bytes", garbage(100), false},
		{"2000 random bytes", garbage(2000), false},
		{"3 bytes then FIN", []byte("GET"), true},
		{"TLS header, absurd version", []byte{0x16, 0x30, 0x00, 0x00, 0x10, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}, false},
		{"TLS handshake record, malformed body", []byte{0x16, 0x03, 0x01, 0x00, 0x05, 0x01, 0x00, 0x00, 0x01, 0x00}, false},
	}
	for _, p := range probes {
		want := probeOutcome(t, ln.Addr().String(), p.data, p.halfClose)
		got := probeOutcome(t, ours, p.data, p.halfClose)
		t.Logf("%-36s stdlib=%.60s", p.name, want)
		if strings.HasPrefix(want, "TIMEOUT") {
			t.Errorf("%s: the reference server timed out — the probe proves nothing", p.name)
		}
		if got != want {
			t.Errorf("%s: differs from a real Go HTTPS server\n stdlib %s\n ours   %s", p.name, want, got)
		}
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
