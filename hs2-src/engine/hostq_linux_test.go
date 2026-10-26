//go:build linux

package engine

import (
	"io"
	"net"
	"testing"
	"time"
)

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
