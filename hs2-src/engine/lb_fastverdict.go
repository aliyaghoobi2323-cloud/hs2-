package engine

import (
	"math"
	"time"
)

// Fast first verdict (HS2_LB_FAST_VERDICT=1, dialing side).
//
// A reading lowers a path by its goodness ratio squared (q2Judge.targets),
// so a path whose loss jumps lands somewhere between 0.1 and 0.3 at its
// first bad reading depending on how much of the jump the smoothing has let
// through and on whether the busy links' delay above their own minimum
// shows it (a path bad from the first packet shows no "queue" at all: the
// lab's B +80 ms from t=0 read 1.00→0.27, just above the rescue bar). The
// cut needs lb2CutRuns readings in a row (5 s more).
//
// With the key on, a reading that is a CLEAR degradation sets the path's
// target to the floor at once (one reading; the cut rule is unchanged and
// still takes its two): its raw loss over lb2CutLoss AND over 2× the best
// other path + 1 point, AND its delay over the busy links' minimum above the
// best other path's by fastDelayMs, with a sample of fastMinSegs segments,
// while some path is not bad (all paths bad: nothing), and not in the
// judge's first fastGrace (the start burst). Only the loss-and-delay pair
// counts: either one alone (a noisy loss reading, a queue under a download)
// is left to the usual rule. The next reading that is not bad clears it.
//
// Off by default: in the lab (8.5) it fired in every bad run and never with
// equal paths, but changed nothing measurable — the plain rule already
// floors such a path at that reading when its delay shows.
var lbFastVerdict bool // loadEnvKeys: HS2_LB_FAST_VERDICT

var (
	fastDelayMs = 40.0
	fastMinSegs = uint64(1000)
	fastGrace   = 10 * time.Second
)

// fastVerdicts marks this reading's clear degradations (q2Path.fast).
func (j *q2Judge) fastVerdicts(now time.Time, rs map[string]q2Reading, isBad func(float64) bool) {
	if !lbFastVerdict {
		return
	}
	if j.start.IsZero() {
		j.start = now
	}
	for k, r := range rs {
		q := j.p[k]
		if q == nil {
			continue
		}
		if !isBad(r.loss) {
			q.fast = false
		}
	}
	if len(rs) < 2 || now.Sub(j.start) < fastGrace || !anyNotBad(rs, isBad) {
		return
	}
	for k, r := range rs {
		q := j.p[k]
		if q == nil || q.cut || q.fast || r.segs < fastMinSegs {
			continue
		}
		bestL, bestQ := math.Inf(1), math.Inf(1)
		for k2, r2 := range rs {
			if k2 == k || r2.segs < lb2MinSegs {
				continue
			}
			bestL, bestQ = math.Min(bestL, r2.loss), math.Min(bestQ, r2.queueMs)
		}
		if math.IsInf(bestL, 1) {
			continue
		}
		if r.loss > lb2CutLoss && r.loss > 2*bestL+0.01 && r.queueMs > bestQ+fastDelayMs {
			q.fast = true
			j.log("lb auto2: path %s clear degradation in one reading (loss %.1f%% vs %.1f%%, delay +%.0fms vs +%.0fms, %d segs) — lowered at once", k, 100*r.loss, 100*bestL, r.queueMs, bestQ, r.segs)
		}
	}
}
