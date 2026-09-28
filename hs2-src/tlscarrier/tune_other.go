//go:build !linux

package tlscarrier

import "net"

// Linux-only socket tuning knobs; present so callers build everywhere.
var (
	NotSentLowat      = 0
	UserTimeoutMs     = 0
	CongestionControl = ""
)

func tuneTCP(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
}
