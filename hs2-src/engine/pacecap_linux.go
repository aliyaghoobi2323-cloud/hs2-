//go:build linux

package engine

import (
	"net"

	"golang.org/x/sys/unix"
)

// setMaxPacing caps the socket's pacing rate at rate bytes/s
// (SO_MAX_PACING_RATE); 0 lifts the cap. The pacing net's (pacenet.go) and
// the window core's lab experiment (WcorePaceCap).
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

// linkCapSys is C4-lite's capSys on a Linux link socket (sendcap.go): its
// TCP_INFO (tcpStats) and SO_MAX_PACING_RATE (wcPaceSet: setMaxPacing, or a
// test's).
type linkCapSys struct{ tc *net.TCPConn }

// newCapSys is the capSys of a link socket (nil: none).
func newCapSys(tc *net.TCPConn) capSys {
	if tc == nil {
		return nil
	}
	return linkCapSys{tc}
}

func (s linkCapSys) info() (tcpStat, bool) { return tcpStats(s.tc) }

func (s linkCapSys) setPacing(rate uint64) bool { return (*wcPaceSet.Load())(s.tc, rate) }
