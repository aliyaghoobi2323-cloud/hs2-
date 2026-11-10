package engine

import (
	"strconv"
	"time"
)

// lb-ready16: HS2_LB_FAST_RECOVER=1 (off by default) — a cut path whose raw
// (unsmoothed) loss reads under the recovery bar lbFastOKRuns readings in a
// row (about 15 s at lb2QualEvery 5 s) drops its smoothed loss to that
// reading, instead of creeping down at lb2AlphaDown (3%/s: from 7% under
// 1.5% took ~50 s), and its FIRST recovery waits lbFastHold (10 s) instead of
// lb2Hold (30 s). The flap hysteresis is untouched: a path cut again within
// lb2FlapWin of its last recovery still doubles its stored hold (30 s, 60 s
// … up to lb2HoldMax) and waits the full doubled hold; the fast hold applies
// only while the stored hold is still the first-cut lb2Hold. The reset also
// applies for lbFastAfter after a recovery, so the path's weight is not held
// down by the old smoothed loss while its links come back.
var lbFastRecover bool // loadEnvKeys

var (
	lbFastOKRuns = 3
	lbFastHold   = 10 * time.Second
	lbFastAfter  = 2 * time.Minute
)

// fastRecoverFold runs after a reading r was folded into q (okBar: the
// recovery bar before this tick). It counts raw readings under the bar and,
// after lbFastOKRuns in a row on a cut (or just recovered) path, resets the
// smoothed loss to the reading.
func fastRecoverFold(q *q2Path, raw, okBar float64, now time.Time) bool {
	if !lbFastRecover {
		return false
	}
	if q.cut && q.okRuns == 0 {
		q.fastResets = 0 // a new cut episode: log its first reset again
	}
	if raw < okBar {
		q.okRuns++
	} else {
		q.okRuns = 0
		return false
	}
	recent := !q.uncutAt.IsZero() && now.Sub(q.uncutAt) < lbFastAfter
	if q.okRuns >= lbFastOKRuns && (q.cut || recent) && q.ewma > raw {
		q.ewma = raw
		q.fastResets++
		return true
	}
	return false
}

// recoverHold is the wait a cut path needs under the bar before it recovers.
func recoverHold(q *q2Path) time.Duration {
	if lbFastRecover && q.hold <= lb2Hold && lbFastHold < q.hold {
		return lbFastHold
	}
	return q.hold
}

// HS2_LB_START_GRACE_S=N (off by default; from lb-ready15, alone): for N
// seconds after the judge's first reading no whole-IP cut verdict ("lossy on
// every … IP") is given (a restart's own loss is not an IP's fault).
var lbIPCutGrace time.Duration // loadEnvKeys

func graceFromEnv(v string) time.Duration {
	n, _ := strconv.Atoi(v)
	if n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}
