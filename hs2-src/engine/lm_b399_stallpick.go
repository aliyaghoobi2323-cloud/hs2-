package engine

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// HS2_B399_STALLPICK (b399, on in rel69r-rc2, off in rel69r-rc4, default ON again since rel69r-rc5 (v3, sk405); =0 disables): new user connections avoid the
// links of a path that has gone silent, before the LB judge's cut verdict.
//
// Why (sk397/sk398): a reconnect storm (the hourly xray restart) that lands
// on a freshly cut path lost 1.2-1.7 % of its opens to 10 s timeouts in both
// rel69q and rel69r. The pick ranks a path's links last only once the
// dialing side's report says the path is cut (steerCutNewLocked,
// HS2_LB_CUT_NONEW); for the first seconds of a cut the path's links look
// like any other — alive, serving, unpressed — and the wave piles onto them.
//
// What: a 100 ms loop reads, per live link, two stamps the link meter keeps
// already (meteredConn, HS2_MCONN_RESCUE on: lastRd = last read with bytes,
// pendSince = first write after that read). No syscall, no TCP_INFO call, no
// cost on the open path beyond one atomic load per candidate link. A path is
// STALLED when all three hold:
//
//  1. no link of the path has read anything for >= X (HS2_B399_STALL_MS,
//     default 300 ms since rel69r-rc2 (sk399b); RTT here is ~75 ms + up to 100 ms queue);
//  2. at least 2 of its links have bytes written and unanswered for >= X —
//     b383's L3FRESH darkened paths on one idle link's lone keepalive; one
//     link is never evidence here, and a single read on ANY link of the path
//     (a pong, a window update, data) clears the whole path (rule 1);
//  3. some other path is healthy (a link read within X). When every path
//     looks stalled nothing is marked: today's pick, unchanged;
//  4. start-up guard: a path is newly marked only if some link of it read
//     within the last 10 s (b399Fresh). A path that never read (pool
//     start-up, fresh dials still handshaking) is not evidence of a cut.
//     A path already marked stays marked while 1-3 hold, so a cut longer
//     than 10 s keeps its mark until the LB judge's cut verdict takes over.
//  5. stricter start-up gate (rel69r-rc2): a path is newly marked only when
//     the pool has been up >= 10 s AND the path has had a live link for
//     >= 10 s (b399Settle). sk399b P2 still saw 3 marks at el -0.4..2.0 s:
//     handshake reads made rule 4 pass on paths that were seconds old.
//  6. re-mark hysteresis (rel69r-rc3, sk401; 3 s since b399 v3, sk402): a
//     path that cleared within the last 60 s (b399ReMem) is marked again
//     only after 1-5 held for >= 3 s (b399ReHold) without a break.
//  7. fresh onset (b399 v3, sk402): a path not marked now starts a mark (or
//     rule 6's hold) only while >= 2 of its pending links read within 2 s
//     (b399Live) of NOW, not just of the path's last read; a mark or hold
//     already running continues on rules 1-5. sk402's off-cut marks were
//     recovering paths 15-45 s after their cut whose links sat 1.4-2 s
//     silent in RTO backoff; they now read again inside the 3 s hold.
//
// Rule 2 counts only recently live links (rel69r-rc3, sk401): a link whose
// last read is more than 2 s (b399Live) older than the path's last read is a
// leftover of a past cut and is not evidence. sk401 found ~90 % of marks on
// paths 5-45 s after their own cut — 2 zombie links plus a 300 ms read gap on
// a now-quiet path — flapping stalled/clear every 0.1-1 s.
//
// Call subs follow the rank like every other placement. rel69r-rc3 let them
// ignore it (part c); sk402 showed a call starting at a cut onset placed its
// second sub on the freshly cut path (16-22 s stalls in 3 of 4 on halves),
// so b399 v3 reverted that: the fresh-cut avoidance helps calls.
//
// The stalled path's links get pickKey.cutNew (ordered after every other
// link, as a reported cut is), so they are still picked when nothing else
// is eligible. Nothing is closed, retired or moved; only new placements.
var b399On atomic.Bool

// b399X is the silence that makes a path stalled (ns).
var b399X atomic.Int64

const b399Tick = 100 * time.Millisecond

