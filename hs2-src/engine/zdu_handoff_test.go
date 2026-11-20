//go:build linux

package engine

import (
	"os"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/handoff"
)

// S5: DRAINSTAT from the old process (over a real hand-off channel) drives
// the foreign-drain term of ctlTarget; the end of the channel clears it.
func TestZDUHandoffDrainStat(t *testing.T) {
	setZDU(t, true)
	m, _ := zduPool(0, 0)
	m.max = 40
	m.target.Store(5)
	a, b, err := handoff.Pair()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	seen := make(chan int, 8)
	sink := m.ZDUDrainSink()
	done := make(chan error, 1)
	go func() { done <- b.FollowDrain(func(n int) { sink(n); seen <- n }, time.Second) }()
	a.SendDrainStat(handoff.DrainStat{Links: 7, Users: 300})
	if n := <-seen; n != 7 {
		t.Fatal(n)
	}
	if got := m.ctlTarget(); got != 12 {
		t.Fatalf("ctlTarget with 7 old links: %d, want 12", got)
	}
	a.SendDrainStat(handoff.DrainStat{Links: 2, Users: 10})
	<-seen
	if got := m.ctlTarget(); got != 7 {
		t.Fatalf("ctlTarget with 2: %d", got)
	}
	a.Close() // the old process exited
	if n := <-seen; n != 0 {
		t.Fatal(n)
	}
	<-done
	if got := m.ctlTarget(); got != 5 {
		t.Fatalf("after exit: %d, want 5", got)
	}
	// key off: the same reports change nothing
	setZDU(t, false)
	m.SetZDUForeignDrain(9)
	if got := m.ctlTarget(); got != 5 {
		t.Fatalf("key off: %d", got)
	}
}

func TestZDUListenFromFile(t *testing.T) {
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	if _, err := ListenFromFile(r); err == nil {
		t.Fatal("a pipe became a listener")
	}
	if _, err := PacketConnFromFile(r); err == nil {
		t.Fatal("a pipe became a packet conn")
	}
}
