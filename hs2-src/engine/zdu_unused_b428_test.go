//go:build linux

package engine

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func b428FreePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := l.Addr().String()
	l.Close()
	return a
}

// b428: a user listener the new process did not adopt is closed in the old
// process at GO (dials refused, the port bindable by another unit), an
// already accepted connection keeps working, and a revert rebinds it.
func TestZDUCloseUnusedTCP(t *testing.T) {
	zduKeyOn(t)
	addr := b428FreePort(t)
	ln, err := listenReuseRcvBuf(addr, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	acc := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			acc <- c
		}
	}()
	c1, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	s1 := <-acc
	defer s1.Close()
	ZDUPauseAccept()
	defer ZDUResumeAccept()
	got := ZDUCloseUnused([]string{ZDULnName(addr), "tcp:127.0.0.1:1", "bogus"})
	if len(got) != 1 || got[0] != ZDULnName(addr) {
		t.Fatalf("closed %v", got)
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Fatal("dial after close-unused succeeded (backlog still open)")
	}
	// Another unit can bind the port now.
	o, err := listenReuseRcvBufCold(addr, 0)
	if err != nil {
		t.Fatalf("port not free: %v", err)
	}
	// The established connection still works.
	c1.Write([]byte("x"))
	b := make([]byte, 1)
	s1.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := s1.Read(b); err != nil || b[0] != 'x' {
		t.Fatalf("established conn: %v", err)
	}
	// Revert while the other unit holds it: rebind fails, nothing else breaks.
	ok, failed := ZDURebindUnused([]string{ZDULnName(addr)})
	if len(ok) != 0 || failed[ZDULnName(addr)] == nil {
		t.Fatalf("rebind with the port taken: ok %v failed %v", ok, failed)
	}
	o.Close()
	ok, failed = ZDURebindUnused([]string{ZDULnName(addr)})
	if len(ok) != 1 || len(failed) != 0 {
		t.Fatalf("rebind: ok %v failed %v", ok, failed)
	}
	ZDUResumeAccept()
	c2, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	select {
	case s := <-acc:
		s.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("no accept after rebind")
	}
}

func TestZDUCloseUnusedUDP(t *testing.T) {
	zduKeyOn(t)
	p, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := p.LocalAddr().String()
	p.Close()
	pc, err := listenPacketUDP(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	got := make(chan string, 4)
	go func() {
		b := make([]byte, 16)
		for {
			n, _, err := pc.ReadFrom(b)
			if err != nil {
				return
			}
			got <- string(b[:n])
		}
	}()
	ZDUPauseAccept()
	defer ZDUResumeAccept()
	if c := ZDUCloseUnused([]string{ZDUPCName(addr)}); len(c) != 1 {
		t.Fatalf("closed %v", c)
	}
	o, err := net.ListenPacket("udp", addr)
	if err != nil {
		t.Fatalf("udp port not free: %v", err)
	}
	o.Close()
	if ok, f := ZDURebindUnused([]string{ZDUPCName(addr)}); len(ok) != 1 || len(f) != 0 {
		t.Fatalf("rebind %v %v", ok, f)
	}
	ZDUResumeAccept()
	cl, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	cl.Write([]byte("hi"))
	select {
	case s := <-got:
		if s != "hi" {
			t.Fatal(s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no read after rebind")
	}
}

// b428: a user bind that fails with EADDRINUSE waits in the background and
// serves once the port is free; other errors still fail at once.
func TestListenUserRetry(t *testing.T) {
	old := bindRetryPeriod
	bindRetryPeriod = 50 * time.Millisecond
	defer func() { bindRetryPeriod = old }()
	addr := b428FreePort(t)
	hold, err := listenReuseRcvBufCold(addr, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var served atomic.Pointer[net.Listener]
	var logs atomic.Int32
	logf := func(string, ...any) { logs.Add(1) }
	w, err := listenUserRetry(ctx, "80/tcp", logf, func() (net.Listener, error) { return listenReuseRcvBufCold(addr, 0) }, func(l net.Listener) { served.Store(&l) })
	if err != nil || !w {
		t.Fatalf("waiting %v err %v", w, err)
	}
	time.Sleep(200 * time.Millisecond)
	if served.Load() != nil {
		t.Fatal("served while the port is held")
	}
	hold.Close()
	dl := time.Now().Add(2 * time.Second)
	for served.Load() == nil && time.Now().Before(dl) {
		time.Sleep(10 * time.Millisecond)
	}
	l := served.Load()
	if l == nil {
		t.Fatal("never bound")
	}
	(*l).Close()
	if logs.Load() != 2 {
		t.Fatalf("%d log lines, want 2", logs.Load())
	}
	if _, err := listenUserRetry(ctx, "x", nil, func() (net.Listener, error) { return net.Listen("tcp", "256.0.0.1:1") }, func(net.Listener) {}); err == nil {
		t.Fatal("non-EADDRINUSE error swallowed")
	}
}
