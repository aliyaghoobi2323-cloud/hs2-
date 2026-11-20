//go:build linux

package handoff

import (
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// nonblock reads O_NONBLOCK of a live socket through its own fd, without
// Fd() (which would itself clear the flag).
func nonblock(t *testing.T, c syscall.Conn) bool {
	t.Helper()
	rc, err := c.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var fl int
	var ferr error
	if err := rc.Control(func(fd uintptr) { fl, ferr = unix.FcntlInt(fd, unix.F_GETFL, 0) }); err != nil {
		t.Fatal(err)
	}
	if ferr != nil {
		t.Fatal(ferr)
	}
	return fl&unix.O_NONBLOCK != 0
}

// Review F1: sending the hand-off FDS must leave the old process's live
// listener and UDP socket non-blocking (Fd() on net's File() dup cleared
// O_NONBLOCK on the shared file description), and the receiver still gets
// working fds.
func TestSendKeepsLiveNonblocking(t *testing.T) {
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	uc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer uc.Close()
	if !nonblock(t, ln) || !nonblock(t, uc) {
		t.Fatal("live sockets blocking before the send")
	}
	lf, err := ln.File()
	if err != nil {
		t.Fatal(err)
	}
	uf, err := uc.File()
	if err != nil {
		t.Fatal(err)
	}
	a, b, err := Pair()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	defer b.Close()
	got := make(chan *Msg, 1)
	go func() {
		m, _ := b.Recv(5 * time.Second)
		got <- m
	}()
	if err := a.Send(&Msg{Type: TypeFDs, FDs: []FD{{Name: "tcp:x", File: lf}, {Name: "udp:x", File: uf}}}, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	lf.Close()
	uf.Close()
	if !nonblock(t, ln) {
		t.Fatal("live listener became blocking after the hand-off send")
	}
	if !nonblock(t, uc) {
		t.Fatal("live UDP socket became blocking after the hand-off send")
	}
	m := <-got
	if m == nil || len(m.FDs) != 2 {
		t.Fatalf("receiver got %+v", m)
	}
	nl, err := net.FileListener(m.FDs[0].File)
	if err != nil {
		t.Fatal(err)
	}
	defer nl.Close()
	if nl.Addr().String() != ln.Addr().String() {
		t.Fatalf("received listener %s, want %s", nl.Addr(), ln.Addr())
	}
	for _, f := range m.FDs {
		f.File.Close()
	}
	// No dup left behind by Send: a closed sender file is refused cleanly.
	if err := a.Send(&Msg{Type: TypeFDs, FDs: []FD{{Name: "tcp:x", File: lf}}}, time.Second); err == nil {
		t.Fatal("send of a closed file succeeded")
	}
}

// Dup returns a usable dup and leaves the source's flags alone.
func TestDupKeepsFlags(t *testing.T) {
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	lf, err := ln.File()
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()
	d, err := Dup(lf, "x")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if !nonblock(t, ln) {
		t.Fatal("Dup made the live listener blocking")
	}
	if _, err := Dup(nil, "x"); err == nil {
		t.Fatal("Dup(nil) ok")
	}
	var _ *os.File = d
}
