package engine

import (
	"context"
	"time"
)

// er92: the RESCUE trigger (HS2_MCONN_RESCUE), replacing er91's 2 s health
// tick. Every rescueWatchEvery the link manager looks at each link's last
// read and its unanswered writes (linkMeter.lastRd / lastWr / pendSince, kept
// by meteredConn while the key is on) and hands rescueSilentLinks the silent
// links of a path whose links went silent together (>= 80 % of >= 3 counted
// links, while another path's links hear — pathSilentPaths).
//
// A link is silent only when it has heard nothing for rescueSilence AND has
// had bytes out unanswered for rescuePend; a quiet link with nothing pending
// counts as alive. A judge-cut path alone is NOT a trigger: the first lab
// round (er92 L1/L2, 3 % / 10 % loss, pend 750 ms + cut trigger) rescued 4-8
// conns per run on lossy-alive paths; a lossy path answers within a few RTOs.

var (
	rescueWatchEvery = time.Duration(envIntDef("HS2_MCONN_RESCUE_TICK_MS", 250)) * time.Millisecond
	rescuePend       = time.Duration(envIntDef("HS2_MCONN_RESCUE_PEND_MS", 1500)) * time.Millisecond
)

// rescueLinkState: silent (counted, dead-looking), idle (not counted), or
// hearing (counted, alive).
func rescueLinkState(mtr *linkMeter, now int64) (silent, idle bool) {
	rd, wr, ps := mtr.lastRd.Load(), mtr.lastWr.Load(), mtr.pendSince.Load()
	if rd == 0 {
		return false, true // nothing read yet with the key on
	}
	if time.Duration(now-rd) < rescueSilence {
		return false, false
	}
	if wr <= rd {
		return false, false // quiet, nothing waits for an answer: counts as alive
	}
	return time.Duration(now-ps) >= rescuePend, false
}

// rescueWatch runs while ctx lives (both sides; only the edge's conns act).
func (m *LinkManager) rescueWatch(ctx context.Context) {
	t := time.NewTicker(rescueWatchEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		ul268DarkOnly.Store(nil) // ul268: this tick's sets only
		ul268DarkAll.Store(nil)
		if !rescueOn.Load() || rescueStat.rings.Load() == 0 && rrStat.conns.Load() == 0 {
			continue
		}
		m.mu.Lock()
		set := m.rescueSilentSetLocked(ctrlNow())
		var smp []ul268Sample
		if ul268On.Load() && ul268Iran.Load() { // the exit never acts: no poll there
			smp = m.ul268SamplesLocked()
		}
		m.mu.Unlock()
		if smp != nil {
			set = ul268Merge(set, smp) // ul268: data-dark links (TCP_INFO)
		}
		rescueSilentLinks(set)
	}
}

// rescueSilentSetLocked: the links to rescue from (nil: none). Caller holds
// m.mu.
func (m *LinkManager) rescueSilentSetLocked(now int64) map[any]bool {
	var view []psLink
	var mls []*managedLink
	for _, ml := range m.links {
		if ml.mtr == nil || !ml.link.Alive() {
			continue
		}
		silent, idle := rescueLinkState(ml.mtr, now)
		if idle {
			continue
		}
		view = append(view, psLink{path: mlPath(ml), silent: silent})
		mls = append(mls, ml)
	}
	dead := pathSilentPaths(view)
	var set map[any]bool
	for i, ml := range mls {
		if !view[i].silent {
			continue
		}
		if dead[view[i].path] {
			if set == nil {
				set = map[any]bool{}
			}
			set[ml.link] = true
		}
	}
	return set
}

// ul268SamplesLocked (caller holds m.mu): the links to sample; TCP_INFO is
// read later, without the lock. State entries of gone links are dropped.
func (m *LinkManager) ul268SamplesLocked() []ul268Sample {
	if m.ul268 == nil {
		m.ul268 = map[Link]*ul268Link{}
	}
	smp := make([]ul268Sample, 0, len(m.links))
	seen := make(map[Link]bool, len(m.links))
	for _, ml := range m.links {
		if ml.mtr == nil || !ml.link.Alive() {
			continue
		}
		st := m.ul268[ml.link]
		if st == nil {
			st = &ul268Link{}
			m.ul268[ml.link] = st
		}
		seen[ml.link] = true
		smp = append(smp, ul268Sample{key: ml.link, path: mlPath(ml), st: st})
	}
	for l := range m.ul268 {
		if !seen[l] {
			delete(m.ul268, l)
		}
	}
	return smp
}

// ul268Merge reads each sample's TCP_INFO and adds the dark links to set.
func ul268Merge(set map[any]bool, smp []ul268Sample) map[any]bool {
	for i := range smp {
		smp[i].ts, smp[i].ok = linkTCPStatsOf(smp[i].key.(Link))
	}
	dark := ul268DarkSet(smp, monoNano(time.Now()))
	if len(dark) == 0 {
		return set
	}
	ul268DarkAll.Store(&dark)
	only := map[any]bool{}
	for k := range dark {
		if !set[k] {
			only[k] = true
		}
	}
	ul268DarkOnly.Store(&only)
	if set == nil {
		return dark
	}
	for k := range dark {
		set[k] = true
	}
	return set
}
