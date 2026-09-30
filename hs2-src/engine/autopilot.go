package engine

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"time"
)

// autopilot is the pure decision core of the adaptive link pool: one sample per
// health tick in, the number of SERVING links the pool should have out. It holds
// no locks, opens no sockets and never reads the clock (time and randomness come
// in through the sample and a.rnd), so its whole behaviour is exercised by the
// flow-level simulator in autopilot_test.go. LinkManager is the actuator.
//
// What the count means. T is the number of links that accept NEW user
// connections ("serving"). A link beyond T is "retiring": it takes no new
// connections and is closed once its connections have ended — shrinking never
// cuts a connection that is still moving data.
//
// What moves T:
//
//   - Floor: enough links for the flows that are really moving data
//     (ceil(flowing/per_link) over the last 10 s). Idle connections — an xray
//     panel keeps hundreds — do not count.
//   - Growth, only when links are PRESSED (their sender is blocked by the path,
//     measured at whichever end sends) and no unpressed link is left for new
//     flows to land on. Growth is a probe: T grows by ~25%, the probe arms only
//     once the new links exist, and it is kept only if the traffic the new links
//     carry ADDED to the total (per-connection throttling: it does) rather than
//     being taken from the old links (the path itself is full: it is not). A
//     failed probe backs off exponentially; a probe that no new flow reached is
//     inconclusive and keeps its links as spares.
//   - Shrink, driven by demand: after 60 s below target, T steps down toward
//     max(floor, what recent pressure needs, what the recent peak throughput
//     needs at 70% of the measured per-link capacity), capped by the links the
//     active flows can actually use. If a shrink immediately causes a shortage,
//     it is undone at once (the retiring links are still up) and held.
//
// Nothing that keeps T up is latched: every input forgets within 60 s (the
// capacity estimate only scales how many links a given throughput needs, and
// the hold expires and is void once demand drops), so when traffic falls the
// pool comes down — the defect of the first version, which ratcheted to 32.
type autopilot struct {
	min, max, perLink int
	tun               apTunables
	rnd               func() float64

	T     int       // committed SERVING target
	hist  []apTick  // ring of the last tun.histTicks ticks
	caps  []apCap   // sustained rates of pressed serving links
	pr    *apProbe  // an in-flight growth probe
	k     int       // failed-probe backoff exponent
	chain int       // consecutive successful probes: bigger steps while demand climbs
	next  time.Time // no new probe before this
	fail  struct {  // where growth last stopped helping
		at    time.Time
		g     float64
		flows int
	}
	belowSince   time.Time
	lastGrowAt   time.Time
	lastShrinkAt time.Time
	shrinkFrom   int
	hold         struct {
		n     int
		g     float64
		until time.Time
	}
	undos []time.Time

	phase  apPhase
	reason string
	cCap   float64 // last per-link capacity estimate, bytes/s (0 = unknown)
	gPeak  float64 // last 60 s peak aggregate, bytes/s
}

// apTunables holds every clock and threshold, so tests can shorten them.
type apTunables struct {
	tick              time.Duration
	histTicks         int // ticks of history the windows look back over (60 s)
	shortWin, shortN  int // shortage = shortTick on >= shortN of the last shortWin ticks
	armTimeout        time.Duration
	settleTicks       int
	looks             []int   // eval ticks at which the probe verdict is checked
	baseTicks         int     // ticks of baseline before a probe
	z                 float64 // noise margin in standard errors
	additivity        float64 // the total must rise by at least this share of what new links carry
	minGain           float64 // ... and by at least this share of the baseline
	earlyFail         float64
	rMinAbs           float64 // bytes/s the new links must carry for a verdict
	rMinFrac          float64 // ... or this share of the per-link baseline
	backoffBase       time.Duration
	backoffMax        time.Duration
	jitter            float64
	successNext       time.Duration
	inconclusiveNext  time.Duration
	abortNext         time.Duration
	capWindow         time.Duration
	capMinSamples     int
	capMax            int
	util              float64 // links run at <= this share of capacity at the recent peak
	minBWForNeed      float64 // below this peak, bandwidth does not justify extra links
	shrinkDwell       time.Duration
	shrinkStep        time.Duration
	noShrinkAfterGrow time.Duration
	overshootWin      time.Duration
	holdBase          time.Duration
	holdMax           time.Duration
	holdVoid          float64
	undoWindow        time.Duration
	kResetAfter       time.Duration
	chainWindow       time.Duration // a success this recent lets the next probe step by half
	activeRate        float64       // bytes/s: a link below this carries no traffic to judge
}

