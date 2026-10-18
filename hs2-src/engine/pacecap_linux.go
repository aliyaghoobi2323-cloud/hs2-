//go:build linux

package engine

import (
	"net"

	"golang.org/x/sys/unix"
)

// setMaxPacing caps the socket's pacing rate at rate bytes/s
// (SO_MAX_PACING_RATE); 0 lifts the cap. The window core's lab experiment
// only (WcorePaceCap).
func setMaxPacing(tc *net.TCPConn, rate uint64) bool {
	if tc == nil {
		return false
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		return false
	}
	v := int(int32(-1)) // ~0U: no cap
	if rate > 0 {
		v = int(min(rate, 1<<31-1))
	}
	ok := false
	rc.Control(func(fd uintptr) {
		ok = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MAX_PACING_RATE, v) == nil
	})
	return ok
}
