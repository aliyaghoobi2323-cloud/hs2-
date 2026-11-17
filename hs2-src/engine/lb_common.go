package engine

import (
	"os"
	"sync/atomic"
)

// HS2_LB_COMMON_GUARD=1 (hv83, off by default): a reading in which most
// paths' resent share jumps at once, far over each path's own smoothed loss,
// is one common cause — this server's own load — not paths to cut.
//
// hv83 lab (10 paths, configured loss 0.2–2 %, 40 simultaneous heavy users
// with crowd/calls/games): in both 40-user runs auto2 cut 5 of 10 paths in
// one reading on both servers (the 0.2 % path read 3.9 %, 0.8 % read
// 10.6 %) while the emulator dropped exactly the configured 0.2 % — the
// resends were the hosts' own (spurious / local), not the paths'. Cutting
// half the paths at once put every user on the rest.
//
// A reading is held (not folded into the smoothing, no cut, no verdict, the
// targets as they were) when at least half the paths with a reading (3 or
// more) jumped — over lb2CutLoss and over 3× their smoothed loss + 1 point —
// unless one IP explains the jump (every jumped path has it and some path
// without it did not jump: that is an IP verdict, cut as before). At most
// lbCommonMaxRuns readings in a row are held: a jump that stays is real and
// is judged as before.
var lbCommonGuard = os.Getenv("HS2_LB_COMMON_GUARD") == "1"

const lbCommonMaxRuns = 6

// lbCommonHeld counts the readings held (status / tests).
var lbCommonHeld atomic.Int64

func (j *q2Judge) commonHold(rs map[string]q2Reading) bool {
	if !lbCommonGuard {
		return false
	}
	common, jumped, n := j.commonJump(rs)
	if !common {
		j.commonRuns = 0
		return false
	}
	j.commonRuns++
	if j.commonRuns > lbCommonMaxRuns {
		return false
	}
	lbCommonHeld.Add(1)
	if j.commonRuns == 1 {
		j.log("lb auto2: %d of %d paths jumped in one reading — a common cause (own load), reading held (HS2_LB_COMMON_GUARD)", jumped, n)
	}
	return true
}

func (j *q2Judge) commonJump(rs map[string]q2Reading) (bool, int, int) {
	var jumped []string
	n := 0
	for k, r := range rs {
		q := j.p[k]
		if q == nil || !q.have {
			continue
		}
		n++
		if r.loss > lb2CutLoss && r.loss > 3*q.ewma+0.01 {
			jumped = append(jumped, k)
		}
	}
	if n < 3 || 2*len(jumped) < n {
		return false, len(jumped), n
	}
	// one IP explains it: every jumped path has it, and it is not on every path
	l0, r0 := pathIPs(jumped[0])
	for _, ip := range []string{l0, r0} {
		all := true
		for _, k := range jumped {
			l, r := pathIPs(k)
			if l != ip && r != ip {
				all = false
				break
			}
		}
		if !all {
			continue
		}
		for k := range rs {
			if l, r := pathIPs(k); l != ip && r != ip {
				return false, len(jumped), n // some path lacks it: an IP verdict
			}
		}
	}
	return true, len(jumped), n
}
