//go:build linux

package engine

import (
	"fmt"
	"net"
	"time"

	"golang.org/x/sys/unix"
)

// tcpBusy scores a link socket for a call chunk (HS2_MCONN_CALL_PICK,
// mconn_call.go): the bytes its kernel holds unsent, plus a large penalty
// while it is in loss recovery — a hole the receiver waits for, so whatever
// is written now arrives only after the retransmit.
func tcpBusy(tc *net.TCPConn) (int64, bool) {
	rc, err := tc.SyscallConn()
	if err != nil {
		return 0, false
	}
	var score int64
	ok := false
	rc.Control(func(fd uintptr) {
		info, e := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
		if e != nil {
			return
		}
		score = int64(info.Notsent_bytes)
		if info.Ca_state >= 3 || info.Lost > 0 || info.Retransmits > 0 { // Recovery, Loss
			score += mCallStalled
		}
		ok = true
	})
	return score, ok
}

// tcpTraceInfo: the socket's state for the per-sub trace (mconn_trace_subs.go).
func tcpTraceInfo(tc *net.TCPConn) string {
	rc, err := tc.SyscallConn()
	if err != nil {
		return ""
	}
	s := ""
	rc.Control(func(fd uintptr) {
		i, e := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
		if e != nil {
			return
		}
		s = fmt.Sprintf("rtt=%dms ca=%d unacked=%d lost=%d retr=%d notsent=%d cwnd=%d",
			i.Rtt/1000, i.Ca_state, i.Unacked, i.Lost, i.Retrans, i.Notsent_bytes, i.Snd_cwnd)
	})
	return s
}

// tcpSRTT: the socket's smoothed RTT (HS2_LB_CALL_LATE, lb_call_late.go).
func tcpSRTT(tc *net.TCPConn) (time.Duration, bool) {
	rc, err := tc.SyscallConn()
	if err != nil {
		return 0, false
	}
	var d time.Duration
	ok := false
	rc.Control(func(fd uintptr) {
		if i, e := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO); e == nil {
			d, ok = time.Duration(i.Rtt)*time.Microsecond, true
		}
	})
	return d, ok
}
