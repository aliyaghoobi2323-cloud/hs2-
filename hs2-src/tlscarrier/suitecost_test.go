package tlscarrier

import (
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

// rel50 §11.8: the CPU cost per byte of a link's data phase by server
// library and TLS 1.3 suite — crypto/tls (the tls front: AES-128 with the
// Chrome preset), uTLS with the target's suite (the reality front: AES-256
// for an OpenSSL target, AES-128 for an AES-128 target). Bulk writes of 16 KiB
// records over loopback, the uTLS Chrome client reading.
//
//	go test ./tlscarrier -run XXX -bench SuiteCost -benchtime 2s
func BenchmarkSuiteCost(b *testing.B) {
	cert := testCert(&testing.T{}, "bench.test")
	for _, c := range []struct {
		name  string
		utls  bool
		suite uint16
	}{
		{"cryptotls", false, 0},
		{"utls-aes128", true, utls.TLS_AES_128_GCM_SHA256},
		{"utls-aes256", true, utls.TLS_AES_256_GCM_SHA384},
		{"utls-chacha", true, utls.TLS_CHACHA20_POLY1305_SHA256},
	} {
		b.Run(c.name, func(b *testing.B) {
			ln, _ := net.Listen("tcp", "127.0.0.1:0")
			defer ln.Close()
			got := make(chan uint16, 1)
			go func() {
				raw, err := ln.Accept()
				if err != nil {
					return
				}
				var w io.Writer
				if c.utls {
					ucfg := &utls.Config{Certificates: []utls.Certificate{{Certificate: cert.Certificate, PrivateKey: cert.PrivateKey}}, MinVersion: utls.VersionTLS13}
					ucfg.GetConfigForClient = func(chi *utls.ClientHelloInfo) (*utls.Config, error) {
						onlySuite13(chi.CipherSuites, c.suite)
						return nil, nil
					}
					s := utls.Server(raw, ucfg)
					if s.Handshake() != nil {
						return
					}
					got <- s.ConnectionState().CipherSuite
					w = s
				} else {
					s := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
					if s.Handshake() != nil {
						return
					}
					got <- s.ConnectionState().CipherSuite
					w = s
				}
				buf := make([]byte, 64<<10)
				for {
					if _, err := w.Write(buf); err != nil {
						return
					}
				}
			}()
			raw, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				b.Fatal(err)
			}
			defer raw.Close()
			u := utls.UClient(raw, &utls.Config{ServerName: "bench.test", InsecureSkipVerify: true}, utls.HelloChrome_Auto)
			if err := u.Handshake(); err != nil {
				b.Fatal(err)
			}
			select {
			case s := <-got:
				b.Logf("suite %#04x", s)
			case <-time.After(5 * time.Second):
				b.Fatal("no handshake")
			}
			buf := make([]byte, 64<<10)
			b.SetBytes(int64(len(buf)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := io.ReadFull(u, buf); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
