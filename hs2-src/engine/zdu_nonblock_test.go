//go:build linux

package engine

import (
	"net"
	"os"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/handoff"
)

// Review F1: after the hand-off FDS send, a pending Accept on the old
// process's listener must still be kicked out by the pause (it needs the
// live socket non-blocking: Fd() on the File() dup made it blocking, the
// accept thread then sat in the kernel and took the next connection).
func TestZDUPauseAfterSendStopsAccept(t *testing.T) {
	zduKeyOn(t)
	hung := time.AfterFunc(30*time.Second, func() { panic("TestZDUPauseAfterSendStopsAccept hung (live listener blocking?)") })
	defer hung.Stop()
	ln, err := zduListen("127.0.0.1:0", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	z := ln.(*zduLn)
	t.Cleanup(func() { z.setPaused(false) })
	pc, err := listenPacketUDP("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
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
	time.Sleep(100 * time.Millisecond) // Accept is pending
	names, files, err := ZDUListenerFiles()
	if err != nil {
		t.Fatal(err)
	}
	fds := make([]handoff.FD, len(files))
	for i := range files {
		fds[i] = handoff.FD{Name: names[i], File: files[i]}
	}
	a, b, err := handoff.Pair()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	defer b.Close()
	go func() {
		if m, err := b.Recv(5 * time.Second); err == nil {
			for _, f := range m.FDs {
				f.File.Close()
			}
		}
	}()
	if err := a.Send(&handoff.Msg{Type: handoff.TypeFDs, FDs: fds}, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		f.Close()
	}
	z.setPaused(true)
	time.Sleep(100 * time.Millisecond)
	c, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case got := <-acc:
		got.Close()
		t.Fatal("paused listener accepted a connection after the hand-off send (live socket blocking)")
	case <-time.After(500 * time.Millisecond):
	}
	z.setPaused(false) // the revert: accept comes back
	select {
	case got := <-acc:
		got.Close()
	case <-time.After(3 * time.Second):
		t.Fatal("resumed listener did not accept")
	}
}

// Review F3: a handed-over address whose fd does not convert is never
// cold-bound (the old process owns the port): the start fails instead.
func TestZDUAdoptedBadFDNoColdBind(t *testing.T) {
	zduKeyOn(t)
	r, w, _ := os.Pipe()
	defer w.Close()
	c0, u0 := ZDUColdListeners.Load(), ZDUColdUDP.Load()
	ZDUAdoptListener(ZDULnName("127.0.0.1:0"), r)
	if ln, err := zduListen("127.0.0.1:0", 0); err == nil {
		ln.Close()
		t.Fatal("bad adopted listener fd: cold bind")
	}
	r2, w2, _ := os.Pipe()
	defer w2.Close()
	ZDUAdoptPacketConn(ZDUPCName("127.0.0.1:0"), r2)
	if pc, err := zduListenPacket("127.0.0.1:0"); err == nil {
		pc.Close()
		t.Fatal("bad adopted udp fd: cold bind")
	}
	if ZDUColdListeners.Load() != c0 || ZDUColdUDP.Load() != u0 {
		t.Fatal("cold bind counted")
	}
}
