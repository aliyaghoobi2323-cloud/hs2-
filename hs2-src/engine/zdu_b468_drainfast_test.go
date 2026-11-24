package engine

import (
	"net"
	"testing"
	"time"
)

// b468: with HS2_B468_DRAINFAST=1 the drain end closes every user
// connection that has a kill, at the pace, while the links stay open; the
// links close together only after the last user; subs and kill-less conns
// are left to the link close. Key unset: the b454 link pace as before.
func TestB468DrainFastUsersFirst(t *testing.T) {
	t.Setenv("HS2_B469_STEP_MS", "25") // the batch count below is for the 25 ms step (b469Step)
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c, err := net.DialTCP("tcp", nil, ln.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := ln.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	m := &linkMeter{tcp: c}
	p := &linkPeers{}
	p.add(m)
	tr := newExitTraffic(p)

	const n = 200
	killed, linkUpAtKill := 0, 0
	for i := 0; i < n; i++ {
		var ec *exitConn
		ec = tr.openKill(m, func() {
			killed++
			if _, err := c.Write([]byte{1}); err == nil {
				linkUpAtKill++
			}
			tr.done(ec)
		})
	}
	tr.openSub(m) // a sub: never killed by the pace
	tr.open(nil)  // no kill: left to the link close
	var slept time.Duration
	sleep := func(d time.Duration) { slept += d }

	prevOn, prevDr, prevP, prevT := zduOn.Load(), zduDraining.Load(), zduExitPeers.Load(), zduExitTraffic.Load()
	defer func() {
		zduOn.Store(prevOn)
		zduDraining.Store(prevDr)
		zduExitPeers.Store(prevP)
		zduExitTraffic.Store(prevT)
	}()
	zduOn.Store(true)
	zduDraining.Store(true)
	zduExitPeers.Store(p)
	zduExitTraffic.Store(tr)

	t.Setenv("HS2_B468_DRAINFAST", "1")
	l, u, b := ZDUDrainEndPace(sleep)
	if killed != n || u != n {
		t.Fatalf("killed %d users %d, want %d", killed, u, n)
	}
	if linkUpAtKill != n {
		t.Fatalf("link closed before user %d of %d", linkUpAtKill, n)
	}
	if l != 1 {
		t.Fatalf("links %d, want 1", l)
	}
	minUsers := max(int(float64(zduPaceRate)*zduPaceMinStep.Seconds()), 1)
	if want := (n+minUsers-1)/minUsers + 1; b != want {
		t.Fatalf("batches %d, want %d (user batches + the link close)", b, want)
	}
	// every user batch but the last waits its share of the rate
	if lo := time.Duration(float64(n-minUsers) / float64(zduPaceRate) * float64(time.Second)); slept < lo-time.Millisecond {
		t.Fatalf("slept %v, want >= %v", slept, lo)
	}
	srv.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4096)
	for {
		if _, err := srv.Read(buf); err != nil {
			break // EOF: the link closed after the users
		}
	}
}

// Key unset: ZDUDrainEndPace keeps the b454 link pace (no user kill).
func TestB468DrainFastOffKeepsLinkPace(t *testing.T) {
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c, err := net.DialTCP("tcp", nil, ln.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := ln.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	m := &linkMeter{tcp: c}
	m.users.Store(3)
	p := &linkPeers{}
	p.add(m)
	tr := newExitTraffic(p)
	killed := 0
	tr.openKill(nil, func() { killed++ })

	prevOn, prevDr, prevP, prevT := zduOn.Load(), zduDraining.Load(), zduExitPeers.Load(), zduExitTraffic.Load()
	defer func() {
		zduOn.Store(prevOn)
		zduDraining.Store(prevDr)
		zduExitPeers.Store(prevP)
		zduExitTraffic.Store(prevT)
	}()
	zduOn.Store(true)
	zduDraining.Store(true)
	zduExitPeers.Store(p)
	zduExitTraffic.Store(tr)
	t.Setenv("HS2_B468_DRAINFAST", "0")
	if l, u, _ := ZDUDrainEndPace(func(time.Duration) {}); l != 1 || u != 3 || killed != 0 {
		t.Fatalf("key off: links %d users %d killed %d, want the link pace and no kill", l, u, killed)
	}
}
