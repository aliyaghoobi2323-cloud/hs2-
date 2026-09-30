package udpcarrier

import (
	"context"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Governor coordinates the carriers of one pool. They all go to the same
// destination IP, and a policer on the path — an ICMP rate limit, a per-IP DPI
// throttle — sees their SUM, not each carrier. Each carrier's own controller
// cannot find such a limit: when it slows down to test whether its loss is
// rate-dependent, the other carriers keep the total above the limit, the loss
// does not fall, and it concludes the loss is the path's own (lab and server:
// eight icmp carriers pushing ~1.8x the policer's rate, most of it parity, and
// a drop episode every few seconds).
//
// So the pool watches the pattern a policer leaves: loss episodes — far above
// the pool's usual loss (the median over the last 30 s), so steady random loss
// never counts however high it is — that hit most carriers at once with no
// queue standing (a full buffer would show a queue first). Two such
// episodes within govDetectWindow cap the WHOLE pool at govCapFrac of what got
// THROUGH on average over that window (rate x (1 - loss), episodes included:
// a policer's long-run pass rate — not the rate before the episodes, which a
// policer with an allowance lets through at line rate for a while), enforced
// by one token bucket every
// carrier's pacer draws from — data and parity alike, so parity counts
// against the budget. More episodes under the cap lower it; a clean spell
// while the cap is the limit raises it slowly (re-probe). If episodes go on
// even at the lowest cap the loss is not rate-dependent: the cap is lifted and
// detection rests for a while. While capped, parity sizes for the loss seen
// between episodes (the path's own), not for the policer's drops — more parity
// would only feed the policer.
type Governor struct {
	log func(string, ...any)

	mu        sync.Mutex
	members   map[*Conn]*govMember
	hist      []govTick // recent ticks, oldest first
	state     govState
	capB      float64 // bytes/s (wire) while capped
	floorB    float64 // lowest cap before giving up
	episodes  []time.Time
	inEpisode bool
	lastRaise time.Time
	cappedAt  time.Time
	restUntil time.Time
	floorEps  int
	cleanLoss float64 // EWMA of pool loss over clean ticks (the path's own)
	last      govTick

	capped   atomic.Bool
	capBits  atomic.Uint64 // math.Float64bits(capB)
	cleanBit atomic.Uint64 // math.Float64bits(cleanLoss)

	bmu    sync.Mutex
	tokens float64
	bLast  time.Time

	now func() time.Time // tests
}

type govState int

const (
	govNormal govState = iota
	govCapped
)

type govMember struct {
	sentPrev uint64
	lossSum  float64
	lossN    int
	qSum     float64
	qN       int
}

// govTick is one governor interval, pool-wide.
type govTick struct {
	at     time.Time
	rate   float64 // bytes/s the pool put on the wire
	loss   float64 // wire loss of what it sent, rate-weighted
	lossy  int     // active carriers with loss >= govCarrierLossy
	active int
	queue  float64 // median standing queue of active carriers, seconds
	burst  bool
}

const (
	govTickEvery     = 500 * time.Millisecond
	govHist          = 60   // ticks kept (30 s)
	govBurstLoss     = 0.05 // an episode tick loses at least this much…
	govBurstOverMed  = 3.0  // …and at least this many times the usual (median) loss…
	govBurstOverAdd  = 0.03 // …plus this
	govMinHist       = 10   // active ticks of history before anything is called an episode
	govCarrierLossy  = 0.02
	govSimultaneous  = 0.6
	govActiveRate    = 20_000 // bytes/s: a carrier below this is idle for detection
	govDetectWindow  = 30 * time.Second
	govCapFrac       = 0.9
	govLowerFrac     = 0.8
	govRaiseEvery    = 10 * time.Second
	govRaiseGain     = 1.05
	govBindingFrac   = 0.85 // the cap is "the limit" when the pool sends at least this share of it
	govFloorFrac     = 0.4  // of the first cap: lowest cap tried (a policer far below its own average pass rate is unlikely)
	govFloorEpisodes = 2
	govRest          = 5 * time.Minute
	govMinCap        = 125_000 // 1 Mbit/s
	govBucket        = 5 * time.Millisecond
)

// NewGovernor makes the governor of one pool; Run it for its lifetime.
func NewGovernor(logf func(string, ...any)) *Governor {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Governor{log: logf, members: map[*Conn]*govMember{}, now: time.Now}
}

// Run ticks until ctx ends.
func (g *Governor) Run(ctx context.Context) {
	t := time.NewTicker(govTickEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.tick()
		}
	}
}

