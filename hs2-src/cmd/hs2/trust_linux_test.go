//go:build linux

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// trustTestDir: d's ancestors (/tmp ...) are not root-owned 0755 and /tmp may
// be tmpfs, so the tests exempt exactly those ancestors and accept any
// filesystem; everything at and below d is checked for real.
func trustTestDir(t *testing.T, d string) {
	t.Helper()
	oldEx, oldFS := trustExempt, localFSTrusted
	t.Cleanup(func() { trustExempt, localFSTrusted = oldEx, oldFS })
	trustExempt = func(q string) bool { return q == "/" || strings.HasPrefix(d, q+"/") }
	localFSTrusted = func(uint32, uint32) bool { return true }
}

// b378: symlinks in the path are followed only from trusted dirs into trusted
// dirs; a g/o-writable or sticky dir anywhere on the way is refused.
func TestResolveTrustedDir(t *testing.T) {
	d := t.TempDir()
	trustTestDir(t, d)
	mk := func(p string, m os.FileMode) {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		os.Chmod(p, m)
	}
	mk(filepath.Join(d, "ok", "rel"), 0o755)
	mk(filepath.Join(d, "w", "sub"), 0o755)
	os.Chmod(filepath.Join(d, "w"), 0o777)
	mk(filepath.Join(d, "st", "sub"), 0o755)
	os.Chmod(filepath.Join(d, "st"), 0o1777)
	os.Symlink(filepath.Join(d, "ok"), filepath.Join(d, "l1"))              // trusted -> trusted
	os.Symlink(filepath.Join(d, "w", "sub"), filepath.Join(d, "ok", "l2"))  // trusted -> under a 0777 dir
	os.Symlink(filepath.Join(d, "ok"), filepath.Join(d, "w", "l3"))         // in a 0777 dir -> trusted
	os.Symlink(filepath.Join(d, "ok", "rel"), filepath.Join(d, "st", "l4")) // in a sticky dir
	for p, want := range map[string]string{
		filepath.Join(d, "ok", "rel"): filepath.Join(d, "ok", "rel"),
		filepath.Join(d, "l1", "rel"): filepath.Join(d, "ok", "rel"),
		filepath.Join(d, "ok", "l2"):  "",
		filepath.Join(d, "w", "l3"):   "",
		filepath.Join(d, "w", "sub"):  "",
		filepath.Join(d, "st", "sub"): "",
		filepath.Join(d, "st", "l4"):  "",
	} {
		got, err := resolveTrustedDir(p)
		if want == "" {
			if err == nil {
				t.Errorf("%s: trusted as %s, want refused", p, got)
			}
		} else if err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", p, got, err, want)
		}
	}
}

