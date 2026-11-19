package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeBin writes an executable shell script that touches marker when run and
// prints an hs2 version line; it returns the script's sha256.
func fakeBin(t *testing.T, path, marker string) string {
	t.Helper()
	body := "#!/bin/sh\ntouch '" + marker + "'\necho 'hs2 v3.test'\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	s := sha256.Sum256([]byte(body))
	return hex.EncodeToString(s[:])
}

func TestParseSha256(t *testing.T) {
	h := strings.Repeat("ab", 32)
	for in, want := range map[string]string{
		h + "  hs2-linux-amd64\r\n":       h,
		strings.ToUpper(h) + "\n":         h,
		"no hash here":                    "",
		"SHA256 (x) = " + h + " trailing": h,
	} {
		if got := parseSha256(in); got != want {
			t.Errorf("parseSha256(%q)=%q want %q", in, got, want)
		}
	}
}

// TestUpgradeBadHashNeverExecuted: a mismatching --sha256 or src.sha256, or
// no expected hash at all, refuses before src (or any copy of it) runs.
func TestUpgradeBadHashNeverExecuted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell scripts")
	}
	d := t.TempDir()
	trustTestDir(t, d)
	src, marker, dst := filepath.Join(d, "hs2-new"), filepath.Join(d, "ran"), filepath.Join(d, "hs2.new")
	good := fakeBin(t, src, marker)
	bad := strings.Repeat("0", 64)
	check := func(name string, flagHash string, allow bool) {
		t.Helper()
		if _, err := stageVerifiedBinary(src, flagHash, dst, allow); err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("%s: src was executed before verification", name)
		}
		if _, err := os.Stat(dst); err == nil {
			t.Fatalf("%s: staged copy left behind", name)
		}
	}
	check("flag mismatch", bad, false)
	check("flag mismatch, allow-unverified", bad, true) // a mismatch is never waved through
	check("flag not hex", "zz", false)
	check("no hash", "", false)
	os.WriteFile(src+".sha256", []byte(bad+"  hs2-new\n"), 0o644)
	check("file mismatch", "", false)
	check("file mismatch, allow-unverified", "", true)
	os.WriteFile(src+".sha256", []byte("garbage\n"), 0o644)
	check("file without hash", "", true)

	// good hash from the file: the staged copy (not src) runs
	os.WriteFile(src+".sha256", []byte(strings.ToUpper(good)+"  hs2-new\r\n"), 0o644)
	ver, err := stageVerifiedBinary(src, "", dst, false)
	if err != nil || !strings.Contains(ver, "hs2") {
		t.Fatalf("good file hash: %q %v", ver, err)
	}
	if b, _ := os.ReadFile(dst); sha256.Sum256(b) != sha256.Sum256(mustRead(t, src)) {
		t.Fatal("staged copy differs from src")
	}
	os.Remove(dst)
	os.Remove(src + ".sha256")
	if _, err := stageVerifiedBinary(src, good, dst, false); err != nil {
		t.Fatalf("good flag hash: %v", err)
	}
	os.Remove(dst)
	if _, err := stageVerifiedBinary(src, "", dst, true); err != nil {
		t.Fatalf("no hash with HS2_ALLOW_UNVERIFIED: %v", err)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// b376: SRC.sha256 is not trusted when SRC or it is not root-owned or the
// directory is group/world-writable; --sha256 still works there.
func TestUpgradeSidecarTrust(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell scripts")
	}
	d := t.TempDir()
	trustTestDir(t, d)
	src, marker, dst := filepath.Join(d, "hs2-new"), filepath.Join(d, "ran"), filepath.Join(d, "hs2.new")
	good := fakeBin(t, src, marker)
	os.WriteFile(src+".sha256", []byte(good+"  hs2-new\n"), 0o644)
	if _, err := stageVerifiedBinary(src, "", dst, false); err != nil {
		t.Fatalf("trusted sidecar refused: %v", err)
	}
	os.Remove(dst)
	os.Remove(marker) // the staged copy ran (as it should)
	refused := func(name string) {
		t.Helper()
		_, err := stageVerifiedBinary(src, "", dst, false)
		if err == nil || !strings.Contains(err.Error(), "--sha256") {
			t.Fatalf("%s: want a refusal naming --sha256, got %v", name, err)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("%s: src executed", name)
		}
		if _, err := stageVerifiedBinary(src, good, dst, false); err != nil {
			t.Fatalf("%s: --sha256 refused: %v", name, err)
		}
		os.Remove(dst)
		os.Remove(marker)
	}
	// a writable dir
	os.Chmod(d, 0o777)
	refused("dir 0777")
	os.Chmod(d, 0o770)
	refused("dir 0770")
	os.Chmod(d, 0o700)
	// a symlinked sidecar is never followed
	os.Rename(src+".sha256", filepath.Join(d, "real.sha256"))
	os.Symlink(filepath.Join(d, "real.sha256"), src+".sha256")
	refused("symlinked sidecar")
	os.Remove(src + ".sha256")
	os.Rename(filepath.Join(d, "real.sha256"), src+".sha256")
	if os.Geteuid() != 0 {
		t.Log("not root: owner cases skipped")
		return
	}
	os.Chown(src+".sha256", 65534, 65534)
	refused("sidecar owned by nobody")
	os.Chown(src+".sha256", 0, 0)
	os.Chown(src, 65534, 65534)
	refused("src owned by nobody")
}