func defaultTunables() apTunables {
	return apTunables{
		tick:              healthTick,
		histTicks:         30,
		shortWin:          5,
		shortN:            3,
		armTimeout:        15 * time.Second,
		settleTicks:       2,
		looks:             []int{5, 10, 15},
		baseTicks:         10,
		z:                 2.5,
		additivity:        0.5,
		minGain:           0.05,
		earlyFail:         0.25,
		rMinAbs:           32 << 10,
		rMinFrac:          0.25,
		backoffBase:       30 * time.Second,
		backoffMax:        8 * time.Minute,
		jitter:            0.2,
		successNext:       4 * time.Second,
		inconclusiveNext:  30 * time.Second,
		abortNext:         60 * time.Second,
		capWindow:         30 * time.Minute,
		capMinSamples:     6,
		capMax:            256,
		util:              0.7,
		minBWForNeed:      16 << 10,
		shrinkDwell:       60 * time.Second,
		shrinkStep:        30 * time.Second,
		noShrinkAfterGrow: 60 * time.Second,
		overshootWin:      60 * time.Second,
		holdBase:          10 * time.Minute,
		holdMax:           2 * time.Hour,
		holdVoid:          0.6,
		undoWindow:        2 * time.Hour,
		kResetAfter:       30 * time.Minute,
		chainWindow:       60 * time.Second,
		activeRate:        float64(pressMinBytes) / healthTick.Seconds(),
	}
}

type apPhase int

const (
	apSteady    apPhase = iota // holding the size the measurements chose
	apScaling                  // raised to the active-flow floor, or a shrink undone
	apProbing                  // grown by a probe; measuring whether it helped
	apHolding                  // growth backed off after the path proved full
	apShrinking                // stepping down toward what demand needs
)

func (p apPhase) String() string {
	switch p {
	case apScaling:
		return "scaling"
	case apProbing:
		return "probing"
	case apHolding:
		return "holding"
	case apShrinking:
		return "shrinking"
	default:
		return "steady"
	}
}

// apLink is one link as the sampler saw it this tick.
type apLink struct {
	id           int
	serving      bool // accepts new connections
	retiring     bool // shrinking: no new connections, closes when empty
	servingSince time.Time
	pressed      bool    // its sender is blocked by the path (either direction)
	rate         float64 // bytes/s both directions, this tick
	rate10       float64 // mean rate over the last 5 ticks
	sustained    float64 // min over 3 ticks of the dominant direction's rate
	flowing      int     // user streams really moving data
	open         int     // user streams open
}

// apSample is one health tick's measurements.
type apSample struct {
	now      time.Time
	links    []apLink
	G        float64 // aggregate bytes/s over every live link
	flowing  int     // flowing user streams over every link
	open     int     // open user connections
	growable bool    // false: the peer cannot add links (reverse exit without pool control)
}

// apDecision is what the pool should do.
type apDecision struct {
	target int     // serving links wanted, clamped to [min,max]
	phase  apPhase // for the live monitor
	reason string  // why, with the numbers — shown in the live monitor
	note   string  // one log line when something was decided, else ""
}

type apTick struct {
	g       float64
	flowing int
	p, s    int
	short   bool
}

type apCap struct {
	t time.Time
	v float64
}

