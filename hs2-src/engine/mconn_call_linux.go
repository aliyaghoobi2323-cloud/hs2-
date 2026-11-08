//go:build linux

package engine

import (
	"net"

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
