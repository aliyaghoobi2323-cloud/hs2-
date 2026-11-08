package engine

import (
	"context"
	"math"
	"os"
	"sort"
	"strconv"
	"time"
)

// Multipath LB, stage 2 (mode "auto") and the slow rebalance (both modes).
//
// Quality. Every lbQualEvery the dialing side reads TCP_INFO of each path's
// live links and keeps, per path: the loss (retransmitted share of the data
// segments this side SENT), the queue (median srtt − minrtt over the links that
// moved at least qBusyBytes), and the share of dials that failed. In reverse
// the dialing side is the kharej exit, whose send direction is the users'
// download (kharej → iran), the direction that matters, so its own TCP_INFO
// sees the loss that counts. In direct the dialing side is the iran edge: its
// TCP_INFO sees only its own send direction (upload) and the queue; download
// loss is not visible here, so auto in direct steers by upload loss + queue.
//
// Score. goodness g = 1 / (1 + loss% / 2 + queue_ms / 50 + 4 × dial-fail
// share); a path's factor targets g / mean(g) over the paths with a reading,
// clamped to [lbMultMin, lbMultMax]. Hysteresis: a target within lbHyst of the
// current factor changes nothing. Rate: one step moves no path's effective
// share by more than lbMaxStepPct points (two steps a minute → ≤10 points per
// minute). A path with no reading keeps its factor. A dead path (dials keep
// failing) is cooled — weight 0 — and gets one probe dial every lbProbeEvery.
// Every live path keeps ≥2 links once the total allows it.
//
// Rebalance. Links are long-lived, so a path that came back (or whose
// effective weight rose) would only regain its share through growth or
// deaths. Every lbRebalEvery, if one path is ≥1 link over its share and
// another ≥1 under, ONE link of the over path that carries no user connection
// is closed; its replacement is then placed by largest remainder, i.e. on the
// path under its share. A link with users is never closed.

// lbHooks lets the balancer read a live link's TCP_INFO and retire it.
type lbHooks struct {
	stats func() (tcpStat, bool)
	// retire closes the link if it carries no user connection; it reports
	// whether it did.
	retire func() bool
	// mtr (may be nil) is the link's meter: bytes and users for the per-path
	// status view (lbview.go).
	mtr *linkMeter
}

// lbQual is one path's latest quality reading.
type lbQual struct {
	loss    float64 // retransmitted share of segments sent (smoothed)
	queueMs float64 // median srtt − minrtt of busy links, ms (smoothed)
	fail    float64 // failed share of the dials since the last reading
	have    bool    // loss has been read at least once
}

// Tunables (vars for tests; HS2_LB_FAST=1 divides the periods by 6 for the
// 60 s lab runs — never set it in production).
var (
	lbQualEvery  = 30 * time.Second
	lbRebalEvery = 60 * time.Second
	lbProbeEvery = 2 * time.Minute
	lbMultMin    = 0.5
	lbMultMax    = 1.5
	lbHyst       = 0.1
	lbMaxStepPct = 5.0
	lbMinSegs    = uint64(300) // segments sent on a path in a tick for a loss reading
)

func init() {
	if os.Getenv("HS2_LB_FAST") == "1" {
		lbQualEvery /= 6
		lbRebalEvery /= 6
		lbProbeEvery /= 6
	}
}

// SetAuto turns mode auto on (before the first dial).
func (b *LB) SetAuto(on bool) {
	b.mu.Lock()
	b.auto = on
	b.mu.Unlock()
}

func (b *LB) cooldownLocked() time.Duration {
	if b.auto {
		return lbProbeEvery
	}
	return lbCooldown
}

// attach registers a live link's hooks (after a successful dial).
func (b *LB) attach(link any, h lbHooks) {
	b.mu.Lock()
	if _, ok := b.held[link]; ok {
		b.hooks[link] = h
	}
	b.mu.Unlock()
}

// minTwoLocked returns an eligible weighted path with fewer than two links when
// the total (with this one) allows two per such path; else -1.
func (b *LB) minTwoLocked(ok func(int) bool) int {
	live, n := 0, 1
	for j, p := range b.paths {
		if ok(j) && p.Weight > 0 {
			live++
			n += b.links[j]
		}
	}
	if live < 2 || n < 2*live {
		return -1
	}
	best := -1
	for j, p := range b.paths {
		if ok(j) && p.Weight > 0 && b.links[j] < 2 && (best < 0 || b.links[j] < b.links[best]) {
			best = j
		}
	}
	return best
}

