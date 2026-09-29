package engine

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/lab/netsim"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/udpcarrier"
)

func sharedKey() []byte { return bytes.Repeat([]byte{0x3c}, 32) }

// acceptOne accepts a single carrier from a listener with a timeout.
func acceptOne(t *testing.T, ln CarrierListener, d time.Duration) (Carrier, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return ln.Accept(ctx)
}

// TestAutoSelectsUDPWhenGood: on a clean path the auto dialer picks the UDP
// carrier.
func TestAutoSelectsUDPWhenGood(t *testing.T) {
	shared := sharedKey()
	ln, err := NewAutoListener("127.0.0.1:0", shared, 1200)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.(*autoListener).udp.ln.LocalAddr().String()

	accCh := make(chan Carrier, 1)
	go func() { c, _ := acceptOne(t, ln, 5*time.Second); accCh <- c }()

	d := NewAutoDialer(addr, "", shared, 1200, t.Logf)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	car, err := d.Dial(ctx)
	if err != nil {
		t.Fatalf("auto dial: %v", err)
	}
	defer car.Close()
	if _, ok := car.(*udpcarrier.Conn); !ok {
		t.Fatalf("auto did not select UDP on a clean path: got %T", car)
	}
	if c := <-accCh; c != nil {
		c.Close()
	}
}

// TestProbeSeesBlockedUDP: a fully black-holed UDP path (netsim AllDrop, the
// in-process equivalent of "iptables -j DROP on UDP") is reported unreachable by
// the independent probe — which is what drives the auto dialer to fall back.
func TestProbeSeesBlockedUDP(t *testing.T) {
	shared := sharedKey()
	udpLn, err := NewUDPListener("127.0.0.1:0", shared, 1200)
	if err != nil {
		t.Fatal(err)
	}
	defer udpLn.Close()
	realAddr := udpLn.(*udpListener).ln.LocalAddr().String()

	relay, err := netsim.NewRelay(realAddr, netsim.Config{ToServer: netsim.AllDrop{}, ToClient: netsim.AllDrop{}})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res, _ := udpcarrier.Probe(ctx, relay.FrontAddr(), shared, 12, 8*time.Millisecond, 200*time.Millisecond)
	if res.Reachable {
		t.Fatal("probe reported a fully blocked UDP path as reachable")
	}
	// Directly (no relay) the same path is reachable.
	res2, _ := udpcarrier.Probe(ctx, realAddr, shared, 12, 8*time.Millisecond, 200*time.Millisecond)
	if !res2.Reachable {
		t.Fatalf("probe should reach the open UDP listener: %+v", res2)
	}
}

// TestAutoFallsBackToTCP: when UDP is unavailable at the target but the TCP
// (noise) carrier is up on the same port, the auto dialer silently returns the
// TCP carrier. This is the silent-fallback / isolation property: UDP being down
// never stops the tunnel.
func TestAutoFallsBackToTCP(t *testing.T) {
	shared := sharedKey()
	// A TCP (noise) listener keyed from the shared secret, exactly as the auto
	// fallback expects. No UDP listener on this port, so the UDP probe fails.
	local, _ := core.StaticFromSeed(shared, "hs2-udp-responder")
	tcpLn, err := newNoiseListener("127.0.0.1:0", local, shared)
	if err != nil {
		t.Fatal(err)
	}
	defer tcpLn.Close()
	addr := tcpLn.ln.Addr().String()

	accCh := make(chan Carrier, 1)
	go func() { c, _ := acceptOne(t, tcpLn, 6*time.Second); accCh <- c }()

	d := NewAutoDialer(addr, "", shared, 1200, t.Logf)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	car, err := d.Dial(ctx)
	if err != nil {
		t.Fatalf("auto dial did not fall back to TCP: %v", err)
	}
	defer car.Close()
	if _, ok := car.(*udpcarrier.Conn); ok {
		t.Fatal("auto returned a UDP carrier when UDP was unavailable")
	}
	srv := <-accCh
	if srv == nil {
		t.Fatal("TCP listener did not accept the fallback carrier")
	}
	defer srv.Close()

	// The fallback TCP carrier carries frames.
	if err := car.SendFrame(1 /*data*/, []byte("over tcp fallback")); err != nil {
		t.Fatalf("send over TCP fallback: %v", err)
	}
	ft, p, err := srv.ReadFrame()
	if err != nil || ft != 1 || string(p) != "over tcp fallback" {
		t.Fatalf("fallback frame not delivered: ft=%d %q err=%v", ft, p, err)
	}
}
