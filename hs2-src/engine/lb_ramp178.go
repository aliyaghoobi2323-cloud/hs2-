package engine

import (
	"fmt"
	"os"
	"time"
)

// wj178: HS2_LB_REC_RAMP (default on; "0" = the old rule) — a cut path that
// recovers climbs back to its full share in steps all the way.
//
// Before: right after the cut was lifted the path's target was r² (r: its
// goodness beside the best path), rising as its smoothed loss decayed
// (0.10→0.35→0.46→0.55), and the moment r reached lb2Dead (0.75, r² ≈ 0.56)
// the verdict turned "ok" and the target jumped to 1 — on the dialing side
// lbLimitStep caps a reading's move in SHARE points (lbMaxStepPct), which with
// many paths lets one path go 0.55→1.00 at once (production, a path cut 15
// times in 24 h, 22:41:40, 23:05:00, 23:50:05).
//
// Now a recovered path's target is also capped by its ramp: the floor at the
// recovery, then +lb2RampStep a reading (two steps when the reading is clean:
// raw loss under the recovery bar), divided by the path's cuts within
// lbFlapWin (HS2_LB_FLAP_WIN_S, 60 min: a path that keeps getting cut — the
// production one was cut 15 times in 24 h, 3 within the hour — comes back
// slower, at least lb2RampMinStep and never on the fast step). A reading over lb2CutLoss holds
// the ramp where it is. The ramp ends at 1; a new cut starts it afresh.
var lbRecRamp = os.Getenv("HS2_LB_REC_RAMP") != "0"

var (
	lb2RampStep    = 0.15
	lb2RampMinStep = 0.07 // over the 0.05 hysteresis of adapt2's step up
)

// rampNoteCut records a cut for the ramp's flap count.
func rampNoteCut(q *q2Path, now time.Time) {
	keep := q.rampCuts[:0]
	for _, t := range q.rampCuts {
		if now.Sub(t) < lbFlapWin {
			keep = append(keep, t)
		}
	}
	q.rampCuts = append(keep, now)
	q.ramp = 0
}

// rampStart: the cut was just lifted.
func rampStart(q *q2Path, floor float64) {
	if lbRecRamp {
		q.ramp = floor
	}
}

// rampStepOf: this reading's step for q.
func rampStepOf(q *q2Path, now time.Time, clean bool) float64 {
	n := 0
	for _, t := range q.rampCuts {
		if now.Sub(t) < lbFlapWin {
			n++
		}
	}
	if n <= 1 {
		if clean {
			return 2 * lb2RampStep
		}
		return lb2RampStep
	}
	return max(lb2RampMinStep, lb2RampStep/float64(n))
}

// rampAdvance moves q's ramp one reading on (uncut paths still climbing).
func rampAdvance(q *q2Path, now time.Time, okBar float64) {
	if !lbRecRamp || q.cut || q.ramp <= 0 {
		return
	}
	read := q.lastRead.Equal(now)
	if read && q.raw > lb2CutLoss {
		return // a bad reading: hold the ramp
	}
	q.ramp += rampStepOf(q, now, read && q.raw < okBar)
	if q.ramp >= 1 {
		q.ramp = 0 // done
	}
}

// rampCap caps an uncut path's target by its ramp.
func rampCap(q *q2Path) {
	if !lbRecRamp || q.cut || q.ramp <= 0 || q.target <= q.ramp {
		return
	}
	q.target = q.ramp
	q.reason = fmt.Sprintf("recovering: step %.2f", q.ramp)
}

// ramping: path k is still climbing its ramp (not settled for the mediocre
// cap: the cap would lower the others at once while k's share is held).
func (j *q2Judge) ramping(k string) bool {
	q := j.p[k]
	return lbRecRamp && q != nil && !q.cut && q.ramp > 0
}
