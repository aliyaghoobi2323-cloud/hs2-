//go:build linux

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// trustExempt: test hook only (the ancestors of a test's TempDir, e.g. /tmp,
// are not root-owned 0755). Always false in hs2.
var trustExempt = func(dir string) bool { return false }

// checkTrustedChain (b378): every directory from r up to / is a real
// directory (not a symlink), owned by root (or hs2's own user) and neither
// group- nor world-writable. The sticky bit does not make a dir trusted
// (/tmp, /var/tmp are not): anyone may create names there.
func checkTrustedChain(r string) error {
	for q := r; ; q = filepath.Dir(q) {
		if !trustExempt(q) {
			fi, err := os.Lstat(q)
			if err != nil {
				return err
			}
			if !fi.IsDir() {
				return fmt.Errorf("%s is not a directory (a symlink?)", q)
			}
			if fi.Mode().Perm()&0o022 != 0 || !trustedOwner(fi) {
				return fmt.Errorf("%s is writable by other users (%04o) or not root-owned", q, fi.Mode().Perm())
			}
		}
		if q == "/" || q == "." {
			return nil
		}
	}
}

// resolveTrustedDir (b378): the symlink-free form of dir, when nobody but
// root could have chosen where it leads: each prefix of dir's absolute path
// is resolved and its whole chain checked (checkTrustedChain). A symlink is
// therefore followed only when it sits in a trusted directory (so root put it
// there) and leads into a trusted chain; e.g. /var -> /data/var is fine,
// /tmp/x -> /opt/old-release is not (/tmp is 1777).
func resolveTrustedDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	cur := "/"
	if err := checkTrustedChain(cur); err != nil {
		return "", err
	}
	for _, part := range strings.Split(abs, "/") {
		if part == "" {
			continue
		}
		r, err := filepath.EvalSymlinks(filepath.Join(cur, part))
		if err != nil {
			return "", err
		}
		if err := checkTrustedChain(r); err != nil {
			return "", err
		}
		cur = r
	}
	return cur, nil
}

// localFSTrusted (b378): the sidecar is trusted only on a local disk
// filesystem (ext4, xfs, btrfs), or overlay (containers) when the inode is
// root-owned. FUSE (a user can mount one with allow_other and report uid 0),
// NFS/CIFS (the server decides the owner), tmpfs and anything else: untrusted,
// --sha256 is needed there.
var localFSTrusted = func(magic uint32, uid uint32) bool {
	switch magic {
	case uint32(unix.EXT4_SUPER_MAGIC), uint32(unix.XFS_SUPER_MAGIC), uint32(unix.BTRFS_SUPER_MAGIC):
		return true
	case uint32(unix.OVERLAYFS_SUPER_MAGIC):
		return uid == 0
	}
	return false
}

func fdFSTrusted(fd int, name string, uid uint32) error {
	var sf unix.Statfs_t
	if err := unix.Fstatfs(fd, &sf); err != nil {
		return &os.PathError{Op: "fstatfs", Path: name, Err: err}
	}
	if m := uint32(sf.Type); !localFSTrusted(m, uid) {
		return fmt.Errorf("%s is on a filesystem not trusted for a sidecar (magic 0x%x: only ext4/xfs/btrfs, or a root-owned overlay)", name, m)
	}
	return nil
}

// sidecarTrust (b376, b378): a SRC.sha256 beside SRC proves nothing when
// someone else could have written or paired both. It returns the sidecar's
// body only when:
//   - SRC (the fd it was read from, srcFI) is a root-owned regular file, and
//     the name SRC is not a symlink and still names that same inode;
//   - SRC's directory and every ancestor up to / are root-owned and not
//     group/world-writable (resolveTrustedDir: a root-made symlink in the
//     path is fine);
//   - the directory is opened once and SRC and SRC.sha256 are opened
//     relative to that fd with O_NOFOLLOW;
//   - SRC.sha256 is a root-owned regular file;
//   - all three are on a local disk filesystem (localFSTrusted).
//
// found=false (and why "") when there is no SRC.sha256 at all. Otherwise
// why != "" is the reason it is not trusted: the caller then needs
// --sha256 HEX.
func sidecarTrust(src string, srcFI os.FileInfo) (body, why string, found bool) {
	if _, err := os.Lstat(src + ".sha256"); errors.Is(err, os.ErrNotExist) {
		return "", "", false
	}
	found = true
	if !srcFI.Mode().IsRegular() || !trustedOwner(srcFI) {
		return "", src + " is not a root-owned regular file", found
	}
	abs, err := filepath.Abs(src)
	if err != nil {
		return "", err.Error(), found
	}
	base := filepath.Base(abs)
	rdir, err := resolveTrustedDir(filepath.Dir(abs))
	if err != nil {
		return "", err.Error(), found
	}
	dfd, err := unix.Open(rdir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", (&os.PathError{Op: "open", Path: rdir, Err: err}).Error(), found
	}
	defer unix.Close(dfd)
	var dst unix.Stat_t
	if err := unix.Fstat(dfd, &dst); err != nil {
		return "", err.Error(), found
	}
	if dst.Mode&0o022 != 0 || !(dst.Uid == 0 || int(dst.Uid) == os.Geteuid()) {
		return "", fmt.Sprintf("%s is writable by other users (%04o) or not root-owned", rdir, dst.Mode&0o7777), found
	}
	if err := fdFSTrusted(dfd, rdir, dst.Uid); err != nil {
		return "", err.Error(), found
	}
	// SRC by name, relative to the dir fd, never through a symlink, and it must
	// be the very inode whose bytes were read.
	sfd, err := unix.Openat(dfd, base, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ELOOP) {
		return "", src + " is a symlink", found
	}
	if err != nil {
		return "", (&os.PathError{Op: "open", Path: src, Err: err}).Error(), found
	}
	defer unix.Close(sfd)
	var sst unix.Stat_t
	if err := unix.Fstat(sfd, &sst); err != nil {
		return "", err.Error(), found
	}
	rs, ok := srcFI.Sys().(*syscall.Stat_t)
	if !ok || uint64(rs.Dev) != uint64(sst.Dev) || uint64(rs.Ino) != uint64(sst.Ino) {
		return "", src + " changed while it was read (not the same file)", found
	}
	if err := fdFSTrusted(sfd, src, sst.Uid); err != nil {
		return "", err.Error(), found
	}
	cfd, err := unix.Openat(dfd, base+".sha256", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return "", "", false
	}
	if errors.Is(err, unix.ELOOP) {
		return "", src + ".sha256 is a symlink", found
	}
	if err != nil {
		return "", (&os.PathError{Op: "open", Path: src + ".sha256", Err: err}).Error(), found
	}
	f := os.NewFile(uintptr(cfd), src+".sha256")
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err.Error(), found
	}
	if !fi.Mode().IsRegular() || !trustedOwner(fi) {
		return "", src + ".sha256 is not a root-owned regular file", found
	}
	uid, _ := fileUID(fi)
	if err := fdFSTrusted(cfd, src+".sha256", uid); err != nil {
		return "", err.Error(), found
	}
	b, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return "", err.Error(), found
	}
	return string(b), "", found
}
