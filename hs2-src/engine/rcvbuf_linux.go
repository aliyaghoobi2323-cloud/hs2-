//go:build linux

package engine

import "syscall"

// setRcvBuf sets (and so locks) a socket's receive buffer to n bytes, bypassing
// net.core.rmem_max with SO_RCVBUFFORCE when the process may (root /
// CAP_NET_ADMIN) and falling back to a plain SO_RCVBUF otherwise. n <= 0 leaves
// the kernel default (autotuning) alone. Best effort: it never fails the caller.
func setRcvBuf(fd uintptr, n int) {
	if n <= 0 {
		return
	}
	if syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUFFORCE, n) == nil {
		return
	}
	_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, n)
}
