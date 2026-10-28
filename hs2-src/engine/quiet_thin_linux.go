//go:build linux

package engine

import (
	"net"

	"golang.org/x/sys/unix"
)

// setThinTimeouts sets TCP_THIN_LINEAR_TIMEOUTS on a QUIET link's socket
// (QuietThin): while it has fewer than four segments in flight, its first
// six retransmission timeouts do not back off. False: not set.
func setThinTimeouts(tc *net.TCPConn) bool {
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

// thinTimeouts reads the option back (the test's).
func thinTimeouts(tc *net.TCPConn) (bool, error) {
	rc, err := tc.SyscallConn()
	if err != nil {
		return false, err
	}
	v := 0
	var gerr error
	rc.Control(func(fd uintptr) {
		v, gerr = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_THIN_LINEAR_TIMEOUTS)
	})
	return v != 0, gerr
}
