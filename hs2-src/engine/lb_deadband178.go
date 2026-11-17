package engine

import (
	"math"
	"os"
	"time"
)

// wj178: HS2_LB_LOWER_EVID (default on; "0" = the old rule) — a healthy path
// is not lowered on noise.
//
// Production (tunnel 1, 6 h audit): ~880 weight changes, most of them a path
// at ~0.2 % smoothed loss dropping 1.00→0.55 and back 5-20 s later; the lab
// shows the same on paths no event touches ("1.00→0.54 (lower: loss 0.2%,
// queue 31ms)": the queue the heavy users themselves put on it). Every such
// drop moves users and feeds the Iran LB rescue's copies. q2Goodness weighed
// every 0.1 % of loss and every millisecond of queue, so beside a path at
// 0.05 % / 5 ms a path at 0.2 % / 30 ms read r < lb2Dead.
//
// With the key on:
//   - dead-band: the goodness counts loss over lb2LossNoise (0.3 %) and queue
//     over lb2QueueNoise (20 ms) only — the levels a healthy path shows under
//     load; real loss still weighs fully past them (1 % beside 0.1 %: r 0.74,
//     lowered; 2 %: r 0.54, target 0.29);
//   - minimum evidence: a path at "ok" is lowered only on its second reading
//     in a row under the bar, unless the reading is clearly bad (raw loss at
//     or over lb2LowerNow, 2 %) or the fast verdict holds — real loss still
//     lowers at once.
var lbLowerEvid = os.Getenv("HS2_LB_LOWER_EVID") != "0"

var (
	lb2LossNoise  = 0.003
	lb2QueueNoise = 20.0
	lb2LowerNow   = 0.02
	lb2LowerRuns  = 2
)

// q2GoodnessEvid is q2Goodness with the dead-band.
func q2GoodnessEvid(q *q2Path) float64 {
	l := math.Max(0, q.ewma-lb2LossNoise)
	qm := math.Max(0, q.queueMs-lb2QueueNoise)
	return 1 / (1 + 100*l/2 + qm/50 + 4*q.fail)
}

// lowerHold: path q reads under the bar now (r < its dead bar); true keeps it
// at "ok" for want of evidence. Counts one per reading (lastRead).
func lowerHold(q *q2Path) bool {
	if !lbLowerEvid {
		return false
	}
	if !q.lastRead.Equal(q.lowAt) {
		q.lowAt = q.lastRead
		q.lowRuns++
	}
	return q.target >= 1 && q.reason == "ok" && !q.fast && q.raw < lb2LowerNow && q.lowRuns < lb2LowerRuns
}

// lowerReset: path q reads at or over the bar.
func lowerReset(q *q2Path) { q.lowRuns, q.lowAt = 0, time.Time{} }
