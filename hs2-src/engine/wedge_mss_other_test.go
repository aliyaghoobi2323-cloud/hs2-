//go:build !linux

package engine

import "syscall"

// wedgeMSS: the segment size is left to the system off Linux.
func wedgeMSS(_, _ string, _ syscall.RawConn) error { return nil }
