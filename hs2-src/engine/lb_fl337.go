package engine

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// b337: the fast lower's log-only mode, the dialing side's fast lower, and a
// per-reading debug line of the paths that are not cut.
//
// HS2_B337_FL_LOGONLY=1 (either side, safe to ship): the fast-lower detector
// (lb_fastlower335.go, with the stall evidence of lb_flstall336.go) runs on
// every reading and logs one line per IP it would lower ("WOULD be lowered
// (log only)", with the evidence: connections stalled, links in RTO backoff
// or resending, links, the next IP's evidence, and the corroborating path's
// resent share against the best other IP's) and per would-be lift, but
// lowers nothing: lowered() says no, no weight changes. It turns the
// detector on by itself (HS2_B335_FASTLOWER not needed) and wins over it.
//
// Dialing side (Kharej in production, where the "darkcall silent
// iran=false" lines are): with HS2_B335_FASTLOWER=1 (and HS2_B336_FL_STALL
// not "0") the LB's 5 s reading (measure) also notes, per link, an RTO
// backoff or ≥ flStallRtLinkK segments resent in the window, and the
// window's per-path raw counts. adapt2 then takes the darkcall stall notes
// of its own paths plus those links and runs the same stall rule
// (flStallHot: several connections or links on ≥ 2 paths, concentrated on
// one IP of a side, corroborated by its own resent counts against a clean
// other IP). Only a stall-hot window lowers (stallOnly: the judge already
// acts on the resent counts). A lowered IP's paths that are not cut take
// the floor as their target ("lower at once: fast lower …") while some
// other path is not lowered: adapt2 moves the factor down at once and the
// edge takes it through the existing report (Factor / EffPct). Lifted
// after flCalm calm readings and flMinHold, as on the edge.
//
// HS2_B337_UNCUT_DEBUG=1: one line per judge reading (at most every
// lbUncutEvery) listing every path that is not cut: its sample, raw resent
// share, smoothed loss, target, and why nothing was done. Off by default.
var (
	flLogOnly    = os.Getenv("HS2_B337_FL_LOGONLY") == "1"
	lbUncutDebug = os.Getenv("HS2_B337_UNCUT_DEBUG") == "1"
)

var (
	flStallRtLinkK = uint64(4)       // dialing side: segments a link resent in one 5 s window to count
	flStallAgeK    = 6 * time.Second // dialing side: notes older than this at a take are dropped
	lbUncutEvery   = 4 * time.Second // at most one uncut debug line per judge this often
)

// flStallLinkShare: a resending link counts as stall evidence only when its
// own resent share in the window is over this (lab b337 at 3x users: busy
// healthy links resend >= 2 segments per tick on nearly every link of
// every IP, so counting them saturates — every IP's 6 links "resend" and the
// bad IP's 6 are never 2x the next IP's; quiet links read about 0.3 %).
var flStallLinkShare = 0.015

// flLinkLossy: rt of segs is over flStallLinkShare (no segments: yes).
func flLinkLossy(rt, segs uint64) bool {
	return segs == 0 || float64(rt) > flStallLinkShare*float64(segs)
}

// flOn: the fast-lower detector runs (it acts only with HS2_B335_FASTLOWER
// and without HS2_B337_FL_LOGONLY).
func flOn() bool { return b335FastLower || flLogOnly }

// Counters (lab and status).
var (
	flWould    atomic.Int64 // would-be lowers (log only)
	flKhLowers atomic.Int64 // dialing side: windows that floored a path's target
)

// FastLowerB337Counters is would-be lowers (log only) and dialing-side floored targets.
func FastLowerB337Counters() (int64, int64) { return flWould.Load(), flKhLowers.Load() }

func flWouldNote(logOnly bool) {
	if logOnly {
		flWould.Add(1)
	}
}

