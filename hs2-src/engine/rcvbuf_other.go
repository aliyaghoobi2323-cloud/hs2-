//go:build !linux

package engine

import "syscall"

// setRcvBuf sets a socket's receive buffer to n bytes (best effort; see the
// Linux version). n <= 0 leaves the default alone.
func setRcvBuf(fd uintptr, n int) {
	if n <= 0 {
		return
	}
	_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, n)
}