// b399Fresh: a path must have read within this to be newly marked (rule 4).
const b399Fresh = int64(10 * time.Second)

// b399Live (rule 2, rel69r-rc3): a link counts when its last read is within
// this of the path's last read (any link of it).
const b399Live = int64(2 * time.Second)

// b399ReHold / b399ReMem (rule 6, rel69r-rc3): re-mark hysteresis.
var b399ReHold atomic.Int64

const b399ReMem = int64(60 * time.Second)

// b399Settle: the pool and the path must be up this long before a path is
// newly marked (rule 5). Settable so loop tests can settle at once.
var b399Settle atomic.Int64

func init() {
	b399ReHold.Store(int64(3 * time.Second)) // rule 6 (b399 v3, sk402)
	b399Settle.Store(int64(10 * time.Second))
	b399On.Store(envBoolDefault("HS2_B399_STALLPICK", true)) // rel69r-rc5: on again (v3, sk405)
	x := 300 * time.Millisecond
	if v, err := strconv.Atoi(strings.TrimSpace(envDefault("HS2_B399_STALL_MS"))); err == nil && v >= 150 && v <= 10000 {
		x = time.Duration(v) * time.Millisecond
	}
	b399X.Store(int64(x))
}

var b399Stat struct {
	stalls atomic.Uint64 // path stall marks (rising edges)
	clears atomic.Uint64
	held   atomic.Uint64 // ticks with a candidate but no healthy alternative (rule 3)
}

// b399Obs is one live link as the loop reads it.
type b399Obs struct {
	path           string
	lastRd, pendSn int64
}

// b399PathSt is one path's evidence at a tick.
type b399PathSt struct {
	links, pend int
	fresh       int // pending links that read within b399Live of now (rule 7)
	maxRd       int64
}

// b399Judge returns the stalled paths (nil: none, or no healthy alternative —
// then held reports whether some path was a candidate).
// prev holds the paths marked at the previous tick (rules 4-5 apply only to
// new marks). poolUp is when the pool's loop started and pathUp when each
// path was first seen live (ctrlNow ns); a nil pathUp / zero poolUp means
// "settled" (unit tests of rules 1-4).
func b399Judge(obs []b399Obs, now, x int64, prev map[string]time.Time, poolUp int64, pathUp map[string]int64) (stalled map[string]b399PathSt, held bool) {
	ps := map[string]*b399PathSt{}
	for _, o := range obs {
		if o.path == "" {
			continue
		}
		s := ps[o.path]
		if s == nil {
			s = &b399PathSt{maxRd: o.lastRd}
			ps[o.path] = s
		}
		s.links++
		if o.lastRd > s.maxRd {
			s.maxRd = o.lastRd
		}
	}
	// rule 2: only links live when the path went quiet count (rel69r-rc3)
	for _, o := range obs {
		if s := ps[o.path]; s != nil && s.maxRd-o.lastRd <= b399Live && o.pendSn > o.lastRd && now-o.pendSn >= x {
			s.pend++
			if now-o.lastRd <= b399Live {
				s.fresh++ // rule 7
			}
		}
	}
	healthy := false
	for p, s := range ps {
		if now-s.maxRd < x {
			healthy = true
			continue
		}
		if _, was := prev[p]; s.pend >= 2 && (was || b399NewOK(p, s, now, poolUp, pathUp)) {
			if stalled == nil {
				stalled = map[string]b399PathSt{}
			}
			stalled[p] = *s
		}
	}
	if len(stalled) > 0 && !healthy {
		return nil, true
	}
	return stalled, false
}

// b399NewOK: rules 4 and 5 for a path not yet marked.
func b399NewOK(p string, s *b399PathSt, now, poolUp int64, pathUp map[string]int64) bool {
	if now-s.maxRd > b399Fresh {
		return false // rule 4: never read, or silent > 10 s
	}
	if poolUp != 0 && now-poolUp < b399Settle.Load() {
		return false // rule 5: pool start-up
	}
	if t0, ok := pathUp[p]; ok && now-t0 < b399Settle.Load() {
		return false // rule 5: path just came up
	}
	return true
}

// b399Hyst is rule 6's state (the loop's own; not shared).
type b399Hyst struct {
	cond    map[string]int64 // since when rules 1-5 have held without a break
	cleared map[string]int64 // when the path's last mark cleared
}

