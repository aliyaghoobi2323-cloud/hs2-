//go:build linux

package engine

import (
	"net"
	"testing"

	"golang.org/x/sys/unix"
)

// The budget's measure: Linux's segments in flight (unacked − sacked −
// lost + retrans) times the segment, and the unsent bytes; the bytes past a
// hole that the peer has (sacked) and the lost ones are not in flight. On a
// socket, both measures read (SIOCOUTQ with BudgetInflight 0).
func TestBudgetInflight(t *testing.T) {
	ti := &unix.TCPInfo{Unacked: 100, Sacked: 30, Lost: 5, Retrans: 2, Snd_mss: 1448, Notsent_bytes: 4000}
	if got, want := inflightOf(ti), 67*1448+4000; got != want {
		t.Fatalf("in flight %d, want %d", got, want)
	}
	ti = &unix.TCPInfo{Unacked: 1, Sacked: 3, Snd_mss: 1448}
	if got := inflightOf(ti); got != 0 {
		t.Fatalf("in flight %d of a count below zero", got)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			defer c.Close()
			c.Read(make([]byte, 1))
		}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, v := range []int{1, 0} {
		BudgetInflight = v
		f := newOutQ(c.(*net.TCPConn))
		if f == nil {
			t.Fatalf("BudgetInflight %d: no measure", v)
		}
		if n, err := f(); err != nil || n != 0 {
			t.Fatalf("BudgetInflight %d: %d, %v on an idle socket", v, n, err)
		}
	}
	BudgetInflight = 1
	if newOutQ(nil) != nil {
		t.Fatal("a measure of no socket")
	}
}
