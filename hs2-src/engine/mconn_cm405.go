package engine

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// b405: HS2_B405_CMSPREAD (Iran, default off, =1 on) — callmove target spread.
//
// sk402: every callmove picks the single lowest-loss path, so the calls that
// cross cmSustain in the same tick all land on it (T1 H0 at el 397: 5 calls
// -> one path, 3 -> another within 30 ms; that path was the el 420 cut, and
// both arms then stalled together). With the key on, a move's target is
// chosen among the eligible paths whose loss is within cmSpreadMargin of the
// best one (and still cmDelta under the moving copy's path and inside the
// b338 damper), preferring the path with the fewest moves in the last
// cmSpreadWin, then the lower loss; a path that already took cmSpreadMax
// moves in cmSpreadWin is not a target. No target left: no move this tick
// (counted as spread-held); the call looks again next poll.
var b405CMSpread atomic.Bool

func init() { b405CMSpread.Store(envBoolDefault("HS2_B405_CMSPREAD", false)) }

// Tunables (vars for tests).
var (
	cmSpreadMargin = 0.0005 // loss fraction above the best target
	cmSpreadWin    = 2 * time.Second
	cmSpreadMax    = 2
)

var cmSpreadHeld atomic.Int64 // moves held back: every near-best target full

var cmSpread struct {
	mu sync.Mutex
	at map[string][]time.Time // target path -> move starts within cmSpreadWin
}

// cmSpreadTake picks the move's target among snap's near-best eligible paths
// not in used and records the move on it. ok false: every such path took
// cmSpreadMax moves within cmSpreadWin.
func cmSpreadTake(snap *cmSnap, used map[string]bool, worstL, bestL float64, now time.Time) (to string, toL float64, ok bool) {
	type cand struct {
		p    string
		loss float64
		n    int
	}
	cmSpread.mu.Lock()
	defer cmSpread.mu.Unlock()
	if cmSpread.at == nil {
		cmSpread.at = map[string][]time.Time{}
	}
	for p, ts := range cmSpread.at {
		k := ts[:0]
		for _, t := range ts {
			if now.Sub(t) < cmSpreadWin {
				k = append(k, t)
			}
		}
		if len(k) == 0 {
			delete(cmSpread.at, p)
		} else {
			cmSpread.at[p] = k
		}
	}
	var cs []cand
	for p, q := range snap.p {
		if !q.ok || used[p] || q.loss > bestL+cmSpreadMargin || worstL-q.loss < cmDelta ||
			worstL < cmRatio*q.loss || q.loss >= cmBestMax {
			continue
		}
		n := len(cmSpread.at[p])
		if n >= cmSpreadMax {
			continue
		}
		cs = append(cs, cand{p, q.loss, n})
	}
	if len(cs) == 0 {
		cmSpreadHeld.Add(1)
		return "", 0, false
	}
	sort.Slice(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		if a.n != b.n {
			return a.n < b.n
		}
		if a.loss != b.loss {
			return a.loss < b.loss
		}
		return a.p < b.p
	})
	c := cs[0]
	cmSpread.at[c.p] = append(cmSpread.at[c.p], now)
	return c.p, c.loss, true
}

// cmSpreadUndo drops the move recorded on p at now (cmAllow refused it).
func cmSpreadUndo(p string, now time.Time) {
	cmSpread.mu.Lock()
	defer cmSpread.mu.Unlock()
	ts := cmSpread.at[p]
	for i := len(ts) - 1; i >= 0; i-- {
		if ts[i].Equal(now) {
			cmSpread.at[p] = append(ts[:i:i], ts[i+1:]...)
			return
		}
	}
}
