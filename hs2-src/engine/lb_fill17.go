package engine

import (
	"os"
	"strconv"
)

// lb-fill17: HS2_LB_FILL_PCT=N (off by default; exit side, reverse).
//
// The exit's spare dials (addSpares, lb_spare.go) were capped at
// lb2SpareMax slots over the edge's target. But the edge's target counts
// SERVING links only: a link the edge retires (a lossy path's link after
// auto lowered it, its replacement already serving on a healthy path) keeps
// its exit slot until its connections end — up to retireForce (20 min) and
// longer while they flow. Those slots are already over the target, so after
// every shift no spare could start for the paths still under their share,
// and the healthy paths refilled only as the retiring links closed.
//
// With the key on, the cap on slots is the target plus N% of it (at least
// lb2SpareMax over it): the retiring links' slots no longer block the spares,
// while live + retiring links stay bounded. Only lb2SpareMax spares are ever
// pending (undialed) at once, and the balancer asks for them at its own rate
// (lb2DrainPerMin), so the extra links come one shift at a time.
var lbFillPct = func() int {
	n, _ := strconv.Atoi(os.Getenv("HS2_LB_FILL_PCT"))
	return max(0, min(n, 100))
}()

// fillLimit is the most slots the pool holds for spare dials with the key on.
func fillLimit(want int) int {
	return max(want+lb2SpareMax, want+want*lbFillPct/100)
}

// HS2_LB_TEST_NO_IDLE=1 (lab only): the rebalance retires no idle link, as on
// a busy production tunnel where none is idle, so every shift goes through
// spareForGap — the path HS2_LB_FILL_PCT is about.
var lbTestNoIdle = os.Getenv("HS2_LB_TEST_NO_IDLE") == "1"

// HS2_LB_SPARE_PER_ROUND=K (off by default; needs HS2_LB_FILL_PCT): a
// rebalance round that finds no idle link to retire asks for up to K spares
// (not one) for the path furthest under its share — no more than it is under
// by, and still counted in lb2DrainPerMin. The pool keeps up to K pending and
// the FILL_PCT slot cap bounds the total, so a shift refills in a few rounds
// instead of one link per round while the lossy paths' retiring links hold.
var lbSparePerRound = func() int {
	n, _ := strconv.Atoi(os.Getenv("HS2_LB_SPARE_PER_ROUND"))
	if lbFillPct == 0 {
		return 1
	}
	return max(1, min(n, 6))
}()
