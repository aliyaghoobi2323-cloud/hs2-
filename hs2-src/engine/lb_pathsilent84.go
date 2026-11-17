package engine

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync/atomic"
	"time"
)

// pathSilentEvents: paths marked silent (HS2_LB_PATH_SILENT), for tests and logs.
var pathSilentEvents atomic.Int64

// lb84: HS2_LB_PATH_SILENT=MS (default 0 = off, production unchanged).
//
// A blackholed path is noticed one link at a time: each link becomes suspect
// (no new connections) after suspectAfter (12 s) with nothing received. In the
// lbv81 soak the crowd failures of a blackhole (164/181 per lab) started at
// +8 s and ended at +22 s: requests on the dead path timing out, and new
// connections still placed on its links until +12-14 s, which then time out
// too. A path whose links ALL go silent together is a different signal from
// one quiet link: every busy link receives a control reply every 3 s and a
// smux keepalive every 4-8 s, so on a working path (lossy or queued) it is
// all but impossible for 80 % of 3+ links to hear nothing for MS at once.
//
// With the key on, after each health tick: a path with >= pathSilentMinLinks
// counted links (alive, measured, received something before, not allowed to
// be quiet by HS2_QUIET_IDLE) of which >= 80 % have received nothing for MS,
// while another path's links are hearing (<= 20 % silent), has its silent
// links marked suspect at once. Nothing is closed: the mark only keeps new
// connections off them (serving), and it clears the tick anything arrives,
// exactly like the per-link suspect. MS is clamped to [1500, suspectAfter).
var lbPathSilent = func() time.Duration {
	v, err := strconv.Atoi(os.Getenv("HS2_LB_PATH_SILENT"))
	if err != nil || v <= 0 {
		return 0
	}
	d := time.Duration(v) * time.Millisecond
	if d < 1500*time.Millisecond {
		d = 1500 * time.Millisecond
	}
	if d >= suspectAfter {
		return 0
	}
	return d
}()

const pathSilentMinLinks = 3

// psLink is one link's view for pathSilentPaths.
type psLink struct {
	path   string
	silent bool // received nothing for lbPathSilent
}

// pathSilentPaths returns the paths whose links went silent together while
// another path's links still hear: >= minLinks counted links, >= 80 % silent,
// and some other path with >= minLinks links <= 20 % silent.
func pathSilentPaths(links []psLink) map[string]bool {
	type agg struct{ n, silent int }
	by := map[string]*agg{}
	for _, l := range links {
		if l.path == "" {
			continue
		}
		a := by[l.path]
		if a == nil {
			a = &agg{}
			by[l.path] = a
		}
		a.n++
		if l.silent {
			a.silent++
		}
	}
	var out map[string]bool
	for p, a := range by {
		if a.n < pathSilentMinLinks || a.silent*10 < a.n*8 {
			continue
		}
		hearing := false
		for q, b := range by {
			if q != p && b.n >= pathSilentMinLinks && b.silent*5 <= b.n {
				hearing = true
				break
			}
		}
		if hearing {
			if out == nil {
				out = map[string]bool{}
			}
			out[p] = true
		}
	}
	return out
}

// pathSilentMark applies HS2_LB_PATH_SILENT after the per-link health loop.
// Caller holds m.mu.
func (m *LinkManager) pathSilentMark(now time.Time, logs []string) []string {
	if lbPathSilent <= 0 {
		return logs
	}
	var view []psLink
	for _, ml := range m.links {
		if ml.mtr == nil || !ml.sampled || !ml.rxSeen || !ml.link.Alive() {
			continue
		}
		if q := ml.mtr.quiet; q != nil && q.lenient() {
			continue // an idle link may be quiet: says nothing about its path
		}
		view = append(view, psLink{path: mlPath(ml), silent: now.Sub(ml.lastRx) >= lbPathSilent})
	}
	dead := pathSilentPaths(view)
	var newly []string
	marked := 0
	for _, ml := range m.links {
		if ml.mtr == nil || !ml.rxSeen || !dead[mlPath(ml)] || now.Sub(ml.lastRx) < lbPathSilent {
			continue
		}
		if !ml.suspect {
			ml.suspect = true
			marked++
		}
	}
	for p := range dead {
		if !m.pathSilentNow[p] {
			newly = append(newly, p)
		}
	}
	m.pathSilentNow = dead
	if len(newly) > 0 {
		sort.Strings(newly)
		pathSilentEvents.Add(int64(len(newly)))
		logs = append(logs, fmt.Sprintf("lb path-silent: path(s) %v — 80%%+ of their links heard nothing for %s while other paths hear; %d link(s) take no new connections until they answer",
			newly, fmtDur(lbPathSilent), marked))
	}
	return logs
}
