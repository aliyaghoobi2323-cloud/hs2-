//go:build linux

package engine

import (
	"net"

	"golang.org/x/sys/unix"
)

// newOutQ reads what the link socket holds unacknowledged, in flight and
// unsent (SIOCOUTQ): the in-flight budget's measure (budget.go); nil: no
// socket to read, or a kernel without the call (one try at the start).
func newOutQ(tc *net.TCPConn) func() (int, error) {
	if tc == nil {
		return nil
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		return nil
	}
	f := func() (int, error) {
		var v int
		var err error
		if e := raw.Control(func(fd uintptr) { v, err = unix.IoctlGetInt(int(fd), unix.SIOCOUTQ) }); e != nil {
			return 0, e
		}
		return v, err
	}
	if _, err := f(); err != nil && hostqUnsupported(err) {
		return nil
	}
	return f
}
