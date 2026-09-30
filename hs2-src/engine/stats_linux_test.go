//go:build linux

package engine

import (
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// loopbackTCP returns both ends of a loopback TCP connection.
func loopbackTCP(t *testing.T) (client, server *net.TCPConn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	acc := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		acc <- c
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	s := <-acc
	if s == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { c.Close(); s.Close() })
	return c.(*net.TCPConn), s.(*net.TCPConn)
}

func setSockOpt(t *testing.T, c *net.TCPConn, level, opt, v int) {
	t.Helper()
	rc, err := c.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var serr error
	if err := rc.Control(func(fd uintptr) { serr = unix.SetsockoptInt(int(fd), level, opt, v) }); err != nil {
		t.Fatal(err)
	}
	if serr != nil {
		t.Fatalf("setsockopt %d/%d: %v", level, opt, serr)
	}
}

// The exit's link meter on a real socket tuned like a link (TCP_NOTSENT_LOWAT,
// as tlscarrier sets it): behind a slow edge reader its writer reads mostly
// blocked, and the socket's chrono counters put that wait on the receiver's
// window (so the edge's rwnd exclusion does not mistake a slow reader for a
// full path); with a reader that keeps up it reads almost never blocked. The
// cumulative TCP_INFO counters tcpStats returns never go backwards.
func TestExitMeterBlockedBehindSlowReaderTCP(t *testing.T) {
	run := func(t *testing.T, slow bool) (blocked, rwndShare float64) {
		edge, exit := loopbackTCP(t)
		setSockOpt(t, exit, unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT, 32<<10)
		setSockOpt(t, edge, unix.SOL_SOCKET, unix.SO_RCVBUF, 64<<10) // a slow reader backs up quickly
		go func() {
			buf := make([]byte, 4<<10)
			for {
				if slow {
					time.Sleep(5 * time.Millisecond) // ~800 KB/s
				}
				if _, err := edge.Read(buf); err != nil {
					return
				}
			}
		}()
		mtr := &linkMeter{}
		w := &meteredConn{Conn: exit, m: mtr}
		frame := make([]byte, 16<<10)
		var prev tcpStat
		sample := func() tcpStat {
			st, ok := tcpStats(exit)
			if !ok {
				t.Fatal("tcpStats unavailable on a Linux TCP socket")
			}
			if st.retrans < prev.retrans || st.busyUs < prev.busyUs || st.rwndUs < prev.rwndUs || st.sndbufUs < prev.sndbufUs {
				t.Fatalf("TCP_INFO counters went backwards: %+v after %+v", st, prev)
			}
			prev = st
			return st
		}
		start := time.Now()
		var b0 int64
		var t0 time.Time
		var s0 tcpStat
		for time.Since(start) < 1500*time.Millisecond {
			if _, err := w.Write(frame); err != nil {
				t.Fatal(err)
			}
			time.Sleep(2 * time.Millisecond) // the application offers ~8 MB/s
			st := sample()
			if t0.IsZero() && time.Since(start) >= 500*time.Millisecond { // past the initial buffer fill
				b0, t0, s0 = mtr.wrBlocked.Load(), time.Now(), st
			}
		}
		blocked = float64(mtr.wrBlocked.Load()-b0) / float64(time.Since(t0))
		end := sample()
		if !end.chronoValid {
			t.Skip("kernel without TCP_INFO chrono counters")
		}
		if end.busyUs > s0.busyUs {
			rwndShare = float64(end.rwndUs-s0.rwndUs) / float64(end.busyUs-s0.busyUs)
		}
		return blocked, rwndShare
	}
	t.Run("slow reader", func(t *testing.T) {
		blocked, rwnd := run(t, true)
		t.Logf("blocked %.0f%%, rwnd-limited %.0f%% of busy", blocked*100, rwnd*100)
		if blocked < 0.6 {
			t.Fatalf("writer behind a slow reader reads %.0f%% blocked, want > 60%%", blocked*100)
		}
		if rwnd < rwndShareMax {
			t.Fatalf("a slow reader's wait is only %.0f%% rwnd-limited: the rwnd exclusion would not see it", rwnd*100)
		}
	})
	t.Run("fast reader", func(t *testing.T) {
		blocked, _ := run(t, false)
		t.Logf("blocked %.0f%%", blocked*100)
		if blocked > 0.2 {
			t.Fatalf("writer with a reader that keeps up reads %.0f%% blocked, want < 20%%", blocked*100)
		}
	})
}
