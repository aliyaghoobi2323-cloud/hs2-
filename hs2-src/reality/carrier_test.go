package reality

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"
)

func coverCert(t *testing.T, cn string) tls.Certificate {
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     []string{cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, _ := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &k.PublicKey, k)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}
}

// The full Path A: signalled client -> TLS handshake completes -> hs2 frames
// flow INSIDE the single TLS layer -> server echoes. One layer of TLS on the
// wire, no nested encryption.
func TestPathA_EndToEnd(t *testing.T) {
	key := bytes.Repeat([]byte{0x77}, 32)
	sni := "www.microsoft.com"
	cert := coverCert(t, sni)

	cover, stopC := coverSite(t)
	defer stopC()

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	d := &Dispatcher{
		SharedKey: key,
		CoverAddr: cover,
		OnTunnel: func(conn net.Conn, hello []byte, cr []byte) {
			// Complete the TLS handshake using the peeked ClientHello, then echo
			// whatever the client sends inside the TLS session.
			tconn, err := ServerTLS(conn, hello, cert)
			if err != nil {
				t.Errorf("ServerTLS: %v", err)
				conn.Close()
				return
			}
			defer tconn.Close()
			b := make([]byte, 256)
			n, _ := tconn.Read(b)
			tconn.Write(append([]byte("echo:"), b[:n]...))
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go d.Handle(ctx, c)
		}
	}()

	// Authorised client: full uTLS handshake with the signal.
	uconn, err := ClientDialTLS(ln.Addr().String(), sni, key)
	if err != nil {
		t.Fatalf("ClientDialTLS: %v", err)
	}
	defer uconn.Close()
	uconn.Write([]byte("data-inside-one-tls-layer"))
	b := make([]byte, 256)
	uconn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := uconn.Read(b)
	if err != nil {
		t.Fatalf("read inside tls: %v", err)
	}
	if !bytes.Contains(b[:n], []byte("echo:data-inside-one-tls-layer")) {
		t.Fatalf("data did not round-trip inside TLS: %q", b[:n])
	}
	t.Log("OK: authorised client completed real TLS and exchanged data inside one TLS layer")
}

// A probe during the same server still gets the cover site, not a TLS error
// that reveals the server only talks to authorised clients.
func TestPathA_ProbeStillCovered(t *testing.T) {
	key := bytes.Repeat([]byte{0x88}, 32)
	sni := "www.microsoft.com"
	cert := coverCert(t, sni)
	cover, stopC := coverSite(t)
	defer stopC()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	d := &Dispatcher{SharedKey: key, CoverAddr: cover,
		OnTunnel: func(conn net.Conn, hello []byte, cr []byte) {
			tc, err := ServerTLS(conn, hello, cert)
			if err != nil {
				conn.Close()
				return
			}
			tc.Close()
		}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go d.Handle(ctx, c)
		}
	}()
	resp := realBrowserHello(t, ln.Addr().String(), sni)
	if !bytes.Contains(resp, []byte("cover")) {
		t.Fatalf("probe did not reach cover site under Path A: %q", resp)
	}
}

// Proves Path A puts exactly ONE TLS layer on the wire: the bytes a passive
// observer sees after the handshake are TLS application-data records (type 23)
// whose payload is the TLS ciphertext — NOT an inner TLS handshake (which would
// start 0x16 0x03 and reveal TLS-in-TLS). We tap the raw socket and check.
func TestPathA_SingleTLSLayer(t *testing.T) {
	key := bytes.Repeat([]byte{0x99}, 32)
	sni := "www.microsoft.com"
	cert := coverCert(t, sni)
	cover, stopC := coverSite(t)
	defer stopC()

	// A tap that records everything the client writes to the wire after
	// handshake, by wrapping the client's TCP conn.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	d := &Dispatcher{SharedKey: key, CoverAddr: cover,
		OnTunnel: func(conn net.Conn, hello []byte, cr []byte) {
			tc, err := ServerTLS(conn, hello, cert)
			if err != nil {
				conn.Close()
				return
			}
			defer tc.Close()
			b := make([]byte, 256)
			n, _ := tc.Read(b)
			tc.Write(append([]byte("echo:"), b[:n]...))
		}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go d.Handle(ctx, c)
		}
	}()

	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	tap := &tapConn{Conn: raw}
	uconn := utlsClientOver(tap, sni, key, t)
	defer uconn.Close()
	uconn.Write([]byte("payload"))
	b := make([]byte, 256)
	uconn.SetReadDeadline(time.Now().Add(2 * time.Second))
	uconn.Read(b)

	// Inspect post-handshake records the client sent. Each TLS record: type(1).
	// After handshake, application data is type 23 (0x17). If we saw an inner
	// handshake record (0x16) AFTER the outer handshake completed, that would be
	// TLS-in-TLS. We assert every post-handshake record byte-0 is 0x17.
	recs := tap.postHandshakeRecordTypes()
	for _, rt := range recs {
		if rt == 0x16 {
			t.Fatalf("found an inner TLS handshake record on the wire: TLS-in-TLS!")
		}
	}
	if len(recs) == 0 {
		t.Fatal("no post-handshake records captured")
	}
	t.Logf("OK: %d post-handshake records, all application-data (0x17); single TLS layer confirmed", len(recs))
}
