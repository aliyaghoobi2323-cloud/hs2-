package engine

import (
	"os"
	"sync"
	"testing"
	"time"
)

// b312: TestStripeOverlapOpensDuringHandshake lost every sub now and then
// (~0.5-3 % of -race runs, with HS2_B307_ASYNCCLOSE on or off). The exit's
// first sub died mid-handshake with "read pipe: i/o timeout": the test ran
// with HS2_MCONN_DEADLINE_FIX off (production has it on), and setReadLimit's
// wake landed after the raw reader had already switched to readLoop.
// setStripeOverlap now turns the fix on as production does.

// TestB312OverlapStaleDeadline forces the gap: the exit's raw reader starts
// only once the exit's setReadLimit stored the limit (0: no raw bytes) and
// before it wakes the reader, so the reader switches to readLoop first. With
// HS2_MCONN_DEADLINE_FIX (setStripeOverlap) the wake is under m.mu while the
// reader is still raw and switchRead clears it: the subs live. Without
// (HS2_B312_NOFIX=1) the wake's past deadline kills the exit's first sub
// mid-handshake and the connection ends: "mconn: every sub died".
func TestB312OverlapStaleDeadline(t *testing.T) {
	setStripeOverlap(t, true, true)
	if os.Getenv("HS2_B312_NOFIX") == "1" {
		mconnDeadlineFix.Store(false) // restored by setStripeOverlap's cleanup
	}
	stored := make(chan struct{})
	var once sync.Once
	h := func(m *mConn) {
		if m.iran {
			return
		}
		once.Do(func() { close(stored) })
		time.Sleep(50 * time.Millisecond) // the reader switches meanwhile
	}
	old := setReadLimitTestHook.Swap(&h)
	t.Cleanup(func() { setReadLimitTestHook.Store(old) })
	data := make([]byte, 2<<20)
	for i := range data {
		data[i] = byte(i * 7)
	}
	_, iran, exit := overlapStart(t, 2)
	select {
	case <-stored:
	case <-time.After(3 * time.Second):
		t.Fatal("the exit never set its read limit")
	}
	done := make(chan error, 2)
	go func() { done <- readN(exit, data) }()
	go func() { done <- xfer(iran, data, 16<<10, 0) }()
	time.Sleep(200 * time.Millisecond)
	exit.wmu.Unlock()
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	mWaitFor(t, "handshake over", 2*time.Second, func() bool { return !handshaking(iran) && !handshaking(exit) })
	iran.Close()
	exit.Close()
}

// TestB312OverlapSlowAsyncClose: b307's async close (every stream close
// going on alone) does not break the overlap upgrade.
func TestB312OverlapSlowAsyncClose(t *testing.T) {
	oldA, oldD := b307AsyncClose.Load(), b307TestCloseDelay
	b307AsyncClose.Store(true)
	b307TestCloseDelay = 20 * time.Millisecond
	t.Cleanup(func() {
		b307AsyncClose.Store(oldA)
		b307TestCloseDelay = oldD
	})
	TestStripeOverlapOpensDuringHandshake(t)
}
