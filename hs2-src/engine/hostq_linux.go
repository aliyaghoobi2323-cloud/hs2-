//go:build linux

package engine

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// hostSysTCP is the guard's hostSys on a Linux link socket.
type hostSysTCP struct {
	raw syscall.RawConn
}

// newHostSys is the hostSys of the TCP socket under c (nil: none).
func newHostSys(c net.Conn) hostSys {
	tc := tcpUnder(c)
	if tc == nil {
		return nil
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		return nil
	}
	return &hostSysTCP{raw: raw}
}

func (s *hostSysTCP) setLowat(n int) error {
	var err error
	if e := s.raw.Control(func(fd uintptr) {
		err = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT, n)
	}); e != nil {
		return e
	}
	return err
}

func (s *hostSysTCP) notsent() (int, error) {
	var v int
	var err error
	if e := s.raw.Control(func(fd uintptr) { v, err = unix.IoctlGetInt(int(fd), unix.SIOCOUTQNSD) }); e != nil {
		return 0, e
	}
	return v, err
}

// waitBelow: RawConn.Write calls the check, and while it says no, sleeps in
// the netpoller until the socket polls writable. The check reads the unsent
// bytes, and only when there is no room asks tcp_poll (poll(2) with no
// timeout, as sendGate does: it arms the socket's wakeup, SOCK_NOSPACE,
// which no write set since the socket last polled writable); a socket that
// polls writable is room enough (its mark is half the cap, so this is
// under a quarter of it unsent, or the mark lags a new cap by a tick).
func (s *hostSysTCP) waitBelow(n int) error {
	var err error
	if e := s.raw.Write(func(fd uintptr) bool {
		v, e := unix.IoctlGetInt(int(fd), unix.SIOCOUTQNSD)
		if e != nil {
			err = e
			return true
		}
		return v < n || pollWritable(fd)
	}); e != nil {
		return e
	}
	return err
}
