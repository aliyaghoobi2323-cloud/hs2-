//go:build linux

package tun

import (
	"os"
	"testing"
	"time"
)

// ZDU S6: a handed-off device reads only after OpenReadGate (GO); a device
// opened cold never waits.
func TestZDUTunReadGate(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pw.Close()
	pw.Write([]byte("pkt"))
	cold := &Device{f: pr}
	b := make([]byte, 8)
	if n, err := cold.Read(b); n != 3 || err != nil {
		t.Fatalf("cold read %d %v", n, err)
	}
	pw.Write([]byte("pkt"))
	g := &Device{f: pr, gate: readGate.c}
	done := make(chan int, 1)
	go func() { n, _ := g.Read(b); done <- n }()
	select {
	case <-done:
		t.Fatal("handed-off device read before GO")
	case <-time.After(150 * time.Millisecond):
	}
	OpenReadGate()
	OpenReadGate() // idempotent
	select {
	case n := <-done:
		if n != 3 {
			t.Fatal(n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no read after the gate opened")
	}
}

// ZDU S7: StopReads holds Read before the next kernel read (offload
// segments already split are handed out first); ResumeReads lets it go.
func TestZDUTunStopReads(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pw.Close()
	d := &Device{f: pr}
	d.StopReads()
	if !d.ReadsStopped() {
		t.Fatal("not stopped")
	}
	pw.Write([]byte("pkt"))
	b := make([]byte, 8)
	done := make(chan int, 1)
	go func() { n, _ := d.Read(b); done <- n }()
	select {
	case <-done:
		t.Fatal("read the kernel queue after StopReads")
	case <-time.After(150 * time.Millisecond):
	}
	d.ResumeReads()
	select {
	case n := <-done:
		if n != 3 {
			t.Fatal(n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no read after ResumeReads")
	}
	// offload: a packet already taken from the kernel is still handed out.
	o := &Device{f: pr, offload: true, rbuf: make([]byte, 128), single: []byte("held")}
	o.StopReads()
	n, err := o.Read(b)
	if err != nil || string(b[:n]) != "held" {
		t.Fatalf("held packet %q %v", b[:n], err)
	}
	go func() { n, _ := o.Read(b); done <- n }()
	select {
	case <-done:
		t.Fatal("offload device read the kernel after StopReads")
	case <-time.After(150 * time.Millisecond):
	}
	o.ResumeReads()
	pw.Close() // the resumed read reaches the kernel (here: EOF)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("offload device: no kernel read after ResumeReads")
	}
}