// Capped reports whether the pool is held under a policer cap now.
func (g *Governor) Capped() bool { return g != nil && g.capped.Load() }

// CapBytes is the current cap in bytes/s (0 when not capped).
func (g *Governor) CapBytes() float64 {
	if !g.Capped() {
		return 0
	}
	return math.Float64frombits(g.capBits.Load())
}

// CleanLoss is the loss the pool sees outside policer episodes — the path's
// own, which parity is for.
func (g *Governor) CleanLoss() float64 { return math.Float64frombits(g.cleanBit.Load()) }

// Last is the most recent pool-wide tick (for status).
func (g *Governor) Last() (rateBytes, loss float64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.last.rate, g.last.loss
}

func (g *Governor) attach(c *Conn) {
	g.mu.Lock()
	g.members[c] = &govMember{sentPrev: c.rc.sent.Load()}
	g.mu.Unlock()
}

func (g *Governor) detach(c *Conn) {
	g.mu.Lock()
	delete(g.members, c)
	g.mu.Unlock()
}

// report takes one carrier's feedback: the wire loss of what it sent over
// the peer's last interval and its standing-queue estimate.
func (g *Governor) report(c *Conn, loss, queue float64) {
	g.mu.Lock()
	if m := g.members[c]; m != nil {
		m.lossSum += loss
		m.lossN++
		m.qSum += queue
		m.qN++
	}
	g.mu.Unlock()
}

// reserve charges n wire bytes to the pool while capped and returns how long
// the caller must wait before sending them (0 when not capped). Every
// carrier's pacer calls it for data and parity alike.
func (g *Governor) reserve(n int) time.Duration {
	if !g.Capped() {
		return 0
	}
	rate := math.Float64frombits(g.capBits.Load())
	if rate <= 0 {
		return 0
	}
	now := g.now()
	g.bmu.Lock()
	defer g.bmu.Unlock()
	if !g.bLast.IsZero() {
		g.tokens += now.Sub(g.bLast).Seconds() * rate
	}
	g.bLast = now
	if max := rate * govBucket.Seconds(); g.tokens > max {
		g.tokens = max
	}
	g.tokens -= float64(n)
	if g.tokens >= 0 {
		return 0
	}
	return time.Duration(-g.tokens / rate * float64(time.Second))
}

func (g *Governor) setCap(b float64) {
	g.capB = math.Max(b, govMinCap)
	g.capBits.Store(math.Float64bits(g.capB))
	g.capped.Store(true)
}

func mbit(b float64) float64 { return b * 8 / 1e6 }

