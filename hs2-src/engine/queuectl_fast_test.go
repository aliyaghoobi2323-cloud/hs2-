package engine

import (
	"testing"
	"time"
)

// HS2_QUEUE_CTL_FAST: a big queue is capped after qFastTicks, a small one
// (and any queue with the key off) after qStartTicks as before.
func TestQueueCtlFastStart(t *testing.T) {
	defer func(v bool) { queueCtlFast = v }(queueCtlFast)
	firstCapAt := func(fast bool, d time.Duration, g float64) int {
		queueCtlFast = fast
		var q qCtl
		now := time.Unix(1e9, 0)
		for i := 1; i <= 10; i++ {
			now = now.Add(healthTick)
			if r, _ := q.step(qIn{now: now, delay: d, g: g}); r > 0 {
				return i
			}
		}
		return 0
	}
	firstCap := func(fast bool, d time.Duration) int { return firstCapAt(fast, d, qC) }
	// lat59/qctl61: a big queue under light traffic (the edge seeing the
	// download's queue behind what it sends) is not capped at all.
	if got := firstCapAt(true, 80*time.Millisecond, 7e6/8); got != 0 {
		t.Errorf("fast, 7 Mbit/s: first cap at tick %d, want none", got)
	}
	if got := firstCapAt(true, 80*time.Millisecond, qFastMinRate); got != qFastTicks {
		t.Errorf("fast, at qFastMinRate: first cap at tick %d, want %d", got, qFastTicks)
	}
	for _, c := range []struct {
		fast bool
		d    time.Duration
		want int
	}{
		{false, 80 * time.Millisecond, qStartTicks},
		{true, 80 * time.Millisecond, qFastTicks},
		{true, qFastDelay, qFastTicks},
		{true, 20 * time.Millisecond, qStartTicks},
		{true, 5 * time.Millisecond, 0},
	} {
		if got := firstCap(c.fast, c.d); got != c.want {
			t.Errorf("fast=%v delay=%v: first cap at tick %d, want %d", c.fast, c.d, got, c.want)
		}
	}
}
