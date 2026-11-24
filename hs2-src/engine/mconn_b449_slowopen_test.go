package engine

import (
	"sync/atomic"
	"testing"
	"time"
)

// b449e: Iran's light open picks a dark link and blocks (OpenStream/header
// write on a blackholed link: 4+ s in lab3 b449c n2, the exit's 3 s grace ran
// out, a heavy download reset). Off: no new sub for 2 s. On: a second open
// after the slow-open time joins in time; the stuck one joins later as a spare.
func TestB449SlowOpenSecond(t *testing.T) {
	for _, so := range []time.Duration{0, time.Second} {
		t.Run(map[bool]string{true: "on", false: "off"}[so > 0], func(t *testing.T) {
			old := b449SlowOpen.Load()
			b449SlowOpen.Store(int64(so))
			t.Cleanup(func() { b449SlowOpen.Store(old) })
			b449SlowOpenRun(t, so > 0)
		})
	}
}

func b449SlowOpenRun(t *testing.T, on bool) {
	_, iran, exit := newLitePair(t)
	t.Cleanup(func() { iran.Close(); exit.Close() })
	hello := randBytes(300, 81)
	both(t, func() error { return xfer(iran, hello, 300, 0) }, func() error { return readN(exit, hello) })
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var calls atomic.Int32
	iran.mu.Lock()
	orig := iran.open
	iran.open = func(p mPlane, avoid []any) (mSub, any, error) {
		if calls.Add(1) == 1 {
			<-release // the first open hangs on a dark link
		}
		return orig(p, avoid)
	}
	n0 := iran.nsubs
	iran.lightOpening = true
	iran.mu.Unlock()
	t0 := time.Now()
	go iran.openLight(nil)
	joined := func() bool {
		iran.mu.Lock()
		defer iran.mu.Unlock()
		return iran.nsubs > n0
	}
	for time.Since(t0) < 2500*time.Millisecond && !joined() {
		time.Sleep(20 * time.Millisecond)
	}
	ok := joined()
	t.Logf("slow open on=%v: a sub joined=%v after %v, opens %d", on, ok, time.Since(t0).Round(time.Millisecond), calls.Load())
	if on != ok {
		t.Fatalf("on=%v but joined=%v", on, ok)
	}
	if on && time.Since(t0) > 2*time.Second {
		t.Fatalf("second open too late: %v", time.Since(t0))
	}
}