// tick builds one pool-wide sample and runs the state machine.
func (g *Governor) tick() {
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	dt := govTickEvery.Seconds()
	if n := len(g.hist); n > 0 {
		if d := now.Sub(g.hist[n-1].at).Seconds(); d > 0.05 {
			dt = d
		}
	}
	var tk govTick
	tk.at = now
	var wLoss, wSum float64
	var qs []float64
	for c, m := range g.members {
		sent := c.rc.sent.Load()
		rate := float64(sent-m.sentPrev) / dt
		m.sentPrev = sent
		tk.rate += rate
		if m.lossN == 0 {
			continue
		}
		loss := m.lossSum / float64(m.lossN)
		q := m.qSum / float64(m.qN)
		m.lossSum, m.lossN, m.qSum, m.qN = 0, 0, 0, 0
		if rate < govActiveRate {
			continue
		}
		tk.active++
		wLoss += loss * rate
		wSum += rate
		if loss >= govCarrierLossy {
			tk.lossy++
		}
		qs = append(qs, q)
	}
	if wSum > 0 {
		tk.loss = wLoss / wSum
	}
	if len(qs) > 0 {
		sort.Float64s(qs)
		tk.queue = qs[len(qs)/2]
	}
	simultaneous := tk.active < 2 || float64(tk.lossy) >= govSimultaneous*float64(tk.active)
	med, n := g.medianLoss()
	tk.burst = tk.active > 0 && n >= govMinHist && tk.loss >= govBurstLoss &&
		tk.loss >= govBurstOverMed*med+govBurstOverAdd && tk.queue < lowQueue.Seconds() && simultaneous
	if tk.active > 0 && !tk.burst {
		g.cleanLoss += 0.1 * (tk.loss - g.cleanLoss)
		g.cleanBit.Store(math.Float64bits(g.cleanLoss))
	}
	g.hist = append(g.hist, tk)
	if len(g.hist) > govHist {
		g.hist = g.hist[len(g.hist)-govHist:]
	}
	g.last = tk

	// An episode is a run of burst ticks; count its start.
	newEpisode := tk.burst && !g.inEpisode
	g.inEpisode = tk.burst
	if newEpisode {
		g.episodes = append(g.episodes, now)
	}
	for len(g.episodes) > 0 && now.Sub(g.episodes[0]) > govDetectWindow {
		g.episodes = g.episodes[1:]
	}

	switch g.state {
	case govNormal:
		if !newEpisode || now.Before(g.restUntil) || len(g.episodes) < 2 {
			return
		}
		passed := g.passedRate(govDetectWindow)
		if passed <= 0 {
			passed = tk.rate * (1 - tk.loss)
		}
		g.state = govCapped
		g.cappedAt, g.lastRaise, g.floorEps = now, now, 0
		g.setCap(govCapFrac * passed)
		g.floorB = math.Max(govFloorFrac*g.capB, govMinCap)
		g.log("dg: policer on the path: %d loss episodes in %s (%.0f%% loss on %d of %d carriers at once, no queue) — whole pool capped at %.1f Mbit/s (%.0f%% of the %.1f Mbit/s that got through on average); parity held to the path's own loss",
			len(g.episodes), govDetectWindow, tk.loss*100, tk.lossy, tk.active, mbit(g.capB), govCapFrac*100, mbit(passed))
	case govCapped:
		binding := tk.rate >= govBindingFrac*g.capB
		switch {
		case newEpisode && binding:
			if g.capB <= g.floorB*1.01 {
				if g.floorEps++; g.floorEps >= govFloorEpisodes {
					g.state = govNormal
					g.capped.Store(false)
					g.episodes = nil
					g.restUntil = now.Add(govRest)
					g.log("dg: loss episodes continue even at %.1f Mbit/s — the loss is not rate-dependent, so it is not a policer: cap lifted (detection rests %s)", mbit(g.capB), govRest)
				}
				return
			}
			// Down to what the policer has been passing, if that is lower
			// than one step: far above it, a single step would take many
			// episodes to get there.
			next := govLowerFrac * g.capB
			if p := govCapFrac * g.passedRate(govDetectWindow/2); p > 0 && p < next {
				next = p
			}
			g.setCap(math.Max(next, g.floorB))
			g.lastRaise = now
			g.log("dg: policer: loss episode under the cap — lowered to %.1f Mbit/s", mbit(g.capB))
		case newEpisode:
			// an episode while well under the cap: not our rate; leave the cap
		case now.Sub(g.lastRaise) >= govRaiseEvery && g.cleanSince(g.lastRaise) && g.bindingShare(g.lastRaise) >= 0.5:
			g.setCap(g.capB * govRaiseGain)
			g.lastRaise = now
		case now.Sub(g.lastRaise) >= govRaiseEvery && !g.cleanSince(g.lastRaise):
			g.lastRaise = now // a burst tick not counted as an episode start: wait another spell
		}
	}
}

// medianLoss is the pool's usual loss: the median over the active ticks of the
// history (episodes are a minority of ticks, so they barely move it; steady
// random loss IS it). n is how many ticks it is based on.
func (g *Governor) medianLoss() (float64, int) {
	var ls []float64
	for _, h := range g.hist {
		if h.active > 0 {
			ls = append(ls, h.loss)
		}
	}
	if len(ls) == 0 {
		return 0, 0
	}
	sort.Float64s(ls)
	return ls[len(ls)/2], len(ls)
}

// passedRate: the mean rate that got through (sent x (1 - loss)) over the
// ticks of the last window in which the pool was sending — for a policer, its
// long-run pass rate, episodes included.
func (g *Governor) passedRate(window time.Duration) float64 {
	from := g.now().Add(-window)
	var sum float64
	n := 0
	for i := len(g.hist) - 1; i >= 0 && g.hist[i].at.After(from); i-- {
		if g.hist[i].active == 0 {
			continue
		}
		sum += g.hist[i].rate * (1 - g.hist[i].loss)
		n++
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

func (g *Governor) cleanSince(t time.Time) bool {
	for i := len(g.hist) - 1; i >= 0 && g.hist[i].at.After(t); i-- {
		if g.hist[i].burst {
			return false
		}
	}
	return true
}

// bindingShare: the share of ticks since t in which the pool sent at the cap
// (raising a cap nobody reaches would tell nothing about the policer).
func (g *Governor) bindingShare(t time.Time) float64 {
	n, b := 0, 0
	for i := len(g.hist) - 1; i >= 0 && g.hist[i].at.After(t); i-- {
		n++
		if g.hist[i].rate >= govBindingFrac*g.capB {
			b++
		}
	}
	if n == 0 {
		return 0
	}
	return float64(b) / float64(n)
}
