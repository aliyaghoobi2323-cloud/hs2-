//go:build linux

package engine

import (
	"net"

	"golang.org/x/sys/unix"
)

// tcpStats reads one TCP_INFO snapshot of a link socket: the cumulative
// retransmits (path loss, used by the degrade logic) and the kernel's "chrono"
// counters (Linux ≥ 4.10), which say how long the socket was busy sending and
// how much of that it was limited by the peer's receive window or by its own
// send buffer. The pool uses the receive-window share to tell "the network will
// not take more" (more links may help) from "the receiver is slow" (they won't).
func tcpStats(tc *net.TCPConn) (tcpStat, bool) {
	if tc == nil {
		return tcpStat{}, false
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		return tcpStat{}, false
	}
	var st tcpStat
	var ok bool
	rc.Control(func(fd uintptr) {
		info, e := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
		if e != nil {
			return
		}
		st = tcpStat{
			retrans:      uint64(info.Total_retrans),
			busyUs:       info.Busy_time,
			rwndUs:       info.Rwnd_limited,
			sndbufUs:     info.Sndbuf_limited,
			deliveryRate: info.Delivery_rate,
			notsent:      info.Notsent_bytes,
			chronoValid:  info.Busy_time > 0,
		}
		ok = true
	})
	return st, ok
}

// retransmits is kept for the exit's control-channel pong.
func retransmits(tc *net.TCPConn) (uint64, bool) {
	st, ok := tcpStats(tc)
	return st.retrans, ok
}
