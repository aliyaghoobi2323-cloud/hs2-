package engine

import "time"

// sp176: HS2_AP_SHRINK_HYST (default on, both sides).
//
// The pattern grew at once when the fewest active connections of the last
// 10 s needed one more link (FLOOR: ceil(fl5 / per_link) > T) and shrank once
// the most of the last minute fitted one less (ceil(fl60 / per_link) < T for
// a minute, the last grow a minute old): the same line both ways, and a
// minute is shorter than how long active connections wander. On the main
// tunnel at night 146–171 active connections sat on the 21 × 8 line:
// "pattern 22 → 21: demand needs ~21 — 146 active", 18 s later
// "21 → 22: 171 active"; in 90 minutes 82 retires and 73 back in service,
// each one also moving the exit's pool target and its spare dials.
//
// With the key on a shrink to T' also needs the most active connections of
// the last apHystWin (5 min) at most (T'-1) × per_link — one link's worth
// under the line the floor grows at — and the last grow apHystAfterGrow
// (3 min) old. Growing is unchanged (immediate), so a burst of users gets its
// links at once; while demand wanders across a line the pattern holds the
// larger size (a link or two more, emptier links), and it still comes down
// once demand has stayed lower for 5 minutes.
var apShrinkHyst bool // loadEnvKeys

const (
	apHystWin       = 5 * time.Minute
	apHystAfterGrow = 3 * time.Minute
)

type apFlow struct {
	at time.Time
	n  int
}

// shrinkNeed records this tick's active count and returns the demand a shrink
// is judged against and how long after a grow a shrink waits: H and after
// unchanged with the key off.
func (a *autopilot) shrinkNeed(H int, now time.Time, flowing int, after time.Duration) (int, time.Duration) {
	if !apShrinkHyst || a.perLink <= 0 {
		a.hyst = nil
		return H, after
	}
	cut := now.Add(-apHystWin)
	i := 0
	for i < len(a.hyst) && !a.hyst[i].at.After(cut) {
		i++
	}
	a.hyst = append(a.hyst[i:], apFlow{now, flowing})
	m := 0
	for _, f := range a.hyst {
		m = max(m, f.n)
	}
	return max(H, a.clamp(ceilDiv(m+a.perLink, a.perLink))), max(after, apHystAfterGrow)
}
