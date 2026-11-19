//go:build unix

package main

import (
	"os"
	"syscall"
)

// fileUID: the owner of fi (from Lstat/Stat/fstat), ok=false when unknown.
func fileUID(fi os.FileInfo) (uint32, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}
