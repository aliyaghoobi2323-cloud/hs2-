//go:build linux

package sdnotify

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func listen(t *testing.T, name string) *net.UnixConn {
	t.Helper()
	c, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: name, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func recv(t *testing.T, c *net.UnixConn) string {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	b := make([]byte, 4096)
	n, err := c.Read(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(b[:n])
}

func TestUnsetIsNoop(t *testing.T) {
	t.Setenv(Env, "")
	for _, f := range []func() (bool, error){Ready, Stopping, func() (bool, error) { return MainPID(42) }} {
		if sent, err := f(); sent || err != nil {
			t.Fatalf("unset: sent %v err %v", sent, err)
		}
	}
}

func TestPathSocket(t *testing.T) {
	p := filepath.Join(t.TempDir(), "notify")
	c := listen(t, p)
	t.Setenv(Env, p)
	if sent, err := Ready(); !sent || err != nil {
		t.Fatalf("ready: %v %v", sent, err)
	}
	if got := recv(t, c); got != "READY=1" {
		t.Fatalf("got %q", got)
	}
	if sent, err := Stopping(); !sent || err != nil {
		t.Fatal(err)
	}
	if got := recv(t, c); got != "STOPPING=1" {
		t.Fatalf("got %q", got)
	}
	if _, err := MainPID(4242); err != nil {
		t.Fatal(err)
	}
	if got := recv(t, c); got != "MAINPID=4242\nREADY=1" {
		t.Fatalf("got %q", got)
	}
	if _, err := Status("drain\n3 links"); err != nil {
		t.Fatal(err)
	}
	if got := recv(t, c); got != "STATUS=drain 3 links" {
		t.Fatalf("got %q", got)
	}
	if sent, err := MainPID(0); sent || err == nil {
		t.Fatal("pid 0 accepted")
	}
}

func TestAbstractSocket(t *testing.T) {
	name := fmt.Sprintf("@hs2-sdnotify-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	c := listen(t, name)
	t.Setenv(Env, name)
	if sent, err := Ready(); !sent || err != nil {
		t.Fatalf("abstract: %v %v", sent, err)
	}
	if got := recv(t, c); got != "READY=1" {
		t.Fatalf("got %q", got)
	}
}

func TestBadAndMissingSocket(t *testing.T) {
	t.Setenv(Env, "vsock:2:1234")
	if sent, err := Ready(); sent || err != ErrBadSocket {
		t.Fatalf("vsock: %v %v", sent, err)
	}
	t.Setenv(Env, filepath.Join(t.TempDir(), "none"))
	if sent, err := Ready(); sent || err == nil {
		t.Fatalf("missing socket: %v %v", sent, err)
	}
}