type apProbe struct {
	from, to int
	start    time.Time
	armed    bool
	armedAt  time.Time
	settled  int
	gb, varB float64
	nb       int // baseline ticks
	before   map[int]float64
	evalG    []float64
	evalNew  []float64
	// evalProbe: total rate of the probe links (a link brought back from
	// retiring keeps the flows it had, so it can be busy without new traffic).
	evalProbe []float64
	// evalShort counts eval ticks on which the links carrying traffic were
	// still short of headroom; below half, the probe relieved the pressure.
	evalShort int
	triggerP  int
	triggerS  int
}

func newAutopilot(min, max, perLink int) *autopilot {
	if min < 1 {
		min = 1
	}
	if max < min {
		max = min
	}
	if perLink < 1 {
		perLink = 8
	}
	return &autopilot{min: min, max: max, perLink: perLink, tun: defaultTunables(),
		rnd: rand.Float64, T: warmSize(min, max)}
}

// spare is how many unpressed links should be left for new flows when p links
// are pressed: one pinned capped flow never triggers growth, several do.
func spare(p int) int {
	if p == 0 {
		return 0
	}
	s := (p + 3) / 4
	if s < 1 {
		s = 1
	}
	if s > 4 {
		s = 4
	}
	return s
}

func ceilDiv(a, b int) int {
	if a <= 0 {
		return 0
	}
	return (a + b - 1) / b
}

func (a *autopilot) clamp(n int) int {
	if n < a.min {
		return a.min
	}
	if n > a.max {
		return a.max
	}
	return n
}

// last returns the most recent n ticks (fewer if the history is shorter),
// newest last.
func (a *autopilot) last(n int) []apTick {
	if n > len(a.hist) {
		n = len(a.hist)
	}
	return a.hist[len(a.hist)-n:]
}

