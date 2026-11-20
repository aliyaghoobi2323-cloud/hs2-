//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// b396 (rel69q sec370 + ZDU): UNIT.peers and UNIT.handover are written like
// the status file: atomically, 0600 (0644 with HS2_RUN_SHARED=1), no temp
// left behind. The upgrade script reads them as root, so 0600 is enough.
func TestZDURunFilesOwnerOnly(t *testing.T) {
	for _, shared := range []bool{false, true} {
		if shared {
			t.Setenv("HS2_RUN_SHARED", "1")
		} else {
			t.Setenv("HS2_RUN_SHARED", "")
		}
		dir := t.TempDir()
		o := &zduOld{runDir: dir, unit: "u", peers: func() (bool, string) { return true, "ZDU-OK links=1 zdu=1" }}
		o.writePeers()
		o.status("PROBATION 42")
		want := os.FileMode(0o600)
		if shared {
			want = 0o644
		}
		for _, n := range []string{"u.peers", "u.handover"} {
			fi, err := os.Stat(filepath.Join(dir, n))
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != want {
				t.Errorf("shared=%v %s mode %o, want %o", shared, n, fi.Mode().Perm(), want)
			}
		}
		b, _ := os.ReadFile(filepath.Join(dir, "u.handover"))
		if strings.TrimSpace(string(b)) != "PROBATION 42" {
			t.Errorf("handover %q", b)
		}
		es, _ := os.ReadDir(dir)
		if len(es) != 2 {
			var ns []string
			for _, e := range es {
				ns = append(ns, e.Name())
			}
			t.Errorf("run dir holds %v, want only u.peers and u.handover (no temp)", ns)
		}
	}
}
