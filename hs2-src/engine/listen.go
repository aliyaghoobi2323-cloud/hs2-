package engine

import (
	"context"
	"net"
	"syscall"
)

// listenReuse creates a TCP listener with SO_REUSEADDR so a fast service
// restart does not fail with "address already in use" while the kernel is still
// releasing the previous socket (TIME_WAIT). This was a real restart bug.
func ListenReuse(addr string) (net.Listener, error) {
	return listenReuseRcvBuf(addr, 0)
}

// listenReuseRcvBuf is ListenReuse that also sets (locks) the receive buffer to
// rcvbuf bytes on the listening socket when rcvbuf > 0. Set before listen, the
// buffer is inherited by every accepted connection and is in place when the
// SYN-ACK negotiates the window scale, so the window can open to it at once.
func listenReuseRcvBuf(addr string, rcvbuf int) (net.Listener, error) {
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var serr error
			if err := c.Control(func(fd uintptr) {
				serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
				setRcvBuf(fd, rcvbuf)
			}); err != nil {
				return err
			}
			return serr
		},
	}
	return lc.Listen(context.Background(), "tcp", addr)
}
