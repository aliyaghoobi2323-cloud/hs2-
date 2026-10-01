//go:build linux

package encap

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// forceSockBufs asks for n-byte send and receive buffers on socket s: forced
// past net.core.[rw]mem_max when the process may (root / CAP_NET_ADMIN), else
// the plain request, which the kernel caps at those limits.
func forceSockBufs(s, n int) {
	if unix.SetsockoptInt(s, unix.SOL_SOCKET, unix.SO_RCVBUFFORCE, n) != nil {
		unix.SetsockoptInt(s, unix.SOL_SOCKET, unix.SO_RCVBUF, n)
	}
	if unix.SetsockoptInt(s, unix.SOL_SOCKET, unix.SO_SNDBUFFORCE, n) != nil {
		unix.SetsockoptInt(s, unix.SOL_SOCKET, unix.SO_SNDBUF, n)
	}
}

// setSockBufs applies forceSockBufs to c's socket (best-effort).
func setSockBufs(c syscall.Conn, n int) {
	rc, err := c.SyscallConn()
	if err != nil {
		return
	}
	rc.Control(func(fd uintptr) { forceSockBufs(int(fd), n) })
}
