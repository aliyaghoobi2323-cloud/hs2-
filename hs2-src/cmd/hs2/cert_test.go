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

// writeCertTo writes a fresh self-signed cert/key to fixed paths with the given
// serial and expiry, so a test can overwrite them and reload.
func writeCertTo(t *testing.T, certPath, keyPath string, serial int64, notAfter time.Time) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "t.example"},
		DNSNames: []string{"t.example"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(k)
	os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
}

func TestCertReloaderHotSwap(t *testing.T) {
	d := t.TempDir()
	c, k := filepath.Join(d, "c.pem"), filepath.Join(d, "k.pem")
	writeCertTo(t, c, k, 1, time.Now().Add(40*24*time.Hour))

	r, err := newCertReloader(c, k)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := r.getCertificate(nil)
	if first == nil || first.Leaf == nil || first.Leaf.SerialNumber.Int64() != 1 {
		t.Fatalf("initial cert serial wrong: %+v", first)
	}
	if dl := r.daysLeft(); dl < 38 || dl > 41 {
		t.Fatalf("daysLeft=%d, want ~40", dl)
	}

	// Renew: overwrite with a new serial + later expiry, then reload.
	writeCertTo(t, c, k, 2, time.Now().Add(80*24*time.Hour))
	r.reload("test")
	got, _ := r.getCertificate(nil)
	if got.Leaf.SerialNumber.Int64() != 2 {
		t.Fatalf("after reload serial=%d, want 2 (cert not hot-swapped)", got.Leaf.SerialNumber.Int64())
	}
	if dl := r.daysLeft(); dl < 78 || dl > 81 {
		t.Fatalf("after reload daysLeft=%d, want ~80", dl)
	}

	// A broken cert file must not clobber the good in-memory cert.
	os.WriteFile(c, []byte("not a cert"), 0o600)
	r.reload("test-broken")
	still, _ := r.getCertificate(nil)
	if still.Leaf.SerialNumber.Int64() != 2 {
		t.Fatalf("a broken reload replaced the good cert: serial=%d", still.Leaf.SerialNumber.Int64())
	}
}
