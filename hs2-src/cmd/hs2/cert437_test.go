package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// b437 F6: one SIGHUP over many cert holders (prod t1: 7) logs one summary
// line when nothing changed; a renewed or failed holder keeps its own line.
func TestCert437OneLinePerReload(t *testing.T) {
	certRegistry.mu.Lock()
	saved := certRegistry.reloaders
	certRegistry.reloaders = nil
	certRegistry.mu.Unlock()
	t.Cleanup(func() { certRegistry.mu.Lock(); certRegistry.reloaders = saved; certRegistry.mu.Unlock() })
	d := t.TempDir()
	var paths [][2]string
	for i := 0; i < 7; i++ {
		c, k := filepath.Join(d, "c"+string(rune('a'+i))+".pem"), filepath.Join(d, "k"+string(rune('a'+i))+".pem")
		writeCertTo(t, c, k, int64(i+1), time.Now().Add(40*24*time.Hour))
		if _, err := newCertReloader(c, k); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, [2]string{c, k})
	}
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	reloadAllCerts("SIGHUP")
	if l := strings.Count(buf.String(), "cert: "); l != 1 || !strings.Contains(buf.String(), "7 of 7 holders unchanged") {
		t.Fatalf("unchanged: %d lines:\n%s", l, buf.String())
	}
	buf.Reset()
	writeCertTo(t, paths[2][0], paths[2][1], 99, time.Now().Add(80*24*time.Hour))
	reloadAllCerts("SIGHUP")
	out := buf.String()
	if strings.Count(out, "cert: ") != 2 || !strings.Contains(out, "now valid until") || !strings.Contains(out, "6 of 7 holders unchanged") {
		t.Fatalf("one renewed: \n%s", out)
	}
}

// b437 F5: a draining old process's "[draining] " prefix is the same error
// kind for the watchdog's "new error kind" rule (no alert at every ZDU).
func TestErrSig437DrainingPrefix(t *testing.T) {
	a := "12:41:47.123456 dial 1.2.3.4:443 failed after 30 ms"
	b := "12:41:48.000001 [draining] dial 5.6.7.8:443 failed after 7 ms"
	if errSig(a) != errSig(b) {
		t.Fatalf("%q != %q", errSig(a), errSig(b))
	}
}
