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
func (b *LB) setSpare(f func(n int) int) {
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
	if n == 0 && b.silentProbeDueLocked(b.now()) {
		n = 1 // HS2_LB_SILENT: a cooled path's probe rides on a spare dial (lb_silent.go)
	}
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
		got := f(n)
		if len(names) == 0 {
			return n // HS2_LB_SILENT: the spare is a cooled path's probe (silentProbeDueLocked), not a short path
		}
		if b.spareLog.Load() == 0 || time.Since(monoTime(b.spareLog.Load())) >= time.Minute {
			b.spareLog.Store(monoNano(time.Now()))
			if got > 0 { // rebal329: say what the pool did, not what was asked
				b.log("lb auto2: %d link(s) short of two per path on %v — %d spare dial(s) started by the pool (nothing closed for them)", n, names, got)
			} else {
				b.log("lb auto2: %d link(s) short of two per path on %v — the pool started no spare dial (at its slot cap, in an outage, or spares already pending)", n, names)
			}
		}
	}
	return n
}

// spareForGap: the rebalance found no idle link to retire. If a live path is
// a link or more under its share while another is a link or more over it,
// one spare dial is asked for (its dial goes to the path furthest under),
// counted in lb2DrainPerMin like a retire.
func (b *LB) spareForGap(now time.Time) bool {
	T := 0
	b.mu.Lock()
	wf := b.spareWant
	b.mu.Unlock()
	if lbSpareEdge && wf != nil {
		T = wf() // the pool's lock, not under b.mu
	}
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
		if lbSpareEdge {
			b.gapClearLocked(-1)
		}
		b.mu.Unlock()
		return false
	}
	room := 8
	if lbSpareEdge { // lb_spare176.go: only spares the edge would serve
		b.gapClearLocked(under)
		n := total
		if T > 0 && T < n {
			n = T
		}
		room = b.edgeRoomLocked(under, eff, n, now)
		if r, ok := b.edgeRoomServLocked(under, eff, now); ok { // b331: the edge's serving frame, per path
			room = r
		}
		if room == 0 || b.gapWaitLocked(under, now) {
			uname := b.paths[under].Name()
			b.mu.Unlock()
			if t := b.gapSkipLog.Load(); t == 0 || time.Since(monoTime(t)) >= 10*time.Minute {
				b.gapSkipLog.Store(monoNano(time.Now()))
				b.log("lb auto2: rebalance — %s is %.1f links under its share of all %d links, but not of the %d the edge serves (or its last spare did not stay): no spare dial", uname, underBy, total, n)
			}
			return false
		}
	}
	k := min(lbSparePerRound, int(underBy), lb2DrainPerMin-len(b.drained), room) // lb_fill17.go; 1 unless HS2_LB_SPARE_PER_ROUND
	k = max(k, 1)
	f, uname := b.spare, b.paths[under].Name()
	b.mu.Unlock()
	got := f(k)
	// rebal329: book only spares the pool started. An ask that started no
	// dial (slot cap: retiring links the edge holds fill the slots) is not a
	// spare that "did not stay" (lb_spare176.go's backoff), and it is not
	// counted against lb2DrainPerMin.
	if got > 0 {
		b.mu.Lock()
		if lbSpareEdge {
			b.gapAskedLocked(under, now)
		}
		for i := 0; i < got; i++ {
			b.drained = append(b.drained, now)
		}
		b.mu.Unlock()
	}
	if got == 0 {
		if t := b.spareCapLog.Load(); t == 0 || time.Since(monoTime(t)) >= 10*time.Minute {
			b.spareCapLog.Store(monoNano(time.Now()))
			b.log("lb auto2: rebalance — %s is %.1f links under its share, but the pool started no spare dial (at its slot cap: retiring links the edge holds fill the slots; or in an outage, or spares already pending)", uname, underBy)
		}
		return false
	}
	if t := b.spareGapLog.Load(); t == 0 || time.Since(monoTime(t)) >= time.Minute {
		b.spareGapLog.Store(monoNano(time.Now()))
		b.log("lb auto2: rebalance — no idle link to retire; %d spare dial(s) started for %s (%.1f links under its share; the edge retires the surplus once empty)", got, uname, underBy)
	}
	return true
}

