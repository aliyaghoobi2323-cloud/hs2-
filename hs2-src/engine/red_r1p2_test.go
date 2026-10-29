package engine

import (
	"context"
	"net"
	"testing"
	"time"
)

// R1 red team, second pass (method step 4; docs/architecture/wip/r1/
// redteam2/REPORT.fa.md): the two-way rule against a download whose app
// acknowledges in its own data (R2-1), and the idle QUIET link's one-way
// delay beat (Z10).

func ring(v ...uint32) []uint32 {
	r := make([]uint32, twoWayWin)
	for i := range r {
		r[i] = v[i%len(v)]
	}
	return r
}

// R2-1: QUIC (or any UDP) inside a VLESS connection, or a VPN over TCP
// inside one, acknowledges in its own data: its user sends 1 to 3 % of
// what it gets, in every tick, far under RTMax. The two-way rule read that
// as a call: neither the exit's hint nor the edge's move, so the download
// stayed on its QUIET link (cubic) at any rate. A call, also a lopsided
// one, stays two-way; a download and an upload acknowledged in their own
// data do not.
func TestRed2TwoWayAckedDownload(t *testing.T) {
	cases := []struct {
		name        string
		user, other []uint32 // bytes a move tick (100 ms)
		seen        int
		old, fixed  int // twoWayState with TwoWayRatio 0 and 16
	}{
		{"call 1.5/1.5 Mbit/s", ring(18750), ring(18750), 10, twoWayYes, twoWayYes},
		{"call held back (clumps)", ring(37500, 0), ring(0, 0, 0, 61440), 10, twoWayNo, twoWayNo},
		{"call, camera small: 0.25 up, 2.4 Mbit/s down", ring(3125), ring(30000), 10, twoWayYes, twoWayYes},
		{"call, camera small, held back: 0.25 up, 4 Mbit/s down in clumps", ring(3125), ring(90000, 0), 10, twoWayYes, twoWayYes},
		{"QUIC download 5 Mbit/s, acks 2 %", ring(1250), ring(62500), 10, twoWayYes, twoWayNo},
		{"QUIC download 40 Mbit/s, acks 2 %", ring(10000), ring(500000), 10, twoWayYes, twoWayNo},
		{"VPN over TCP, 10 Mbit/s down, acks 3 %", ring(3750), ring(125000), 10, twoWayYes, twoWayNo},
		{"QUIC upload 3.2 Mbit/s, acks 3 %", ring(40000), ring(1200), 10, twoWayYes, twoWayNo},
		{"QUIC download, young (3 ticks)", ring(1250), ring(62500), 3, twoWayPending, twoWayNo},
		{"call, young (3 ticks)", ring(18750), ring(18750), 3, twoWayPending, twoWayPending},
		{"plain download (request once)", ring(400, 0, 0, 0, 0, 0, 0, 0, 0, 0), ring(500000), 10, twoWayNo, twoWayNo},
	}
	for _, c := range cases {
		for _, k := range []struct {
			ratio, want int
		}{{0, c.old}, {16, c.fixed}} {
			withTune(t, TwoWayRatio, k.ratio)
			if got := twoWayState(c.user, c.other, twoWayWin-1, c.seen); got != k.want {
				t.Errorf("%s, ratio %d: state %d, want %d", c.name, k.ratio, got, k.want)
			}
		}
	}
}

