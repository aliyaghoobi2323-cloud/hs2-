package engine

import (
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Multipath LB, fast auto (HS2_LB_AUTO2=1, off by default).
//
// The auto of lb_auto.go reads quality every 30 s, smooths it by halves, moves
// ≤5 share points a step and never below half a path's share, and rebalances
// by one idle link a minute: on the main tunnel users stayed for many minutes
// on paths resending 7% while others resent 0.3%. With the key on, both
// servers judge the paths the same way (q2Judge) and act on it, each with
// what it controls:
//
//   - The DIALING side (the kharej exit in reverse) places the links: a
//     reading every lb2QualEvery (5 s), loss smoothed fast toward worse and
//     slowly toward better; a path over lb2CutLoss in lb2CutRuns readings
//     running is cut to lb2Floor of its share at once (no step limit down;
//     up stays limited), others follow their goodness ratio to the best path
//     (no 0.5 floor), no path over lb2CapX × its configured share, and up to
//     lb2DrainBatch idle links a round leave the paths over their share while
//     the pool is whole (lb2DrainPerMin a minute at most).
//   - The EDGE (Iran) places the users: new user connections and mconn subs
//     go to a path in proportion to its weight (a cut path lb2SteerFloor), so
//     they avoid a bad path the moment it is judged, while the connections
//     already on it finish naturally. On a cut path only lb2Keep links take
//     new connections at all; the others empty and the exit retires them.
//     Light subs go to the best paths only.
//
// IP verdicts. A path is a (local IP, remote IP) pair. When every path of one
// IP (a row or column of the Iran × kharej matrix, two paths or more) resends
// over lb2CutLoss in the same reading while some other path does not, it is
// that IP: all its paths are cut at the first reading, not the second.
//
// Recovery. A cut path keeps its links (≥2 on the dialing side) and is read
// all the time; once its smoothed loss has stayed under lb2OKLoss for the
// hold (lb2Hold, doubled for each cut within lb2FlapWin of the last recovery,
// up to lb2HoldMax), the cut is lifted and it climbs back step by step. A cut
// path that gives no reading for lb2Quiet is lifted after its hold all the
// same (a probe): bad, it is cut again within two readings.
//
// If every path with a reading is lossy, nothing is cut: the shares are the
// configured ones (no path is better to move to).

var (
	lbAuto2 = os.Getenv("HS2_LB_AUTO2") == "1"
	// lbSteer: the edge's steering; follows HS2_LB_AUTO2 unless
	// HS2_LB_STEER says otherwise (lab ablation).
	lbSteer = envBoolDefault("HS2_LB_STEER", lbAuto2)
)

func envBoolDefault(k string, def bool) bool {
	switch os.Getenv(k) {
	case "1":
		return true
	case "0":
		return false
	}
	return def
}

// Tunables (vars for tests).
var (
	lb2QualEvery   = 5 * time.Second
	lb2RebalEvery  = 10 * time.Second
	lb2Floor       = 0.1  // dialing side: a cut path's factor (of its share)
	lb2SteerFloor  = 0.05 // edge: a cut path's weight for new connections
	lb2CutLoss     = 0.03
	lb2CutRuns     = 2
	lb2OKLoss      = 0.015
	lb2Hold        = 30 * time.Second
	lb2HoldMax     = 5 * time.Minute
	lb2FlapWin     = 10 * time.Minute
	lb2Quiet       = 30 * time.Second
	lb2Dead        = 0.75 // goodness ratio at or above which a path is as good as the best (0.9% vs 0.4% loss: 0.83 — the load moved onto a path raises its loss a little; that must not push it back)
	lb2CapX        = 3.0
	lb2RiseStep    = 0.1 // edge: weight a reading may rise
	lb2DrainBatch  = 2
	lb2DrainPerMin = 12
	lb2Keep        = 2
	lb2MinSegs     = uint64(200) // segments a reading needs (per path, per direction)
	// Smoothing, per second: toward worse fast, toward better slowly.
	lb2AlphaUp   = 0.3
	lb2AlphaDown = 0.03
)

func init() {
	if v, err := strconv.ParseFloat(os.Getenv("HS2_LB_CAPX"), 64); err == nil && v >= 1 {
		lb2CapX = v
	}
	if lbSteer {
		lbAdvertEvery = lb2QualEvery // the edge reads the dialing side's report this often
	}
}

// q2Reading is one path's reading for a tick.
type q2Reading struct {
	loss    float64 // resent share
	queueMs float64
	fail    float64
}

// q2Path is one path's judged state.
type q2Path struct {
	ewma     float64
	queueMs  float64
	fail     float64
	have     bool
	raw      float64 // the latest reading
	bad      int     // readings running over lb2CutLoss
	cut      bool
	cutWhy   string
	cutAt    time.Time
	okSince  time.Time
	lastRead time.Time
	hold     time.Duration
	uncutAt  time.Time
	factor   float64 // the edge's applied weight (the dialing side keeps its own)
	target   float64
	reason   string
}

// q2Judge judges a set of paths named "local>remote".
type q2Judge struct {
	p      map[string]*q2Path
	floor  float64
	localN string // what the local / remote IPs are, for the verdicts ("Iran", "kharej")
	remN   string
	allBad bool
	// verdicts: the IP-level verdicts of the last reading that had one, kept
	// while their paths stay cut.
	verdicts map[string]string // IP -> sentence
	log      func(string, ...any)
}

func newQ2Judge(floor float64, localN, remN string, logf func(string, ...any)) *q2Judge {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &q2Judge{p: map[string]*q2Path{}, floor: floor, localN: localN, remN: remN, verdicts: map[string]string{}, log: logf}
}

func (j *q2Judge) path(k string) *q2Path {
	q := j.p[k]
	if q == nil {
		q = &q2Path{factor: 1, target: 1}
		j.p[k] = q
	}
	return q
}

// forget drops the paths not in keep.
func (j *q2Judge) forget(keep map[string]bool) {
	for k := range j.p {
		if !keep[k] {
			delete(j.p, k)
		}
	}
}

func q2Alpha(perSec float64, dt time.Duration) float64 {
	return 1 - math.Pow(1-perSec, math.Max(dt.Seconds(), 0.1))
}

// update folds one tick's readings (paths absent: no reading) and sets every
// known path's target factor and reason.
func (j *q2Judge) update(now time.Time, dt time.Duration, rs map[string]q2Reading) {
	// A reading is "bad" over lb2CutLoss AND well above the best path read
	// this tick (2× + 1 point): loss every path shares (a full queue, the
	// whole border) is not one path's fault, and moving users would not help.
	best := math.Inf(1)
	for _, r := range rs {
		best = math.Min(best, r.loss)
	}
	isBad := func(l float64) bool {
		return l > lb2CutLoss && (len(rs) < 2 || l > 2*best+0.01)
	}
	for k, r := range rs {
		q := j.path(k)
		q.raw = r.loss
		dt := dt
		if q.have { // readings may come less often than the ticks (a quiet path, the report)
			dt = min(max(now.Sub(q.lastRead), dt), 10*time.Second)
		}
		if !q.have {
			q.ewma, q.queueMs = r.loss, r.queueMs
		} else {
			a := q2Alpha(lb2AlphaDown, dt)
			if r.loss > q.ewma {
				a = q2Alpha(lb2AlphaUp, dt)
			}
			q.ewma += a * (r.loss - q.ewma)
			a = q2Alpha(lb2AlphaDown, dt)
			if r.queueMs > q.queueMs {
				a = q2Alpha(lb2AlphaUp, dt)
			}
			q.queueMs += a * (r.queueMs - q.queueMs)
		}
		q.fail = r.fail
		q.have, q.lastRead = true, now
		if isBad(r.loss) {
			q.bad++
		} else {
			q.bad = 0
		}
	}
	// IP verdicts: every path of one IP over the bar in this reading, while
	// some path read this tick is not.
	ipBad := map[string]string{}
	anyGood := false
	for _, r := range rs {
		if !isBad(r.loss) {
			anyGood = true
		}
	}
	if anyGood {
		type grp struct{ n, bad int }
		loc, rem := map[string]*grp{}, map[string]*grp{}
		for k, r := range rs {
			l, re := pathIPs(k)
			for _, x := range []struct {
				m  map[string]*grp
				ip string
			}{{loc, l}, {rem, re}} {
				g := x.m[x.ip]
				if g == nil {
					g = &grp{}
					x.m[x.ip] = g
				}
				g.n++
				if isBad(r.loss) {
					g.bad++
				}
			}
		}
		// Only IPs whose every known path was read (a path not read this
		// tick could be the good one).
		known := func(ip string, local bool) int {
			n := 0
			for k := range j.p {
				l, re := pathIPs(k)
				if local && l == ip || !local && re == ip {
					n++
				}
			}
			return n
		}
		for ip, g := range loc {
			if g.n >= 2 && g.bad == g.n && known(ip, true) == g.n {
				ipBad[ip] = fmt.Sprintf("%s IP %s lossy on every %s IP", j.localN, ip, j.remN)
			}
		}
		for ip, g := range rem {
			if g.n >= 2 && g.bad == g.n && known(ip, false) == g.n {
				ipBad[ip] = fmt.Sprintf("%s IP %s lossy on every %s IP", j.remN, ip, j.localN)
			}
		}
	}
	for ip, s := range ipBad {
		if j.verdicts[ip] == "" {
			j.log("lb auto2: %s — its paths are cut together", s)
		}
		j.verdicts[ip] = s
	}
	// Cuts and recoveries.
	for k, q := range j.p {
		if !q.have {
			continue
		}
		l, re := pathIPs(k)
		why := ""
		if s := ipBad[l]; s != "" {
			why = s
		} else if s := ipBad[re]; s != "" {
			why = s
		}
		if !q.cut && (q.bad >= lb2CutRuns || why != "") {
			q.cut, q.cutAt, q.okSince = true, now, time.Time{}
			if why == "" {
				why = fmt.Sprintf("loss %.1f%%", 100*q.raw)
			}
			q.cutWhy = why
			if !q.uncutAt.IsZero() && now.Sub(q.uncutAt) < lb2FlapWin && q.hold > 0 {
				q.hold = min(2*q.hold, lb2HoldMax)
			} else {
				q.hold = lb2Hold
			}
			j.log("lb auto2: path %s cut (%s); recovery after %s under %.1f%%", k, why, fmtDur(q.hold), 100*lb2OKLoss)
			continue
		}
		if !q.cut {
			continue
		}
		if q.ewma < lb2OKLoss && q.lastRead.Equal(now) {
			if q.okSince.IsZero() {
				q.okSince = now
			}
		} else if q.lastRead.Equal(now) {
			q.okSince = time.Time{}
		}
		quiet := now.Sub(q.lastRead) >= lb2Quiet && now.Sub(q.cutAt) >= q.hold
		if (!q.okSince.IsZero() && now.Sub(q.okSince) >= q.hold) || quiet {
			q.cut, q.uncutAt, q.bad = false, now, 0
			what := "loss back under " + ftoa1(100*lb2OKLoss) + "%"
			if quiet {
				what = "no reading for " + fmtDur(now.Sub(q.lastRead)) + ": probing"
			}
			j.log("lb auto2: path %s recovering (%s) — its share comes back step by step", k, what)
		}
	}
	for ip := range j.verdicts {
		still := false
		for k, q := range j.p {
			l, re := pathIPs(k)
			if (l == ip || re == ip) && q.cut {
				still = true
			}
		}
		if !still {
			delete(j.verdicts, ip)
		}
	}
	j.targets()
}

func ftoa1(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) }

