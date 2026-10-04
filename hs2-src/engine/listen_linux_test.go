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

// Every hs2 listener (carrier, user ports, dgtun) is plain TCP even where the
// default is MPTCP: the test sets GODEBUG=multipathtcp=1 (Go 1.24+'s default
// for listeners), so it guards the explicit SetMultipathTCP(false). An
// accepted MPTCP socket ignores tcp_notsent_lowat, so a user whose app stops
// reading would hold its whole send buffer and the stall guard never see it.
// Only the listener's protocol tells: a plain-TCP client accepted on an MPTCP
// listener still reports IPPROTO_TCP (and still ignores the lowat).
func TestListenersArePlainTCP(t *testing.T) {
	t.Setenv("GODEBUG", "multipathtcp=1")
	for _, rcvbuf := range []int{0, 1 << 20} {
		ln, err := listenReuseRcvBuf("127.0.0.1:0", rcvbuf)
		if err != nil {
			t.Fatal(err)
		}
		p := sockProto(t, ln.(*net.TCPListener))
		ln.Close()
		if p != unix.IPPROTO_TCP {
			t.Fatalf("listener (rcvbuf %d) is protocol %d, want plain TCP (%d)", rcvbuf, p, unix.IPPROTO_TCP)
		}
	}
}

// go.mod turns the default off for every other listener (noise, reality, the
// cover backend): a plain net.Listen is TCP too.
func TestModuleDefaultIsPlainTCP(t *testing.T) {
	t.Setenv("GODEBUG", "")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := sockProto(t, ln.(*net.TCPListener))
	ln.Close()
	if p != unix.IPPROTO_TCP {
		t.Fatalf("net.Listen is protocol %d, want plain TCP (%d): go.mod's godebug multipathtcp=0 is missing", p, unix.IPPROTO_TCP)
	}
}
