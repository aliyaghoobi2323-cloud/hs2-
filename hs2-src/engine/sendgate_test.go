package engine

import (
	"crypto/tls"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// slowSink accepts one connection on a small receive buffer and reads it at
// about rate bytes/s, so its sender's socket backs up. The sender's socket
// has the link sockets' TCP_NOTSENT_LOWAT (32 KiB).
func slowSink(t *testing.T, rate int) *net.TCPConn {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.(*net.TCPConn).SetReadBuffer(64 << 10)
		buf := make([]byte, 4<<10)
		for {
			n, err := c.Read(buf)
			if err != nil {
				return
			}
			time.Sleep(time.Duration(n) * time.Second / time.Duration(rate))
		}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	tc := c.(*net.TCPConn)
	rc, _ := tc.SyscallConn()
	rc.Control(func(fd uintptr) {
		unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT, 32<<10)
	})
	return tc
}

// unsentOf is how many bytes c's socket holds not yet sent.
func unsentOf(c *net.TCPConn) int {
	rc, _ := c.SyscallConn()
	n := -1
	rc.Control(func(fd uintptr) {
		if ti, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO); err == nil {
			n = int(ti.Notsent_bytes)
		}
	})
	return n
}

// tellRate has g measure rate bytes/s for one sample.
func tellRate(g *sendGate, rate int64) {
	g.r = 0
	g.took(int(rate*int64(gateSample)/int64(time.Second)), gateSample)
}

// A record-sized writer on a backed-up socket: without the gate the socket
// fills to about 64 KiB unsent whatever TCP_NOTSENT_LOWAT says; with it, it
// holds under half the cap and a burst, and the writer still keeps up with
// what the link takes.
func TestSendGateHoldsUnsentToLowat(t *testing.T) {
	const rate = 1 << 20
	run := func(on bool) (most int, moved int64) {
		tc := slowSink(t, rate)
		g := newSendGate(tc)
		if on {
			tellRate(g, rate)
		}
		if g.on != on {
			t.Fatalf("gate on %v, want %v", g.on, on)
		}
		var sent atomic.Int64
		stop := make(chan struct{})
		go func() {
			rec := make([]byte, 1400)
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := g.Write(rec); err != nil {
					return
				}
				sent.Add(int64(len(rec)))
			}
		}()
		time.Sleep(300 * time.Millisecond)
		s0 := sent.Load()
		for i := 0; i < 100; i++ {
			time.Sleep(10 * time.Millisecond)
			most = max(most, unsentOf(tc))
		}
		moved = sent.Load() - s0
		close(stop)
		return most, moved
	}
	most, moved := run(true)
	t.Logf("gated: unsent at most %d B, %d B written in 1 s", most, moved)
	if most > 16<<10+gateBurst+2*1400 {
		t.Fatalf("gated socket held %d B unsent, want under 16 KiB and a burst", most)
	}
	if moved < rate/2 {
		t.Fatalf("gated writer moved %d B in about a second, the link takes %d", moved, rate)
	}
	most, _ = run(false)
	t.Logf("not gated: unsent at most %d B", most)
}

// The gate is only for slow links, and only where there is a socket to poll.
func TestSendGateOnlyForSlowLinks(t *testing.T) {
	g := newSendGate(slowSink(t, 1<<20))
	for _, c := range []struct {
		rate int64
		on   bool
	}{{1 << 20, true}, {int64(SendGateRate) - 1024, true}, {int64(SendGateRate) + 1024, false}, {1 << 30, false}, {64 << 10, true}} {
		tellRate(g, c.rate)
		if g.on != c.on {
			t.Errorf("rate %d: gate on %v, want %v", c.rate, g.on, c.on)
		}
	}
	// a sample is taken only once gateSample of writing is tallied, and
	// averaged with the one before: one fast write does not open the gate
	tellRate(g, 1<<20)
	g.took(1<<20, time.Millisecond)
	if !g.on {
		t.Error("one fast write turned the gate off before a sample")
	}
	defer func(r int) { SendGateRate = r }(SendGateRate)
	SendGateRate = 0
	if tellRate(g, 1<<10); g.on {
		t.Error("gate on with SendGateRate 0")
	}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	p := newSendGate(a)
	if tellRate(p, 1<<20); p.on {
		t.Error("a gate without a socket under it is on")
	}
}

