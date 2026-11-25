package engine

import (
	"testing"
	"time"
)

func setHedge8(t *testing.T, on bool, wait time.Duration) {
	o8, ow, od := liteHedge8, hedge8Wait, b466Dbg
	setHedge7(t, on)
	liteHedge8, hedge8Wait, b466Dbg = on, wait, true
	t.Cleanup(func() { liteHedge8, hedge8Wait, b466Dbg = o8, ow, od })
}

// b466: the exit's hedge sub waits up to hedge8Wait for the first sub's conn
// (key on, hedge role only); it joins a conn published in time, also one
// reserved in time and published later, and goes on (nil) when none came.
func TestB466ExitWait(t *testing.T) {
	stop := make(chan struct{})
	defer close(stop)
	setHedge8(t, false, 30*time.Millisecond)
	tbl := newMConnTable()
	t0 := time.Now()
	if exitHedge8Wait(tbl, 1, mconnRoleHedge, stop) != nil || time.Since(t0) > 10*time.Millisecond {
		t.Fatal("key off: waited or joined")
	}
	setHedge8(t, true, 30*time.Millisecond)
	if exitHedge8Wait(tbl, 1, mconnRoleFirst, stop) != nil {
		t.Fatal("first role waited")
	}
	// (a) the first sub publishes within the wait
	mc := newMConnExit(2)
	closeAtEnd(t, mc)
	go func() {
		time.Sleep(5 * time.Millisecond)
		if tbl.reserve(2, mc) {
			tbl.publish(2, mc)
		}
	}()
	j0 := hedge8Stat.joined.Load()
	if got := exitHedge8Wait(tbl, 2, mconnRoleHedge, stop); got != mc || hedge8Stat.joined.Load() != j0+1 {
		t.Fatalf("in time: got %v", got)
	}
	// (b) nothing came: nil after the wait, no slot left
	m0 := hedge8Stat.made.Load()
	t0 = time.Now()
	if exitHedge8Wait(tbl, 3, mconnRoleHedge, stop) != nil {
		t.Fatal("none: joined")
	}
	if d := time.Since(t0); d < 25*time.Millisecond || d > 500*time.Millisecond || hedge8Stat.made.Load() != m0+1 {
		t.Fatalf("none: waited %v", d)
	}
	if tbl.has(3) {
		t.Fatal("none: slot left")
	}
	mc3 := newMConnExit(3)
	closeAtEnd(t, mc3)
	if !tbl.reserve(3, mc3) {
		t.Fatal("none: reserve after the wait failed")
	}
	// (c) reserved within the wait, published after it: joins
	mc4 := newMConnExit(4)
	closeAtEnd(t, mc4)
	go func() {
		time.Sleep(10 * time.Millisecond)
		tbl.reserve(4, mc4)
		time.Sleep(60 * time.Millisecond)
		tbl.publish(4, mc4)
	}()
	if got := exitHedge8Wait(tbl, 4, mconnRoleHedge, stop); got != mc4 {
		t.Fatalf("reserved: got %v", got)
	}
}

// b466: a hedged pair with HEDGE8 (+HEDGE7/4/3) moves a heavy download and a
// light request/response byte exact (the wait changes nothing after attach).
func TestB466HedgedBytes(t *testing.T) {
	setHedge8(t, true, 20*time.Millisecond)
	_, iran, exit := newHedge2Pair(t, nil)
	mWaitFor(t, "two subs", time.Second, func() bool { return liveSubs(iran) == 2 })
	exit.exitHedge3Mark(false)
	req := randBytes(3000, 41)
	both(t, func() error { return xfer(iran, req, 1000, 0) }, func() error { return readN(exit, req) })
	resp := randBytes(4<<20, 42)
	both(t, func() error { return xfer(exit, resp, 16<<10, 0) }, func() error { return readN(iran, resp) })
	up := randBytes(1<<20, 43)
	both(t, func() error { return xfer(iran, up, 16<<10, 0) }, func() error { return readN(exit, up) })
	iran.end(errMConnClosed, false)
	exit.end(errMConnClosed, false)
}