func (a *autopilot) decide(s apSample) apDecision {
	t := &a.tun
	now := s.now
	if len(s.links) == 0 {
		// No link is up (an outage, or the reverse edge before the exit has
		// dialed): nothing was measured, which is not the same as no demand.
		// Keep the size and the history as they are.
		a.T = a.clamp(a.T)
		a.reason = "no link is up — waiting for links"
		return apDecision{target: a.T, phase: a.phase, reason: a.reason}
	}

	// ---- measurements for this tick -------------------------------------
	S, R, P := 0, 0, 0
	for _, l := range s.links {
		switch {
		case l.serving:
			S++
			if l.pressed {
				P++
				if l.sustained > 0 {
					a.caps = append(a.caps, apCap{now, l.sustained})
				}
			}
		case l.retiring:
			R++
		}
	}
	if len(a.caps) > t.capMax {
		a.caps = a.caps[len(a.caps)-t.capMax:]
	}
	shortTick := P >= 1 && S-P < spare(P)
	a.hist = append(a.hist, apTick{g: s.G, flowing: s.flowing, p: P, s: S, short: shortTick})
	if len(a.hist) > t.histTicks {
		a.hist = a.hist[len(a.hist)-t.histTicks:]
	}

	shortage := 0
	for _, h := range a.last(t.shortWin) {
		if h.short {
			shortage++
		}
	}
	isShort := shortage >= t.shortN
	shortIn60 := false
	fl5 := math.MaxInt
	for _, h := range a.last(5) {
		if h.flowing < fl5 {
			fl5 = h.flowing
		}
	}
	fl60, p60 := 0, 0
	gPeak := 0.0
	prevG := -1.0
	for _, h := range a.hist {
		if h.flowing > fl60 {
			fl60 = h.flowing
		}
		if h.p > p60 {
			p60 = h.p
		}
		if h.short {
			shortIn60 = true
		}
		g := h.g
		if prevG >= 0 {
			g = (h.g + prevG) / 2 // a single-tick spike is not a peak
		}
		if g > gPeak {
			gPeak = g
		}
		prevG = h.g
	}
	fGrow := ceilDiv(fl5, a.perLink)
	fHold := ceilDiv(fl60, a.perLink)
	// Links the active flows can use (connections are pinned): one each plus
	// two for arrivals, and never fewer than the pressed links plus their
	// spares (a pressed link carries at least one active flow even when its
	// flows are too throttled to be counted).
	U := a.clamp(max(fl60+2, p60+spare(p60)))
	cCap := a.capEstimate(now)
	needBW := 0
	if cCap > 0 && gPeak >= t.minBWForNeed {
		needBW = int(math.Ceil(gPeak / (t.util * cCap)))
	}
	needSat := p60 + spare(p60)
	holdN := 0
	if now.Before(a.hold.until) && gPeak >= t.holdVoid*a.hold.g {
		holdN = a.hold.n
	}
	need := needSat
	if needBW > need {
		need = needBW
	}
	if need > U {
		need = U
	}
	// A hold (a shrink that proved too deep) is not capped by U: U is what
	// the model says flows can use; the hold is what was measured.
	if holdN > need {
		need = holdN
	}
	if fHold > need {
		need = fHold
	}
	H := a.clamp(need)
	a.cCap, a.gPeak = cCap, gPeak

	// Backoff resets when demand clearly and steadily outgrew the last ceiling
	// (re-check now) or long after the last failure. "Steadily": over the last
	// 10 s, not a peak — a queue draining can burst above the path's rate.
	if a.k > 0 {
		gSus, _ := meanVar(a.last(5), func(h apTick) float64 { return h.g })
		switch {
		case gSus > 1.3*a.fail.g || fl5 > int(1.5*float64(a.fail.flows))+2:
			a.k = 0
			if a.next.After(now) {
				a.next = now
			}
		case now.Sub(a.fail.at) >= t.kResetAfter:
			a.k = 0
		}
	}

	why := fmt.Sprintf("%d active of %d open connections, %d of %d serving links at their limit, peak %.1f Mbit/s",
		s.flowing, s.open, P, S, mbitps(gPeak))
	if cCap > 0 {
		why += fmt.Sprintf(" (one link carries ~%.1f Mbit/s)", mbitps(cCap))
	}

	// ---- 1 FLOOR: enough links for the flows really moving data ----------
	floor := a.clamp(fGrow)
	if !s.growable && S+R >= a.min && floor > S+R {
		floor = S + R // the peer cannot add links; do not ask every tick
	}
	if floor > a.T {
		old := a.T
		a.pr = nil
		a.T = floor
		a.lastGrowAt = now
		return a.out(s, S, R, apScaling, fmt.Sprintf("%d active connections need %d links (per_link %d)", fl5, a.T, a.perLink),
			fmt.Sprintf("pattern %d → %d links: %d active connections (per_link %d)", old, a.T, fl5, a.perLink))
	}

	// ---- 2 PROBE in flight ------------------------------------------------
	if a.pr != nil {
		return a.judge(s, S, R, fl60, why)
	}

	// ---- 3 RESTORE: a shrink caused a shortage — undo it at once ----------
	if isShort && a.shrinkFrom > a.T && now.Sub(a.lastShrinkAt) <= t.overshootWin {
		old := a.T
		a.T = a.shrinkFrom
		a.shrinkFrom = 0
		j := 0
		kept := a.undos[:0]
		for _, u := range a.undos {
			if now.Sub(u) <= t.undoWindow {
				kept = append(kept, u)
			}
		}
		a.undos = append(kept, now)
		j = len(a.undos) - 1
		ttl := t.holdBase << j
		if ttl > t.holdMax || ttl <= 0 {
			ttl = t.holdMax
		}
		a.hold.n, a.hold.g, a.hold.until = a.T, gPeak, now.Add(ttl)
		a.lastGrowAt = now
		return a.out(s, S, R, apScaling, fmt.Sprintf("shrinking to %d left links at their limit — back to %d, held %s", old, a.T, fmtDur(ttl)),
			fmt.Sprintf("pattern %d → %d links: the shrink to %d left links at their limit — undone, held for %s", old, a.T, old, fmtDur(ttl)))
	}

	// ---- 4 GROW: pressed links and nowhere unpressed for new flows ---------
	// (A probe needs a full baseline to be judged against.)
	if isShort && s.growable && S >= a.T && a.T < U && a.T < a.max && !now.Before(a.next) && len(a.hist) >= t.baseTicks {
		// Grow by a quarter; while probes keep succeeding back to back (demand
		// is climbing), by half — each step is still verified before it is
		// kept, so a full path costs one failed probe either way.
		step := (a.T + 3) / 4
		if a.chain > 0 && now.Sub(a.lastGrowAt) <= t.chainWindow {
			step = (a.T + 1) / 2
		}
		to := a.T + step
		if to > U {
			to = U
		}
		if to > a.max {
			to = a.max
		}
		if to > a.T {
			base := a.last(t.baseTicks)
			gb, varB := meanVar(base, func(h apTick) float64 { return h.g })
			nb := len(base)
			before := map[int]float64{}
			for _, l := range s.links {
				if l.retiring {
					before[l.id] = l.rate10
				}
			}
			a.pr = &apProbe{from: a.T, to: to, start: now, gb: gb, varB: varB, nb: nb, before: before, triggerP: P, triggerS: S}
			old := a.T
			a.T = to
			return a.out(s, S, R, apProbing, fmt.Sprintf("%d of %d links at their limit — trying %d", P, S, to),
				fmt.Sprintf("pattern %d → %d links (probe): %d of %d serving links at their limit, %s", old, to, P, S, why))
		}
	}

	// ---- 5 SHRINK: demand is below the target ------------------------------
	if H < a.T {
		if a.belowSince.IsZero() {
			a.belowSince = now
		}
	} else {
		a.belowSince = time.Time{}
	}
	if H < a.T && now.Sub(a.belowSince) >= t.shrinkDwell && (!shortIn60 || a.T > U) &&
		now.Sub(a.lastShrinkAt) >= t.shrinkStep && now.Sub(a.lastGrowAt) >= t.noShrinkAfterGrow {
		old := a.T
		step := (a.T - H + 1) / 2
		if step < 1 {
			step = 1
		}
		a.T -= step
		if a.T < H {
			a.T = H
		}
		a.shrinkFrom, a.lastShrinkAt = old, now
		return a.out(s, S, R, apShrinking, fmt.Sprintf("demand needs ~%d links: %s", H, why),
			fmt.Sprintf("pattern %d → %d links: demand needs ~%d — %s; extra links take no new connections and close when theirs end", old, a.T, H, why))
	}

	// ---- 6 hold ------------------------------------------------------------
	if isShort && now.Before(a.next) {
		return a.out(s, S, R, apHolding, fmt.Sprintf("links at their limit but more did not help at %.1f Mbit/s; next check in %s",
			mbitps(a.fail.g), fmtDur(a.next.Sub(now))), "")
	}
	r := "sized for current demand: " + why
	if H < a.T {
		r = fmt.Sprintf("demand needs ~%d; stepping down after a steady minute — %s", H, why)
	}
	return a.out(s, S, R, apSteady, r, "")
}

