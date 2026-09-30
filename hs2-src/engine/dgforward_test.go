package engine

import (
	"context"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"
)

// DgTunPort exists so the exit never listens on a panel's port: it must be a
// valid port and must not be one of the ports panels usually sit on.
func TestDgTunPortIsPrivate(t *testing.T) {
	n, err := strconv.Atoi(DgTunPort)
	if err != nil || n < 1 || n > 65535 {
		t.Fatalf("DgTunPort %q is not a valid port", DgTunPort)
	}
	for _, p := range []string{"80", "443", "2052", "2053", "2082", "2083", "2086", "2087", "2095", "2096", "8080", "8443", "8880"} {
		if DgTunPort == p {
			t.Fatalf("DgTunPort %s is a common panel port — the exit would fight the panel for it", p)
		}
	}
}

// The exit must start and forward even when a panel already binds the user port
// on 0.0.0.0 — the setup that crash-looped the exit (bind: address already in
// use) when it listened on the user port. It is given the user port list the
// way older installers wrote it, and must ignore it: it listens only on
// DgTunPort on the tun address. The edge is simulated by dialing that port.
func TestDgExitForwarderSurvivesPanelOnWildcard(t *testing.T) {
	const userPort = "18443" // stands in for the user/panel port (e.g. 8443)
	panelWild, err := net.Listen("tcp", "0.0.0.0:"+userPort)
	if err != nil {
		t.Skipf("cannot bind 0.0.0.0:%s for the test: %v", userPort, err)
	}
	defer panelWild.Close()
	go echoAccept(panelWild)

	panel := echoListener(t, "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	nolog := func(string, ...any) {}

	if err := StartDgForwarders(ctx, false, []string{userPort}, "", "", "127.0.0.1", panel, false, nolog); err != nil {
		t.Fatalf("exit forwarder did not start despite the panel on the wildcard: %v", err)
	}
	roundTrip(t, "127.0.0.1:"+DgTunPort)
}

// The user ports are configured once, on the edge: an exit started with NO
// port list still delivers every edge user port to the panel, through the one
// on-tun port. Two user ports, both must reach the panel.
func TestDgForwarderUserPortsLiveOnlyOnEdge(t *testing.T) {
	panel := echoListener(t, "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	nolog := func(string, ...any) {}

	// The exit's tun address is 127.0.0.3 here (the wildcard test uses 127.0.0.1),
	// so the two tests never contend for DgTunPort.
	if err := StartDgForwarders(ctx, false, nil, "", "", "127.0.0.3", panel, false, nolog); err != nil {
		t.Fatalf("exit forwarder with no port list did not start: %v", err)
	}
	p1, p2 := freePortOn(t, "127.0.0.2"), freePortOn(t, "127.0.0.2")
	if err := StartDgForwarders(ctx, true, []string{p1, p2}, "127.0.0.2", "127.0.0.3", "", "", false, nolog); err != nil {
		t.Fatalf("edge forwarder did not start: %v", err)
	}
	for _, p := range []string{p1, p2} {
		roundTrip(t, "127.0.0.2:"+p)
	}
}

// An exit without a panel (a pure routed tunnel) opens nothing.
func TestDgExitWithoutPanelOpensNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := StartDgForwarders(ctx, false, []string{"8443"}, "", "", "127.0.0.4", "", false, func(string, ...any) {}); err != nil {
		t.Fatalf("exit without a panel returned an error: %v", err)
	}
	if c, err := net.DialTimeout("tcp", "127.0.0.4:"+DgTunPort, 300*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("exit without a panel is listening on the tun forwarder port")
	}
}

// roundTrip connects to addr (retrying while the forwarders come up) and checks
// that bytes come back unchanged from the echo panel.
func roundTrip(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err != nil {
			lastErr = err
			time.Sleep(50 * time.Millisecond)
			continue
		}
		msg := []byte("hs2-forward-roundtrip " + addr)
		c.SetDeadline(time.Now().Add(2 * time.Second))
		_, werr := c.Write(msg)
		got := make([]byte, len(msg))
		_, rerr := io.ReadFull(c, got)
		c.Close()
		if werr != nil || rerr != nil {
			lastErr = firstErr(werr, rerr)
			continue
		}
		if string(got) != string(msg) {
			t.Fatalf("%s: echo mismatch: got %q", addr, got)
		}
		return
	}
	t.Fatalf("%s never reached the panel through the forwarder: %v", addr, lastErr)
}

func firstErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// echoListener starts an echo server and returns its address.
func echoListener(t *testing.T, addr string) string {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go echoAccept(ln)
	return ln.Addr().String()
}

// freePortOn returns a TCP port on ip that is free right now.
func freePortOn(t *testing.T, ip string) string {
	t.Helper()
	ln, err := net.Listen("tcp", ip+":0")
	if err != nil {
		t.Fatal(err)
	}
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()
	return p
}

func echoAccept(ln net.Listener) {
	var wg sync.WaitGroup
	for {
		c, err := ln.Accept()
		if err != nil {
			wg.Wait()
			return
		}
		wg.Add(1)
		go func(c net.Conn) { defer wg.Done(); defer c.Close(); io.Copy(c, c) }(c)
	}
}
