//go:build linux || darwin || freebsd

package engine

import (
	"errors"
	"net"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// acmePeekRequestLine peeks (MSG_PEEK: nothing is consumed) at a fresh TCP
// connection until its first bytes either cannot be an ACME challenge request
// (false at once), form a complete challenge request line (returned, true), or
// the timeout ends (false). The read deadline is always cleared on return.
func acmePeekRequestLine(c net.Conn, timeout time.Duration) ([]byte, bool) {
	sc, ok := c.(syscall.Conn)
	if !ok {
		return nil, false
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return nil, false
	}
	c.SetReadDeadline(time.Now().Add(timeout))
	defer c.SetReadDeadline(time.Time{})
	buf := make([]byte, acmeMaxLine)
	var (
		line []byte
		hit  bool
	)
	err = rc.Read(func(fd uintptr) bool {
		n, _, e := unix.Recvfrom(int(fd), buf, unix.MSG_PEEK|unix.MSG_DONTWAIT)
		if e != nil {
			if errors.Is(e, unix.EAGAIN) || errors.Is(e, unix.EINTR) {
				return false // wait for data (or the deadline)
			}
			return true // a socket error: let the normal path see it
		}
		if n == 0 {
			return true // EOF
		}
		switch st, l := acmeClassify(buf[:n]); st {
		case acmeNo:
			return true
		case acmeLine:
			line, hit = append([]byte(nil), buf[:l]...), true
			return true
		}
		// Still a possible challenge: the poller wakes us on the next arrival
		// (edge-triggered) or the deadline.
		return false
	})
	_ = err // timeout or close: not ours (the normal path sees the state)
	return line, hit
}
