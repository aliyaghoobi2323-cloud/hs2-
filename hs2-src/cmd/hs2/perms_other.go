//go:build !linux

package main

import (
	"io/fs"
	"os"
	"path/filepath"
)

// chmodNoFollow: the portable fallback (Lstat, then Chmod; a small race).
func chmodNoFollow(p string, mode os.FileMode) (bool, error) {
	fi, err := os.Lstat(p)
	if err != nil || fi.Mode()&fs.ModeSymlink != 0 {
		return false, nil
	}
	return true, os.Chmod(p, mode)
}

// chmodBeneath: the portable fallback.
func chmodBeneath(prefix, rel string, mode os.FileMode) (bool, error) {
	return chmodNoFollow(filepath.Join(prefix, rel), mode)
}

func openat2Works() bool { return false }
