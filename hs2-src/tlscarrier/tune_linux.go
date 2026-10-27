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
	// slow links and turning it off cost 3-7x. The kernel checks the cap only
	// when a write starts a new segmentation-offload buffer, so a busy socket
	// still fills to about 64 KiB unsent; on a slow link engine's sendGate
	// holds it to half the cap and a burst (Linux 5.10+; the cap and a
	// burst before).
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

// SetCongestion sets the congestion control of a link socket that is up
// (a QUIET link's: engine.QuietCC); false: the kernel refused it (the
// socket keeps its own).
func SetCongestion(tc *net.TCPConn, cc string) bool {
	if tc == nil || cc == "" {
		return false
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		return false
	}
	ok := false
	rc.Control(func(fd uintptr) {
		ok = unix.SetsockoptString(int(fd), unix.IPPROTO_TCP, unix.TCP_CONGESTION, cc) == nil
	})
	return ok
}

// SetThinLinear turns on TCP_THIN_LINEAR_TIMEOUTS on tc: a thin stream
// (fewer than 4 packets in flight) keeps its retransmission timeout linear
// for its first 6 retries instead of doubling it (hs2's QUIET links, the
// R1 round-3 lead's note: light messages behind a loss). False: not set.
func SetThinLinear(tc *net.TCPConn) bool {
	if tc == nil {
		return false
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		return false
	}
	ok := false
	rc.Control(func(fd uintptr) {
		ok = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_THIN_LINEAR_TIMEOUTS, 1) == nil
	})
	return ok
}
