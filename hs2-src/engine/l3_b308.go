package engine

import (
	"sync/atomic"
	"time"
)

// The dup's second copy avoids the path the first copy just left stuck or
// cut (HS2_B308_DUPAVOID, default on, with HS2_B303_DUPSTICK; both sides
// act on their own sends).
//
// ab306 (lab3, targeted 5 s data-dark on the first copy's path, IPv6 too):
// with b303 on, 14 / 4 / 13 second copies went onto the dark path right after
// the first moved off it (b278 trace R1B: first P6 -> P9 at +0.7 s, second
// P9 -> P6 for 0.66 s until darkcall cut P6; then 3.8 s with both copies on
// P9, because the cut-aware dupSecond picked P9 and b278's only other-path
// candidate was the stuck P6 link). The cause: when the first copy's link
// changes the full pick runs again, and the stuck verdict is per link: the
// lane left link F because F's socket made no ack progress, but the other
// links of F's path carry nothing yet, so they read "not stuck" and rank
// like any healthy link until the darkcall cut (~1.5 s) or their own stuck
// verdict (>= 400 ms of traffic) arrives.
//
// With the key on, a lane flow whose first copy's link changes away from a
// link that is stuck (its cached b278 verdict) or on a cut path marks that
// link's path as avoided for b308Hold. While the hold lasts, a second copy
// whose pick (kept or full) lands on the avoided path or on the new first's
// path is re-picked by rendezvous over not-stuck links on a third path
// (light and off cut first, then off cut, then any). With no third path the
// pick is left as it was: with two paths a copy on the other (dark) path
// still beats both on one path, and the second copy is never dropped.
// Key off: exactly b303.
var b308On atomic.Bool

func init() { b308On.Store(envBoolDefault("HS2_B308_DUPAVOID", true)) }

var b308Hold = int64(3 * time.Second)

// b308Held: paths put on hold; b308Moved: second copies re-picked off the
// held path or the first's path during a hold.
var b308Held, b308Moved atomic.Int64

// b308Avoid updates the flow's view of its first copy's link and returns
// the path its second copy avoids now ("" none).
func (k *l3DupKeep) b308Avoid(first *l3Link, now int64) string {
	if !b308On.Load() {
		return ""
	}
	if p := k.prevFirst; p != nil && p != first && p.path != "" && p.path != first.path &&
		(p.stall.stuck.Load() || l3OnCut(p)) {
		k.avoid, k.avoidUntil = p.path, now+b308Hold
		b308Held.Add(1)
	}
	k.prevFirst = first
	if k.avoid == "" || now >= k.avoidUntil || k.avoid == first.path {
		k.avoid = ""
		return ""
	}
	return k.avoid
}

// b308Repick moves second off the avoided path and off first's path when a
// third path has a usable link; otherwise second is returned unchanged.
func b308Repick(links []*l3Link, h uint32, first, second *l3Link, avoid string, now int64) *l3Link {
	if second != nil && second.path != avoid && (first.path == "" || second.path != first.path) {
		return second
	}
	third := func(l *l3Link) bool {
		return l != first && l.canDup() && l.path != "" && l.path != avoid && l.path != first.path
	}
	offCut := func(l *l3Link) bool { return third(l) && !l3OnCut(l) }
	for _, ok := range [...]func(*l3Link) bool{
		func(l *l3Link) bool { return offCut(l) && !l.isHeavy() }, offCut, third,
	} {
		if l := rendezvousNotStuck(links, h, now, ok); l != nil {
			b308Moved.Add(1)
			return l
		}
	}
	return second
}
