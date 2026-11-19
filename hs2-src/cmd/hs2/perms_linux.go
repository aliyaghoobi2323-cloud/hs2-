package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// chmodNoFollow (b376) chmods the inode at p; see chmodBeneath (b378), with
// p's parent as the prefix.
func chmodNoFollow(p string, mode os.FileMode) (bool, error) {
	return chmodBeneath(filepath.Dir(p), filepath.Base(p), mode)
}

// chmodBeneath (b376, b378) chmods the inode at prefix/rel. The prefix (the
// system part, e.g. /var/lib or /etc) may hold symlinks, as on hosts with
// /var -> /data/var, but only when every directory on the way is root-owned
// and not group/world-writable (resolveTrustedDir: only root chose where it
// leads). rel (the part under hs2's own dirs) is opened once with
// O_PATH|O_NOFOLLOW from the prefix's fd with openat2 RESOLVE_NO_SYMLINKS|
// RESOLVE_BENEATH (Linux 5.6+), so a directory swapped for a symlink fails
// too; the type is checked on that fd and the chmod goes through
// /proc/self/fd/N, i.e. to exactly the inode opened. Gone since the scan:
// (false, nil). A symlink in rel, or an untrusted symlinked prefix: an error
// (not fixed, and the caller says so). Limits: on kernels without openat2 the
// prefix is opened as is and only rel's last component is protected, and
// without /proc nothing is chmodded.
func chmodBeneath(prefix, rel string, mode os.FileMode) (bool, error) {
	p := filepath.Join(prefix, rel)
	have2 := true
	dirHow := &unix.OpenHow{Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_NO_SYMLINKS}
	pfd, err := unix.Openat2(unix.AT_FDCWD, prefix, dirHow)
	if errors.Is(err, unix.ENOSYS) || (errors.Is(err, unix.EPERM) && !openat2Works()) {
		have2 = false
		pfd, err = unix.Open(prefix, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	} else if errors.Is(err, unix.ELOOP) {
		r, terr := resolveTrustedDir(prefix)
		if terr != nil {
			return false, fmt.Errorf("%s: not fixed: %s holds a symlink through a directory others can write (%v)", p, prefix, terr)
		}
		pfd, err = unix.Openat2(unix.AT_FDCWD, r, dirHow)
	}
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, &os.PathError{Op: "open", Path: prefix, Err: err}
	}
	defer unix.Close(pfd)
	flags := unix.O_PATH | unix.O_NOFOLLOW | unix.O_CLOEXEC
	var fd int
	if have2 {
		fd, err = unix.Openat2(pfd, rel, &unix.OpenHow{Flags: uint64(flags), Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_BENEATH})
	} else {
		fd, err = unix.Openat(pfd, rel, flags, 0)
	}
	if errors.Is(err, unix.ENOENT) {
		return false, nil // gone since the scan
	}
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EXDEV) {
		return false, fmt.Errorf("%s: not fixed: a symlink in the path (never followed)", p)
	}
	if err != nil {
		return false, &os.PathError{Op: "open", Path: p, Err: err}
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return false, &os.PathError{Op: "fstat", Path: p, Err: err}
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		return false, fmt.Errorf("%s: not fixed: it is a symlink (never followed)", p)
	}
	if err := unix.Chmod(fmt.Sprintf("/proc/self/fd/%d", fd), uint32(mode.Perm())); err != nil {
		return false, &os.PathError{Op: "chmod", Path: p, Err: err}
	}
	return true, nil
}

// openat2Works: false when the kernel lacks openat2 (seccomp may give EPERM).
func openat2Works() bool {
	fd, err := unix.Openat2(unix.AT_FDCWD, "/", &unix.OpenHow{Flags: unix.O_PATH | unix.O_CLOEXEC})
	if err != nil {
		return false
	}
	unix.Close(fd)
	return true
}
