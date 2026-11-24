package engine

import (
	"net"
	"testing"
	"time"
)

func paceLinks(users ...int) ([]zduPaceLink, *[]int) {
	var order []int
	ls := make([]zduPaceLink, len(users))
	for i, u := range users {
		ls[i] = zduPaceLink{users: u, close: func() { order = append(order, u) }}
	}
	return ls, &order
}

// b447/b454: idle links go first, every batch (the last included) waits its
// users' share of the rate, the whole close takes users/rate, and past the
// budget the rate rises (every batch still paced, none past the budget).
func TestZDUPaceCloseBatches(t *testing.T) {
	ls, order := paceLinks(300, 0, 100, 250, 0, 50)
	var slept []time.Duration
	// 1000 users/s, min step 100 ms = 100 users: {0,0,50,100} {250} {300}
	l, u, b := zduPaceClose(ls, 1000, 100*time.Millisecond, 30*time.Second, func(d time.Duration) { slept = append(slept, d) })
	if l != 6 || u != 700 || b != 3 {
		t.Fatalf("links %d users %d batches %d, want 6 700 3", l, u, b)
	}
	if got := *order; got[0] != 0 || got[1] != 0 || got[len(got)-1] != 300 {
		t.Fatalf("close order %v: idle first, largest last", got)
	}
	if len(slept) != 2 || slept[0] != 150*time.Millisecond || slept[1] != 250*time.Millisecond {
		t.Fatalf("slept %v, want [150ms 250ms] (the last batch paced too)", slept)
	}
}

// b454: at 1x, 2x, 4x and 8x users (192 links) every batch is paced, the
// close takes users/rate (capped by the budget) and no batch is a burst.
func TestB454PaceScales(t *testing.T) {
	for _, total := range []int{6000, 12068, 24000, 48000, 96000} {
		us := make([]int, 192)
		for i := range us {
			us[i] = total / 192
		}
		ls, _ := paceLinks(us...)
		var sum time.Duration
		steps := 0
		l, u, b := zduPaceClose(ls, 2667, 25*time.Millisecond, 30*time.Second, func(d time.Duration) { sum += d; steps++ })
		if l != 192 || u != (total/192)*192 {
			t.Fatalf("%d: links %d users %d", total, l, u)
		}
		if steps != b-1 {
			t.Fatalf("%d: %d batches but %d steps: a batch went unpaced", total, b, steps)
		}
		want := zduPaceDur(u, 2667, 30*time.Second)
		maxBatch := u / b * 2
		if sum > want || sum < want*8/10 {
			t.Fatalf("%d users: paced %v, want ~%v", total, sum, want)
		}
		if maxBatch > 1000 {
			t.Fatalf("%d users: batches of ~%d users: a burst", total, maxBatch/2)
		}
		if got := ZDUDrainEndPaceLead(u); zduPaceRate == 2667 && got != want {
			t.Fatalf("lead %v want %v", got, want)
		}
	}
	// one link over the bound is a batch of its own; rate 0 closes at once; empty input is a no-op
	ls, _ := paceLinks(5000)
	if l, _, b := zduPaceClose(ls, 2667, 25*time.Millisecond, time.Second, func(time.Duration) { t.Fatal("slept") }); l != 1 || b != 1 {
		t.Fatalf("single big link: links %d batches %d", l, b)
	}
	ls, _ = paceLinks(10, 20, 30)
	if l, _, b := zduPaceClose(ls, 0, 25*time.Millisecond, time.Second, func(time.Duration) { t.Fatal("slept with rate 0") }); l != 3 || b != 1 {
		t.Fatalf("rate 0: links %d batches %d", l, b)
	}
	if l, u, b := zduPaceClose(nil, 2667, time.Millisecond, time.Second, func(time.Duration) { t.Fatal("slept") }); l+u+b != 0 {
		t.Fatal("empty input closed something")
	}
	if zduPaceRateEnv("") != 2667 || zduPaceRateEnv("x") != 2667 || zduPaceRateEnv("0") != 0 || zduPaceRateEnv(" 5000 ") != 5000 {
		t.Fatal("HS2_ZDU_PACE_RATE parse")
	}
	if ZDUDrainEndPaceLead(0) != 0 || zduPaceDur(1000, 0, time.Second) != 0 {
		t.Fatal("lead with no users or rate 0")
	}
}

// ZDUDrainEndPace acts only on a draining exit with the key on, and closes
// the exit's link sockets.
func TestZDUDrainEndPaceGate(t *testing.T) {
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
	m.users.Store(7)
	p := &linkPeers{}
	p.add(m)
	prevOn, prevDr, prevP := zduOn.Load(), zduDraining.Load(), zduExitPeers.Load()
	defer func() { zduOn.Store(prevOn); zduDraining.Store(prevDr); zduExitPeers.Store(prevP) }()
	zduExitPeers.Store(p)
	t.Setenv("HS2_B468_DRAINFAST", "0") // the b454 link pace (b468 has its own test)

	zduOn.Store(false)
	zduDraining.Store(true)
	if l, _, _ := ZDUDrainEndPace(func(time.Duration) {}); l != 0 {
		t.Fatal("key off: closed links")
	}
	zduOn.Store(true)
	zduDraining.Store(false)
	if l, _, _ := ZDUDrainEndPace(func(time.Duration) {}); l != 0 {
		t.Fatal("not draining: closed links")
	}
	zduDraining.Store(true)
	if l, u, b := ZDUDrainEndPace(func(time.Duration) {}); l != 1 || u != 7 || b != 1 {
		t.Fatalf("draining exit: links %d users %d batches %d, want 1 7 1", l, u, b)
	}
	srv.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := srv.Read(make([]byte, 1)); err == nil {
		t.Fatal("link socket still open after the paced close")
	}
}
