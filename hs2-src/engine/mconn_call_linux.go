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

// tcpAckState (HS2_MCONN_STRIPE_KEEPSTALL, hs115): the socket's smoothed RTT,
// its cumulative bytes acked, the bytes it has in flight and whether it is in
// RTO loss recovery (ok=false: unknown). A path dark from this side keeps
// unacked>0 with bytes_acked frozen. (last_ack_recv is no proof: with only
// the forward direction dark the peer's own segments keep arriving and
// refresh it — hs115 lab D1.) A sub that is slow only because the peer does
// not read it has nothing in flight, or its window probes are acked.
func tcpAckState(tc *net.TCPConn) (srtt time.Duration, acked uint64, unacked uint32, rtoLoss, ok bool) {
	rc, err := tc.SyscallConn()
	if err != nil {
		return 0, 0, 0, false, false
	}
	rc.Control(func(fd uintptr) {
		if i, e := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO); e == nil {
			srtt, acked, unacked, ok = time.Duration(i.Rtt)*time.Microsecond, i.Bytes_acked, i.Unacked, true
			rtoLoss = i.Ca_state == 4 && i.Backoff > 0 // TCP_CA_Loss after an RTO
		}
	})
	return
}

// tcpDarkInfo (HS2_MCONN_DARKCALL, mconn_darkcall.go): cumulative bytes
// acked, bytes in flight, the RTO backoff exponent and the socket's total
// retransmits (ok=false: unknown).
func tcpDarkInfo(tc *net.TCPConn) (acked uint64, unacked uint32, backoff uint8, retrans uint32, ok bool) {
	rc, err := tc.SyscallConn()
	if err != nil {
		return
	}
	rc.Control(func(fd uintptr) {
		if i, e := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO); e == nil {
			acked, unacked, backoff, retrans, ok = i.Bytes_acked, i.Unacked, i.Backoff, i.Total_retrans, true
		}
	})
	return
}

// tcpOutq (dl243): the bytes the link socket holds unacked + unsent
// (SIOCOUTQ); ok=false: unknown.
func tcpOutq(tc *net.TCPConn) (n uint64, ok bool) {
	rc, err := tc.SyscallConn()
	if err != nil {
		return 0, false
	}
	rc.Control(func(fd uintptr) {
		if v, e := unix.IoctlGetInt(int(fd), unix.TIOCOUTQ); e == nil && v >= 0 {
			n, ok = uint64(v), true
		}
	})
	return
}
