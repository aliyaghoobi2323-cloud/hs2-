//go:build linux

package tlscarrier

import (
	"net"

	"golang.org/x/sys/unix"
)

var (
	// NotSentLowat caps how many not-yet-sent bytes the kernel queues on a
	// link socket. Without it the send buffer autotunes to megabytes, which on
	// a lossy long-haul path is seconds of queueing delay for every packet
	// behind it. With it, the writer blocks early and smux's per-stream
	// scheduling holds the backlog instead, where one stream cannot bury the
	// others. In the lab 16-32 KiB were best; 64 KiB and up added latency on
	// slow links and turning it off cost 3-7x.
	NotSentLowat = 32 << 10
	// userTimeoutMs fails a link whose sent data stays unacknowledged this
	// long (a black-holed path), instead of letting it hang.
	UserTimeoutMs = 20000
)

// congestionControl is set on every link socket regardless of the system
// default: BBR keeps throughput on lossy, policed paths where cubic collapses,
// and does not fill the path's buffers the way loss-based algorithms do. If
// the kernel lacks it the socket keeps the system default.
var CongestionControl = "bbr"

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
		if NotSentLowat > 0 {
			unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT, NotSentLowat)
		}
		unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, UserTimeoutMs)
		if CongestionControl != "" {
			unix.SetsockoptString(int(fd), unix.IPPROTO_TCP, unix.TCP_CONGESTION, CongestionControl)
		}
	})
}
