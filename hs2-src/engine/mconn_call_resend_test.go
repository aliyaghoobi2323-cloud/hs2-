package engine

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// r68fix (mconn_call_resend.go): a call whose two subs sit on a path that
// goes dark (both ways: writes vanish) gets a fresh sub from the stuck-call
// trigger; the old subs are given up with a CLOSE-REQ that the dark path
// swallows, and die a moment later. With HS2_MCONN_CALL_RESEND the chunks
// only the dark subs carried are sent again on the fresh sub: the call keeps
// flowing and neither side ends. With the key off (the rel68 rule) the
// connection is reset (a sub died holding the only copy / a hole that never
// fills) — the lab's call cut for the rest of the run.
func TestMConnCallResendDarkPath(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(map[bool]string{true: "resend", false: "off"}[on], func(t *testing.T) {
			ended, flowing := callDarkPath(t, on)
			if on && (ended || !flowing) {
				t.Fatalf("resend on: ended=%v flowing=%v, want the call alive", ended, flowing)
			}
			if !on {
				t.Logf("resend off (the old rule): ended=%v flowing=%v", ended, flowing)
			}
		})
	}
}

func callDarkPath(t *testing.T, resend bool) (ended, flowing bool) {
	before := runtime.NumGoroutine()
	withRescue(t, true)
	withCallRescue(t, true)
	setCloseHS(t, true)
	setCall(t, "1")
	oldR, oldC := stripeResend.Load(), mconnCallResend.Load()
	stripeResend.Store(true)
	mconnCallResend.Store(resend)
	t.Cleanup(func() { stripeResend.Store(oldR); mconnCallResend.Store(oldC) })
	// the busy hook is set before the pair starts and switched by an atomic
	// (the manage goroutines read callStuckBusy; reassigning it races)
	var dark atomic.Bool
	withCallStuck(t, map[any]string{0: "B", 1: "B"}, func(s *msub) bool {
		return dark.Load() && containsAny([]any{0, 1}, s.key)
	})
	n, iran, exit := newLitePair(t)
	// b266: the pair (its manage and sub goroutines) must be gone before the
	// cleanups above restore the plain globals they read (lbRescue,
	// lbCallStuck, callStuckBusy): cleanups run last-in first-out, so this
	// one runs first, also when the test fails early. The race detector
	// caught a closed pair's manage loop still ticking during the restore.
	t.Cleanup(func() { closeQuiet(before, iran, exit) })
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var lat atomic.Int64
	callStream(iran, exit, 2500, stop, &wg, &lat)
	mWaitFor(t, "call on two subs", mLiteAfter+4*time.Second, func() bool {
		return liveSubs(iran) == 2 && liveSubs(exit) == 2
	})
	time.Sleep(1500 * time.Millisecond) // the call is call-like on both sides
	o0 := callStuckOpened.Load()
	// path B goes dark both ways: every write on links 0 and 1 vanishes
	for _, k := range []int{0, 1} {
		l := n.link(k)
		l.iran.drop.Store(true)
		l.exit.drop.Store(true)
	}
	dark.Store(true)
	mWaitFor(t, "fresh sub opened", 3*time.Second, func() bool { return callStuckOpened.Load() > o0 })
	time.Sleep(700 * time.Millisecond)
	// the dark links die (their sockets time out)
	for _, k := range []int{0, 1} {
		l := n.link(k)
		l.iran.Close()
		l.exit.Close()
	}
	time.Sleep(1500 * time.Millisecond)
	lat.Store(0)
	time.Sleep(700 * time.Millisecond)
	flowing = lat.Load() > 0
	iran.mu.Lock()
	ended = iran.ended
	iran.mu.Unlock()
	exit.mu.Lock()
	ended = ended || exit.ended
	exit.mu.Unlock()
	close(stop)
	iran.Close()
	exit.Close()
	wg.Wait()
	closeQuiet(before, iran, exit)
	return ended, flowing
}
