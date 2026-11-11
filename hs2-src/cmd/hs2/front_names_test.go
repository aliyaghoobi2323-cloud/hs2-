package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTestCert(t *testing.T, cn string, sans []string) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn}, DNSNames: sans,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "cert.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// rel48: a direct-mode listener has no "sni"; its "both" IPs must still
// answer our tls name (the certificate's names), or every tls dial is
// spliced to the target and a dialer without the key never gets in.
func TestTLSNamesFromCert(t *testing.T) {
	cn := writeTestCert(t, "Lab.Example.com", nil)
	if n := tlsNames(fileConfig{CertFile: cn}); !n["lab.example.com"] || len(n) != 1 {
		t.Fatalf("CN fallback: %v", n)
	}
	san := writeTestCert(t, "ignored", []string{"a.example.com", "b.example.com"})
	if n := tlsNames(fileConfig{CertFile: san}); !n["a.example.com"] || !n["b.example.com"] || n["ignored"] {
		t.Fatalf("SAN fallback: %v", n)
	}
	// an explicit sni wins: the certificate is not read
	if n := tlsNames(fileConfig{SNI: "x.example.com", CertFile: san}); !n["x.example.com"] || len(n) != 1 {
		t.Fatalf("sni set: %v", n)
	}
	if n := tlsNames(fileConfig{CertFile: filepath.Join(t.TempDir(), "missing.pem")}); len(n) != 0 {
		t.Fatalf("missing cert: %v", n)
	}
	ipcn := writeTestCert(t, "10.0.0.1", nil)
	if n := tlsNames(fileConfig{CertFile: ipcn}); len(n) != 0 {
		t.Fatalf("an IP CN is not a tls name: %v", n)
	}
}
