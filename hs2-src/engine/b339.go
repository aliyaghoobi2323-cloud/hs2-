package engine

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// b339 (ab337 follow-up; prod Iran t1 2026-11-18: a path cut, 10 links judged
// stuck at once, then "lb cut-drain: 9 link(s) ... with 840 connection(s)").
//
// 1. HS2_B339_PATHHYST (default on, with HS2_B337_PATHSTALL): once a path is
// found stalled whole, its links are held from stuck verdicts for
// stuckPathWait from that FIRST sample, even when part of the path answers
// again. ab337 X5: as soon as some of a cut path's links resumed, "waiting >
// prompt" failed and the RTO-backoff stragglers were judged at 8 s (23 links
// in one sample). A path is re-armed only after it has not been stalled for
// stuckPathWait, so a path that flaps every sample is not held forever; a
// path stalled for good still has its links judged once they wait
// stuckPathWait (the b337 rule). Links throttled alone elsewhere are judged
// as before.
//
// 2. HS2_B339_CUTGRACE (default off): a cut-drained link's connections are
// not reclaimed (idle ones closed, light ones evacuated, trickling ones forced)
// until its path's cut has lasted cutGrace, or at once when the path was cut
// again within cutFlapWindow of its last cut ending (a flapping path is bad).
// Short cuts (5-30 s, ~40 a day on one prod Iran IP) then close nothing: idle
// connections send nothing and lose nothing by waiting, flowing ones stay as
// before, and a trickling one that really stalls is moved by the rescue
// machinery. The cut-drained links still take no new connections, and the
// exit is still asked for replacements, as before.
var (
	b339PathHyst atomic.Bool
	b339CutGrace atomic.Bool
)

func init() {
	b339PathHyst.Store(envBoolDefault("HS2_B339_PATHHYST", true))
	b339CutGrace.Store(envBoolDefault("HS2_B339_CUTGRACE", false))
}

// cutGrace: a cut path's connections are left alone this long into its cut.
var cutGrace = 30 * time.Second

// cutFlapWindow: a path cut again within this of its last cut ending gets no
// grace.
var cutFlapWindow = 2 * time.Minute

var b339Stat struct {
	hystHeld   atomic.Int64 // stuck verdicts held only by the hysteresis (link-samples)
	graceHeld  atomic.Int64 // cut-drained link reclaims skipped in the grace (link-ticks)
	cuts       atomic.Int64 // cut episodes seen
	flaps      atomic.Int64 // ... of which flaps (no grace)
	graceEnded atomic.Int64 // cuts that ended inside the grace (nothing closed)
	remarked   atomic.Int64 // retiring links re-marked cut-drained on a new cut
}

// b339Line: " b339: hyst=N grace=N cuts=N flaps=N short=N remark=N" on the mem line once used.
func b339Line() string {
	h, g := b339Stat.hystHeld.Load(), b339Stat.graceHeld.Load()
	if h == 0 && g == 0 && b339Stat.cuts.Load() == 0 {
		return ""
	}
	return fmt.Sprintf(" b339: hyst=%d grace=%d cuts=%d flaps=%d short=%d remark=%d", h, g,
		b339Stat.cuts.Load(), b339Stat.flaps.Load(), b339Stat.graceEnded.Load(), b339Stat.remarked.Load())
}

// pathHold is a path's stall hysteresis state.
type pathHold struct {
	first, last time.Time // first stalled sample of this hold, latest stalled sample
}

// b339HoldPathsLocked updates the hysteresis with this sample's stalled
// paths and returns the paths whose links are held regardless of the
// current count (within stuckPathWait of their first stalled sample). Caller
// holds m.mu.
func (m *LinkManager) b339HoldPathsLocked(stalled map[string]bool, now time.Time) map[string]bool {
	if !b339PathHyst.Load() || !b337PathStall.Load() {
		return nil
	}
	if m.b339Hold == nil {
		if len(stalled) == 0 {
			return nil
		}
		m.b339Hold = map[string]*pathHold{}
	}
	for p := range stalled {
		h := m.b339Hold[p]
		if h == nil {
			h = &pathHold{first: now}
			m.b339Hold[p] = h
		}
		h.last = now
	}
	var out map[string]bool
	for p, h := range m.b339Hold {
		if now.Sub(h.first) < stuckPathWait {
			if out == nil {
				out = map[string]bool{}
			}
			out[p] = true
			continue
		}
		if now.Sub(h.last) >= stuckPathWait { // quiet for a while: re-armable
			delete(m.b339Hold, p)
		}
	}
	return out
}