// judge runs the in-flight probe for one tick.
func (a *autopilot) judge(s apSample, S, R, fl60 int, why string) apDecision {
	t := &a.tun
	pr := a.pr
	now := s.now
	if !pr.armed {
		if S >= pr.to {
			pr.armed, pr.armedAt = true, now
		} else if now.Sub(pr.start) > t.armTimeout {
			a.T = pr.from
			a.pr = nil
			a.chain = 0
			a.next = now.Add(t.abortNext)
			return a.out(s, S, R, apHolding, fmt.Sprintf("wanted %d links, only %d came up", pr.to, S),
				fmt.Sprintf("pattern back to %d links: wanted %d but only %d came up in %s (peer not dialing or dials failing); retry in %s",
					pr.from, pr.to, S, fmtDur(t.armTimeout), fmtDur(t.abortNext)))
		}
		return a.out(s, S, R, apProbing, fmt.Sprintf("trying %d links — waiting for them to come up (%d up)", pr.to, S), "")
	}
	if pr.settled < t.settleTicks {
		pr.settled++
		return a.out(s, S, R, apProbing, fmt.Sprintf("trying %d links — letting new connections land", pr.to), "")
	}
	newSum, probeSum := 0.0, 0.0
	for _, l := range s.links {
		if l.serving && !l.servingSince.Before(pr.start) {
			newSum += l.rate - pr.before[l.id]
			probeSum += l.rate
		}
	}
	pr.evalG = append(pr.evalG, s.G)
	pr.evalNew = append(pr.evalNew, newSum)
	pr.evalProbe = append(pr.evalProbe, probeSum)
	// Did the probe give the links that carry traffic headroom? When the
	// path itself is full, every link moving data stays blocked by it, however
	// many there are; when demand was simply met, they stop being blocked.
	// Links without traffic (a probe link nothing reached yet, an old link
	// whose flows moved) say nothing either way.
	act, pAct := 0, 0
	for _, l := range s.links {
		if l.serving && l.rate >= t.activeRate {
			act++
			if l.pressed {
				pAct++
			}
		}
	}
	if pAct >= 1 && act-pAct < spare(pAct) {
		pr.evalShort++
	}
	n := len(pr.evalG)
	isLook := false
	for _, l := range t.looks {
		if n == l {
			isLook = true
		}
	}
	lastLook := t.looks[len(t.looks)-1]
	if !isLook {
		return a.out(s, S, R, apProbing, fmt.Sprintf("trying %d links — measuring (%d/%d)", pr.to, n, lastLook), "")
	}
	rNew := mean(pr.evalNew)
	pTot := mean(pr.evalProbe)
	relieved := 2*pr.evalShort < n
	gA, varA := meanVarF(pr.evalG)
	dG := gA - pr.gb
	se := math.Sqrt(pr.varB/float64(pr.nb) + varA/float64(n))
	rMin := t.rMinAbs
	if pr.from > 0 {
		if v := t.rMinFrac * pr.gb / float64(pr.from); v > rMin {
			rMin = v
		}
	}
	busy := pTot >= rMin
	need := t.additivity * rNew
	if v := t.z * se; v > need {
		need = v
	}
	if v := t.minGain * pr.gb; v > need {
		need = v
	}
	switch {
	case rNew >= rMin && dG >= need:
		a.pr = nil
		a.k = 0
		a.chain++
		a.next = now.Add(t.successNext)
		a.lastGrowAt = now
		return a.out(s, S, R, apSteady, fmt.Sprintf("%d links: the new links added %.1f Mbit/s", a.T, mbitps(dG)),
			fmt.Sprintf("pattern %d → %d links kept: +%.1f Mbit/s (new links carried %.1f)", pr.from, pr.to, mbitps(dG), mbitps(rNew)))
	case rNew >= rMin && relieved && n == lastLook:
		// The added links took new connections and no link is short any more,
		// but the total barely moved: demand was nearly met already. Keep them
		// as headroom (the shrink rule returns them if demand does not need
		// them), without the backoff a full path earns.
		a.pr = nil
		a.chain = 0
		a.next = now.Add(t.inconclusiveNext)
		a.lastGrowAt = now
		return a.out(s, S, R, apSteady, fmt.Sprintf("%d links: no link is short of capacity any more", a.T),
			fmt.Sprintf("pattern %d → %d links kept as headroom: new links carried %.1f Mbit/s and no link is at its limit any more (total %+.1f)",
				pr.from, pr.to, mbitps(rNew), mbitps(dG)))
	case !relieved && (rNew >= rMin && (n == lastLook || (n == t.looks[1] && dG < t.earlyFail*rNew)) ||
		n == lastLook && busy):
		// The added links carried traffic, the others stayed at their limit,
		// and the total did not rise enough: the path itself is full. (busy
		// covers links brought back from retiring: they carry the flows they
		// already had, so little of it is new, yet nothing was gained.)
		a.pr = nil
		a.chain = 0
		a.T = pr.from
		a.k++
		a.fail.at, a.fail.g, a.fail.flows = now, pr.gb, fl60
		back := t.backoffBase << (a.k - 1)
		if back > t.backoffMax || back <= 0 {
			back = t.backoffMax
		}
		back = time.Duration(float64(back) * (1 - t.jitter + 2*t.jitter*a.rnd()))
		a.next = now.Add(back)
		return a.out(s, S, R, apHolding, fmt.Sprintf("path full at ~%.1f Mbit/s: more links did not add throughput; next check in %s", mbitps(pr.gb), fmtDur(back)),
			fmt.Sprintf("sized to %d links at ~%.1f Mbit/s — %d more links carried %.1f Mbit/s but the total rose only %.1f (path is full); next check in %s",
				pr.from, mbitps(pr.gb), pr.to-pr.from, mbitps(max(rNew, pTot)), mbitps(dG), fmtDur(back)))
	case n == lastLook:
		a.pr = nil
		a.chain = 0
		a.next = now.Add(t.inconclusiveNext)
		return a.out(s, S, R, apSteady, fmt.Sprintf("%d links: no new connection reached the added links yet — kept as spares", a.T),
			fmt.Sprintf("pattern %d → %d links kept as spares: no new connection reached them yet (connections stay on their link)", pr.from, pr.to))
	}
	return a.out(s, S, R, apProbing, fmt.Sprintf("trying %d links — measuring (%d/%d)", pr.to, n, lastLook), "")
}

