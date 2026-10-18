//go:build linux

package engine

import (
	"net"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

// tcpInfoRead reads a socket's TCP_INFO and the length the kernel filled
// (-1: unknown): getsockoptTCPInfo. A test stands in an older kernel for it
// (atomic: the runners other tests left read it meanwhile).
var tcpInfoRead atomic.Pointer[func(fd int) (*unix.TCPInfo, int, error)]

func init() {
	f := getsockoptTCPInfo
	tcpInfoRead.Store(&f)
}

// getsockoptTCPInfo is unix.GetsockoptTCPInfo that keeps the length the
// kernel returned: a kernel fills as much of struct tcp_info as it knows,
// and the fields past that read 0 here. (32-bit kernels before 4.3 have no
// direct getsockopt call: ENOSYS, and the length is not known; such a
// kernel has no field of 5.4 anyway.)
func getsockoptTCPInfo(fd int) (*unix.TCPInfo, int, error) {
	var info unix.TCPInfo
	n := uint32(unix.SizeofTCPInfo)
	_, _, e := unix.Syscall6(unix.SYS_GETSOCKOPT, uintptr(fd), unix.IPPROTO_TCP, unix.TCP_INFO,
		uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&n)), 0)
	if e == unix.ENOSYS {
		p, err := unix.GetsockoptTCPInfo(fd, unix.IPPROTO_TCP, unix.TCP_INFO)
		return p, -1, err
	}
	if e != 0 {
		return nil, 0, e
	}
	return &info, int(n), nil
}

// tcpInfoHasOoo says whether a TCP_INFO of n bytes has tcpi_rcv_ooopack in
// it (Linux 5.4 on). Before it the field reads 0 whatever arrived behind a
// hole: the window core must not take that for a clean path.
func tcpInfoHasOoo(n int) bool {
	return n >= int(unsafe.Offsetof(unix.TCPInfo{}.Rcv_ooopack)+unsafe.Sizeof(unix.TCPInfo{}.Rcv_ooopack))
}

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
		info, n, e := (*tcpInfoRead.Load())(int(fd))
		if e != nil || info == nil {
			return
		}
		st = tcpStat{
			retrans:       uint64(info.Total_retrans),
			busyUs:        info.Busy_time,
			rwndUs:        info.Rwnd_limited,
			sndbufUs:      info.Sndbuf_limited,
			deliveryRate:  info.Delivery_rate,
			notsent:       info.Notsent_bytes,
			rtt:           info.Rtt,
			rttVar:        info.Rttvar,
			minRTT:        info.Min_rtt,
			chronoValid:   info.Busy_time > 0,
			segsOut:       info.Data_segs_out,
			segsIn:        info.Data_segs_in,
			rcvOOO:        info.Rcv_ooopack,
			oooOK:         tcpInfoHasOoo(n),
			bytesReceived: info.Bytes_received,
			bytesAcked:    info.Bytes_acked,
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