// A write waiting at the gate ends once the socket is closed under it, as a
// blocked socket write would.
func TestSendGateEndsOnClose(t *testing.T) {
	tc := slowSink(t, 1<<10) // next to nothing
	g := newSendGate(tc)
	tellRate(g, 1<<20)
	done := make(chan error, 1)
	go func() {
		rec := make([]byte, 1400)
		for {
			if _, err := g.Write(rec); err != nil {
				done <- err
				return
			}
		}
	}()
	time.Sleep(300 * time.Millisecond)
	tc.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the write after close did not fail")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a write waiting at the gate did not end when its socket closed")
	}
}

// A link session over a slow socket finds the link slow from its own writes
// (meteredConn) and gates its socket: what the socket holds unsent stays near
// half TCP_NOTSENT_LOWAT, and the bulk stream still moves at the link's rate.
func TestSessionGatesASlowLink(t *testing.T) {
	const rate = 1 << 20
	tc := slowSink(t, rate)
	mtr := &linkMeter{statsPoll: make(chan struct{}, 1)}
	sess, _, err := newSession(tc, false, nil, mtr)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	// the sink never answers, so each stream has the window a peer assumes
	// before any update (256 KiB): eight keep the socket backed up throughout
	for i := 0; i < 8; i++ {
		st, err := sess.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		go io.Copy(st, zeroReader{})
	}
	time.Sleep(300 * time.Millisecond)
	w0 := mtr.wrBytes.Load()
	most := 0
	for i := 0; i < 50; i++ {
		time.Sleep(10 * time.Millisecond)
		most = max(most, unsentOf(tc))
	}
	moved := mtr.wrBytes.Load() - w0
	t.Logf("unsent at most %d B, %d B written in 0.5 s", most, moved)
	if most > 16<<10+gateBurst+2*1400 {
		t.Fatalf("the session's socket held %d B unsent, want under 16 KiB and a burst", most)
	}
	if moved == 0 {
		t.Fatal("the stream moved nothing")
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// slowTLSSink is slowSink under TLS, as a link runs: the sink reads about
// rate bytes/s until stop is set, then nothing more (the path stops taking
// data: its window closes, the sender's socket stays full). peer gets the
// sink's TLS conn once it stops.
func slowTLSSink(t *testing.T, rate int, stop *atomic.Bool, peer chan<- *tls.Conn) *tls.Conn {
	t.Helper()
	cert := testCert(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.(*net.TCPConn).SetReadBuffer(64 << 10)
		tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}})
		buf := make([]byte, 4<<10)
		for !stop.Load() {
			n, err := tc.Read(buf)
			if err != nil {
				return
			}
			time.Sleep(time.Duration(n) * time.Second / time.Duration(rate))
		}
		peer <- tc
		<-done
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	rc, _ := c.(*net.TCPConn).SyscallConn()
	rc.Control(func(fd uintptr) {
		unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT, 32<<10)
	})
	tc := tls.Client(c, &tls.Config{InsecureSkipVerify: true})
	if err := tc.Handshake(); err != nil {
		t.Fatal(err)
	}
	return tc
}

// A gated link whose path stops taking data: its writer waits at the gate,
// outside the TLS conn. Closing the link (the health loop drops it, a verdict)
// or the other end ending it (watchConn.onErr) ends the session at once, as
// when the writer waited inside the TLS conn's Write -- not after the 5 s
// tls.Conn.Close gives its close_notify behind the full socket.
func TestSendGateCloseEndsAWait(t *testing.T) {
	for _, byPeer := range []bool{false, true} {
		var stop atomic.Bool
		peer := make(chan *tls.Conn, 1)
		tc := slowTLSSink(t, 1<<20, &stop, peer)
		mtr := &linkMeter{statsPoll: make(chan struct{}, 1)}
		sess, _, err := newSession(tc, false, nil, mtr)
		if err != nil {
			t.Fatal(err)
		}
		defer sess.Close()
		for i := 0; i < 8; i++ {
			st, err := sess.OpenStream()
			if err != nil {
				t.Fatal(err)
			}
			go io.Copy(st, zeroReader{})
		}
		time.Sleep(500 * time.Millisecond) // the gate turns on
		stop.Store(true)
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if w := mtr.wrStart.Load(); w != 0 && ctrlNow()-w >= int64(300*time.Millisecond) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the writer never waited on the full socket")
			}
		}
		t0 := time.Now()
		if byPeer {
			(<-peer).CloseWrite() // its close_notify: our reader ends
			for !sess.IsClosed() && time.Since(t0) < 10*time.Second {
				time.Sleep(time.Millisecond)
			}
		} else {
			sess.Close() // the session, then its conn
		}
		if took := time.Since(t0); took > time.Second {
			t.Errorf("ended by the peer %v: the session took %s to end", byPeer, took)
		}
	}
}
