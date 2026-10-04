//go:build linux

package engine

import (
	"net"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// sockProto reads a socket's SO_PROTOCOL: IPPROTO_TCP (6) or IPPROTO_MPTCP (262).
func sockProto(t *testing.T, sc syscall.Conn) int {
	t.Helper()
	rc, err := sc.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var v int
	var serr error
	if err := rc.Control(func(fd uintptr) {
		v, serr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_PROTOCOL)
	}); err != nil {
		t.Fatal(err)
	}
	if serr != nil {
		t.Fatal(serr)
	}
	return v
}

// Every hs2 listener (carrier, user ports, dgtun) is plain TCP, also where the
// build's default is MPTCP (GODEBUG=multipathtcp=1, as Go 1.24+ has it): an
// accepted MPTCP socket ignores tcp_notsent_lowat, so a user whose app stops
// reading holds its whole send buffer and the stall guard never sees it.
// (Run with GODEBUG=multipathtcp=1 to check the helper on its own; go.mod
// turns the default off for the rest.)
func TestListenersArePlainTCP(t *testing.T) {
	for _, rcvbuf := range []int{0, 1 << 20} {
		ln, err := listenReuseRcvBuf("127.0.0.1:0", rcvbuf)
		if err != nil {
			t.Fatal(err)
		}
		if p := sockProto(t, ln.(*net.TCPListener)); p != unix.IPPROTO_TCP {
			ln.Close()
			t.Fatalf("listener (rcvbuf %d) is protocol %d, want plain TCP (%d)", rcvbuf, p, unix.IPPROTO_TCP)
		}
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			ln.Close()
			t.Fatal(err)
		}
		a, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		p := sockProto(t, a.(*net.TCPConn))
		a.Close()
		c.Close()
		ln.Close()
		if p != unix.IPPROTO_TCP {
			t.Fatalf("accepted connection (rcvbuf %d) is protocol %d, want plain TCP (%d)", rcvbuf, p, unix.IPPROTO_TCP)
		}
	}
}