// out finalises a decision: clamp T and remember phase and reason for the
// monitor. Without pool control on the peer the pool cannot grow past what is up.
func (a *autopilot) out(s apSample, S, R int, ph apPhase, reason, note string) apDecision {
	a.T = a.clamp(a.T)
	if !s.growable && a.T > S+R && S+R >= a.min {
		a.T = S + R
	}
	a.phase, a.reason = ph, reason
	return apDecision{target: a.T, phase: ph, reason: reason, note: note}
}

// capEstimate is the median sustained rate of pressed serving links over the
// capacity window — the per-link limit where one has actually been observed,
// 0 (unknown) otherwise, so an unconstrained path never holds links for
// bandwidth it does not need.
func (a *autopilot) capEstimate(now time.Time) float64 {
	var vs []float64
	kept := a.caps[:0]
	for _, c := range a.caps {
		if now.Sub(c.t) <= a.tun.capWindow {
			kept = append(kept, c)
			vs = append(vs, c.v)
		}
	}
	a.caps = kept
	if len(vs) < a.tun.capMinSamples {
		return 0
	}
	sort.Float64s(vs)
	return vs[len(vs)/2]
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func meanVarF(xs []float64) (float64, float64) {
	m := mean(xs)
	if len(xs) < 2 {
		return m, 0
	}
	v := 0.0
	for _, x := range xs {
		v += (x - m) * (x - m)
	}
	return m, v / float64(len(xs)-1)
}

func meanVar(ts []apTick, f func(apTick) float64) (float64, float64) {
	xs := make([]float64, len(ts))
	for i, t := range ts {
		xs[i] = f(t)
	}
	return meanVarF(xs)
}

// fmtDur prints a duration the way an operator reads it: 45s, 4m, 1h10m.
func fmtDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()+0.5))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()+0.5))
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// mbitps converts bytes/sec to Mbit/s for human-readable notes.
func mbitps(bytesPerSec float64) float64 { return bytesPerSec * 8 / 1e6 }
