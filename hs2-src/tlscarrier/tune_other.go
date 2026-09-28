//go:build !linux

package tlscarrier

import "net"

func tuneTCP(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
}
