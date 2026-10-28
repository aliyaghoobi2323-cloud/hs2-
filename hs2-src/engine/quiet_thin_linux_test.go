//go:build linux

package engine

import (
	"net"
	"testing"
)

// A QUIET link's socket takes TCP_THIN_LINEAR_TIMEOUTS (QuietThin).
func TestQuietThinTimeouts(t *testing.T) {
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
	tc := c.(*net.TCPConn)
	if on, err := thinTimeouts(tc); err != nil || on {
		t.Fatalf("before: %v %v", on, err)
	}
	if !setThinTimeouts(tc) {
		t.Fatal("not set")
	}
	if on, err := thinTimeouts(tc); err != nil || !on {
		t.Fatalf("after: %v %v", on, err)
	}
	if setThinTimeouts(nil) {
		t.Fatal("set on no socket")
	}
}
