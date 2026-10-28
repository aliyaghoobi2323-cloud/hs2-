//go:build linux

package engine

import "syscall"

// setMaxSeg sets a socket's TCP_MAXSEG before it connects, so its SYN
// announces that segment size and the other end sends in it (best effort).
func setMaxSeg(fd uintptr, n int) {
	if n > 0 {
		_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_MAXSEG, n)
	}
}
