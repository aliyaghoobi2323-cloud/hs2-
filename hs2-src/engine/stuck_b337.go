package engine

import (
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// b337 (ab334/ab337 lab2, 4.5x load): HS2_B337_PATHSTALL (default on; =0:
// the rel69p stuck rule as it was).
//
// The stuck rule (stuck.go) is for a link throttled on its own while the
// path works for the others. Its "the path is slow" test counts waiting
// links against prompt ones over ALL paths, so one path stalled whole — the
// lab's dc254 cutter dropping one direction of one of 4 paths for 5-6 s,
// a real path blackout or reroute — never passes for slow: the other 3
// paths' links answer in ~100 ms and outnumber it. When the cut ends, TCP
// on that path's links resumes by RTO backoff and its queued data, so their
// oldest pings have waited 8-10 s by the next two health samples while
// they move a few KB: every busy link of the path was judged stuck at once
// (17-38 per cut, drainHeadroom), and a stuck link closes at once every
// connection that moved no data for drainStall — at 4.5x the long-lived
// idle ones, thousands — which then reconnect into the storm: ab337 lab2
// 4500, rel69p-pre1 96-101 stuck verdicts per run (every down-cut >= 5 s)
// against 32 for rel69m in the same round, the open fails and the
// light-user tails of ab334.
//
// With the key on, a waiting link whose own path is stalled whole — two or
// more of its links wait (stuckWait+, moving under activeBytes, as for the
// slow test) and they outnumber that path's links that answer promptly — is
// not judged stuck until its wait reaches stuckPathWait: by then a path
// that was cut and came back has resumed (RTO backoff after a 6 s cut ends
// within a few seconds), while a path that stays throttled for every link
// still has its links judged, ~stuckPathWait later than before. Paths of
// links without a meter path ("") are never held. Other paths and links
// throttled one by one (a minority of their path) are judged as before.
var b337PathStall atomic.Bool

func init() { b337PathStall.Store(envBoolDefault("HS2_B337_PATHSTALL", true)) }

// stuckPathWait: the wait from which a link of a whole-path stall is judged.
const stuckPathWait = 20 * time.Second

var b337Stat struct {
	held  atomic.Int64 // stuck verdicts held off (link-samples)
	paths atomic.Int64 // path stalls seen (path-samples)
}

// b337Line: " b337: held=N paths=N" on the mem line once used.
func b337Line() string {
	h := b337Stat.held.Load()
	if h == 0 {
		return ""
	}
	return fmt.Sprintf(" b337: held=%d paths=%d", h, b337Stat.paths.Load())
}

// pathStallCount counts, per link path, the links waiting and the links
// answering promptly in one health sample.
type pathStallCount struct{ wait, prompt int }

// b337PathOf is a link's path key for the per-path stall test ("" = none).
func b337PathOf(ml *managedLink) string {
	if ml.mtr == nil {
		return ""
	}
	return ml.mtr.path
}

// b337StalledPaths is the set of paths stalled whole in this sample (nil:
// none, or the key off).
func b337StalledPaths(per map[string]*pathStallCount) map[string]bool {
	if !b337PathStall.Load() {
		return nil
	}
	var out map[string]bool
	for p, c := range per {
		if p != "" && c.wait >= 2 && c.wait > c.prompt {
			if out == nil {
				out = make(map[string]bool)
			}
			out[p] = true
		}
	}
	return out
}

// b337Hold reports whether a stuck candidate is held off (its path stalled
// whole and its wait under stuckPathWait).
func b337Hold(stalled map[string]bool, c stuckObs) bool {
	if len(stalled) == 0 || c.wait >= stuckPathWait || !stalled[b337PathOf(c.ml)] {
		return false
	}
	b337Stat.held.Add(1)
	return true
}

// b337HoldLog is the once-a-minute line for held verdicts.
func b337HoldLog(stalled map[string]bool, per map[string]*pathStallCount, held int) string {
	ps := make([]string, 0, len(stalled))
	for p := range stalled {
		c := per[p]
		ps = append(ps, fmt.Sprintf("%s %d waiting / %d prompt", p, c.wait, c.prompt))
	}
	sort.Strings(ps)
	return fmt.Sprintf("%d stuck verdict(s) held: their path is stalled whole (%s) — the path, not those links: judged only after %s (b337)",
		held, strings.Join(ps, ", "), fmtDur(stuckPathWait))
}