// arrivalWantedLocked (reverse edge, HS2_LB_AUTO2): a link that arrives while
// the pool has its target is born retiring — except one the balancer dialed
// as a spare for a path under its share: its path has fewer than lb2Keep
// serving links, or its address is under its share (the dialing side's, from
// its report — shrinkShares). It then serves, and the pool, over its target,
// retires a link from the path most over its share instead (reconcile), once
// that one is empty. Without this the spare was closed and its path stayed
// empty. Caller holds m.mu.
func (m *LinkManager) arrivalWantedLocked(ml *managedLink) bool {
	path := mlPath(ml)
	if !lbAuto2 || m.shrinkShares == nil || path == "" {
		return false
	}
	var ms []*managedLink
	np := 0
	for _, x := range m.links {
		if x.serving() {
			ms = append(ms, x)
			if mlPath(x) == path {
				np++
			}
		}
	}
	if np < lb2Keep {
		return true
	}
	if ok, known := m.arrivalPathLocked(path, ms); known && ok { // b331: per path (lb_sparepath331.go)
		return true
	}
	ms = append(ms, ml)
	keys, w := m.shrinkShares(ms)
	k := keys[len(keys)-1]
	W := 0
	for _, x := range w {
		if x > 0 {
			W += x
		}
	}
	if k == "" || W == 0 || w[k] <= 0 {
		return false
	}
	c := 0
	for _, kk := range keys {
		if kk == k {
			c++
		}
	}
	return float64(c) <= float64(len(ms))*float64(w[k])/float64(W)+0.5
}

// reportShares is the dialing side's effective share of each of this side's
// addresses (its paths summed by remote IP), as weights for revShrinkKeys.
func reportShares(rep LBReport) []LBAdvertIP {
	sum := map[string]float64{}
	var order []string
	for _, p := range rep.Paths {
		if _, ok := sum[p.Remote]; !ok {
			order = append(order, p.Remote)
		}
		sum[p.Remote] += p.EffPct
	}
	out := make([]LBAdvertIP, 0, len(order))
	for _, ip := range order {
		out = append(out, LBAdvertIP{IP: ip, Weight: int(sum[ip]*10 + 0.5)})
	}
	return out
}

// keepPathMin un-drops shrink picks that would leave a path (mlPath) with
// fewer than lb2Keep serving links (HS2_LB_AUTO2): a cut path keeps its two
// probe links — with few or no users they look like the first to retire, and
// retiring them left the main tunnel's cut paths at 0 links, never measured
// again. The pool then stays a link or two over its target.
func keepPathMin(serving []*managedLink, drop []bool) {
	left := map[string]int{}
	for i, ml := range serving {
		if !drop[i] {
			left[mlPath(ml)]++
		}
	}
	for i, ml := range serving {
		p := mlPath(ml)
		if drop[i] && p != "" && left[p] < lb2Keep {
			drop[i] = false
			left[p]++
		}
	}
}

// lb2Carry: a path whose ticks bring fewer than lb2MinSegs segments adds them
// up for this long (and at least lb2MinSegs/4 of them) before it gives a
// reading: a cut path's two probe links carry next to no users, and without
// a reading it could never be seen recovering.
var lb2Carry = time.Minute

type lb2Acc struct {
	segs, retr uint64
	since      time.Time
	sil        silentAcc // HS2_LB_SILENT: the links' acked bytes over the carried window
}

// carryLocked adds a tick's counts of path j and returns a reading's counts
// when there are enough; noAck: the reading's links were measured and got
// none of their bytes acked (HS2_LB_SILENT, lb_silent.go). Caller holds b.mu.
func (b *LB) carryLocked(j int, segs, retr uint64, sil silentAcc) (uint64, uint64, bool, bool) {
	if segs >= lb2MinSegs {
		delete(b.acc2, b.paths[j].Name())
		return segs, retr, sil.noAck(), true
	}
	if b.acc2 == nil {
		b.acc2 = map[string]*lb2Acc{}
	}
	now := b.now()
	a := b.acc2[b.paths[j].Name()]
	if a == nil {
		a = &lb2Acc{since: now}
		b.acc2[b.paths[j].Name()] = a
	}
	a.segs += segs
	a.retr += retr
	a.sil.links += sil.links
	a.sil.acked += sil.acked
	if a.segs >= lb2MinSegs || a.segs >= lb2MinSegs/4 && now.Sub(a.since) >= lb2Carry {
		s, r, na := a.segs, a.retr, a.sil.noAck()
		delete(b.acc2, b.paths[j].Name())
		return s, r, na, true
	}
	return 0, 0, false, false
}