func q2Goodness(q *q2Path) float64 {
	return 1 / (1 + 100*q.ewma/2 + q.queueMs/50 + 4*q.fail)
}

// targets sets every path's target factor and reason from the judged state.
func (j *q2Judge) targets() {
	gmax, nHave, nBad := 0.0, 0, 0
	for _, q := range j.p {
		if !q.have {
			continue
		}
		nHave++
		if q.cut || q.ewma > lb2CutLoss {
			nBad++
			continue
		}
		gmax = math.Max(gmax, q2Goodness(q))
	}
	j.allBad = nHave >= 1 && nBad == nHave
	for _, q := range j.p {
		switch {
		case !q.have:
			q.target, q.reason = 1, "no reading yet"
		case j.allBad:
			q.target, q.reason = 1, "all paths lossy: shares as set"
		case q.cut:
			q.target, q.reason = j.floor, "cut: "+q.cutWhy
		default:
			r := 1.0
			if gmax > 0 {
				r = q2Goodness(q) / gmax
			}
			if r >= lb2Dead {
				q.target, q.reason = 1, "ok"
			} else {
				q.target = math.Max(j.floor, r*r)
				q.reason = fmt.Sprintf("lower: loss %.1f%%", 100*q.ewma)
				if q.queueMs > 25 {
					q.reason += fmt.Sprintf(", queue %.0fms", q.queueMs)
				}
			}
		}
	}
}

