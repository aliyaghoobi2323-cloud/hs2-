package engine

import (
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// audit246 L6: a slow per-link read in measure (the edge's `ip route get`
// exec) must not hold r.mu: track/forget of other links goes on meanwhile,
// and the counters read are stored back (a link forgotten meanwhile stays
// forgotten).
func TestQRunnerMeasureNoLockAcrossExec(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only (TCP_INFO)")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	dial := func() *tlscarrier.Carrier {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return tlscarrier.NewCarrier(c)
	}
	a, b := dial(), dial()
	if _, err := a.TCPConn().Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // acked on loopback

	entered, release := make(chan struct{}, 4), make(chan struct{})
	old := qLinkDevFn
	qLinkDevFn = func(*net.TCPConn) string {
		entered <- struct{}{}
		<-release
		return "eth9"
	}
	defer func() { qLinkDevFn = old }()

	r := &qRunner{on: true, byDev: true, links: map[*tlscarrier.Carrier]qPrev{}}
	r.track(a, false)
	forgetB := r.track(b, false)
	done := make(chan map[string]*qGroup, 1)
	go func() { done <- r.measure(time.Second) }()
	<-entered // measure is inside the slow read

	c := dial()
	tracked := make(chan func(), 1)
	go func() { tracked <- r.track(c, true) }()
	select {
	case <-tracked:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("track blocked on r.mu while measure ran the per-link exec")
	}
	forgetB() // forgotten mid-scan: must not come back
	close(release)
	g := <-done
	if g["eth9"] == nil {
		t.Fatalf("groups %v: want the slow read's dev", g)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.links[b]; ok {
		t.Fatal("a link forgotten during measure was stored back")
	}
	if p := r.links[a]; !p.devSet || p.dev != "eth9" || p.acked == 0 {
		t.Fatalf("link a after measure: %+v, want dev eth9 and counters stored", p)
	}
	if p, ok := r.links[c]; !ok || p.devSet || !p.accepted {
		t.Fatalf("link tracked mid-scan: %+v ok=%v", p, ok)
	}
}
