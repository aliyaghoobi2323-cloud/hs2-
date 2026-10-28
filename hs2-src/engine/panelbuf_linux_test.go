//go:build linux

package engine

import (
	"net"
	"syscall"
)

// sockRcvbuf is a TCP socket's SO_RCVBUF.
func sockRcvbuf(c net.Conn) (int, bool) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return 0, false
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		return 0, false
	}
	n, gerr := 0, error(nil)
	if err := rc.Control(func(fd uintptr) { n, gerr = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF) }); err != nil || gerr != nil {
		return 0, false
	}
	return n, true
}
