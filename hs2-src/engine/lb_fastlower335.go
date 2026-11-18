package engine

import (
	"fmt"
	"math"
	"sort"
	"sync/atomic"
	"time"
)

// Fast lower of a lossy IP (HS2_B335_FASTLOWER=1, edge, off by default).
//
// Production (tunnel 1, 2026-11-18 12:56): a 12 % loss burst on one Iran IP
// stalled user streams at once, but the LB lowered that IP's paths 10 s
// later and cut them at 15 s: the dialing side reads every lb2QualEvery
// (5 s), the edge takes its word from a report it fetches every 5 s and
// judges on its own 2 s health tick. New users kept landing on the bad IP
// for 10–15 s.
//
// With the key on, the edge reads the counts it already has every health
// tick (healthTick, 2 s): per path, the download resent share the exit's
// control pongs report and the upload resent share of its own TCP_INFO, the
// same counts its judge sums — but over one tick, with a smaller sample
// (flMinSegs). An IP (a row or column of the path matrix) is HOT in a
// window when at least two of its paths with a sample — and at least
// flAgree of them — read over lb2CutLoss AND over 2× the best path of the
// other IPs + 1 point, while some path of another IP is not bad (loss
// every path shares is not one IP's fault). flNeed windows hot in a row
// lower the IP: its paths take lb2SteerFloor of their weight for new
// connections, light connections go elsewhere, and the rescue
// (HS2_LB_RESCUE) treats them as lowered paths for the light users already
// there. Nothing is cut: the cut still waits for the dialing side's slower
// confirmation, and its flap penalty is unchanged.
//
// Hysteresis: a lowered IP is lifted after flCalm windows in a row with no
// path of it over lb2CutLoss, and not before flMinHold. Flap guard: an IP
// lowered flFlapN times within lb2FlapWin needs flNeedFlap windows next
// time. One noisy window never lowers (flNeed ≥ 2).
var b335FastLower bool // loadEnvKeys: HS2_B335_FASTLOWER

var (
	flMinSegs  = uint64(60) // a path's segments in a window for a reading
	flAgree    = 0.6        // share of an IP's read paths that must be bad
	flNeed     = 2          // hot windows in a row to lower
	flNeedFlap = 3          // ... after flFlapN lowers within lb2FlapWin
	flFlapN    = 2
	flCalm     = 3 // calm windows in a row to lift
	flMinHold  = 10 * time.Second
)

// Counters (lab and status).
var (
	flLowers   atomic.Int64 // IPs lowered
	flLifts    atomic.Int64 // IPs lifted
	flHotWin   atomic.Int64 // hot windows seen (lowering or not)
	flLowTicks atomic.Int64 // windows with some IP lowered
)

// FastLowerCounters is lowers, lifts, hot windows, windows with an IP lowered.
func FastLowerCounters() (int64, int64, int64, int64) {
	return flLowers.Load(), flLifts.Load(), flHotWin.Load(), flLowTicks.Load()
}

type flIP struct {
	hotRuns  int
	calmRuns int
	lowered  bool
	lowAt    time.Time
	lowers   []time.Time
}

// fastLowerDet is the per-IP detector.
type fastLowerDet struct {
	ip  map[string]*flIP
	log func(string, ...any)
	// logOnly (HS2_B337_FL_LOGONLY=1, lb_fl337.go): the verdicts are
	// computed and logged, lowered() never says yes.
	logOnly bool
	// stallOnly (the dialing side, lb_fl337.go): only a stall-hot window
	// lowers; its own judge already acts on the resent counts.
	stallOnly bool
}

func newFastLowerDet(logf func(string, ...any)) *fastLowerDet {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &fastLowerDet{ip: map[string]*flIP{}, log: logf, logOnly: flLogOnly}
}

// lowered says whether path k ("a>b") is on a lowered IP.
func (d *fastLowerDet) lowered(k string) bool {
	if d == nil || d.logOnly {
		return false
	}
	l, r := pathIPs(k)
	if s := d.ip[l]; s != nil && s.lowered {
		return true
	}
	if s := d.ip[r]; s != nil && s.lowered {
		return true
	}
	return false
}

// flRead is one path's window: resent and segments.
type flRead struct{ rt, segs uint64 }

// step folds one window's counts (all live paths in alive; a path without
// enough segments has no reading).
func (d *fastLowerDet) step(now time.Time, counts map[string]flRead, alive map[string]bool) {
	d.stepStall(now, counts, alive, nil)
}

