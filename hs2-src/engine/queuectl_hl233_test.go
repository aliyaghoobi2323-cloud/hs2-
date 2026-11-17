package engine

import (
	"fmt"
	"testing"
	"time"
)

// hl233 (lab hl232 fS2 d8, exit side): a cap kept at 470.2 (qKeepSure x seen
// 477.4) took the burst probe to 484.3; the next tick queued 20 ms at 458.6
// and the onset rule took the cap to 465.5 (qSeenFrac x seen), under the kept
// cap that had fit. Now it goes back to the kept cap. A queue >= qFastDelay,
// or no burst probe before it, still takes qSeenFrac x seen.
func TestQueueCtlHl233OnsetBackToKept(t *testing.T) {
	defer func(k bool) { queueCtlKeep = k }(queueCtlKeep)
	queueCtlKeep = true
	if !qSeenOn() {
		t.Skip("HS2_QUEUE_CTL_SEEN=0")
	}
	for _, c := range []struct {
		qd   float64
		want float64
	}{
		{20, 470.2},
		{45, qSeenFrac * 477.4},
	} {
		now := time.Unix(1e9, 0)
		q := &qCtl{rate: mb232(470.2), seen: mb232(477.4), seenAt: now, kept: true, fitRate: mb232(470.2), fitAt: now,
			path: mb232(477.4), pathAt: now, clearRun: qKeepSureTicks}
		var tr string
		tick := func(g, d float64) {
			now = now.Add(healthTick)
			q.step(qIn{now: now, delay: time.Duration(d * float64(time.Millisecond)), g: mb232(g)})
			tr += fmt.Sprintf(" %.0f/%.0f->%.1f", g, d, q.rate*8/1e6)
		}
		for i := 0; i < 5; i++ {
			tick(14, 5)
		}
		tick(442.5, 4)
		tick(470.0, 3)
		probed := q.rate
		tick(458.6, c.qd)
		if probed <= mb232(470.3) {
			t.Fatalf("no burst probe: %s", tr)
		}
		if q.hold != 0 && c.qd < 40 {
			t.Fatalf("onset held: %s", tr)
		}
		if d := q.rate*8/1e6 - c.want; d < -0.2 || d > 0.2 {
			t.Fatalf("queue %v ms: cap %.1f, want %.1f: %s", c.qd, q.rate*8/1e6, c.want, tr)
		}
	}
}