func newB399Hyst() *b399Hyst {
	return &b399Hyst{cond: map[string]int64{}, cleared: map[string]int64{}}
}

// filter applies rules 6 and 7 to b399Judge's result st: a path not marked
// now (prev) starts only with >= 2 fresh pending links (rule 7; the onset
// starts rule 6's hold, which then runs on 1-5); one cleared within
// b399ReMem stays unmarked until st has held it for b399ReHold. Paths
// already marked pass.
func (h *b399Hyst) filter(st map[string]b399PathSt, prev map[string]time.Time, now int64) map[string]b399PathSt {
	for p := range h.cond {
		if _, ok := st[p]; !ok {
			delete(h.cond, p)
		}
	}
	for p, t := range h.cleared {
		if now-t > b399ReMem {
			delete(h.cleared, p)
		}
	}
	for p, s := range st {
		_, was := prev[p]
		if _, ok := h.cond[p]; !ok {
			if !was && s.fresh < 2 {
				delete(st, p) // rule 7: no fresh onset
				continue
			}
			h.cond[p] = now
		}
		if was {
			continue
		}
		if _, rc := h.cleared[p]; rc && now-h.cond[p] < b399ReHold.Load() {
			delete(st, p)
		}
	}
	return st
}

// clear notes that path p's mark cleared at now.
func (h *b399Hyst) clear(p string, now int64) { h.cleared[p] = now }

// b399Stalled is the pick's view of link ml (one atomic load).
func b399Stalled(ml *managedLink) bool {
	return b399On.Load() && ml.b399Stall.Load()
}

// b399Prune drops paths with no live link (a path that comes back is new).
func b399Prune(pathUp map[string]int64, obs []b399Obs) {
	live := make(map[string]bool, len(obs))
	for _, o := range obs {
		live[o.path] = true
	}
	for p := range pathUp {
		if !live[p] {
			delete(pathUp, p)
		}
	}
}

// b399Loop marks the links of stalled paths until ctx ends.
func (m *LinkManager) b399Loop(ctx context.Context) {
	t := time.NewTicker(b399Tick)
	defer t.Stop()
	since := map[string]time.Time{}
	poolUp := ctrlNow()
	pathUp := map[string]int64{}
	hy := newB399Hyst()
	var obs []b399Obs
	var mls []*managedLink
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		x := b399X.Load()
		now := ctrlNow()
		obs, mls = obs[:0], mls[:0]
		m.mu.RLock()
		for _, ml := range m.links {
			if ml.mtr == nil || !ml.aliveCheap() {
				continue
			}
			mls = append(mls, ml)
			obs = append(obs, b399Obs{path: ml.mtr.path, lastRd: ml.mtr.lastRd.Load(), pendSn: ml.mtr.pendSince.Load()})
		}
		m.mu.RUnlock()
		for _, o := range obs {
			if _, ok := pathUp[o.path]; !ok && o.path != "" {
				pathUp[o.path] = now
			}
		}
		b399Prune(pathUp, obs)
		st, held := b399Judge(obs, now, x, since, poolUp, pathUp)
		if held {
			b399Stat.held.Add(1)
		}
		st = hy.filter(st, since, now)
		for i, ml := range mls {
			_, s := st[obs[i].path]
			ml.b399Stall.Store(s)
		}
		wall := time.Now()
		var on []string
		for p := range st {
			if _, ok := since[p]; !ok {
				on = append(on, p)
			}
		}
		sort.Strings(on)
		for _, p := range on {
			s := st[p]
			since[p] = wall
			b399Stat.stalls.Add(1)
			m.log("b399 stallpick: path %s stalled: %d/%d links unanswered >= %s, no read on the path for %s (marks %d)",
				p, s.pend, s.links, time.Duration(x), time.Duration(now-s.maxRd).Round(time.Millisecond), b399Stat.stalls.Load())
		}
		for p, t0 := range since {
			if _, ok := st[p]; !ok {
				delete(since, p)
				hy.clear(p, now)
				b399Stat.clears.Add(1)
				m.log("b399 stallpick: path %s clear after %s", p, wall.Sub(t0).Round(time.Millisecond))
			}
		}
	}
}
