package engine

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// up148: HS2_LB_UPLOSS=1 (edge; loadEnvKeys; off by default) — a path that
// loses packets only in the upload direction (Iran -> kharej) is lowered or
// cut for new connections like a path with download loss.
//
// In reverse the exit (kharej) dials and runs the judge (adapt2) on its own
// TCP_INFO: the loss of what it sends, the download. The edge steers new
// connections by the exit's report alone (steerTickLocked: the report
// replaces the edge's own readings), so an upload-only loss never reached a
// verdict: the path kept its full share and every user drawn onto it got a
// poor upload.
//
// The edge is the sender of the upload: its tunnel sockets' TCP_INFO
// (retransmits / segments out, already counted per path in note as upRt /
// upSegs) is the sender's truth for the upload, the same measure the exit
// uses for the download. With the key on the edge keeps a second judge, fed
// with those upload counts only, summed over lb2QualEvery (5 s, the exit's
// reading cadence) so its readings are as large as the exit's and the same
// rules apply unchanged: bad = over lb2CutLoss AND over 2x the best path's
// reading + 1 point (loss every path shares — Iran's own uplink — is no
// path's fault), cut after lb2CutRuns bad readings, the hold, okBar and the
// flap doubling for recovery, step-up lb2RiseStep a reading, "all paths
// lossy" keeps the shares. Its applied weight then multiplies the report's
// share and factor for the path, its cut joins the report's cut, and its
// smoothed loss raises the report's loss (mediocre), before anything reads
// them (the draw, light picks, cut-path keepers, rescue, HS2_LB_CUT_NONEW).
// No wire change: an exit of any version.
//
// Client-side loss is never counted: the readings are the tunnel sockets'
// own retransmits between the two servers. A user's loss on its way to the
// edge is the user's TCP's business — the tunnel only carries what arrived.
var lbUpLoss bool

// lbUpState is the edge's upload judge (HS2_LB_UPLOSS).
type lbUpState struct {
	judge   *q2Judge
	rt      map[string]uint64 // upload retransmits / segments since the last reading
	segs    map[string]uint64
	readAt  time.Time
	applied map[string]float64 // last logged weight per path (for the log)
}

func newUpState(logf func(string, ...any)) *lbUpState {
	return &lbUpState{
		judge: newQ2Judge(lb2SteerFloor, "Iran", "Kharej", func(f string, a ...any) {
			logf(strings.Replace(f, "lb auto2:", "lb auto2 upload:", 1), a...)
		}),
		rt: map[string]uint64{}, segs: map[string]uint64{}, applied: map[string]float64{},
	}
}

// add sums one tick's upload counts.
func (u *lbUpState) add(rt, segs map[string]uint64) {
	for p, n := range segs {
		u.segs[p] += n
		u.rt[p] += rt[p]
	}
}

// tick judges the summed counts once lb2QualEvery has passed since the last
// reading (paths with fewer than lb2MinSegs segments: no reading). alive is
// the set of live paths.
func (u *lbUpState) tick(now time.Time, alive map[string]bool) {
	u.judge.forget(alive)
	for p := range alive {
		u.judge.path(p)
	}
	if u.readAt.IsZero() {
		u.readAt = now
	}
	dt := now.Sub(u.readAt)
	if dt < lb2QualEvery {
		return
	}
	rs := map[string]q2Reading{}
	for p, n := range u.segs {
		if alive[p] && n >= lb2MinSegs {
			rs[p] = q2Reading{loss: float64(u.rt[p]) / float64(n), segs: n}
		}
	}
	u.rt, u.segs, u.readAt = map[string]uint64{}, map[string]uint64{}, now
	u.judge.update(now, dt, rs)
	u.judge.stepEdge()
	for k, q := range u.judge.p {
		if old, ok := u.applied[k]; !ok || math.Abs(old-q.factor) >= 0.05 {
			if ok || q.factor < 1 {
				u.judge.log("lb auto2: path %s weight %.2f (%s)", k, q.factor, q.reason)
			}
			u.applied[k] = q.factor
		}
	}
	for k := range u.applied {
		if _, ok := u.judge.p[k]; !ok {
			delete(u.applied, k)
		}
	}
}

// fold applies the upload verdicts to the report's per-path maps (the
// steering's one source of truth). Only paths the report covers.
func (u *lbUpState) fold(share, factor, loss map[string]float64, cut map[string]bool) {
	for k, q := range u.judge.p {
		if _, ok := share[k]; !ok || !q.have {
			continue
		}
		share[k] *= q.factor
		factor[k] *= q.factor
		loss[k] = math.Max(loss[k], q.ewma)
		if q.cut {
			cut[k] = true
		}
	}
}

// String is the upload judge's state per path (logs, tests).
func (u *lbUpState) String() string {
	s := ""
	for k, q := range u.judge.p {
		s += fmt.Sprintf("%s:%.2f/%v/%.1f%% ", k, q.factor, q.cut, 100*q.ewma)
	}
	return s
}