// stepStall is step with this window's stall events (b336, lb_flstall336.go).
func (d *fastLowerDet) stepStall(now time.Time, counts map[string]flRead, alive map[string]bool, stalls []flStallEv) {
	sHot := flStallHot(counts, alive, stalls)
	loss := map[string]float64{}
	for k, c := range counts {
		if alive[k] && c.segs >= flMinSegs {
			loss[k] = float64(c.rt) / float64(c.segs)
		}
	}
	// IPs and their live paths.
	ips := map[string][]string{}
	for k := range alive {
		l, r := pathIPs(k)
		ips[l] = append(ips[l], k)
		if r != "" {
			ips[r] = append(ips[r], k)
		}
	}
	names := make([]string, 0, len(ips))
	for ip := range ips {
		names = append(names, ip)
	}
	sort.Strings(names)
	for _, ip := range names {
		ks := ips[ip]
		s := d.ip[ip]
		if s == nil {
			s = &flIP{}
			d.ip[ip] = s
		}
		mine := map[string]bool{}
		for _, k := range ks {
			mine[k] = true
		}
		// The best of the other IPs' paths, and whether one is not bad.
		best := math.Inf(1)
		for k, l := range loss {
			if !mine[k] {
				best = math.Min(best, l)
			}
		}
		read, bad, over := 0, 0, 0
		worst := 0.0
		for _, k := range ks {
			l, ok := loss[k]
			if !ok {
				// b336: a small sample over the bar is not calm (a lowered IP
				// carries little; lab b336 r1: lifted mid-burst on no readings)
				if c := counts[k]; b336CalmSmall && alive[k] && c.segs >= flStallSegs && float64(c.rt) > lb2CutLoss*float64(c.segs) {
					over++
				}
				continue
			}
			read++
			if l > lb2CutLoss {
				over++
			}
			if l > lb2CutLoss && !math.IsInf(best, 1) && l > 2*best+0.01 {
				bad++
				worst = math.Max(worst, l)
			}
		}
		hot := !d.stallOnly && len(ks) >= 2 && bad >= 2 && float64(bad) >= flAgree*float64(read) && !math.IsInf(best, 1) && best <= lb2CutLoss
		why := ""
		if sh := sHot[ip]; sh != "" {
			// b336: stalls from several connections and links of this IP,
			// corroborated by its own resent counts (flStallHot)
			why, hot = sh, true
			flStallWins.Add(1)
		}
		if hot {
			flHotWin.Add(1)
			s.hotRuns++
			s.calmRuns = 0
		} else {
			s.hotRuns = 0
			// Calm: no path of it over the bar. A window without a reading
			// counts too: a lowered (or cut) IP carries little, and waiting
			// for readings would keep it lowered for good (lab b335 r1/r2);
			// the dialing side's slower judge still holds a real cut.
			if over == 0 {
				s.calmRuns++
			} else {
				s.calmRuns = 0
			}
		}
		// Flap guard: lowers within lb2FlapWin.
		recent := s.lowers[:0]
		for _, t := range s.lowers {
			if now.Sub(t) < lb2FlapWin {
				recent = append(recent, t)
			}
		}
		s.lowers = recent
		need := flNeed
		if len(s.lowers) >= flFlapN {
			need = flNeedFlap
		}
		act, lift := "lowered for new connections", "lifted"
		if d.logOnly {
			act, lift = "WOULD be lowered (log only)", "would be lifted (log only)"
		}
		switch {
		case !s.lowered && why != "" && need == flNeed && s.hotRuns < need:
			// b336: one stall-evidenced window lowers at once — not under
			// the flap guard (need > flNeed), which still waits its windows
			s.lowered, s.lowAt, s.calmRuns = true, now, 0
			s.lowers = append(s.lowers, now)
			flLowers.Add(1)
			flStallLowers.Add(1)
			flWouldNote(d.logOnly)
			d.log("lb fastlower: IP %s %s at once: %s", ip, act, why)
		case !s.lowered && s.hotRuns >= need:
			s.lowered, s.lowAt, s.calmRuns = true, now, 0
			s.lowers = append(s.lowers, now)
			flLowers.Add(1)
			flWouldNote(d.logOnly)
			d.log("lb fastlower: IP %s %s: %d of %d paths resend (worst %.1f%%, best other %.1f%%) %d windows in a row", ip, act, bad, read, 100*worst, 100*best, s.hotRuns)
		case s.lowered && s.calmRuns >= flCalm && now.Sub(s.lowAt) >= flMinHold:
			s.lowered, s.hotRuns = false, 0
			flLifts.Add(1)
			d.log("lb fastlower: IP %s %s after %s (%d calm windows)", ip, lift, fmtDur(now.Sub(s.lowAt)), s.calmRuns)
		}
	}
	any := false
	for ip, s := range d.ip {
		if _, ok := ips[ip]; !ok {
			delete(d.ip, ip)
			continue
		}
		any = any || s.lowered
	}
	if any {
		flLowTicks.Add(1)
	}
}

// ---- edge hooks ---------------------------------------------------------------

// fastLowerTickLocked runs the detector on this tick's counts (before the
// tick clears them). Caller holds m.mu.
func (st *lbSteerState) fastLowerTickLocked(now time.Time, alive map[string]bool) {
	if !flOn() {
		return
	}
	if st.fl == nil {
		st.fl = newFastLowerDet(func(f string, a ...any) { st.logs = append(st.logs, fmt.Sprintf(f, a...)) })
	}
	c := map[string]flRead{}
	add := func(rt, segs map[string]uint64) {
		for p, n := range segs {
			r := c[p]
			// the worse direction: the larger resent share wins
			if n > 0 && (r.segs == 0 || float64(rt[p])/float64(n) > float64(r.rt)/float64(r.segs)) && n >= flMinSegs {
				r = flRead{rt[p], n}
			} else if r.segs == 0 {
				r = flRead{rt[p], n}
			}
			c[p] = r
		}
	}
	add(st.dnRt, st.dnSegs)
	add(st.upRt, st.upSegs)
	st.fl.stepStall(now, c, alive, append(flStallTake(now), st.flRTO...))
	st.flRTO = nil
}

// fastLowered says whether path p is on an IP the fast detector lowered.
func (st *lbSteerState) fastLowered(p string) bool {
	return b335FastLower && st != nil && st.fl.lowered(p)
}

// fastSomeOK says whether some live path is not fast-lowered (somewhere to go).
func (st *lbSteerState) fastSomeOK() bool {
	if st == nil || st.judge == nil {
		return false
	}
	for p := range st.judge.p {
		if !st.fastLowered(p) {
			return true
		}
	}
	return false
}