// R2-1 in the realtime lane: a steady download of 3 Mbit/s acknowledged in
// its own data (the exit's side: it sends the download, gets the acks) got
// the hold (RTHold) as a call does once it turned to clumps; with the ratio
// it is never loosely a call, so its hold runs out. A call in clumps keeps
// it either way (TestRTHoldKeepsCall).
func TestRed2HoldAckedDownload(t *testing.T) {
	steady := func(n int) [][2]int {
		var v [][2]int
		for range n {
			v = append(v, [2]int{37500, 900})
		}
		return v
	}
	clumps := func(n int) [][2]int {
		var v [][2]int
		for i := range n {
			tx := 0
			if i%4 == 3 {
				tx = 60 << 10
			}
			v = append(v, [2]int{tx, 900})
		}
		return v
	}
	zeros := map[int]int{}
	for _, ratio := range []int{0, 16} {
		withTune(t, TwoWayRatio, ratio)
		withTune(t, RTHold, 20)
		var c flowClassifier
		var f flowStats
		var at time.Duration
		classRun(&c, &f, &at, steady(30))
		b := classRun(&c, &f, &at, clumps(60))
		for _, v := range b {
			if v == 0 {
				zeros[ratio]++
			}
		}
		t.Logf("ratio %d: the acked download in clumps for 6 s: %d of %d ticks without a realtime allowance", ratio, zeros[ratio], len(b))
	}
	if zeros[0] > 10 {
		t.Fatalf("without the ratio the acked download lost the hold (%d ticks without): the case is not shown", zeros[0])
	}
	if zeros[16] < 30 {
		t.Fatalf("with the ratio the acked download kept the realtime hold (%d of 60 ticks without)", zeros[16])
	}
}

// Z10: an idle QUIET link's one-way delay loop sends nothing while no BULK
// rate is noted (OWDIdle), sends at once when one is, and parks again
// owdIdleAfter after the last; with OWDIdle 0 it beats every 0.5 to 1.5 s.
func TestRed2OWDIdleParks(t *testing.T) {
	run := func(idle int, steps func(a, b *owdLink)) {
		withTune(t, OWDIdle, idle)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		c1, c2 := net.Pipe()
		a, b := &owdLink{quiet: true}, &owdLink{quiet: true}
		done := make(chan struct{}, 2)
		go func() { owdLoop(ctx, c1, a); done <- struct{}{} }()
		go func() { owdLoop(ctx, c2, b); done <- struct{}{} }()
		steps(a, b)
		cancel()
		<-done
		<-done
	}
	run(0, func(a, b *owdLink) {
		time.Sleep(3500 * time.Millisecond)
		if n := a.msgs.Load() + b.msgs.Load(); n < 3 {
			t.Fatalf("OWDIdle 0: %d messages in 3.5 s, want the 1 s beat", n)
		}
		t.Logf("OWDIdle 0, idle 3.5 s: %d + %d messages", a.msgs.Load(), b.msgs.Load())
	})
	run(1, func(a, b *owdLink) {
		time.Sleep(2500 * time.Millisecond)
		if n := a.msgs.Load() + b.msgs.Load(); n != 0 {
			t.Fatalf("idle pool: %d messages in 2.5 s, want none", n)
		}
		t0 := time.Now()
		a.noteRate(1e6)
		b.noteRate(1e6)
		for a.msgs.Load() == 0 || b.msgs.Load() == 0 {
			if time.Since(t0) > 300*time.Millisecond {
				t.Fatalf("a busy pool: no message both ways 300 ms after the rate (%d, %d)", a.msgs.Load(), b.msgs.Load())
			}
			time.Sleep(5 * time.Millisecond)
		}
		first := time.Since(t0)
		// busy for 2 s: the beat follows the rate (owdTickOf)
		for range 40 {
			a.noteRate(1e6)
			b.noteRate(1e6)
			time.Sleep(50 * time.Millisecond)
		}
		busy := a.msgs.Load() + b.msgs.Load()
		// idle again: parked after owdIdleAfter and a beat
		time.Sleep(owdIdleAfter + 400*time.Millisecond)
		n0 := a.msgs.Load() + b.msgs.Load()
		time.Sleep(2 * time.Second)
		n1 := a.msgs.Load() + b.msgs.Load()
		t.Logf("first message both ways %v after the rate; %d messages in 2 s busy; %d in the 2 s after parking (parks %d, %d)",
			first.Round(time.Millisecond), busy, n1-n0, a.parks.Load(), b.parks.Load())
		if busy < 20 {
			t.Fatalf("busy: %d messages in 2 s at 1 MB/s (the beat owdTickOf: ~100 ms)", busy)
		}
		if n1 != n0 {
			t.Fatalf("idle again: %d messages after parking", n1-n0)
		}
	})
}
