package engine

import (
	"os"
	"strconv"
)

// lbv81: HS2_LB_DEAD_HYST=P (default 0 = off, production unchanged) — a
// hysteresis band around lb2Dead. Without it a steadily lossy path whose
// goodness ratio sits at lb2Dead (0.8-0.9 % loss beside 0.1 %) flips between
// "lower" (r < 0.75, target r²≈0.55) and "ok" (target 1) every few readings
// (lbv81 soak: one 0.8 % path reversed 12+ times in 5 min with no change on
// the path). With P > 0 a path already lowered (target < 1, not cut) comes
// back to "ok" only at r >= lb2Dead + P/100; a path at "ok" is lowered at
// r < lb2Dead as before.
var lbDeadHyst = func() float64 {
	v, err := strconv.ParseFloat(os.Getenv("HS2_LB_DEAD_HYST"), 64)
	if err != nil || v <= 0 || v > 20 {
		return 0
	}
	return v / 100
}()

// deadBar is the goodness ratio path q needs this reading to count as good
// as the best: lb2Dead, raised by the band while q is lowered.
func deadBar(q *q2Path) float64 {
	if lbDeadHyst > 0 && q.target < 1 && !q.cut {
		return lb2Dead + lbDeadHyst
	}
	return lb2Dead
}
