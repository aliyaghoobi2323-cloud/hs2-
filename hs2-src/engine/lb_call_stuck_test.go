package engine

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// withCallStuck sets the stuck-call trigger, the paths of links and the
// busy reading for a test.
func withCallStuck(t *testing.T, paths map[any]string, busy func(*msub) bool) {
	t.Helper()
	oldOn, oldBusy := lbCallStuck, callStuckBusy
	lbCallStuck = true
	callStuckBusy = busy
	for k, p := range paths {
		callLinkPath.Store(k, p)
	}
	t.Cleanup(func() {
		lbCallStuck, callStuckBusy = oldOn, oldBusy
		callLinkPath.Range(func(k, _ any) bool { callLinkPath.Delete(k); return true })
	})
}

func onKeys(keys ...any) func(*msub) bool {
	return func(s *msub) bool { return containsAny(keys, s.key) }
}

// HS2_LB_CALL_STUCK (lb_call_stuck.go): a call whose two subs are on one
// path, both sockets busy (loss recovery / backlog) for callStuckFor, gets a
// fresh sub off that path at once (no lowered path needed); the old subs are
// given up only after the fresh one has read, never leaving zero subs.
func TestMConnCallStuckBusy(t *testing.T) {
	withRescue(t, true)
	withCallRescue(t, true)
	setCloseHS(t, true)
	setCall(t, "1")
	withCallStuck(t, map[any]string{0: "B", 1: "B"}, func(*msub) bool { return false })
	before := runtime.NumGoroutine()
	n, iran, exit := newLitePair(t)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var lat atomic.Int64
	callStream(iran, exit, 2500, stop, &wg, &lat)
	mWaitFor(t, "call on two subs", mLiteAfter+4*time.Second, func() bool {
		return liveSubs(iran) == 2 && liveSubs(exit) == 2
	})
	o0, m0 := callStuckOpened.Load(), callStuckMoved.Load()
	callStuckBusy = onKeys(0, 1) // path B goes bad: both sockets busy (not lowered)
	var zero atomic.Bool
	watch := make(chan struct{})
	go func() {
		defer close(watch)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if liveSubs(iran) == 0 {
				zero.Store(true)
			}
			time.Sleep(time.Millisecond)
		}
	}()
	start := time.Now()
	mWaitFor(t, "fresh sub opened", 3*time.Second, func() bool { return callStuckOpened.Load() > o0 })
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("trigger after %v", d)
	}
	mWaitFor(t, "call moved off path B", 5*time.Second, func() bool {
		return callStuckMoved.Load() >= m0+2 && !subOnKeys(iran, 0, 1)
	})
	l := n.link(2)
	if l == nil || l.plane != planeLight {
		t.Fatal("no fresh light sub")
	}
	marked := false
	for _, a := range l.avoid {
		if c, ok := a.(callStuckMark); ok && c.path == "B" {
			marked = true
		}
	}
	if !marked || !containsKey(l.avoid, 0) || !containsKey(l.avoid, 1) {
		t.Fatalf("fresh sub avoid %v: want the stuck mark for B and the old links", l.avoid)
	}
	lat.Store(0)
	time.Sleep(500 * time.Millisecond)
	if lat.Load() == 0 {
		t.Fatal("the call stopped flowing after the move")
	}
	close(stop)
	wg.Wait()
	<-watch
	if zero.Load() {
		t.Fatal("the connection was left with no sub")
	}
	if callStuckOpened.Load() != o0+1 {
		t.Fatalf("%d triggers, want 1", callStuckOpened.Load()-o0)
	}
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// No trigger on a healthy path (sockets not busy, data flowing), nor when
// the subs are on two paths even if busy, nor with HS2_LB_CALL_STUCK=0.
func TestMConnCallStuckNoFalse(t *testing.T) {
	for _, tc := range []struct {
		name  string
		paths map[any]string
		busy  func(*msub) bool
		off   bool
	}{
		{"healthy", map[any]string{0: "B", 1: "B"}, func(*msub) bool { return false }, false},
		{"two paths", map[any]string{0: "A", 1: "B"}, onKeys(0, 1), false},
		{"key off", map[any]string{0: "B", 1: "B"}, onKeys(0, 1), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withRescue(t, true)
			withCallRescue(t, true)
			setCloseHS(t, true)
			setCall(t, "1")
			withCallStuck(t, tc.paths, tc.busy)
			if tc.off {
				lbCallStuck = false
			}
			before := runtime.NumGoroutine()
			n, iran, exit := newLitePair(t)
			stop := make(chan struct{})
			var wg sync.WaitGroup
			var lat atomic.Int64
			callStream(iran, exit, 2500, stop, &wg, &lat)
			mWaitFor(t, "call on two subs", mLiteAfter+4*time.Second, func() bool {
				return liveSubs(iran) == 2 && liveSubs(exit) == 2
			})
			o0 := callStuckOpened.Load()
			time.Sleep(2 * time.Second)
			if got := callStuckOpened.Load() - o0; got != 0 || n.count(planeLight) != 2 {
				t.Fatalf("%d triggers, %d light opens", got, n.count(planeLight))
			}
			close(stop)
			wg.Wait()
			closeAndWait(t, iran, exit)
			noLeak(t, before)
		})
	}
}

