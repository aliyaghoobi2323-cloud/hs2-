//go:build linux

package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeNotify listens on a unixgram socket and points $NOTIFY_SOCKET at it.
func fakeNotify(t *testing.T) *net.UnixConn {
	t.Helper()
	p := filepath.Join(t.TempDir(), "notify.sock")
	c, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: p, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	t.Setenv("NOTIFY_SOCKET", p)
	return c
}

func recvNotify(t *testing.T, c *net.UnixConn, d time.Duration) string {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(d))
	b := make([]byte, 4096)
	n, err := c.Read(b)
	if err != nil {
		return ""
	}
	return string(b[:n])
}

func resetNotifyOnce(t *testing.T) {
	readyOnce, stoppingOnce = sync.Once{}, sync.Once{}
	t.Cleanup(func() { readyOnce, stoppingOnce = sync.Once{}, sync.Once{} })
}

// TestZDUNotifyStreamEdge: a real runStream edge (HS2_ZDU unset, the peer
// down) sends READY=1 to $NOTIFY_SOCKET once its user port is bound, without
// waiting for a link (R3: no key dependency, no peer dependency); STOPPING=1
// follows on cancel through exitOnCancel.
func TestZDUNotifyStreamEdge(t *testing.T) {
	os.Unsetenv("HS2_ZDU")
	resetNotifyOnce(t)
	old := statusRunDir
	statusRunDir = t.TempDir()
	t.Cleanup(func() { statusRunDir = old })
	ns := fakeNotify(t)

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	dead, _ := net.Listen("tcp", "127.0.0.1:0") // a peer that never answers TLS
	t.Cleanup(func() { dead.Close() })

	fc := fileConfig{Mode: "dial", Carrier: "mtcp", Addr: dead.Addr().String(), SNI: "lab.example.com",
		SharedKey: strings.Repeat("5a", 32), UserListenIP: "127.0.0.1", ForwardPorts: itoa(port)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); runStream(ctx, fc, false, 0) }()

	msg := recvNotify(t, ns, 10*time.Second)
	if !strings.HasPrefix(msg, "READY=1\nSTATUS=hs2 up: stream edge") {
		cancel()
		t.Fatalf("no READY: %q", msg)
	}
	// READY means the user port is bound
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+itoa(port), time.Second)
	if err != nil {
		cancel()
		t.Fatalf("READY before the user port: %v", err)
	}
	c.Close()
	notifyReady("again") // once per process
	if m := recvNotify(t, ns, 200*time.Millisecond); m != "" {
		t.Fatalf("second READY: %q", m)
	}

	exited := make(chan struct{})
	go exitOnCancel(ctx, 10*time.Millisecond, func() { close(exited) })
	cancel()
	if m := recvNotify(t, ns, 5*time.Second); m != "STOPPING=1\nSTATUS=hs2 stopping" {
		t.Fatalf("no STOPPING: %q", m)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("exit not called")
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runStream did not return")
	}
}

// TestZDUNotifyUnset: without $NOTIFY_SOCKET nothing is sent and nothing is
// logged (today's Type=simple units see the exact pre-ZDU behaviour).
func TestZDUNotifyUnset(t *testing.T) {
	resetNotifyOnce(t)
	t.Setenv("NOTIFY_SOCKET", "")
	var logged []string
	old := notifyLogf
	notifyLogf = func(f string, a ...any) { logged = append(logged, f) }
	t.Cleanup(func() { notifyLogf = old })
	notifyReady("x")
	notifyStopping()
	if len(logged) != 0 {
		t.Fatalf("logged %v", logged)
	}
}

func TestZDUFeaturesLine(t *testing.T) {
	if got := featuresLine(); got != "features: zdu zduwin" {
		t.Fatalf("%q", got)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
