package engine

import (
	"testing"
	"time"
)

// The R1 round-3 lead's note, measured on the move rule as it stands (an
// open item, not a fix): the edge cannot see the exit's send queue, so a
// download moves on what arrives alone (moveRxMin a tick, moveRxRun ticks
// in a row, moveBytes over 2 s). An app-limited flow of 1.5 Mbit/s each way
// (a call; its writers never wait) so moves to BULK, as a download, within
// half a second, although its sender never had a backlog; one of 300
// kbit/s (3.75 KB a tick, under moveRxMin) stays on QUIET; and the upload
// direction of the call, a writer inside Write 1 ms a tick, never moves it.
func TestMoveRuleAppLimitedCall(t *testing.T) {
	run := func(bps float64, ticks int) (int, string) {
		var r moveRule
		per := uint64(bps / 8 / float64(time.Second/moveTick))
		var now int64
		var tx, rx uint64
		var busy time.Duration
		for i := 0; i < ticks; i++ {
			now += int64(moveTick)
			tx += per
			rx += per
			busy += time.Millisecond
			if due, dir := r.tick(tx, rx, busy, now, true, true); due {
				return i, dir
			}
		}
		return -1, ""
	}
	at, dir := run(1.5e6, 50)
	if at < 0 || dir != "down" || at > 5 {
		t.Fatalf("a 1.5 Mbit/s call: moved at tick %d, %q; want down within 5 ticks (the rule as it is)", at, dir)
	}
	t.Logf("a 1.5 Mbit/s app-limited call moves to BULK as a download at tick %d (%v)", at, time.Duration(at+1)*moveTick)
	if at, dir := run(0.3e6, 50); at >= 0 {
		t.Fatalf("a 300 kbit/s call moved (%s) at tick %d", dir, at)
	}
}
