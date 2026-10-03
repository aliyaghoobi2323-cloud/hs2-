package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// certReloader holds the TLS-server certificate and reloads it from disk without
// restarting the tunnel, so a Let's Encrypt renewal never drops connections. The
// renewal deploy-hook sends SIGHUP (systemctl reload); as a belt-and-braces
// backstop the reloader also polls the files' mtime and reloads on a change. A
// reload that fails validation keeps the old certificate and logs loudly.
type certReloader struct {
	certFile, keyFile string
	cur               atomic.Pointer[tls.Certificate]
	mu                sync.Mutex
	lastMod           time.Time
	notAfter          atomic.Int64 // unix seconds of leaf expiry (0 = unknown)
}

// certRegistry lets one SIGHUP reload every server's certificate.
var certRegistry struct {
	mu        sync.Mutex
	reloaders []*certReloader
}

// newCertReloader loads and validates the initial certificate. It fails hard if
// the first load is bad (a tunnel with no usable cert cannot serve TLS).
func newCertReloader(certFile, keyFile string) (*certReloader, error) {
	r := &certReloader{certFile: certFile, keyFile: keyFile}
	if err := r.load(); err != nil {
		return nil, err
	}
	certRegistry.mu.Lock()
	certRegistry.reloaders = append(certRegistry.reloaders, r)
	certRegistry.mu.Unlock()
	return r, nil
}

// load reads the cert/key, validates the pair, records expiry, and swaps it in.
func (r *certReloader) load() error {
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return err
	}
	if len(cert.Certificate) > 0 {
		if leaf, err := x509.ParseCertificate(cert.Certificate[0]); err == nil {
			cert.Leaf = leaf
			r.notAfter.Store(leaf.NotAfter.Unix())
		}
	}
	r.cur.Store(&cert)
	if fi, err := os.Stat(r.certFile); err == nil {
		r.mu.Lock()
		r.lastMod = fi.ModTime()
		r.mu.Unlock()
	}
	return nil
}

// getCertificate is the tls.Config.GetCertificate callback.
func (r *certReloader) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return r.cur.Load(), nil
}

// reload re-reads the files, keeping the current cert if the new one is bad.
func (r *certReloader) reload(reason string) {
	old := r.notAfter.Load()
	if err := r.load(); err != nil {
		log.Printf("cert: reload (%s) failed, keeping the current certificate: %v", reason, err)
		return
	}
	if n := r.notAfter.Load(); n != old {
		log.Printf("cert: reloaded (%s) — now valid until %s", reason, time.Unix(n, 0).UTC().Format("2006-01-02"))
	} else {
		log.Printf("cert: reloaded (%s) — unchanged", reason)
	}
}

// changedOnDisk reports whether the cert file's mtime moved since the last load.
func (r *certReloader) changedOnDisk() bool {
	fi, err := os.Stat(r.certFile)
	if err != nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return fi.ModTime().After(r.lastMod)
}

// daysLeft returns whole days until expiry, or -1 if unknown.
func (r *certReloader) daysLeft() int {
	n := r.notAfter.Load()
	if n == 0 {
		return -1
	}
	return int(time.Until(time.Unix(n, 0)).Hours() / 24)
}

// reloadAllCerts reloads every registered server's certificate (SIGHUP handler).
func reloadAllCerts(reason string) {
	certRegistry.mu.Lock()
	rs := append([]*certReloader(nil), certRegistry.reloaders...)
	certRegistry.mu.Unlock()
	for _, r := range rs {
		r.reload(reason)
	}
}

// watchCerts polls for on-disk changes (renewal without a reload signal) and
// logs a warning as expiry approaches, until ctx ends.
func watchCerts(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	warned := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		certRegistry.mu.Lock()
		rs := append([]*certReloader(nil), certRegistry.reloaders...)
		certRegistry.mu.Unlock()
		for _, r := range rs {
			if r.changedOnDisk() {
				r.reload("file changed on disk")
				warned = false
			}
			if d := r.daysLeft(); d >= 0 && d <= 7 && !warned {
				log.Printf("cert: %s expires in %d day(s) — renew it now; 'hs2 doctor' (cert renewal) shows whether and how it renews", r.certFile, d)
				warned = true
			}
		}
	}
}

// firstCertExpiryDays returns the soonest cert expiry across all servers in
// whole days, or -1 if none/unknown — for the live status file.
func firstCertExpiryDays() int {
	certRegistry.mu.Lock()
	defer certRegistry.mu.Unlock()
	best := -1
	for _, r := range certRegistry.reloaders {
		if d := r.daysLeft(); d >= 0 && (best < 0 || d < best) {
			best = d
		}
	}
	return best
}
