//go:build linux

package engine

import (
	"io"
	"net"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// TestLowatAdaptsLinux: TestLowatAdapts on a real link socket: its
// TCP_INFO (the delivery rate, bytes acknowledged and segments made up by
// tcpInfoRead, as a busy socket's: 100 then 10 Mbit/s, no sample
// app-limited), C4-lite's reading of it, C3, and the socket's own
// TCP_NOTSENT_LOWAT read back with getsockopt: the new mark is on the
// socket within 600 ms of the drop.
func TestLowatAdaptsLinux(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	acc := make(chan net.Conn, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			acc <- c
		}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	srv := <-acc
	defer srv.Close()
	tc := c.(*net.TCPConn)
	var rate, acked uint64
	var segs uint32
	fake := func(fd int) (*unix.TCPInfo, int, error) {
		info, n, err := getsockoptTCPInfo(fd)
		if err != nil {
			return info, n, err
		}
		info.Delivery_rate, info.Bytes_acked, info.Data_segs_out, info.Snd_mss = rate, acked, segs, 1448
		(*[8]byte)(unsafe.Pointer(info))[7] &^= 1 // not app-limited
		return info, n, nil
	}
	cur := tcpInfoRead.Load()
	tcpInfoRead.Store(&fake)
	defer tcpInfoRead.Store(cur)
	raw, err := tc.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	mark := func() int {
		v := 0
		raw.Control(func(fd uintptr) { v, _ = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT) })
		return v
	}
	h := newHostQ(tc, newHostSys(tc))
	l := newSendCapLink(newCapSys(tc), h)
	var g hostGate
	g.Update(100*mbit, 1448)
	_, _, want := g.Update(10*mbit, 1448)
	drop, at := 2450*time.Millisecond, time.Duration(-1)
	written := uint64(0)
	for now := 100 * time.Millisecond; now <= 4*time.Second; now += 100 * time.Millisecond {
		r := 100 * mbit
		if now >= drop {
			r = 10 * mbit
		}
		rate = uint64(r)
		acked += uint64(r / 10)
		segs += uint32(r/10/1448) + 1
		written += uint64(r / 10)
		l.step(now, written)
		if now < drop && now > time.Second && mark() != Lowat(100*mbit, 1448) {
			t.Fatalf("at %v (100 Mbit/s) the socket's mark is %d, want %d", now, mark(), Lowat(100*mbit, 1448))
		}
		if now >= drop && at < 0 && mark() == want {
			at = now - drop
		}
	}
	t.Logf("the socket's mark %d → %d B %v after the drop", Lowat(100*mbit, 1448), want, at)
	if at < 0 || at > 600*time.Millisecond {
		t.Fatalf("the socket's mark was %d after the drop (want %d within 600 ms; at %v)", mark(), want, at)
	}
}

// TestHostQLinuxSocket: the guard on a real loopback socket whose reader
// takes 400 KB/s: the gate (2 Mbit/s: cap 2896 B) keeps the socket's unsent
// bytes under the cap, and its waits in the netpoller always wake (no lost
// wakeup: the writer keeps up with the reader).
func TestHostQLinuxSocket(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	acc := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.(*net.TCPConn).SetReadBuffer(16 << 10)
			acc <- c
		}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	srv := <-acc
	defer srv.Close()
	read := make(chan int64, 1)
	go func() {
		var n int64
		buf := make([]byte, 4<<10)
		for {
			k, err := io.ReadFull(srv, buf)
			n += int64(k)
			if err != nil {
				read <- n
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	h := newHostQ(c, newHostSys(c))
	if h.fb.Load() != nil {
		t.Fatal("the guard fell back on this kernel")
	}
	h.Update(2*mbit, 1448)
	if !h.on.Load() || h.cap.Load() != 2896 {
		t.Fatalf("gate %v cap %d", h.on.Load(), h.cap.Load())
	}
	frame := make([]byte, 1<<10)
	worst, wrote := 0, 0
	end := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(end) {
		if _, err := h.Write(frame); err != nil {
			t.Fatal(err)
		}
		wrote += len(frame)
		ns, err := h.sys.notsent()
		if err != nil {
			t.Fatal(err)
		}
		worst = max(worst, ns)
	}
	c.Close()
	got := <-read
	t.Logf("wrote %d B in 1.5 s (the reader took %d), unsent at most %d B, gate waits %v", wrote, got, worst, time.Duration(h.blocked.Load()))
	if worst > 2896 {
		t.Fatalf("%d B unsent, past the cap", worst)
	}
	if wrote < 300<<10 {
		t.Fatalf("only %d B written in 1.5 s against a 400 KB/s reader: a wait that did not wake", wrote)
	}
}
