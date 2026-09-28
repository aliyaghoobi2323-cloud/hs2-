package tlscarrier

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

func testCert(t *testing.T, cn string) tls.Certificate {
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		DNSNames: []string{cn}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &k.PublicKey, k)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}
}

// backend stands in for the real website probes get forwarded to.
func backend(t *testing.T) (string, func()) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				b := make([]byte, 512)
				c.SetReadDeadline(time.Now().Add(time.Second))
				c.Read(b)
				c.Write([]byte("HTTP/1.1 200 OK\r\nServer: real-backend\r\n\r\nhi"))
			}(c)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

func startServer(t *testing.T, key []byte, cert tls.Certificate, backendAddr string, onTunnel func(*Carrier)) (string, func()) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := &Server{SharedKey: key, Cert: cert, BackendAddr: backendAddr}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.Handle(ctx, c, onTunnel)
		}
	}()
	return ln.Addr().String(), func() { cancel(); ln.Close() }
}

// Authorised client reaches the tunnel and exchanges data inside TLS.
func TestClientReachesTunnel(t *testing.T) {
	key := bytes.Repeat([]byte{0x11}, 32)
	cert := testCert(t, "vpn.example.com")
	be, stopB := backend(t)
	defer stopB()
	got := make(chan *Carrier, 1)
	addr, stop := startServer(t, key, cert, be, func(c *Carrier) { got <- c })
	defer stop()

	car, err := Dial(addr, "vpn.example.com", key)
	if err != nil {
		t.Fatal(err)
	}
	defer car.Close()
	car.SendFrame(1, []byte("hello-tunnel"))
	select {
	case sc := <-got:
		ft, p, err := sc.ReadFrame()
		if err != nil || ft != 1 || string(p) != "hello-tunnel" {
			t.Fatalf("tunnel frame wrong: ft=%d p=%q err=%v", ft, p, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client never reached tunnel")
	}
}

// A probe: real TLS handshake (validates the cert), then speaks HTTP. It must
// get the real backend's response, not a tunnel error.
func TestProbeGetsBackend(t *testing.T) {
	key := bytes.Repeat([]byte{0x22}, 32)
	cert := testCert(t, "vpn.example.com")
	be, stopB := backend(t)
	defer stopB()
	tunnelHit := false
	addr, stop := startServer(t, key, cert, be, func(c *Carrier) { tunnelHit = true; c.Close() })
	defer stop()

	// Probe uses a normal uTLS handshake and validates the cert chain shape,
	// then sends an HTTP request (no auth token).
	raw, _ := net.Dial("tcp", addr)
	u := utls.UClient(raw, &utls.Config{ServerName: "vpn.example.com", InsecureSkipVerify: true}, utls.HelloChrome_133)
	if err := u.Handshake(); err != nil {
		t.Fatalf("probe TLS handshake failed: %v", err)
	}
	// The cert the probe sees must be the real (test) cert for the domain.
	st := u.ConnectionState()
	if len(st.PeerCertificates) == 0 || st.PeerCertificates[0].Subject.CommonName != "vpn.example.com" {
		t.Fatal("probe did not receive the domain cert")
	}
	u.Write([]byte("GET / HTTP/1.1\r\nHost: vpn.example.com\r\n\r\n"))
	u.SetReadDeadline(time.Now().Add(2 * time.Second))
	out, _ := io.ReadAll(u)
	if tunnelHit {
		t.Fatal("probe wrongly reached the tunnel")
	}
	if !bytes.Contains(out, []byte("real-backend")) {
		t.Fatalf("probe did not get backend response: %q", out)
	}
}

// A replayed auth record is rejected (forwarded to backend), not accepted twice.
func TestReplayRejected(t *testing.T) {
	key := bytes.Repeat([]byte{0x33}, 32)
	cert := testCert(t, "vpn.example.com")
	be, stopB := backend(t)
	defer stopB()
	count := 0
	var mu sync.Mutex
	addr, stop := startServer(t, key, cert, be, func(c *Carrier) { mu.Lock(); count++; mu.Unlock(); c.Close() })
	defer stop()

	rec := makeAuthRecord(key) // one fixed record, sent twice
	send := func() []byte {
		raw, _ := net.Dial("tcp", addr)
		u := utls.UClient(raw, &utls.Config{ServerName: "vpn.example.com", InsecureSkipVerify: true}, utls.HelloChrome_133)
		u.Handshake()
		u.Write(rec)
		u.SetReadDeadline(time.Now().Add(time.Second))
		out, _ := io.ReadAll(u)
		return out
	}
	send()
	out2 := send()
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	c := count
	mu.Unlock()
	if c > 1 {
		t.Fatalf("replayed record accepted %d times", c)
	}
	if !bytes.Contains(out2, []byte("real-backend")) {
		t.Fatalf("replayed connection not forwarded to backend: %q", out2)
	}
}
