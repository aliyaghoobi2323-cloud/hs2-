//go:build linux

package engine

import (
	"net"
	"testing"
	"time"
)

// Review 4 low: the accept queue of a paused listener is visible (the old
// process's hung-child check in probation), and empties once accepted.
func TestZDUAcceptQueue(t *testing.T) {
	setZDU(t, true)
	ln, err := zduListenKind("127.0.0.1:0", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	z := ln.(*zduLn)
	z.setPaused(true)
	q0 := ZDUAcceptQueue()
	var cs []net.Conn
	for i := 0; i < 3; i++ {
		c, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		cs = append(cs, c)
	}
	defer func() {
		for _, c := range cs {
			c.Close()
		}
	}()
	time.Sleep(50 * time.Millisecond)
	if q := ZDUAcceptQueue(); q-q0 != 3 {
		t.Fatalf("queue %d (before %d), want +3", q, q0)
	}
	z.setPaused(false)
	for i := 0; i < 3; i++ {
		c, err := z.Accept()
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
	}
	if q := ZDUAcceptQueue(); q != q0 {
		t.Fatalf("queue %d after accept, want %d", q, q0)
	}
}