// flStallTakeFor returns the notes on paths in keep (younger than age) and
// leaves the others (another tunnel's LB, the edge) in the registry.
func flStallTakeFor(now time.Time, age time.Duration, keep map[string]bool) []flStallEv {
	flStallReg.mu.Lock()
	var out []flStallEv
	rest := flStallReg.ev[:0]
	for _, e := range flStallReg.ev {
		switch {
		case now.Sub(e.at) > age:
		case keep[e.path]:
			out = append(out, e)
		default:
			rest = append(rest, e)
		}
	}
	for i := len(rest); i < len(flStallReg.ev); i++ {
		flStallReg.ev[i] = flStallEv{} // no stale link references
	}
	flStallReg.ev = rest
	flStallReg.mu.Unlock()
	flStallNotes.Add(int64(len(out)))
	return out
}

// fastLower2Locked is the dialing side's fast lower (after judge.update, before
// adapt2 reads the targets). Caller holds b.mu.
func (b *LB) fastLower2Locked(now time.Time, keep map[string]bool) {
	cnt, rto := b.flCnt, b.flRTO
	b.flCnt, b.flRTO = nil, nil
	if !flOn() || !flStallOn || b.judge == nil {
		return
	}
	if b.fl == nil {
		b.fl = newFastLowerDet(b.log)
		b.fl.stallOnly = true
	}
	alive := map[string]bool{}
	for k := range keep {
		alive[k] = true
	}
	ev := append(flStallTakeFor(now, flStallAgeK, keep), rto...)
	b.fl.stepStall(now, cnt, alive, ev)
	if b.fl.logOnly || !b335FastLower || b.judge.allBad {
		return
	}
	someOK := false
	for k := range keep {
		if !b.fl.lowered(k) {
			someOK = true
			break
		}
	}
	if !someOK {
		return
	}
	n := 0
	for k := range keep {
		q := b.judge.p[k]
		if q == nil || q.cut || !b.fl.lowered(k) || q.target <= b.judge.floor {
			continue
		}
		q.target = b.judge.floor
		q.reason = "lower at once: fast lower (stalls on its IP)"
		n++
	}
	if n > 0 {
		flKhLowers.Add(1)
	}
}

// uncutDbg (HS2_B337_UNCUT_DEBUG=1): one line of this reading's paths that
// are not cut and why nothing was done. hold: the common guard held it.
func (j *q2Judge) uncutDbg(now time.Time, rs map[string]q2Reading, hold bool) {
	if !lbUncutDebug || j.log == nil || now.Sub(j.uncutAt) < lbUncutEvery {
		return
	}
	j.uncutAt = now
	best := math.Inf(1)
	for _, r := range rs {
		best = math.Min(best, r.loss)
	}
	ks := make([]string, 0, len(j.p))
	for k, q := range j.p {
		if !q.cut {
			ks = append(ks, k)
		}
	}
	if len(ks) == 0 {
		return
	}
	sort.Strings(ks)
	var sb strings.Builder
	for _, k := range ks {
		q := j.p[k]
		r, read := rs[k]
		why := ""
		switch {
		case hold:
			why = "common-guard"
		case !read:
			why = "no-reading"
		case r.loss <= lb2CutLoss:
			why = "under-bar"
		case len(rs) >= 2 && r.loss <= 2*best+0.01:
			why = "not-over-best"
		case q.bad < lb2CutRuns:
			why = fmt.Sprintf("bad-%d/%d", q.bad, lb2CutRuns)
		default:
			why = "other"
		}
		if q.target < 1 {
			why += ",target:" + q.reason
		}
		if read {
			fmt.Fprintf(&sb, " | %s segs %d raw %.2f%% ewma %.2f%% t %.2f %s", k, r.segs, 100*r.loss, 100*q.ewma, q.target, why)
		} else {
			fmt.Fprintf(&sb, " | %s ewma %.2f%% t %.2f %s", k, 100*q.ewma, q.target, why)
		}
	}
	if math.IsInf(best, 1) {
		best = 0
	}
	j.log("lb uncutdbg: %d read, best %.2f%%%s", len(rs), 100*best, sb.String())
}
