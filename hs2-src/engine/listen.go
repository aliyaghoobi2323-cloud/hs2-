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
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var serr error
			if err := c.Control(func(fd uintptr) {
				serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
			}); err != nil {
				return err
			}
			return serr
		},
	}
	return lc.Listen(context.Background(), "tcp", addr)
}
