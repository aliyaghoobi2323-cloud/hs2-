//go:build linux

package engine

import (
	"net"
	"syscall"
	"testing"
	"time"
)

// sockErr: the socket's pending error (an RST after the peer's FIN shows
// only here: reads keep returning EOF).
func sockErr(c *net.TCPConn) error {
	rc, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var se int
	rc.Control(func(fd uintptr) { se, err = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_ERROR) })
	if err != nil {
		return err
	}
	if se != 0 {
		return syscall.Errno(se)
	}
	return nil
}

// (hc183b) the edge gives up a half-closed connection (halfCloseIdle: the
// user sent its FIN, the target never answers nor closes): the exit learns it
// at once (an RST record; a raw first sub / the one-stream path: smux's second
// FIN, PeerGone) and resets the target, instead of holding its side for its
// own, longer idle limit.
func TestHalfCloseIdleReachesExit(t *testing.T) {
	oldIdle, oldPoll := halfCloseIdle.Load(), halfClosePoll.Load()
	const idle = 6 * time.Second // the edge's; the exit's is 9 s
	halfCloseIdle.Store(int64(idle))
	halfClosePoll.Store(int64(200 * time.Millisecond))
	t.Cleanup(func() { halfCloseIdle.Store(oldIdle); halfClosePoll.Store(oldPoll) })
	hcPlanes(t, func(t *testing.T) {
		ended := make(chan time.Duration, 4)
		addr := startTunnelTarget(t, 3, 3, hcTarget(t, func(c *net.TCPConn) {
			req, _ := readAllT(c, 20*time.Second)
			if string(req) != "HOLD" {
				return
			}
			t0 := time.Now() // the user's FIN came; hold, never answer or close
			for time.Since(t0) < 30*time.Second {
				// reads give EOF (the FIN) even after an RST: SO_ERROR shows it
				time.Sleep(100 * time.Millisecond)
				if sockErr(c) != nil {
					break
				}
			}
			ended <- time.Since(t0)
		}))
		waitHC(t, addr)
		idles, gone := halfCloseIdles.Load(), halfCloseGone.Load()
		c := hcDial(t, addr)
		defer c.Close()
		c.Write([]byte("HOLD"))
		c.CloseWrite()
		var d time.Duration
		select {
		case d = <-ended:
		case <-time.After(25 * time.Second):
			t.Fatal("the target was never released")
		}
		t.Logf("target released %v after the user's FIN; idles +%d gone +%d", d.Round(time.Millisecond),
			halfCloseIdles.Load()-idles, halfCloseGone.Load()-gone)
		if d < idle-time.Second || d >= idle+idle/2-500*time.Millisecond {
			t.Fatalf("target released after %v: want the edge's give-up (%v) to reach the exit before its own %v", d, idle, idle+idle/2)
		}
		if _, err := readAllT(c, 5*time.Second); err == nil {
			t.Fatal("the user got a clean FIN, want a reset")
		}
	})
}
