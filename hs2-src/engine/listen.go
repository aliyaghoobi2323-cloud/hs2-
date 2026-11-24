package engine

import (
	"context"
	"net"
	"syscall"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/freebind"
)

// listenReuse creates a TCP listener with SO_REUSEADDR so a fast service
// restart does not fail with "address already in use" while the kernel is still
// releasing the previous socket (TIME_WAIT). This was a real restart bug.
func ListenReuse(addr string) (net.Listener, error) {
	return listenReuseRcvBuf(addr, 0)
}

// ListenLink is ListenReuse for a LINK listener (the reverse edge's, the
// direct exit's). HS2_ZDU off: exactly ListenReuse. Key on: a new process
// accepts links on it before GO while its user listeners wait, and the old
// process stops accepting on it at handover start (zdu_gate.go).
func ListenLink(addr string) (net.Listener, error) {
	if zduOn.Load() {
		return zduListenKind(addr, 0, true)
	}
	return listenReuseRcvBufCold(addr, 0)
}

// listenReuseRcvBuf is ListenReuse that also sets (locks) the receive buffer to
// rcvbuf bytes on the listening socket when rcvbuf > 0. Set before listen, the
// buffer is inherited by every accepted connection and is in place when the
// SYN-ACK negotiates the window scale, so the window can open to it at once.
//
// The listener is plain TCP, never MPTCP. Go 1.24+ opens TCP listeners as
// MPTCP by default, and a connection accepted on one (even a client that
// speaks plain TCP) ignores tcp_notsent_lowat: a user whose app stops reading
// then holds its whole send buffer (4-7 MB) of kernel memory and keeps its
// link busy, and the stall guard never sees a blocked write. In the 300-link
// load test that was echo p99 8 s for everyone else on those links; plain TCP
// 0.5 s. (go.mod sets the same default for every other listener.)
func listenReuseRcvBuf(addr string, rcvbuf int) (net.Listener, error) {
	if zduOn.Load() {
		return zduListen(addr, rcvbuf) // ZDU S6: adopt/pausable (zdu_takeover.go)
	}
	return listenReuseRcvBufCold(addr, rcvbuf)
}

func listenReuseRcvBufCold(addr string, rcvbuf int) (net.Listener, error) {
	lc := net.ListenConfig{
		// b425: freebind for a specific address, so a boot never fails on
		// an IPv6 address still in DAD or an address added late.
		Control: freebind.Chain(func(network, address string, c syscall.RawConn) error {
			var serr error
			if err := c.Control(func(fd uintptr) {
				serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
				setRcvBuf(fd, rcvbuf)
			}); err != nil {
				return err
			}
			return serr
		}),
	}
	lc.SetMultipathTCP(false)
	ln, err := lc.Listen(context.Background(), "tcp", addr)
	if err == nil {
		freebind.Watch(addr)
	}
	return ln, err
}
