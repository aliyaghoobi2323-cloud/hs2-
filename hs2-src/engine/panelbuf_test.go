package engine

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestLocalTarget(t *testing.T) {
	for _, c := range []struct {
		a    string
		want bool
	}{
		{"127.0.0.1:5211", true}, {"127.9.9.9:1", true}, {"[::1]:443", true},
		{"[::ffff:127.0.0.1]:80", true}, {"192.0.2.77:80", false}, {"example.com:80", false}, {"nonsense", false},
	} {
		if got := localTarget(c.a); got != c.want {
			t.Fatalf("%s: %v, want %v", c.a, got, c.want)
		}
	}
}

// panelDial to a loopback panel sets the receive buffer (Linux doubles what
// it is given); 0 leaves the kernel's autotuning.
func TestPanelDialRcvbuf(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	defer func(v int) { TunePanelRcvbuf = v }(TunePanelRcvbuf)
	for _, n := range []int{256 << 10, 0} {
		TunePanelRcvbuf = n
		c, err := panelDial(context.Background(), "tcp", ln.Addr().String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := sockRcvbuf(c)
		c.Close()
		if !ok {
			t.Skip("no SO_RCVBUF read here")
		}
		if n > 0 && (got < n || got > 2*n) {
			t.Fatalf("set %d: the socket holds %d", n, got)
		}
	}
}