// b378: the downgrade pairing: a user-made symlink to an older root-owned
// release dir, a symlinked SRC, a sidecar on an untrusted filesystem, and a
// SRC whose name no longer is the inode read are all refused; --sha256 works.
func TestSidecarTrustPaths(t *testing.T) {
	d := t.TempDir()
	trustTestDir(t, d)
	old := filepath.Join(d, "old")
	os.Mkdir(old, 0o755)
	src, marker, dst := filepath.Join(old, "hs2-new"), filepath.Join(d, "ran"), filepath.Join(d, "hs2.new")
	good := fakeBin(t, src, marker)
	os.WriteFile(src+".sha256", []byte(good+"  hs2-new\n"), 0o644)
	if _, err := stageVerifiedBinary(src, "", dst, false); err != nil {
		t.Fatalf("trusted sidecar refused: %v", err)
	}
	os.Remove(dst)
	refused := func(name, s, want string) {
		t.Helper()
		_, err := stageVerifiedBinary(s, "", dst, false)
		if err == nil || !strings.Contains(err.Error(), "--sha256") || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: want a refusal naming --sha256 and %q, got %v", name, want, err)
		}
		os.Remove(dst)
		if _, err := stageVerifiedBinary(s, good, dst, false); err != nil {
			t.Fatalf("%s: --sha256 refused: %v", name, err)
		}
		os.Remove(dst)
	}
	w := filepath.Join(d, "w")
	os.Mkdir(w, 0o777)
	os.Chmod(w, 0o777)
	os.Symlink(old, filepath.Join(w, "rel"))
	refused("symlink to the release dir from a 0777 dir", filepath.Join(w, "rel", "hs2-new"), "writable")
	os.Symlink(src, filepath.Join(old, "hs2-link"))
	os.Symlink(src+".sha256", filepath.Join(old, "hs2-link.sha256"))
	refused("symlinked SRC", filepath.Join(old, "hs2-link"), "symlink")
	localFSTrusted = func(uint32, uint32) bool { return false }
	refused("untrusted filesystem", src, "filesystem")
	localFSTrusted = func(uint32, uint32) bool { return true }

	// SRC's name now points to another inode than the one read
	fi, _ := os.Stat(src)
	other := filepath.Join(old, "other")
	fakeBin(t, other, marker)
	os.Rename(other, src)
	if _, why, _ := sidecarTrust(src, fi); !strings.Contains(why, "not the same file") {
		t.Fatalf("swapped SRC: %q", why)
	}

	// no sidecar at all, even in a writable dir: the message says so
	os.Remove(src + ".sha256")
	os.Chmod(old, 0o777)
	_, err := stageVerifiedBinary(src, "", dst, false)
	if err == nil || !strings.Contains(err.Error(), "no "+src+".sha256 found") {
		t.Fatalf("no sidecar: %v", err)
	}
}

func TestLocalFSTrusted(t *testing.T) {
	for _, c := range []struct {
		magic, uid uint32
		ok         bool
	}{
		{uint32(unix.EXT4_SUPER_MAGIC), 0, true},
		{uint32(unix.XFS_SUPER_MAGIC), 0, true},
		{uint32(unix.BTRFS_SUPER_MAGIC), 0, true},
		{uint32(unix.OVERLAYFS_SUPER_MAGIC), 0, true},
		{uint32(unix.OVERLAYFS_SUPER_MAGIC), 1000, false},
		{uint32(unix.FUSE_SUPER_MAGIC), 0, false},
		{uint32(unix.NFS_SUPER_MAGIC), 0, false},
		{uint32(unix.TMPFS_MAGIC), 0, false},
		{uint32(unix.CIFS_SUPER_MAGIC), 0, false},
	} {
		if got := localFSTrusted(c.magic, c.uid); got != c.ok {
			t.Errorf("magic 0x%x uid %d: %v want %v", c.magic, c.uid, got, c.ok)
		}
	}
}

// b378: the real checks, no hooks: as root, a release staged in a fresh
// 0700 dir under /root on ext4/xfs/btrfs is trusted (the common runbook path).
func TestSidecarTrustRealRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	d, err := os.MkdirTemp("/root", "hs2-trust-")
	if err != nil {
		t.Skip(err)
	}
	defer os.RemoveAll(d)
	var sf unix.Statfs_t
	if unix.Statfs(d, &sf) != nil || !localFSTrusted(uint32(sf.Type), 0) {
		t.Skipf("/root is on fs 0x%x", uint32(sf.Type))
	}
	if err := checkTrustedChain("/root"); err != nil {
		t.Skipf("this host's /root chain is not trusted: %v", err)
	}
	src, marker, dst := filepath.Join(d, "hs2-new"), filepath.Join(d, "ran"), filepath.Join(d, "hs2.new")
	good := fakeBin(t, src, marker)
	os.WriteFile(src+".sha256", []byte(good+"  hs2-new\n"), 0o644)
	if _, err := stageVerifiedBinary(src, "", dst, false); err != nil {
		t.Fatalf("/root staging refused: %v", err)
	}
}