// b339Hold reports whether a stuck candidate is held off: by the b337 rule
// while its path is stalled whole now (wait under stuckPathWait), or by the
// hysteresis once part of the path answers again (wait under twice that: a
// link waiting that long is judged whatever its path did).
func b339Hold(stalled, hyst map[string]bool, c stuckObs) bool {
	p := b337PathOf(c.ml)
	if stalled[p] {
		return b337Hold(stalled, c)
	}
	if len(hyst) == 0 || !hyst[p] || c.wait >= 2*stuckPathWait {
		return false
	}
	b339Stat.hystHeld.Add(1)
	return true
}

// cutEpisode is one cut of one path (keyed as the cut report).
type cutEpisode struct {
	start, end time.Time // end zero while cut
	flap       bool      // cut again within cutFlapWindow of the previous end
}

// cutReportKey is the cut map key a link path matches ("" none).
func cutReportKey(cut map[string]int, path string) string {
	if len(cut) == 0 || path == "" {
		return ""
	}
	if _, ok := cut[path]; ok {
		return path
	}
	if i := strings.IndexByte(path, '>'); i >= 0 {
		if _, ok := cut[path[:i]+">*"]; ok {
			return path[:i] + ">*"
		}
	}
	return ""
}

// b339CutTrackLocked records cut episode starts and ends per path from the
// dialing side's report (cut: the cut paths now). Caller holds m.mu.
func (m *LinkManager) b339CutTrackLocked(cut map[string]int, now time.Time) {
	if m.b339Cuts == nil {
		if len(cut) == 0 {
			return
		}
		m.b339Cuts = map[string]*cutEpisode{}
	}
	for p := range cut {
		e := m.b339Cuts[p]
		switch {
		case e == nil:
			m.b339Cuts[p] = &cutEpisode{start: now}
			b339Stat.cuts.Add(1)
		case !e.end.IsZero():
			flap := now.Sub(e.end) < cutFlapWindow
			if flap {
				b339Stat.flaps.Add(1)
			}
			*e = cutEpisode{start: now, flap: flap}
			b339Stat.cuts.Add(1)
		}
	}
	for p, e := range m.b339Cuts {
		if _, ok := cut[p]; ok {
			continue
		}
		if e.end.IsZero() {
			e.end = now
			if now.Sub(e.start) < cutGrace && !e.flap {
				b339Stat.graceEnded.Add(1)
			}
			continue
		}
		if now.Sub(e.end) >= cutFlapWindow {
			delete(m.b339Cuts, p)
		}
	}
}

// b339GraceLocked reports whether a cut-drained link's connections are left
// alone now (HS2_B339_CUTGRACE: its path's cut is younger than cutGrace and
// not a flap). Caller holds m.mu.
func (m *LinkManager) b339GraceLocked(ml *managedLink, now time.Time) bool {
	if !b339CutGrace.Load() || !ml.cutDrain || ml.cutKey == "" {
		return false
	}
	e := m.b339Cuts[ml.cutKey]
	if e == nil || !e.end.IsZero() || e.flap {
		return false
	}
	return now.Sub(e.start) < cutGrace
}

// b339RemarkLocked (HS2_B339_CUTGRACE): a link already retiring when its
// path is cut (left from an earlier cut whose mark was cleared when it
// ended, or retired by the pattern) is marked cut-drained too, from now, so
// a long cut moves its users after the grace as it does a serving link's
// (ab339 1x: a 60 s cut 125 s after a 5 s one moved nothing, the path's
// surplus links still retiring from the first). Its slot is not replaced
// (cutLate: it was not serving), and it does not come back into service
// while the path is cut. Draining links are left to the stuck/loss rules.
// Caller holds m.mu.
func (m *LinkManager) b339RemarkLocked(cut map[string]int, now time.Time) {
	if !b339CutGrace.Load() {
		return
	}
	for _, ml := range m.links {
		if ml.cutDrain || !ml.retiring || ml.draining || ml.degraded || !ml.link.Alive() {
			continue
		}
		p := mlPath(ml)
		if _, ok := cutKeep(cut, p); !ok {
			continue
		}
		ml.cutDrain, ml.cutLate, ml.cutKey, ml.retireSince = true, true, cutReportKey(cut, p), now
		b339Stat.remarked.Add(1)
	}
}
