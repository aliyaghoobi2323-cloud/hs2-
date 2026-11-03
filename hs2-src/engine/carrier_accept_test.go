package engine

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
)

// A client that connects and says nothing must not hold up the next one: each
// Noise handshake runs on its own, so a valid carrier is accepted at once while
// the silent connection still waits out its handshake deadline.
func TestNoiseListenerSilentClientDoesNotBlock(t *testing.T) {
	shared := sharedKey()
	srv, _ := core.StaticFromSeed(shared, "hs2-udp-responder")
	cli, _ := core.StaticFromSeed(shared, "hs2-udp-initiator")
	ln, err := newNoiseListener("127.0.0.1:0", srv, shared)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.ln.Addr().String()

	silent, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	time.Sleep(50 * time.Millisecond) // the silent one is accepted first

	acc := make(chan Carrier, 1)
	go func() { c, _ := acceptOne(t, ln, 5*time.Second); acc <- c }()
	d := &noiseDialer{addr: addr, local: cli, remoteStatic: srv.Public, psk: shared}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	car, err := d.Dial(ctx)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer car.Close()
	s := <-acc
	if s == nil {
		t.Fatal("the valid carrier was not accepted")
	}
	defer s.Close()
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the valid carrier waited %v behind a silent connection", took)
	}
	if err := car.SendFrame(1, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	if ft, p, err := s.ReadFrame(); err != nil || ft != 1 || string(p) != "hi" {
		t.Fatalf("frame: %d %q %v", ft, p, err)
	}
}

// Accept on a closed listener ends with an error instead of waiting forever.
func TestNoiseListenerCloseEndsAccept(t *testing.T) {
	shared := sharedKey()
	srv, _ := core.StaticFromSeed(shared, "hs2-udp-responder")
	ln, err := newNoiseListener("127.0.0.1:0", srv, shared)
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(50 * time.Millisecond); ln.Close() }()
	if _, err := acceptOne(t, ln, 3*time.Second); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("accept after close: %v, want net.ErrClosed", err)
	}
}

// A reality carrier whose conn is gone reports the failed send instead of
// pretending the frame left.
func TestRealitySendFrameReportsClosedConn(t *testing.T) {
	a, b := net.Pipe()
	b.Close()
	a.Close()
	c := &realityCarrier{conn: a}
	if err := c.SendFrame(1, []byte("x")); err == nil {
		t.Fatal("SendFrame on a closed conn returned nil")
	}
}

// Close on a link with no TLS conn under it (a bare session) does not panic.
func TestMtcpLinkCloseBare(t *testing.T) {
	if err := (&mtcpLink{}).Close(); err != nil {
		t.Fatal(err)
	}
}
