package engine

import (
	"sync/atomic"
	"time"
)

// The dup's second copy stays on its link per flow (HS2_B303_DUPSTICK,
// default on; both sides act on their own sends).
//
// sk300 (lab 42): b278's "dup copies moved off the first's path" counter rose
// ~19/s for 25 min. The picks are not random: dupSecond and b278Second are
// rendezvous over the flow hash, so a flow whose dupSecond winner shares the
// first copy's path gets the SAME link from b278Second on every packet. The
// counter counted packets, and each packet paid two to three full scans of
// the set's links (up to ~130 in l3mtcp) plus the b278 path check.
//
// With the key on, an interactive flow keeps the second link the full pick
// (dupSecond + b278Second) gave it, in its lane slot, while all of these hold:
// the first copy's link is the same, the set's link list has not changed,
// the link is alive, its peer reads TypeDataDup, it is not stuck (b278
// TCP_INFO view, same 50 ms cache as before), it is not heavy now when it
// was light at the pick, it is not on a cut path now when it was not at the
// pick, it is not on the first's path, and the pick is younger than
// b303Refresh. A pick on the first's path (b278 found no free other-path
// link) or no pick is not kept. Otherwise the full pick runs again.
// So the second copy never stays on a link the full pick would leave for
// being dead, stuck, heavy, cut or on the first's path; the only difference
// is that a newly light or newly un-cut better-ranked link is adopted within
// b303Refresh instead of on the next packet (the lane keeps the first copy
// on its link the same way). b278OtherPath now counts flow moves (the
// second link changed and was moved off the first's path), not packets.
// Key off (or b278 off): the full pick per packet, exactly as before.
var b303On atomic.Bool

func init() { b303On.Store(envBoolDefault("HS2_B303_DUPSTICK", true)) }

var b303Refresh = int64(time.Second)

// b303Hit / b303Pick: second copies sent on a kept link / full picks made.
var b303Hit, b303Pick atomic.Int64

// l3DupKeep is a lane flow's kept second link (pumpTun only).
type l3DupKeep struct {
	link  *l3Link // nil: none kept
	first *l3Link // the first copy's link it was picked for
	gen   uint64  // l3Set.gen at the pick
	at    int64   // ctrlNow of the pick
	heavy bool    // link was heavy at the pick
	cut   bool    // link was on a cut path at the pick
	// HS2_B308_DUPAVOID (l3_b308.go): the first copy's link last seen, and
	// the path it left stuck or cut, avoided by the second copy until
	// avoidUntil.
	prevFirst  *l3Link
	avoid      string
	avoidUntil int64
	// HS2_B309_DUPSCAN (l3_b309.go): the path of a kept second link left
	// stuck or cut, avoided by the second copy until avoid2Until.
	avoid2      string
	avoid2Until int64
}

// dupSecondFor is the second link for an interactive flow in lane slot e
// whose first copy goes over first. Called with s.mu read-locked from
// pumpTun.
func (s *l3Set) dupSecondFor(e *l3Flow, h uint32, first *l3Link, now int64) *l3Link {
	if !b303On.Load() || !b278On.Load() {
		second := dupSecond(s.links, h, first)
		return b278Second(s.links, h, first, second, now)
	}
	k := &e.dup2
	avoid := k.b308Avoid(first, now)
	if l := k.link; l != nil && k.first == first && (avoid == "" || l.path != avoid) && k.gen == s.gen.Load() && now-k.at < b303Refresh &&
		l.Alive() && l.canDup() && (k.heavy || !l.isHeavy()) && (k.cut || !l3OnCut(l)) &&
		(first.path == "" || l.path != first.path) && !l.txStuck(now) {
		b303Hit.Add(1)
		return l
	}
	b303Pick.Add(1)
	avoid2 := k.b309Avoid(first, now)
	second, moved := b278SecondMoved(s.links, h, first, dupSecond(s.links, h, first), now)
	if moved && second != k.link {
		b278OtherPath.Add(1)
	}
	if avoid != "" {
		second = b308Repick(s.links, h, first, second, avoid, now)
	}
	second = b309Fix(s.links, h, first, second, avoid, avoid2, now)
	// nothing kept when there is no second, or it shares the first's path
	// (b278 found no free other-path link: the fallback re-picks per packet,
	// as before, so a freed other-path link is taken at once)
	if second == nil || first.path != "" && second.path == first.path {
		*k = l3DupKeep{prevFirst: k.prevFirst, avoid: k.avoid, avoidUntil: k.avoidUntil,
			avoid2: k.avoid2, avoid2Until: k.avoid2Until}
		return second
	}
	*k = l3DupKeep{link: second, first: first, gen: s.gen.Load(), at: now,
		heavy: second.isHeavy(), cut: l3OnCut(second),
		prevFirst: k.prevFirst, avoid: k.avoid, avoidUntil: k.avoidUntil,
		avoid2: k.avoid2, avoid2Until: k.avoid2Until}
	return second
}
