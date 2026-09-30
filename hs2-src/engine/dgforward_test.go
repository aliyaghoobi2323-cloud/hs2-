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

// tunForwardPort must be a bijection on 1..65535 (distinct user ports never
// share an on-tun port) and must move a port away from itself (so the exit's
// tun-side listener does not fight a panel bound on the same user port).
func TestTunForwardPort(t *testing.T) {
	seen := map[string]int{}
	for p := 1; p <= 65535; p++ {
		s := strconv.Itoa(p)
		m := tunForwardPort(s)
		n, err := strconv.Atoi(m)
		if err != nil || n < 1 || n > 65535 {
			t.Fatalf("tunForwardPort(%d) = %q, not a valid port", p, m)
		}
		if m == s {
			t.Fatalf("tunForwardPort(%d) did not move the port (still %s) — a panel on that port would still collide", p, m)
		}
		if prev, ok := seen[m]; ok {
			t.Fatalf("tunForwardPort collision: %d and %d both map to %s", prev, p, m)
		}
		seen[m] = p
	}
	// Non-numeric input is passed through untouched.
	if got := tunForwardPort("nope"); got != "nope" {
		t.Fatalf("tunForwardPort(nope) = %q, want nope", got)
	}
	// The common panel ports land clear of themselves.
	for _, p := range []string{"443", "8443", "2053", "2096"} {
		if tunForwardPort(p) == p {
			t.Fatalf("port %s not shifted", p)
		}
	}
}

// The EXIT forwarder must start and forward even when a panel already binds the
// user port on 0.0.0.0 — the exact setup that crash-looped the exit
// (bind: address already in use) before the on-tun port was shifted. The exit
// and its panel are on the same host here (as on a real kharej); the edge lives
// on the other server, so the edge's own user-port listener is not co-located
// and is exercised by the installer's namespace test instead. Here the edge is
// simulated by dialing the exit's on-tun port directly, which is what the edge
// does over the tun.
func TestDgExitForwarderSurvivesPanelOnWildcard(t *testing.T) {
	const userPort = "18443" // stands in for the user/panel port (e.g. 8443)

	// A panel bound on the WILDCARD at the user port — exactly what x-ui or
	// marzban do. Before the fix its presence made the exit's tun-side bind of
	// the same port fail; now the exit uses tunForwardPort(userPort) instead.
	panelWild, err := net.Listen("tcp", "0.0.0.0:"+userPort)
	if err != nil {
		t.Skipf("cannot bind 0.0.0.0:%s for the test: %v", userPort, err)
	}
	defer panelWild.Close()
	go echoAccept(panelWild)

	// The panel the exit actually dials (its expose). A distinct loopback port
	// keeps the round-trip deterministic; the test is about the exit's LISTEN
	// bind surviving the wildcard, not which panel answers.
	panel, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer panel.Close()
	go echoAccept(panel)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logf := func(string, ...any) {}

	// Exit: listen on the tun IP (127.0.0.1 here) at the SHIFTED port, dial the
	// panel. With the old code this tried 127.0.0.1:18443 and failed against the
	// wildcard bind above; now it listens on 127.0.0.1:tunForwardPort(18443).
	if err := StartDgForwarders(ctx, false, []string{userPort}, "", "", "127.0.0.1", panel.Addr().String(), false, logf); err != nil {
		t.Fatalf("exit forwarder did not start despite the panel on the wildcard: %v", err)
	}

	// The edge dials the peer's tun IP at the same shifted port; do that here.
	onTun := "127.0.0.1:" + tunForwardPort(userPort)
	deadline := time.Now().Add(3 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", onTun, 500*time.Millisecond)
		if err != nil {
			lastErr = err
			time.Sleep(50 * time.Millisecond)
			continue
		}
		msg := []byte("hs2-forward-roundtrip")
		c.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Write(msg); err != nil {
			c.Close()
			lastErr = err
			continue
		}
		got := make([]byte, len(msg))
		if _, err := io.ReadFull(c, got); err != nil {
			c.Close()
			lastErr = err
			continue
		}
		c.Close()
		if string(got) != string(msg) {
			t.Fatalf("echo mismatch: got %q", got)
		}
		return // success: the exit listened despite the wildcard panel and forwarded
	}
	t.Fatalf("forwarded connection never reached the panel: %v", lastErr)
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
