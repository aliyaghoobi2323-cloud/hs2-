package engine

import (
	"testing"
	"time"
)

// The lead's note for round 3 (a class study of r1-r2j): the edge cannot
// see the exit's send queue, so a download moves to BULK on what arrives
// (moveRxMin a tick, moveRxRun ticks in a row, and moveBytes over 2 s): an
// app-limited flow, a video call or a live stream, moves to BULK once it
// carries a few hundred kbit/s, whatever its sender's backlog. This test
// measures where an app-limited bidirectional flow ends up (its writer
// never waits in Write: busy 0) at 0.25, 0.5, 1.5 and 5 Mbit/s each way, as
// the rule stands: the 1.5 Mbit/s one moves within half a second. The fix
// the note proposes (a "backlogged" bit from the exit, the download's
// sender) is a wire change and an open item of round 3.
func TestMoveRuleAppLimited(t *testing.T) {
	tick := int64(moveTick)
	for _, c := range []struct {
		mbit  float64
		moves bool
	}{{0.25, false}, {0.5, true}, {1.5, true}, {5, true}} {
		var r moveRule
		now := int64(time.Hour)
		r.tick(0, 0, 0, now, true)
		per := uint64(c.mbit * 1e6 / 8 / 10) // bytes a tick, each way
		var tx, rx uint64
		at := -1
		for i := 0; i < 50; i++ {
			now += tick
			tx += per
			rx += per
			if due, _ := r.tick(tx, rx, 0, now, true); due {
				at = i
				break
			}
		}
		t.Logf("app-limited %.2f Mbit/s each way: moves to BULK %v (tick %d)", c.mbit, at >= 0, at)
		if (at >= 0) != c.moves {
			t.Fatalf("%.2f Mbit/s: moved %v, want %v (the rule as it stands)", c.mbit, at >= 0, c.moves)
		}
		if c.mbit == 1.5 && at > 5 {
			t.Fatalf("1.5 Mbit/s moved at tick %d, want within half a second", at)
		}
	}
}
