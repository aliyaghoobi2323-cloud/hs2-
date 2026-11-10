package engine

import (
	"os"
	"time"
)

// lb-rec20: HS2_LB_REC_CONFIRM=1 (off by default) — a cut path keeps only a
// couple of probe links, so its readings are tiny samples where a single
// retransmit reads as several percent of loss. Without the key one such
// reading over the recovery bar resets the path's okSince, and with a long
// (flap-doubled) hold of up to lb2HoldMax a healthy path can stay cut for many
// holds. With the key, while a path is cut, its okSince is reset only by
// lbRecConfirmRuns raw readings in a row at or over the bar (a lone spike
// from a small sample is ignored; lbRecBigSegs), it starts on a raw reading under the bar, and the path
// recovers once the hold has passed and its latest raw reading is under the
// bar (its smoothed loss is then dropped to that reading, so a lone spike
// does not hold its weight down). The cut itself (lb2CutRuns readings over
// lb2CutLoss, the IP verdicts) is unchanged, so a loaded bad path is cut as
// before, and a still-bad cut path (readings over the bar in a row) keeps
// resetting its okSince as before.
var lbRecConfirm = os.Getenv("HS2_LB_REC_CONFIRM") == "1"

// HS2_LB_REC_DEBUG=1 (off by default): log every reading of a cut path
// (raw loss, sample segments, smoothed loss, bar, okSince) — lab diagnosis.
var lbRecDebug = os.Getenv("HS2_LB_REC_DEBUG") == "1"

var (
	lbRecConfirmRuns = 2
	// lbRecBigSegs: a reading over the bar from at least this many segments
	// (~14 Mbit/s for 5 s) is no small-sample spike — it resets okSince at
	// once, as without the key (a cut path still carrying heavy flows that
	// reads bad once between good readings stays cut). 0 segments (unknown:
	// the edge's reading from the dialing side's report) counts as small; the
	// dialing side's own cut is taken over by the edge anyway (forceCut).
	lbRecBigSegs = uint64(5000)
	// lbRecSlack: the readings come every lb2QualEvery give or take a few
	// ms; a hold of 2 readings must not wait for a 3rd one.
	lbRecSlack = 500 * time.Millisecond
)

// recConfirmStep is the HS2_LB_REC_CONFIRM okSince rule for a cut path q
// that has a reading this tick (raw: q.raw). It returns whether the path
// may recover now (hold passed and the latest reading under the bar).
func recConfirmStep(q *q2Path, okBar float64, now time.Time, hold time.Duration) bool {
	if q.raw >= okBar {
		q.overRuns++
		if q.overRuns >= lbRecConfirmRuns || q.segs >= lbRecBigSegs {
			q.okSince = time.Time{}
		}
		return false
	}
	q.overRuns = 0
	if q.okSince.IsZero() {
		q.okSince = now
	}
	return now.Sub(q.okSince) >= hold-lbRecSlack
}
