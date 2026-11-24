package engine

import (
	"fmt"
	"sync/atomic"
)

// b436 (logscan 2026-11-20 finding 1): the LB path report during a Kharej ZDU
// drain comes from the NEW process only.
//
// In reverse the exit (Kharej) dials and judges the paths; the edge (Iran)
// takes the exit's report (lb_report.go) from the answer to an advert
// exchange on ONE live link (LBAdvert.runEdge) and its cuts at once
// (lb_auto2.go: forceCut "the dialing side cut it"; the report's loss replaces
// the edge's own estimate). While the old Kharej drains, about half of Iran's
// links are the old process's (peerOld, zdu_counts.go), and runEdge took
// whichever link the map handed it first: every other report was the old
// process's. That process judges paths over its own shrinking link set, with
// no spares and no autopilot (zdu_drainmode.go), so its cuts recover late or
// never and its loss reads are over a few links. Iran's steer therefore
// flipped between the two exits' views for the whole drain (prod t1
// 12:11-12:41 UTC: path cuts seen twice, rescue lowering the paths a call
// sits on, "lb call stuck" 5-15 a minute, 516 call subs given up) and
// stopped exactly at the old process's exit.
//
// Two halves, each enough on its own, so the fix works whichever side was
// upgraded first:
//
//   - edge (zduReportLinks): runEdge tries the current instance's links
//     first and an older instance's only when no current one answered (the
//     new process died in probation and the marks have not cleared, or a
//     pool of only old links). With HS2_ZDU off, or one instance only, the
//     order is the map's, as before.
//   - exit (zduReportOut): a draining old exit answers the advert without the
//     report (the wire already allows it: readFrontTail / readReport, "no
//     report" = keep the last one), so an older edge keeps the new process's
//     report too.
//
// Neither touches placement: calls and other connections pinned to the old
// instance (zdu_pin.go) keep their subs there until they end; only the path
// verdicts the edge steers by are the new process's.

// zduReportLinks orders the edge's advert links: the current instance's
// first, then the rest (old instance or unknown) in the given order. cur is
// LinkManager.zduCurrent (nil or zero: unchanged).
func zduReportLinks(ls []Link, cur func() zduInst) []Link {
	if !zduOn.Load() || cur == nil || len(ls) < 2 {
		return ls
	}
	c := cur()
	if !c.known() {
		return ls
	}
	out := make([]Link, 0, len(ls))
	for _, l := range ls {
		if linkZDUInst(l) == c {
			out = append(out, l)
		}
	}
	if len(out) == 0 || len(out) == len(ls) {
		return ls
	}
	zduRepCur.Add(1)
	for _, l := range ls {
		if linkZDUInst(l) != c {
			out = append(out, l)
		}
	}
	return out
}

// zduReportOut is the exit's report bytes for an advert answer: none while
// this process drains (HS2_ZDU on, after the commit).
func zduReportOut(f func() []byte) []byte {
	if zduDraining.Load() {
		zduRepHeld.Add(1)
		return nil
	}
	return f()
}

// zduRepCur: edge exchanges that put the current instance's links first (an
// older instance was live); zduRepHeld: advert answers a draining exit sent
// without its report. " zdu_rep_cur= zdu_rep_held=" on the mem line once
// either moved.
var zduRepCur, zduRepHeld atomic.Int64

func zduReportLine() string {
	c, h := zduRepCur.Load(), zduRepHeld.Load()
	if c == 0 && h == 0 {
		return ""
	}
	return fmt.Sprintf(" zdu_rep_cur=%d zdu_rep_held=%d", c, h)
}
