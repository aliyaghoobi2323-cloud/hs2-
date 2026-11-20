//go:build linux

package engine

import (
	"net"
	"testing"
	"time"
)

// Gate fix: an Accept whose accept(2) raced the pause kick returns its
// connection (served by this process, never dropped) and counts it as a late
// accept; the next Accept waits until resume.
func TestZDULateAcceptServed(t *testing.T) {
	setZDU(t, true)
	ln, err := zduListenKind("127.0.0.1:0", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	z := ln.(*zduLn)
	h := func(z *zduLn) { z.setPaused(true) } // the pause lands mid-accept
	zduAcceptHook.Store(&h)
	t.Cleanup(func() { zduAcceptHook.Store(nil) })
	l0 := ZDULateAccepts.Load()
	c, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sc, err := z.Accept()
	if err != nil {
		t.Fatalf("late accept dropped: %v", err)
	}
	// served: bytes flow both ways
	if _, err := c.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 1)
	sc.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := sc.Read(b); err != nil || b[0] != 'x' {
		t.Fatalf("served conn read %q %v", b, err)
	}
	sc.Close()
	if d := ZDULateAccepts.Load() - l0; d != 1 {
		t.Fatalf("late accepts +%d, want +1", d)
	}
	zduAcceptHook.Store(nil)
	// paused now: the next Accept waits (no accept while paused)
	got := make(chan error, 1)
	go func() {
		c, err := z.Accept()
		if err == nil {
			c.Close()
		}
		got <- err
	}()
	c2, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	select {
	case <-got:
		t.Fatal("paused listener accepted")
	case <-time.After(200 * time.Millisecond):
	}
	z.setPaused(false)
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no accept after resume")
	}
	if d := ZDULateAccepts.Load() - l0; d != 1 {
		t.Fatalf("late accepts +%d after resume, want +1", d)
	}
}
