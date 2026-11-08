package engine

import "time"

// Spare dials for paths under their share (HS2_LB_AUTO2).
//
// In reverse the exit dials a link only when the pool grows or a link dies;
// the rebalance moves links only by retiring an IDLE one, whose replacement is
// then placed on the path under its share. On a busy tunnel no link is ever
// idle and few die, so a path added at runtime (a reload) stayed at 0 links
// for good — the main tunnel: a new IPv6 pair at 0/15 after 10 minutes, the
// IPv4 paths over their targets (40/32) — and "every live path keeps two
// links" held only at dial time.
//
// So the balancer asks the pool for a spare dial: one more slot, beyond the
// edge's target, whose dial is placed like any other (acquire: a path under
// two links first, then the one furthest below its share). Make before
// break: nothing is closed for it. The pool is then over its target, and the
// edge — which alone sees the users — retires a surplus link once it is empty
// (on the path most over its share), the slot of that link not redialing.
//
//   - Every quality tick (and at once after the paths change), a live path
//     with fewer than lb2Keep links (dials in flight included) gets spare
//     dials for the difference.
//   - A rebalance round that finds no idle link to retire while a path is a
//     link or more under its share asks for one spare instead (counted in
//     lb2DrainPerMin like a retire).
//   - The pool holds at most lb2SpareMax slots over the edge's target.
var lb2SpareMax = 2

// setSpare installs the pool's spare-dial request: n spares wanted now (the
// pool starts what is missing, within lb2SpareMax over its target).
func (b *LB) setSpare(f func(n int)) {
	b.mu.Lock()
	b.spare = f
	b.mu.Unlock()
}

// minDeficitLocked is how many links the live paths lack to have lb2Keep each
// (dials in flight count). None before the first link is up (startup is the
// pool's own fill) or with one live path. Caller holds b.mu.
func (b *LB) minDeficitLocked(now time.Time) int {
	live, total, need := 0, 0, 0
	for j, p := range b.paths {
		total += b.links[j]
		if p.Weight > 0 && b.eligibleLocked(j, now) {
			live++
			if b.links[j] < lb2Keep {
				need += lb2Keep - b.links[j]
			}
		}
	}
	if live < 2 || total == 0 || b.upAt.IsZero() {
		return 0
	}
	return need
}

// fillMin asks the pool for spare dials for the paths under lb2Keep links.
func (b *LB) fillMin() int {
	b.mu.Lock()
	if !b.auto2On() || b.spare == nil {
		b.mu.Unlock()
		return 0
	}
	n := b.minDeficitLocked(b.now())
	f := b.spare
	var names []string
	if n > 0 {
		now := b.now()
		for j, p := range b.paths {
			if p.Weight > 0 && b.eligibleLocked(j, now) && b.links[j] < lb2Keep {
				names = append(names, p.Name())
			}
		}
	}
	b.mu.Unlock()
	if n > 0 {
		f(n)
		if b.spareLog.Load() == 0 || time.Since(time.Unix(0, b.spareLog.Load())) >= time.Minute {
			b.spareLog.Store(time.Now().UnixNano())
			b.log("lb auto2: %d link(s) short of two per path on %v — spare dial(s) asked of the pool (nothing closed for them)", n, names)
		}
	}
	return n
}

// spareForGap: the rebalance found no idle link to retire. If a live path is
// a link or more under its share while another is a link or more over it,
// one spare dial is asked for (its dial goes to the path furthest under),
// counted in lb2DrainPerMin like a retire.
func (b *LB) spareForGap(now time.Time) bool {
	b.mu.Lock()
	if b.spare == nil || len(b.drained) >= lb2DrainPerMin {
		b.mu.Unlock()
		return false
	}
	eff := b.effWeightsLocked()
	total, w := 0, 0
	for j := range b.paths {
		total += b.links[j]
		if b.eligibleLocked(j, now) {
			w += eff[j]
		}
	}
	if w == 0 || total < 2 {
		b.mu.Unlock()
		return false
	}
	under, over := -1, -1
	var underBy, overBy float64
	for j := range b.paths {
		share := 0.0
		if b.eligibleLocked(j, now) {
			share = float64(total) * float64(eff[j]) / float64(w)
		}
		d := float64(b.links[j]) - share
		if d > overBy {
			over, overBy = j, d
		}
		if b.eligibleLocked(j, now) && eff[j] > 0 && -d > underBy {
			under, underBy = j, -d
		}
	}
	if under < 0 || over < 0 || underBy < 1 || overBy < 1 {
		b.mu.Unlock()
		return false
	}
	b.drained = append(b.drained, now)
	f, uname := b.spare, b.paths[under].Name()
	b.mu.Unlock()
	f(1)
	if t := b.spareGapLog.Load(); t == 0 || time.Since(time.Unix(0, t)) >= time.Minute {
		b.spareGapLog.Store(time.Now().UnixNano())
		b.log("lb auto2: rebalance — no idle link to retire; a spare dial asked for %s (%.1f links under its share; the edge retires the surplus once empty)", uname, underBy)
	}
	return true
}
