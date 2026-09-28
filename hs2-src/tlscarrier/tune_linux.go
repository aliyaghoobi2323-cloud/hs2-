//go:build linux

package tlscarrier

import (
	"net"

	"golang.org/x/sys/unix"
)

const (
	// notSentLowat caps how many not-yet-sent bytes the kernel queues on a
	// link socket. Without it the send buffer autotunes to megabytes, which on
	// a lossy long-haul path is seconds of queueing delay for every packet
	// behind it. With it, the writer blocks early and the tunnel's own short
	// queue (and the inner TCP) sees the congestion instead.
	notSentLowat = 128 << 10
	// userTimeoutMs fails a link whose sent data stays unacknowledged this
	// long (a black-holed path), instead of letting it hang.
	userTimeoutMs = 20000
)

// tuneTCP applies the latency-oriented socket options to a link's TCP conn.
func tuneTCP(c net.Conn) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return
	}
	tc.SetNoDelay(true)
	rc, err := tc.SyscallConn()
	if err != nil {
		return
	}
	rc.Control(func(fd uintptr) {
		unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT, notSentLowat)
		unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, userTimeoutMs)
	})
}
