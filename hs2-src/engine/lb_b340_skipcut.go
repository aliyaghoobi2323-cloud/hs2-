package engine

import (
	"os"
	"sync/atomic"
)

// b340 (lab/rb339 R4 31006): the dual upgrade / call copy / rescue
// placement put a call's replacement copy back onto links of a CUT path 7
// times in 47 s while unmarked links were free; rescue-cut then gave each
// up again (the b338 backoff only spaced that loop out).
//
// HS2_B340_SKIPCUT (default on, =0 off): a light sub is first placed off
// every cut-marked link (rescueCutLinks, HS2_LB_RESCUE_CUT); only when no
// unmarked link takes it does the usual placement (cut links included) run.
// The pass costs nothing while no link is marked.
var b340SkipCut atomic.Bool

// b340 counters: light placements made while some link was cut-marked —
// b340OnCut landed on a marked link (SKIPCUT off, or the fallback: nothing
// unmarked), b340Skipped took an unmarked one in the skip pass.
var (
	b340OnCut, b340Skipped, b340Fallback atomic.Int64
	// rescueCutCount: links currently in rescueCutLinks (a cheap "any marked").
	rescueCutCount atomic.Int64
)

// HS2_B340_CUTCLEAR (default on, =0 off): a path the dialing side still
// reports cut but whose raw readings are clean again (lbFastOKRuns in a row
// under the recovery bar: the cut itself is over, only the recovery hold —
// up to minutes after flaps — is still running) no longer marks its links
// cut for the rescue (rescueCutLinks): rescue-cut stops giving up copies on
// a recovered path and SKIPCUT stops passing over it. Its links stay
// lowered (rescueLow) until the path really recovers.
var b340CutClear atomic.Bool

func init() {
	b340SkipCut.Store(os.Getenv("HS2_B340_SKIPCUT") != "0")
	b340CutClear.Store(os.Getenv("HS2_B340_CUTCLEAR") != "0")
}

// cutReadsClean (dialing side, report): q is cut but its last lbFastOKRuns
// readings or more were all under the recovery bar (HS2_LB_FAST_RECOVER's
// run count; any reading over it resets the run).
func cutReadsClean(q *q2Path) bool {
	return q != nil && q.cut && lbFastRecover && q.okRuns >= lbFastOKRuns
}

// skipCutMark in an avoid list: pickPlaneTiersFP passes over cut-marked links.
type skipCutMark struct{}

func hasSkipCutMark(avoid []any) bool {
	for _, a := range avoid {
		if _, ok := a.(skipCutMark); ok {
			return true
		}
	}
	return false
}

// skipCutOn: a light placement now takes the skip pass.
func skipCutOn(p mPlane) bool {
	return p == planeLight && lbRescueCut && b340SkipCut.Load() && rescueCutCount.Load() > 0
}

// withSkipCut returns a copy of avoid with the mark (never aliasing the
// caller's backing array).
func withSkipCut(avoid []any) []any {
	out := make([]any, len(avoid), len(avoid)+1)
	copy(out, avoid)
	return append(out, skipCutMark{})
}

// b340Note counts where a light placement went while links were marked.
func b340Note(p mPlane, l Link, skipped bool) {
	if p != planeLight || l == nil || !lbRescueCut || rescueCutCount.Load() == 0 {
		return
	}
	switch {
	case skipped:
		b340Skipped.Add(1)
	case rescueCutKey(l):
		b340OnCut.Add(1)
	}
}