// Unit: the busy evidence must hold callStuckFor without a break, one
// trigger per callStuckEvery per connection, the receive gap triggers alone,
// the process bound, the mark, the path record.
func TestCallStuckLocked(t *testing.T) {
	withRescue(t, true)
	withCallRescue(t, true)
	busy := true
	withCallStuck(t, map[any]string{10: "B", 11: "B"}, func(*msub) bool { return busy })
	newM := func() (*mConn, []*msub) {
		m := &mConn{iran: true, lite: true}
		m.callOn = true
		a, b := &msub{key: 10}, &msub{key: 11}
		return m, []*msub{a, b}
	}
	// the opener fails at once (openLight clears lightOpening); the trigger
	// is observed through the counters
	m, live := newM()
	m.open = func(mPlane, []any) (mSub, any, error) { return nil, nil, errMConnClosed }
	now := time.Now()
	o0 := callStuckOpened.Load()
	m.callStuckLocked(live, now)
	busy = false
	m.callStuckLocked(live, now.Add(300*time.Millisecond)) // a break
	busy = true
	m.callStuckLocked(live, now.Add(400*time.Millisecond))
	m.callStuckLocked(live, now.Add(800*time.Millisecond)) // 400 ms since the break
	if callStuckOpened.Load() != o0 {
		t.Fatal("trigger before callStuckFor of unbroken evidence")
	}
	m.callStuckLocked(live, now.Add(950*time.Millisecond))
	if callStuckOpened.Load() != o0+1 || m.stuckKeys == nil {
		t.Fatal("no trigger after callStuckFor")
	}
	time.Sleep(50 * time.Millisecond) // the failed open clears lightOpening
	m.mu.Lock()
	m.stuckKeys = nil
	m.lightRetryAt = time.Time{}
	m.lightOpening = false
	m.mu.Unlock()
	for d := time.Second; d < 5*time.Second; d += 100 * time.Millisecond {
		m.callStuckLocked(live, now.Add(d))
	}
	if callStuckOpened.Load() != o0+1 {
		t.Fatal("second trigger within callStuckEvery")
	}
	// the receive gap alone (sockets fine): this side sends, nothing comes
	busy = false
	m2, live2 := newM()
	m2.open = m.open
	live2[0].lastRecv.Store(now.Add(-time.Second).UnixNano())
	live2[0].lastPush = now.Add(-10 * time.Millisecond)
	g0 := callStuckGapN.Load()
	m2.callStuckLocked(live2, now)
	if callStuckGapN.Load() != g0+1 {
		t.Fatal("no trigger on a receive gap while sending")
	}
	// a gap while this side is silent too is no evidence
	m3, live3 := newM()
	live3[0].lastRecv.Store(now.Add(-time.Second).UnixNano())
	live3[0].lastPush = now.Add(-time.Second)
	m3.callStuckLocked(live3, now)
	if callStuckGapN.Load() != g0+1 {
		t.Fatal("trigger on a gap with no sends")
	}
	// the process bound
	sec := time.Unix(now.Unix()+100, 0)
	for i := 0; i < callStuckPerSec; i++ {
		if !callStuckAllow(sec) {
			t.Fatalf("refused at %d", i)
		}
	}
	if callStuckAllow(sec) {
		t.Fatal("past the per-second bound")
	}
	// mark
	rest, p, ok := takeCallStuckMark([]any{1, callStuckMark{path: "B"}, 2})
	if !ok || p != "B" || len(rest) != 2 {
		t.Fatalf("mark: %v %q %v", rest, p, ok)
	}
	// path record forgets gone links
	var l Link = &meteredFakeLink{}
	prev := callPathMark(nil, map[string][]*managedLink{"X": {{link: l}}})
	if callPathOf(l) != "X" {
		t.Fatal("path not recorded")
	}
	callPathMark(prev, map[string][]*managedLink{})
	if callPathOf(l) != "" {
		t.Fatal("gone link kept its path")
	}
}
