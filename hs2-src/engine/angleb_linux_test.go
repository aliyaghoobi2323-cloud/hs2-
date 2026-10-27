//go:build linux

package engine

import (
	"net"
	"sync"
	"testing"
	"time"
)

// C4-lite on every link socket (§3.4), the exit's too: it caps the socket's
// pacing from its first reading that is not app-limited (1.25 times the
// delivery rate), and the exit's pacing net, once it engages, no longer sets
// the socket itself but hands its cap to it: the lesser of the two is in
// force, one writer for the cap (r1run.go).
func TestAngleBPaceNetHandsCap(t *testing.T) {
	c, s := loopbackTCP(t)
	var mu sync.Mutex
	share := 0.06
	wnFakeInfo(t, s, &share, &mu)
	var caps []uint64
	set := func(tc *net.TCPConn, rate uint64) bool {
		if tc == s {
			mu.Lock()
			caps = append(caps, rate)
			mu.Unlock()
		}
		return true
	}
	oldSet := wcPaceSet.Load()
	wcPaceSet.Store(&set)
	defer wcPaceSet.Store(oldSet)
	mtr := &linkMeter{}
	srv, _, err := newSession(s, true, nil, mtr)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	cli, _, err := newSession(c, false, nil, &linkMeter{})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if mtr.wc == nil || !mtr.wc.pnet || mtr.wc.sc.Load() == nil {
		t.Fatal("the exit's link socket has no C4-lite for its pacing net to hand its cap to")
	}
	got := func() []uint64 {
		mu.Lock()
		defer mu.Unlock()
		return append([]uint64(nil), caps...)
	}
	wcWaitFor(t, 5*time.Second, "no cap", func() bool { return len(got()) > 0 })
	if v, want := got()[0], uint64(wcNetKOf()*30e6); v != want {
		t.Fatalf("C4-lite capped at %d, want %d", v, want)
	}
	wcWaitFor(t, 5*time.Second, "the pacing net never engaged", func() bool { return mtr.wc.inStats.netHeld.Load() > 0 })
	sc := mtr.wc.sc.Load()
	sc.mu.Lock()
	pn, own, inForce := sc.core.pn, sc.core.own, sc.core.set
	sc.mu.Unlock()
	if pn == 0 {
		t.Fatal("the pacing net did not hand its cap to C4-lite")
	}
	if inForce != min(pn, own) {
		t.Fatalf("cap in force %d, the net's %d, C4-lite's own %d: want the lesser", inForce, pn, own)
	}
	if v := got(); v[len(v)-1] != inForce {
		t.Fatalf("the socket's latest cap %d, in force %d", v[len(v)-1], inForce)
	}
}
