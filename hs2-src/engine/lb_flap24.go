package engine

import (
	"os"
	"strconv"
	"time"
)

// flap24: HS2_LB_FLAP_PEN=1 (default on for the exit side, envdef.Table) — a FLAPPING path (one
// local>remote pair: a load-dependent lossy route that reads clean while cut
// and breaks again when its users return) is kept out for a long time.
//
//   - Only RELATIVE cuts count (the path rule: lb2CutRuns readings over
//     lb2CutLoss AND clearly over the best path); whole-IP verdicts and
//     forced cuts (the dialing side's report) do not. lbFlapN of them within
//     lbFlapWin make the path a flapper: it stays cut for lbFlapHold
//     (doubling with each penalty, up to lbFlapMax), after which the normal
//     recovery (lb2Hold under the bar) applies.
//   - It keeps its probe links (a cut path's floor share, as any cut path),
//     so its loss is still read: clean (okSince, REC_CONFIRM rules) for a
//     third of its penalty (lbFlapEarlyDiv) it comes back early, on
//     probation: one more relative cut within lbFlapProb → the next penalty
//     at once (doubled).
//   - Safeguards: at most a third of the paths (round down; 1 with 3–5
//     paths, 0 with fewer than 3) in penalty at once, and never when that
//     would leave fewer than 2 healthy (uncut) paths or under lbFlapHealthy
//     of the paths healthy. If the other paths degrade (half of them cut or
//     lossy, the best of them over lb2CutLoss, fewer than 2 healthy, all
//     paths lossy) every penalty lifts at once and the normal hold applies.
//
// lbFlapPen is read in loadEnvKeys (defaults22.go), through the table.
var lbFlapPen bool

func envSecs(k string, def time.Duration) time.Duration {
	if n, err := strconv.Atoi(os.Getenv(k)); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return def
}

var (
	lbFlapN        = envIntMin("HS2_LB_FLAP_N", 4, 2)
	lbFlapWin      = envSecs("HS2_LB_FLAP_WIN_S", 60*time.Minute)
	lbFlapHold     = envSecs("HS2_LB_FLAP_HOLD_S", 30*time.Minute)
	lbFlapMax      = envSecs("HS2_LB_FLAP_MAX_S", 2*time.Hour)
	lbFlapProb     = envSecs("HS2_LB_FLAP_PROB_S", 30*time.Minute)
	lbFlapEarlyDiv = envIntMin("HS2_LB_FLAP_EARLY_DIV", 3, 1)
	lbFlapHealthy  = 0.6
)

func envIntMin(k string, def, lo int) int {
	if n, err := strconv.Atoi(os.Getenv(k)); err == nil && n >= lo {
		return n
	}
	return def
}

// q2Flap is a path's flap-penalty state.
type q2Flap struct {
	cuts    []time.Time // relative cuts within lbFlapWin
	pen     bool        // in penalty now
	penHold time.Duration
	level   int       // penalties served (the next one doubles)
	probEnd time.Time // probation after an early/normal return from a penalty
	lastEnd time.Time // when the last penalty ended
}

// flapPenalized: paths in penalty now.
func (j *q2Judge) flapPenalized() int {
	n := 0
	for _, q := range j.p {
		if q.flap.pen {
			n++
		}
	}
	return n
}

// flapCap is how many paths may be in penalty at once.
func (j *q2Judge) flapCap() (cap, n int) {
	for _, q := range j.p {
		if q.have {
			n++
		}
	}
	if n < 3 {
		return 0, n
	}
	return max(1, n/3), n
}

// flapDegraded: the paths outside penalty (and other than skip) are
// degrading — penalties must not hold (or start).
func (j *q2Judge) flapDegraded(skip string) (bool, string) {
	others, bad, healthy := 0, 0, 0
	best := 1.0
	for k, q := range j.p {
		if !q.have || q.flap.pen || k == skip {
			continue
		}
		others++
		if q.cut || q.ewma > lb2CutLoss {
			bad++
		}
		if !q.cut {
			healthy++
		}
		if !q.cut {
			best = min(best, q.ewma)
		}
	}
	switch {
	case j.allBad:
		return true, "all paths lossy"
	case others == 0 || healthy < 2:
		return true, "fewer than 2 healthy paths left"
	case 2*bad >= others:
		return true, "other paths degrading (" + strconv.Itoa(bad) + " of " + strconv.Itoa(others) + " cut or lossy)"
	case best > lb2CutLoss:
		return true, "best other path over " + ftoa1(100*lb2CutLoss) + "%"
	}
	return false, ""
}