// stepEdge moves the edge's applied weights toward the targets: down at once,
// up by lb2RiseStep a reading.
func (j *q2Judge) stepEdge() {
	for _, q := range j.p {
		if q.target < q.factor {
			q.factor = q.target
		} else {
			q.factor = math.Min(q.target, q.factor+lb2RiseStep)
		}
		if q.factor < q.target && !q.cut && q.have && !j.allBad {
			q.reason = "recovering"
		}
	}
}

// verdictList is the IP verdicts, sorted.
func (j *q2Judge) verdictList() []string {
	var out []string
	for _, s := range j.verdicts {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// ---- dialing side (LB) -------------------------------------------------------

// SetSides names this side's and the other side's IPs for the verdicts
// ("Iran", "kharej").
func (b *LB) SetSides(local, remote string) {
	b.mu.Lock()
	b.localN, b.remN = local, remote
	b.mu.Unlock()
}

func (b *LB) auto2On() bool { return lbAuto2 && b.auto }

// adapt2 is adapt with the fast judge. raw: this tick's per-path readings.
func (b *LB) adapt2(rs map[int]q2Reading) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if b.judge == nil {
		ln, rn := b.localN, b.remN
		if ln == "" {
			ln, rn = "local", "remote"
		}
		b.judge = newQ2Judge(lb2Floor, ln, rn, b.log)
	}
	keep := map[string]bool{}
	in := map[string]q2Reading{}
	for j, p := range b.paths {
		if p.Weight <= 0 || !b.eligibleLocked(j, now) {
			continue
		}
		keep[p.Name()] = true
		if r, ok := rs[j]; ok {
			in[p.Name()] = r
		}
	}
	b.judge.forget(keep)
	b.judge.update(now, lb2QualEvery, in)
	b.judgeAt = now
	target := append([]float64(nil), b.mult...)
	for j, p := range b.paths {
		if q := b.judge.p[p.Name()]; q != nil {
			target[j] = q.target
			b.qual[j].loss, b.qual[j].queueMs, b.qual[j].have = q.ewma, q.queueMs, q.have
		}
	}
	target = lb2Cap(b.paths, target, keep)
	// Down at once, up by lbMaxStepPct share points a reading.
	down := append([]float64(nil), b.mult...)
	for j := range down {
		if target[j] < down[j]-1e-9 {
			down[j] = target[j]
		}
	}
	up := append([]float64(nil), target...)
	for j := range up {
		if up[j] < down[j] {
			up[j] = down[j]
		}
		if math.Abs(up[j]-down[j]) < 0.05 && up[j] != 1 {
			up[j] = down[j] // hysteresis on the way up
		}
	}
	next := lbLimitStep(b.paths, down, up, lbMaxStepPct)
	old := b.mult
	b.mult = next
	var s string
	for j, p := range b.paths {
		if math.Abs(next[j]-old[j]) > 0.02 {
			why := ""
			if q := b.judge.p[p.Name()]; q != nil {
				why = " (" + q.reason + ")"
			}
			s += " " + p.Name() + " " + ftoa2(old[j]) + "→" + ftoa2(next[j]) + why
		}
	}
	if s != "" {
		b.log("lb auto2: weight factor%s", s)
	}
}

// lb2Cap lowers the factors so no path's share exceeds lb2CapX × its
// configured share (among the paths in live).
func lb2Cap(paths []LBPath, m []float64, live map[string]bool) []float64 {
	out := append([]float64(nil), m...)
	wAll := 0
	for _, p := range paths {
		if live[p.Name()] {
			wAll += p.Weight
		}
	}
	if wAll == 0 {
		return out
	}
	for it := 0; it < 8; it++ {
		tot := 0.0
		for j, p := range paths {
			if live[p.Name()] {
				tot += float64(p.Weight) * out[j]
			}
		}
		if tot == 0 {
			return out
		}
		moved := false
		for j, p := range paths {
			if !live[p.Name()] || p.Weight == 0 {
				continue
			}
			cfg := float64(p.Weight) / float64(wAll)
			capS := math.Min(1, lb2CapX*cfg)
			sh := float64(p.Weight) * out[j] / tot
			if sh > capS+1e-6 && capS < 1 {
				// share s = w m / (w m + rest): solve for the m giving capS
				rest := tot - float64(p.Weight)*out[j]
				out[j] = capS * rest / ((1 - capS) * float64(p.Weight))
				moved = true
			}
		}
		if !moved {
			break
		}
	}
	return out
}

// rebalance2 retires up to lb2DrainBatch idle links from the paths furthest
// over their share, while the pool is whole: the links (dials in flight
// included) are back to what they were at the last retire, or 30 s passed.
func (b *LB) rebalance2() int {
	b.mu.Lock()
	now := b.now()
	total := 0
	for j := range b.paths {
		total += b.links[j]
	}
	cut := now.Add(-time.Minute)
	kept := b.drained[:0]
	for _, t := range b.drained {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	b.drained = kept
	if total < 2 || (total < b.drainFloor && now.Sub(b.drainAt) < 30*time.Second) || len(b.drained) >= lb2DrainPerMin {
		b.mu.Unlock()
		return 0
	}
	eff := b.effWeightsLocked()
	w := 0
	for j := range b.paths {
		if b.eligibleLocked(j, now) {
			w += eff[j]
		}
	}
	if w == 0 {
		b.mu.Unlock()
		return 0
	}
	links := append([]int(nil), b.links...)
	type cand struct {
		path   int
		retire func() bool
	}
	byPath := map[int][]func() bool{}
	for k, i := range b.held {
		if h, ok := b.hooks[k]; ok && h.retire != nil {
			byPath[i] = append(byPath[i], h.retire)
		}
	}
	room := min(lb2DrainBatch, lb2DrainPerMin-len(b.drained))
	var picks []cand
	for len(picks) < room {
		over, under := -1, -1
		var overBy, underBy float64
		for j := range b.paths {
			share := 0.0
			if b.eligibleLocked(j, now) {
				share = float64(total) * float64(eff[j]) / float64(w)
			}
			d := float64(links[j]) - share
			if len(byPath[j]) > 0 && (over < 0 || d > overBy) {
				over, overBy = j, d
			}
			if b.eligibleLocked(j, now) && eff[j] > 0 && (under < 0 || -d > underBy) {
				under, underBy = j, -d
			}
		}
		if over < 0 || under < 0 || over == under || overBy < 1 || underBy < 1 {
			break
		}
		if links[over] <= lb2Keep && b.eligibleLocked(over, now) {
			break // keep two links on every live path
		}
		rs := byPath[over]
		picks = append(picks, cand{over, func() bool {
			for _, r := range rs {
				if r() {
					return true
				}
			}
			return false
		}})
		links[over]--
		byPath[over] = nil // one per path a round: its retire funcs are tried in turn
	}
	b.mu.Unlock()
	n := 0
	var names []string
	for _, c := range picks {
		if c.retire() {
			n++
			b.mu.Lock()
			b.rebal[c.path]++
			names = append(names, b.paths[c.path].Name())
			b.mu.Unlock()
		}
	}
	if n > 0 {
		b.mu.Lock()
		b.drainFloor, b.drainAt = total, now
		for i := 0; i < n; i++ {
			b.drained = append(b.drained, now)
		}
		b.mu.Unlock()
		b.log("lb auto2: rebalance — retired %d idle link(s) on %s so their replacements go to the paths under their share", n, strings.Join(names, ", "))
	}
	return n
}

// auto2Stat fills a path's auto columns (factor, reason). Caller holds b.mu.
func (b *LB) auto2StatLocked(j int, s *LBPathStat) {
	s.AutoFactor = round2(b.mult[j])
	if b.judge != nil {
		if q := b.judge.p[b.paths[j].Name()]; q != nil {
			s.AutoReason = q.reason
			if !q.cut && b.mult[j] < q.target-0.02 && !b.judge.allBad {
				s.AutoReason = "recovering"
			}
		}
	}
}

// Verdicts is the IP-level verdicts of the fast auto (nil: none / off).
func (b *LB) Verdicts() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.judge == nil {
		return nil
	}
	return b.judge.verdictList()
}

// ---- edge: steering new connections -------------------------------------------

// lbSteerState is the edge's judge and per-tick counters (HS2_LB_STEER).
type lbSteerState struct {
	judge *q2Judge
	// this tick's counts per path: download resent / segments, upload too
	dnRt, dnSegs, upRt, upSegs map[string]uint64
	active                     bool      // ≥2 paths known and some weight under 1
	rep                        *LBReport // the dialing side's latest report
	repUsed                    time.Time
	logs                       []string
}

func newSteerState() *lbSteerState {
	st := &lbSteerState{}
	st.judge = newQ2Judge(lb2SteerFloor, "Iran", "kharej", func(f string, a ...any) {
		st.logs = append(st.logs, fmt.Sprintf(f, a...))
	})
	return st
}

// takeLogs returns the judge's lines since the last call (logged by the
// caller once the pool lock is released).
func (st *lbSteerState) takeLogs() []string {
	l := st.logs
	st.logs = nil
	return l
}

// steerLink is a link's steering counters (the previous readings).
type steerLink struct {
	primed   bool
	peer     peerLossRec
	retrans  uint64
	segsOut  uint32
	off      bool // a cut path's link beyond the lb2Keep that take new connections
	picksAll uint64
}

// steerNote adds one link's counters since its last tick. Caller holds m.mu.
func (st *lbSteerState) note(ml *managedLink, o *linkObs) {
	path := mlPath(ml)
	if path == "" || ml.mtr == nil {
		return
	}
	s := &ml.steer
	if st.dnRt == nil {
		st.dnRt, st.dnSegs, st.upRt, st.upSegs = map[string]uint64{}, map[string]uint64{}, map[string]uint64{}, map[string]uint64{}
	}
	if !s.primed {
		s.primed = true
		if o.peerLoss != nil {
			s.peer = *o.peerLoss
		}
		s.retrans, s.segsOut = o.ts.retrans, o.ts.segsOut
		return
	}
	if r := o.peerLoss; r != nil && r.n != s.peer.n {
		p := s.peer
		if p.n != 0 && r.n > p.n && r.rt >= p.rt && r.rd >= p.rd {
			// Segments: what came in, or bytes / mss when more (GRO merges
			// segments before TCP counts them, which read 2× the sender's loss).
			dR := r.rt - p.rt
			segs := uint64(float64(r.rd-p.rd) / mss)
			if r.segsIn != 0 && p.segsIn != 0 {
				segs = max(segs, uint64(r.segsIn-p.segsIn))
			}
			st.dnRt[path] += dR
			st.dnSegs[path] += segs + dR
		}
		s.peer = *r
	}
	if o.tsOK {
		if o.ts.retrans >= s.retrans && o.ts.segsOut != 0 && s.segsOut != 0 {
			st.upRt[path] += o.ts.retrans - s.retrans
			st.upSegs[path] += uint64(o.ts.segsOut - s.segsOut)
		}
		s.retrans, s.segsOut = o.ts.retrans, o.ts.segsOut
	}
}

// tick judges the paths from this tick's counts and marks the links of cut
// paths beyond the lb2Keep that keep taking new connections. Caller holds m.mu.
func (m *LinkManager) steerTickLocked(now time.Time, dt time.Duration) {
	st := m.steer
	rs := map[string]q2Reading{}
	frac := func(rt, segs uint64) (float64, bool) {
		if segs < lb2MinSegs {
			return 0, false
		}
		return float64(rt) / float64(segs), true
	}
	for p := range st.dnSegs {
		d, okD := frac(st.dnRt[p], st.dnSegs[p])
		u, okU := frac(st.upRt[p], st.upSegs[p])
		switch {
		case okD && okU:
			rs[p] = q2Reading{loss: math.Max(d, u)}
		case okD:
			rs[p] = q2Reading{loss: d}
		case okU:
			rs[p] = q2Reading{loss: u}
		}
	}
	for p := range st.upSegs {
		if _, ok := rs[p]; !ok {
			if u, ok := frac(st.upRt[p], st.upSegs[p]); ok {
				rs[p] = q2Reading{loss: u}
			}
		}
	}
	st.dnRt, st.dnSegs, st.upRt, st.upSegs = nil, nil, nil, nil
	// The dialing side's report (lb_report.go), when fresh, replaces this
	// side's own estimate for the paths it covers: its loss is the sender's
	// own count, and its cuts are taken at once (both sides then agree).
	forced := map[string]string{}
	if r := st.rep; r != nil && now.Sub(r.At) <= 20*time.Second {
		fresh := r.At.After(st.repUsed)
		st.repUsed = r.At
		for _, p := range r.Paths {
			if p.Local == "" {
				continue
			}
			k := p.Remote + ">" + p.Local
			delete(rs, k)
			if !fresh {
				continue
			}
			if p.raw {
				rs[k] = q2Reading{loss: p.RawPct / 100}
			}
			if p.Cut {
				forced[k] = p.Reason
			}
		}
	}
	alive := map[string]bool{}
	byPath := map[string][]*managedLink{}
	for _, ml := range m.links {
		if p := mlPath(ml); p != "" && ml.link.Alive() {
			alive[p] = true
			byPath[p] = append(byPath[p], ml)
		}
	}
	st.judge.forget(alive)
	for p := range alive {
		st.judge.path(p)
	}
	st.judge.update(now, dt, rs)
	for k, why := range forced {
		if alive[k] {
			st.judge.forceCut(k, why, now)
		}
	}
	st.judge.stepEdge()
	st.active = false
	if len(alive) >= 2 {
		for _, q := range st.judge.p {
			if q.factor < 0.999 {
				st.active = true
			}
		}
	}
	for p, ls := range byPath {
		q := st.judge.p[p]
		cut := st.active && q != nil && q.cut
		// The keepers: the lb2Keep serving links that take new connections on
		// a cut path — those kept before first (stable), then the ones with
		// the most users (they take longest to empty), then the oldest.
		sort.Slice(ls, func(a, b int) bool {
			if ls[a].steer.off != ls[b].steer.off {
				return !ls[a].steer.off
			}
			ua, ub := ls[a].users.Load(), ls[b].users.Load()
			if ua != ub {
				return ua > ub
			}
			return ls[a].id < ls[b].id
		})
		kept := 0
		for _, ml := range ls {
			if cut && ml.serving() && kept < lb2Keep {
				kept++
				ml.steer.off = false
				continue
			}
			ml.steer.off = cut
		}
	}
}

// steerWeightLocked is a link's path weight for new connections (1 = full).
func (m *LinkManager) steerWeightLocked(ml *managedLink) float64 {
	if m.steer == nil || !m.steer.active {
		return 1
	}
	if q := m.steer.judge.p[mlPath(ml)]; q != nil {
		return q.factor
	}
	return 1
}

// steerDrawLocked draws the path a new connection should go to, in proportion
// to weight × candidate links (light: among the best paths only); "" when
// steering is off or not needed. cand says which links may take it.
func (m *LinkManager) steerDrawLocked(light bool, cand func(*managedLink) bool) string {
	if m.steer == nil || !m.steer.active {
		return ""
	}
	cnt := map[string]int{}
	for _, ml := range m.links {
		if p := mlPath(ml); p != "" && !ml.steer.off && cand(ml) {
			cnt[p]++
		}
	}
	if len(cnt) < 2 {
		return ""
	}
	best := 0.0
	for p := range cnt {
		best = math.Max(best, m.steerFactor(p))
	}
	keys := make([]string, 0, len(cnt))
	for p := range cnt {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	tot := 0.0
	ws := make([]float64, len(keys))
	for i, p := range keys {
		f := m.steerFactor(p)
		if light && f < 0.9*best {
			continue
		}
		ws[i] = f * float64(cnt[p])
		tot += ws[i]
	}
	if tot <= 0 {
		return ""
	}
	x := rand.Float64() * tot
	for i, p := range keys {
		if x < ws[i] {
			return p
		}
		x -= ws[i]
	}
	return keys[len(keys)-1]
}

func (m *LinkManager) steerFactor(p string) float64 {
	if q := m.steer.judge.p[p]; q != nil {
		return q.factor
	}
	return 1
}

// steerRankLocked is a candidate's place in the order for a new connection
// (pickKey.path): 0 on the drawn path, 1 on another path not cut, 2 on a cut
// path, 3 turned off (a cut path's link beyond the keepers).
func (m *LinkManager) steerRankLocked(ml *managedLink, drawn string) uint8 {
	if m.steer == nil || !m.steer.active || drawn == "" {
		if ml.steer.off && m.steer != nil && m.steer.active {
			return 3
		}
		return 0
	}
	switch p := mlPath(ml); {
	case ml.steer.off:
		return 3
	case p == drawn:
		return 0
	case m.steerFactor(p) >= 0.5:
		return 1
	}
	return 2
}

// SteerRows is the edge's per-path auto columns: weight and reason by the
// full "local>remote" path, and the IP verdicts.
func (m *LinkManager) steerRows() (map[string]float64, map[string]string, []string) {
	if m.steer == nil {
		return nil, nil, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	f, r := map[string]float64{}, map[string]string{}
	for p, q := range m.steer.judge.p {
		f[p] = round2(q.factor)
		r[p] = q.reason
	}
	return f, r, m.steer.judge.verdictList()
}

// SteerReport hands the edge the dialing side's latest report.
func (m *LinkManager) SteerReport(r LBReport) {
	if m.steer == nil {
		return
	}
	m.mu.Lock()
	m.steer.rep = &r
	m.mu.Unlock()
}

// forceCut cuts path k now (the dialing side cut it: why is its reason).
func (j *q2Judge) forceCut(k, why string, now time.Time) {
	q := j.path(k)
	if q.cut {
		return
	}
	q.cut, q.cutAt, q.okSince, q.have = true, now, time.Time{}, true
	if q.lastRead.IsZero() {
		q.lastRead = now
	}
	why = strings.TrimPrefix(why, "cut: ")
	if why == "" {
		why = "the dialing side cut it"
	}
	q.cutWhy = why
	if !q.uncutAt.IsZero() && now.Sub(q.uncutAt) < lb2FlapWin && q.hold > 0 {
		q.hold = min(2*q.hold, lb2HoldMax)
	} else {
		q.hold = lb2Hold
	}
	j.log("lb auto2: path %s cut (%s, as the dialing side judged)", k, why)
	j.targets()
}
func LBAuto2On() bool { return lbAuto2 } // HS2_LB_AUTO2 is set
