package tlscarrier

import (
	"bytes"
	"crypto/hmac"
	"hash"

	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"golang.org/x/crypto/blake2s"
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

// A captured auth record replayed on a new connection is rejected (it is bound
// to the original TLS session) and the replayer is served the website.
func TestReplayRejected(t *testing.T) {
	key := bytes.Repeat([]byte{0x33}, 32)
	cert := testCert(t, "vpn.example.com")
	be, stopB := backend(t)
	defer stopB()
	count := 0
	var mu sync.Mutex
	addr, stop := startServer(t, key, cert, be, func(c *Carrier) { mu.Lock(); count++; mu.Unlock(); c.Close() })
	defer stop()

	dial := func() (*utls.UConn, *utls.Config) {
		raw, _ := net.Dial("tcp", addr)
		cfg := &utls.Config{ServerName: "vpn.example.com", InsecureSkipVerify: true}
		u := utls.UClient(raw, cfg, utls.HelloChrome_133)
		if err := u.Handshake(); err != nil {
			t.Fatal(err)
		}
		return u, cfg
	}
	// first connection: a genuine record for its own session
	u1, cfg1 := dial()
	ekm, err := clientEKM(u1, cfg1)
	if err != nil {
		t.Fatal(err)
	}
	rec, nonce := makeClientAuth(key, ekm)
	u1.Write(rec)
	u1.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := readServerProof(u1, key, nonce, ekm); err != nil {
		t.Fatalf("genuine client refused: %v", err)
	}
	u1.Close()
	// second connection replays the captured record
	u2, _ := dial()
	u2.Write(rec)
	u2.SetReadDeadline(time.Now().Add(2 * time.Second))
	out, _ := io.ReadAll(u2)
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	c := count
	mu.Unlock()
	if c != 1 {
		t.Fatalf("tunnel accepted %d times, want 1", c)
	}
	if !bytes.Contains(out, []byte("real-backend")) {
		t.Fatalf("replayed connection not forwarded to backend: %q", out)
	}
}

// mitm terminates the client's TLS with its own certificate and relays the
// decrypted bytes over its own TLS connection to the real server, which is
// exactly what an interceptor with a forged certificate would do.
func mitm(t *testing.T, target string) (string, func()) {
	cert := testCert(t, "vpn.example.com") // a different, attacker-made cert
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				front := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}})
				if front.Handshake() != nil {
					return
				}
				raw, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				back := utls.UClient(raw, &utls.Config{ServerName: "vpn.example.com", InsecureSkipVerify: true}, utls.HelloChrome_133)
				if back.Handshake() != nil {
					return
				}
				defer back.Close()
				done := make(chan struct{}, 2)
				go func() { io.Copy(back, front); done <- struct{}{} }()
				go func() { io.Copy(front, back); done <- struct{}{} }()
				<-done
			}(c)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

// An interceptor in the middle can neither get a tunnel from the server nor
// make the client believe it reached the server.
func TestMITMRejected(t *testing.T) {
	key := bytes.Repeat([]byte{0x44}, 32)
	cert := testCert(t, "vpn.example.com")
	be, stopB := backend(t)
	defer stopB()
	hit := make(chan struct{}, 1)
	addr, stop := startServer(t, key, cert, be, func(c *Carrier) { hit <- struct{}{}; c.Close() })
	defer stop()
	maddr, stopM := mitm(t, addr)
	defer stopM()

	car, err := Dial(maddr, "vpn.example.com", key)
	if err == nil {
		car.Close()
		t.Fatal("client accepted a link through a man-in-the-middle")
	}
	select {
	case <-hit:
		t.Fatal("server granted a tunnel to a relayed (intercepted) client")
	case <-time.After(300 * time.Millisecond):
	}
	// sanity: the same client reaches the real server directly
	car, err = Dial(addr, "vpn.example.com", key)
	if err != nil {
		t.Fatalf("direct dial failed: %v", err)
	}
	car.Close()
}

// A server that does not know the key cannot pass the client's check.
func TestWrongServerKey(t *testing.T) {
	cert := testCert(t, "vpn.example.com")
	be, stopB := backend(t)
	defer stopB()
	addr, stop := startServer(t, bytes.Repeat([]byte{0x55}, 32), cert, be, func(c *Carrier) { c.Close() })
	defer stop()
	if _, err := Dial(addr, "vpn.example.com", bytes.Repeat([]byte{0x66}, 32)); err == nil {
		t.Fatal("dial with the wrong key succeeded")
	}
}

// A short request (fewer bytes than an auth record) must be answered by the
// backend promptly, like a real web server, not left hanging.
func TestShortProbeAnswered(t *testing.T) {
	key := bytes.Repeat([]byte{0x77}, 32)
	cert := testCert(t, "vpn.example.com")
	be, stopB := backend(t)
	defer stopB()
	addr, stop := startServer(t, key, cert, be, func(c *Carrier) { c.Close() })
	defer stop()
	raw, _ := net.Dial("tcp", addr)
	u := utls.UClient(raw, &utls.Config{ServerName: "vpn.example.com", InsecureSkipVerify: true}, utls.HelloChrome_133)
	if err := u.Handshake(); err != nil {
		t.Fatal(err)
	}
	u.Write([]byte("GET / HTTP/1.0\r\n\r\n")) // 18 bytes
	u.SetReadDeadline(time.Now().Add(2 * time.Second))
	out, _ := io.ReadAll(u)
	if !bytes.Contains(out, []byte("real-backend")) {
		t.Fatalf("short probe not answered by backend: %q", out)
	}
}

// A pre-v2 (unbound) auth record is refused and logged.
func TestLegacyClientRefused(t *testing.T) {
	key := bytes.Repeat([]byte{0x88}, 32)
	if !isLegacyAuth(key, legacyRecord(key)) {
		t.Fatal("legacy record not recognised")
	}
	cs := &fakeEKM{}
	ekm, _ := exportEKM(cs)
	if _, _, ok := parseClientAuth(key, ekm, legacyRecord(key)); ok {
		t.Fatal("legacy record accepted as v2")
	}
}

type fakeEKM struct{}

func (fakeEKM) ExportKeyingMaterial(string, []byte, int) ([]byte, error) {
	return bytes.Repeat([]byte{1}, 32), nil
}

// legacyRecord builds a v1 (pre channel binding) auth record.
func legacyRecord(key []byte) []byte {
	nonce := bytes.Repeat([]byte{9}, 16)
	m := hmac.New(func() hash.Hash { h, _ := blake2s.New256(nil); return h }, key)
	m.Write([]byte("hs2-tls-auth"))
	m.Write(nonce)
	m.Write(be64(minuteBucket()))
	return append(nonce, m.Sum(nil)[:16]...)
}