// Run drives the quality readings (auto) and the rebalance until ctx ends.
func (b *LB) Run(ctx context.Context) {
	qe, re := lbQualEvery, lbRebalEvery
	if lbAuto2 { // HS2_LB_AUTO2 (lb_auto2.go): the fast auto's periods
		qe, re = lb2QualEvery, lb2RebalEvery
	}
	q := time.NewTicker(qe)
	defer q.Stop()
	r := time.NewTicker(re)
	defer r.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-q.C:
			b.fillMin() // HS2_LB_AUTO2: every live path two links (lb_spare.go)
			if b.isAuto() {
				if rs := b.measure(qe); b.isAuto2() {
					b.adapt2(rs)
				} else {
					b.adapt()
				}
			}
		case <-r.C:
			if b.isAuto2() {
				b.rebalance2()
			} else {
				b.rebalance()
			}
		}
	}
}

func (b *LB) isAuto2() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.auto2On()
}

func (b *LB) isAuto() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.auto
}

// measure reads every live link's TCP_INFO and updates each path's quality;
// it returns this tick's raw readings (the fast auto judges them itself).
func (b *LB) measure(dt time.Duration) map[int]q2Reading {
	b.mu.Lock()
	type link struct {
		key  any
		path int
		f    func() (tcpStat, bool)
	}
	var ls []link
	for k, h := range b.hooks {
		if h.stats != nil {
			ls = append(ls, link{k, b.held[k], h.stats})
		}
	}
	b.mu.Unlock()
	// Read the sockets outside the lock.
	type read struct {
		link
		st tcpStat
	}
	var rs []read
	for _, l := range ls {
		if st, ok := l.f(); ok {
			rs = append(rs, read{l, st})
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(b.paths)
	segs, retr := make([]uint64, n), make([]uint64, n)
	delays := make([][]float64, n)
	for _, r := range rs {
		if _, ok := b.held[r.key]; !ok {
			continue // ended meanwhile
		}
		cur := qPrev{acked: r.st.bytesAcked, segsOut: uint64(r.st.segsOut), retrans: r.st.retrans}
		prev, seen := b.qprev[r.key]
		b.qprev[r.key] = cur
		if !seen || cur.acked < prev.acked || cur.segsOut < prev.segsOut || cur.retrans < prev.retrans {
			continue
		}
		segs[r.path] += cur.segsOut - prev.segsOut
		retr[r.path] += cur.retrans - prev.retrans
		if cur.acked-prev.acked >= qBusyBytes && r.st.minRttUs > 0 && r.st.rttUs >= r.st.minRttUs {
			delays[r.path] = append(delays[r.path], float64(r.st.rttUs-r.st.minRttUs)/1000)
		}
	}
	auto2 := b.auto2On()
	raw := map[int]q2Reading{}
	for j := range b.paths {
		if auto2 {
			if segs[j] >= lb2MinSegs {
				r := q2Reading{loss: float64(retr[j]) / float64(segs[j])}
				if d := delays[j]; len(d) > 0 {
					sort.Float64s(d)
					r.queueMs = d[len(d)/2]
				}
				if b.dials[j] > 0 {
					r.fail = float64(b.dfails[j]) / float64(b.dials[j])
				}
				raw[j] = r
			}
		} else {
			b.qual[j] = lbUpdateQual(b.qual[j], segs[j], retr[j], delays[j], b.dials[j], b.dfails[j])
		}
		b.dials[j], b.dfails[j] = 0, 0
	}
	return raw
}

// lbUpdateQual folds one tick's counts into a path's reading (half-weight
// smoothing; no reading leaves the old one).
func lbUpdateQual(q lbQual, segs, retr uint64, delays []float64, dials, fails int) lbQual {
	mix := func(old, v float64, first bool) float64 {
		if first {
			return v
		}
		return (old + v) / 2
	}
	if segs >= lbMinSegs {
		l := float64(retr) / float64(segs)
		q.loss = mix(q.loss, l, !q.have)
		if len(delays) > 0 {
			sort.Float64s(delays)
			q.queueMs = mix(q.queueMs, delays[len(delays)/2], !q.have)
		}
		q.have = true
	}
	if dials > 0 {
		q.fail = float64(fails) / float64(dials)
	} else {
		q.fail = 0
	}
	return q
}

// lbGoodness is a path's score (higher is better, 1 = perfect).
func lbGoodness(q lbQual) float64 {
	return 1 / (1 + 100*q.loss/2 + q.queueMs/50 + 4*q.fail)
}

// adapt moves the quality factors toward their targets (hysteresis, rate cap).
func (b *LB) adapt() {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	var gs []float64
	idx := []int{}
	for j := range b.paths {
		if b.qual[j].have && b.eligibleLocked(j, now) && b.paths[j].Weight > 0 {
			gs = append(gs, lbGoodness(b.qual[j]))
			idx = append(idx, j)
		}
	}
	if len(idx) < 2 {
		return // nothing to compare
	}
	mean := 0.0
	for _, g := range gs {
		mean += g
	}
	mean /= float64(len(gs))
	target := append([]float64(nil), b.mult...)
	moved := false
	for k, j := range idx {
		t := math.Max(lbMultMin, math.Min(lbMultMax, gs[k]/mean))
		if math.Abs(t-b.mult[j]) >= lbHyst {
			target[j] = t
			moved = true
		}
	}
	if !moved {
		return
	}
	next := lbLimitStep(b.paths, b.mult, target, lbMaxStepPct)
	old := b.mult
	b.mult = next
	var s string
	for j, p := range b.paths {
		if math.Abs(next[j]-old[j]) > 1e-9 {
			s += " " + p.Name() + " " + ftoa2(old[j]) + "→" + ftoa2(next[j]) +
				" (loss " + ftoa2(100*b.qual[j].loss) + "%, queue " + ftoa2(b.qual[j].queueMs) + "ms)"
		}
	}
	if s != "" {
		b.log("lb auto: weight factor%s", s)
	}
}

func ftoa2(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) }

// lbShares is each path's share (percent) of the weight sum under factors m.
func lbShares(paths []LBPath, m []float64) []float64 {
	tot := 0.0
	for j, p := range paths {
		tot += float64(p.Weight) * m[j]
	}
	out := make([]float64, len(paths))
	if tot == 0 {
		return out
	}
	for j, p := range paths {
		out[j] = 100 * float64(p.Weight) * m[j] / tot
	}
	return out
}

// lbLimitStep moves the factors from cur toward target by the largest
// fraction (halving) that changes no path's share by more than maxPct points.
func lbLimitStep(paths []LBPath, cur, target []float64, maxPct float64) []float64 {
	base := lbShares(paths, cur)
	f := 1.0
	for k := 0; k < 12; k++ {
		next := make([]float64, len(cur))
		for j := range cur {
			next[j] = cur[j] + f*(target[j]-cur[j])
		}
		sh := lbShares(paths, next)
		worst := 0.0
		for j := range sh {
			worst = math.Max(worst, math.Abs(sh[j]-base[j]))
		}
		if worst <= maxPct+1e-9 {
			return next
		}
		f /= 2
	}
	return append([]float64(nil), cur...)
}

// rebalance retires at most one idle link from the path furthest over its
// share when another path is at least one link under its share.
func (b *LB) rebalance() bool {
	b.mu.Lock()
	now := b.now()
	eff := b.effWeightsLocked()
	total, w := 0, 0
	for j := range b.paths {
		if b.eligibleLocked(j, now) {
			w += eff[j]
		}
		total += b.links[j]
	}
	if w == 0 || total < 2 {
		b.mu.Unlock()
		return false
	}
	over, under := -1, -1
	var overBy, underBy float64
	for j := range b.paths {
		share := 0.0
		if b.eligibleLocked(j, now) {
			share = float64(total) * float64(eff[j]) / float64(w)
		}
		d := float64(b.links[j]) - share
		if over < 0 || d > overBy {
			over, overBy = j, d
		}
		if b.eligibleLocked(j, now) && eff[j] > 0 && (under < 0 || -d > underBy) {
			under, underBy = j, -d
		}
	}
	if over < 0 || under < 0 || over == under || overBy < 1 || underBy < 1 {
		b.mu.Unlock()
		return false
	}
	if b.auto && b.links[over] <= 2 && b.eligibleLocked(over, now) {
		b.mu.Unlock()
		return false // keep two links on every live path
	}
	var cands []func() bool
	for k, i := range b.held {
		if i == over {
			if h, ok := b.hooks[k]; ok && h.retire != nil {
				cands = append(cands, h.retire)
			}
		}
	}
	name, uname := b.paths[over].Name(), b.paths[under].Name()
	b.mu.Unlock()
	for _, r := range cands {
		if r() {
			b.mu.Lock()
			b.rebal[over]++
			b.mu.Unlock()
			b.log("lb: rebalance — retired one idle link on %s (%.1f over its share) so its replacement goes to %s (%.1f under)", name, overBy, uname, underBy)
			return true
		}
	}
	return false
}
