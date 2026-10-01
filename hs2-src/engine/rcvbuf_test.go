//go:build linux

package engine

import (
	"context"
	"net"
	"syscall"
	"testing"
)

// rcvBufOf reads a TCP connection's SO_RCVBUF (the kernel reports twice what was
// set: it reserves the other half for bookkeeping).
func rcvBufOf(t *testing.T, c net.Conn) int {
	t.Helper()
	rc, err := c.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var v int
	var serr error
	if err := rc.Control(func(fd uintptr) {
		v, serr = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF)
	}); err != nil {
		t.Fatal(err)
	}
	if serr != nil {
		t.Fatal(serr)
	}
	return v
}

// The forwarding connection that rides the tun must get the large, locked
// receive buffer on BOTH ends: the exit's accepted socket (inherited from the
// listener) and the edge's dialed socket. A plain listener/dialer keeps the
// kernel default. The point is the window: with the autotuned buffer the
// out-of-order data the tunnel delivers overflowed it and the kernel clamped the
// window to two segments (the field's rwnd-limited ~0.5 Mbit/s per connection).
func TestForwarderTunLegRcvBuf(t *testing.T) {
	const want = 4 << 20

	// Exit side: listener with the buffer -> accepted conns inherit it.
	ln, err := listenReuseRcvBuf("127.0.0.1:0", want)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	acc := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); acc <- c }()

	// Edge side: dialer with the buffer.
	dc, err := forwardDialer(want).DialContext(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer dc.Close()
	ac := <-acc
	defer ac.Close()

	if got := rcvBufOf(t, ac); got < want {
		t.Fatalf("exit (accepted) SO_RCVBUF=%d, want >= %d", got, want)
	}
	if got := rcvBufOf(t, dc); got < want {
		t.Fatalf("edge (dialed) SO_RCVBUF=%d, want >= %d", got, want)
	}

	// Plain listener/dialer (rcvbuf 0): the default, well under the tun value —
	// the user and panel legs are left to the kernel.
	ln2, err := listenReuseRcvBuf("127.0.0.1:0", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()
	go func() { c, _ := ln2.Accept(); acc <- c }()
	dc2, err := forwardDialer(0).DialContext(context.Background(), "tcp", ln2.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer dc2.Close()
	ac2 := <-acc
	defer ac2.Close()
	if got := rcvBufOf(t, ac2); got >= want {
		t.Fatalf("a local-leg listener got the tun buffer (%d); only the tun leg should", got)
	}
}

// The default is 4 MB; HS2_TUN_RCVBUF overrides it and 0 restores autotuning.
func TestDgTunRcvBufDefault(t *testing.T) {
	if dgTunRcvBuf != 4<<20 {
		t.Skipf("HS2_TUN_RCVBUF is set in this environment (%d)", dgTunRcvBuf)
	}
}