// b378: doctor --perms-fix works through a root-made symlinked system dir
// (/var -> /data/var), refuses one reached through a g/o-writable dir, and
// then lists what is not fixed and exits non-zero.
func TestPermsFixSymlinkedPrefix(t *testing.T) {
	if !openat2Works() {
		t.Skip("no openat2")
	}
	d := t.TempDir()
	trustTestDir(t, d)
	hs := filepath.Join(d, "data", "var", "lib", "hs2")
	os.MkdirAll(hs, 0o755)
	os.Chmod(hs, 0o700)
	f := filepath.Join(hs, "history.json")
	os.WriteFile(f, []byte("{}"), 0o644)
	os.Symlink(filepath.Join(d, "data", "var"), filepath.Join(d, "var"))
	oldRoots := permRootsFunc
	defer func() { permRootsFunc = oldRoots }()
	t.Setenv("HS2_RUN_SHARED", "")

	permRootsFunc = func() []string { return []string{filepath.Join(d, "var", "lib", "hs2")} }
	var out bytes.Buffer
	if rc := doctorPerms(true, &out); rc != 0 || mode(t, f) != 0o600 {
		t.Fatalf("trusted symlinked /var: rc %d mode %o: %s", rc, mode(t, f), out.String())
	}

	os.Chmod(f, 0o644)
	w := filepath.Join(d, "w")
	os.Mkdir(w, 0o777)
	os.Chmod(w, 0o777)
	os.Symlink(filepath.Join(d, "data", "var"), filepath.Join(w, "var"))
	permRootsFunc = func() []string { return []string{filepath.Join(w, "var", "lib", "hs2")} }
	out.Reset()
	rc := doctorPerms(true, &out)
	if rc == 0 || mode(t, f) != 0o644 || !strings.Contains(out.String(), "NOT FIXED") || !strings.Contains(out.String(), "ERROR") {
		t.Fatalf("untrusted symlinked prefix: rc %d mode %o: %s", rc, mode(t, f), out.String())
	}
}

// f379: the real checks, no hooks: a release staged on tmpfs (a fresh tmpfs
// mounted on a 0700 dir under /root, so only the fs type differs from
// TestSidecarTrustRealRoot) is refused with the sidecar alone, the refusal
// names --sha256, nothing is executed, and the same files install with
// --sha256 (what the runbook says for /tmp, /dev/shm, ZFS and the rest).
func TestSidecarTmpfsNeedsFlag(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	d, err := os.MkdirTemp("/root", "hs2-tmpfs-")
	if err != nil {
		t.Skip(err)
	}
	defer os.RemoveAll(d)
	if err := checkTrustedChain("/root"); err != nil {
		t.Skipf("this host's /root chain is not trusted: %v", err)
	}
	if err := unix.Mount("tmpfs", d, "tmpfs", 0, "mode=0700,size=8m"); err != nil {
		t.Skipf("cannot mount a tmpfs: %v", err)
	}
	defer unix.Unmount(d, unix.MNT_DETACH)
	src, marker, dst := filepath.Join(d, "hs2-new"), filepath.Join(d, "ran"), filepath.Join(d, "hs2.new")
	good := fakeBin(t, src, marker)
	os.WriteFile(src+".sha256", []byte(good+"  hs2-new\n"), 0o644)
	_, err = stageVerifiedBinary(src, "", dst, false)
	if err == nil || !strings.Contains(err.Error(), "--sha256") || !strings.Contains(err.Error(), "NOT installed") {
		t.Fatalf("tmpfs sidecar: want a refusal naming --sha256, got %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("src executed on a refusal")
	}
	if _, err := os.Stat(dst); err == nil {
		t.Fatal("dst left behind on a refusal")
	}
	if _, err := stageVerifiedBinary(src, good, dst, false); err != nil {
		t.Fatalf("tmpfs with --sha256 refused: %v", err)
	}
	if _, err := stageVerifiedBinary(src, strings.Repeat("0", 64), dst, false); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("tmpfs with a wrong --sha256: want a mismatch, got %v", err)
	}
}
