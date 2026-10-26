package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The daemon tells systemd it is up (§5.1 «بازگشت‌پذیری»): exactly one
// "READY=1" datagram on $NOTIFY_SOCKET — a path, or "@name" in the abstract
// namespace. Without NOTIFY_SOCKET (Type=simple) it does nothing, and no error.
func TestSdNotifyReady(t *testing.T) {
	for _, name := range []string{
		filepath.Join(t.TempDir(), "notify.sock"),
		fmt.Sprintf("@hs2-test-notify-%d", os.Getpid()),
	} {
		l, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: name, Net: "unixgram"})
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("NOTIFY_SOCKET", name)
		if err := sdNotifyReady(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		buf := make([]byte, 64)
		l.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := l.Read(buf)
		if err != nil || string(buf[:n]) != "READY=1" {
			t.Fatalf("%s: got %q, %v; want exactly READY=1", name, buf[:n], err)
		}
		l.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		if n, err := l.Read(buf); err == nil {
			t.Fatalf("%s: a second datagram %q", name, buf[:n])
		}
		l.Close()
	}
	os.Unsetenv("NOTIFY_SOCKET")
	if err := sdNotifyReady(); err != nil {
		t.Fatalf("without NOTIFY_SOCKET: %v", err)
	}
	t.Setenv("NOTIFY_SOCKET", "")
	if err := sdNotifyReady(); err != nil {
		t.Fatalf("empty NOTIFY_SOCKET: %v", err)
	}
	// a socket nobody listens on: an error to log, never a panic or a hang
	t.Setenv("NOTIFY_SOCKET", filepath.Join(t.TempDir(), "gone.sock"))
	if err := sdNotifyReady(); err == nil {
		t.Fatal("no error for a socket nobody listens on")
	}
}
