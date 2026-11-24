package engine

import (
	"sync/atomic"
	"testing"
	"time"
)

// rc14b (lab1 2026-11-24): with the b457 pick cap on, a light rejoin at the
// ZDU drain end waits > 1 s for a cap slot; the slow open must not count that
// wait (off: a second open fires while the first is still queued; on: no
// second open), and must still fire for an open that hangs after its wait
// (a dark link), timed from the end of the wait.
func TestB449eCapWaitSlowOpen(t *testing.T) {
	for _, tc := range []struct {
		name         string
		capOn        bool
		wait         time.Duration // the first open queued behind the cap
		hangAfter    bool          // the open hangs on a dark link after its cap wait
		wantSecond   bool
		minAt, maxAt time.Duration // when the second open may start
	}{
		{"off_queued", false, 2 * time.Second, false, true, 900 * time.Millisecond, 2 * time.Second},
		{"on_queued", true, 2 * time.Second, false, false, 0, 0},
		{"on_dark_after_wait", true, 700 * time.Millisecond, true, true, 1600 * time.Millisecond, 2800 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldSO, oldCW := b449SlowOpen.Load(), b449eCapWait.Load()
			b449SlowOpen.Store(int64(time.Second))
			b449eCapWait.Store(tc.capOn)
			t.Cleanup(func() { b449SlowOpen.Store(oldSO); b449eCapWait.Store(oldCW) })
			b449eCapWaitRun(t, tc.wait, tc.hangAfter, tc.wantSecond, tc.minAt, tc.maxAt)
		})
	}
}

func b449eCapWaitRun(t *testing.T, wait time.Duration, hangAfter, wantSecond bool, minAt, maxAt time.Duration) {
	_, iran, exit := newLitePair(t)
	t.Cleanup(func() { iran.Close(); exit.Close() })
	hello := randBytes(300, 82)
	both(t, func() error { return xfer(iran, hello, 300, 0) }, func() error { return readN(exit, hello) })
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var calls atomic.Int32
	var secondAt atomic.Int64
	t0 := time.Now()
	iran.mu.Lock()
	orig := iran.open
	iran.open = func(p mPlane, avoid []any) (mSub, any, error) {
		if calls.Add(1) == 1 {
			done := iran.b449CapWait() // queued behind the b457 cap
			time.Sleep(wait)
			done()
			if hangAfter {
				<-release // then the picked link is dark
			}
		} else {
			secondAt.CompareAndSwap(0, int64(time.Since(t0)))
		}
		return orig(p, avoid)
	}
	iran.lightOpening = true
	iran.mu.Unlock()
	go iran.openLight(nil)
	time.Sleep(3 * time.Second)
	at := time.Duration(secondAt.Load())
	t.Logf("opens %d, second at %v", calls.Load(), at.Round(time.Millisecond))
	if got := calls.Load() > 1; got != wantSecond {
		t.Fatalf("second open=%v, want %v", got, wantSecond)
	}
	if wantSecond && (at < minAt || at > maxAt) {
		t.Fatalf("second open at %v, want in [%v,%v]", at, minAt, maxAt)
	}
}

// rc14b: "0 = off" of HS2_B449_SLOWOPEN_MS / HS2_B449_TGT_WAIT_MS works.
func TestB449EnvMs0(t *testing.T) {
	for _, c := range []struct {
		v    string
		want int
	}{{"", 1000}, {"0", 0}, {" 0 ", 0}, {"250", 250}, {"-1", 1000}, {"x", 1000}} {
		t.Setenv("HS2_RC14B_T", c.v)
		if got := b449EnvMs0("HS2_RC14B_T", 1000); got != c.want {
			t.Fatalf("%q: got %d want %d", c.v, got, c.want)
		}
	}
}