// flapNoteCut runs when path k was just cut by the relative path rule (not
// an IP verdict). It may put the path in penalty (q.hold: the penalty).
func (j *q2Judge) flapNoteCut(k string, q *q2Path, now time.Time) {
	if !lbFlapPen {
		return
	}
	f := &q.flap
	keep := f.cuts[:0]
	for _, t := range f.cuts {
		if now.Sub(t) < lbFlapWin {
			keep = append(keep, t)
		}
	}
	f.cuts = append(keep, now)
	onProb := !f.probEnd.IsZero() && now.Before(f.probEnd)
	if len(f.cuts) < lbFlapN && !onProb {
		return
	}
	if !f.lastEnd.IsZero() && now.Sub(f.lastEnd) > lbFlapMax && !onProb {
		f.level = 0 // a long clean spell: start over
	}
	why := strconv.Itoa(len(f.cuts)) + " cuts in " + fmtDur(lbFlapWin)
	if onProb {
		why = "cut again on probation"
	}
	cp, n := j.flapCap()
	if j.flapPenalized()+1 > cp {
		j.log("lb auto2: path %s flapping: %s — no penalty (cap %d of %d paths already used)", k, why, cp, n)
		return
	}
	healthy := 0
	for k2, q2 := range j.p {
		if k2 != k && q2.have && !q2.cut && !q2.flap.pen {
			healthy++
		}
	}
	if healthy < 2 || float64(healthy) < lbFlapHealthy*float64(n)-1e-9 {
		j.log("lb auto2: path %s flapping: %s — no penalty (only %d of %d paths healthy)", k, why, healthy, n)
		return
	}
	if bad, s := j.flapDegraded(k); bad {
		j.log("lb auto2: path %s flapping: %s — no penalty (%s)", k, why, s)
		return
	}
	h := lbFlapHold
	for i := 0; i < f.level && h < lbFlapMax; i++ {
		h *= 2
	}
	h = min(h, lbFlapMax)
	f.level++
	f.pen, f.penHold, f.probEnd = true, h, time.Time{}
	j.log("lb auto2: path %s flapping: %s — kept out %s (penalty %d of max %d)", k, why, fmtDur(h), j.flapPenalized(), cp)
}

// flapHold is a penalized cut path's recovery hold (okSince): a third of the
// penalty (early, on probation) until the penalty is served, then lb2Hold.
func flapHold(q *q2Path, now time.Time) time.Duration {
	if now.Sub(q.cutAt) >= q.flap.penHold {
		return lb2Hold
	}
	return q.flap.penHold / time.Duration(lbFlapEarlyDiv)
}

// flapRecovered runs when a cut path recovers: a penalty ends on probation.
func (j *q2Judge) flapRecovered(k string, q *q2Path, now time.Time) {
	f := &q.flap
	if !f.pen {
		return
	}
	f.pen, f.lastEnd, f.probEnd = false, now, now.Add(lbFlapProb)
	q.hold = lb2Hold
	j.log("lb auto2: path %s back from flap penalty after %s, on probation %s", k, fmtDur(now.Sub(q.cutAt)), fmtDur(lbFlapProb))
}

// flapRelease lifts every penalty when the other paths degrade.
func (j *q2Judge) flapRelease(now time.Time) {
	if !lbFlapPen || j.flapPenalized() == 0 {
		return
	}
	bad, s := j.flapDegraded("")
	if !bad {
		return
	}
	for _, q := range j.p {
		if q.flap.pen {
			q.flap.pen, q.flap.lastEnd = false, now
			q.hold = lb2Hold
		}
	}
	j.log("lb auto2: flap penalties lifted: %s", s)
}
