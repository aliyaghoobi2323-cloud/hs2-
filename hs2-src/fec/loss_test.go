package fec

import "math/rand/v2"

// lossModel decides, packet by packet, whether the path drops it.
type lossModel interface{ drop() bool }

// netemLoss is the kernel's `tc netem loss P% C%` generator (get_crandom in
// net/sched/sch_netem.c): each draw mixes a fresh uniform value with the
// previous one by the correlation, and the packet is lost when the result is
// below P. Correlation shortens the gaps between losses (bursts) and, as in
// the kernel, also lowers the realised loss rate below P.
type netemLoss struct {
	rng  *rand.Rand
	p    uint64 // threshold in 2^32 units
	rho  uint64
	last uint64
}

func newNetem(seed uint64, p, corr float64) *netemLoss {
	return &netemLoss{
		rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)),
		p:   uint64(p * (1 << 32)),
		rho: uint64(corr * (1 << 32)),
	}
}

func (n *netemLoss) drop() bool {
	v := uint64(n.rng.Uint32())
	var a uint64
	if n.rho == 0 {
		a = v
	} else {
		rho := n.rho + 1
		a = (v*((1<<32)-rho) + n.last*rho) >> 32
		n.last = a
	}
	return n.p >= a
}

// geLoss is a Gilbert-Elliott channel: a good state with loss pg and a bad
// state with loss pb, switching with p(good->bad)=g2b and p(bad->good)=b2g.
// It produces genuine bursts (runs of loss in the bad state) at a chosen mean
// loss rate, which netem's correlation cannot.
type geLoss struct {
	rng      *rand.Rand
	bad      bool
	pg, pb   float64
	g2b, b2g float64
}

func newGE(seed uint64, pg, pb, g2b, b2g float64) *geLoss {
	return &geLoss{rng: rand.New(rand.NewPCG(seed, seed+1)), pg: pg, pb: pb, g2b: g2b, b2g: b2g}
}

func (g *geLoss) drop() bool {
	if g.bad {
		if g.rng.Float64() < g.b2g {
			g.bad = false
		}
	} else if g.rng.Float64() < g.g2b {
		g.bad = true
	}
	if g.bad {
		return g.rng.Float64() < g.pb
	}
	return g.rng.Float64() < g.pg
}

// iidLoss drops each packet independently.
type iidLoss struct {
	rng *rand.Rand
	p   float64
}

func newIID(seed uint64, p float64) *iidLoss {
	return &iidLoss{rng: rand.New(rand.NewPCG(seed, seed+7)), p: p}
}

func (l *iidLoss) drop() bool { return l.rng.Float64() < l.p }

// timeGE is a Gilbert-Elliott channel whose states last for exponentially
// distributed TIMES (not packet counts): bursts are "15 ms of 85% loss"
// whatever the packet rate, which is how a path's bursts actually behave.
type timeGE struct {
	rng           *rand.Rand
	bad           bool
	until         int64 // ns, end of the current state
	pg, pb        float64
	goodMs, badMs float64 // mean state durations
	now           int64
}

func newTimeGE(seed uint64, pg, pb, goodMs, badMs float64) *timeGE {
	return &timeGE{rng: rand.New(rand.NewPCG(seed, seed^55)), pg: pg, pb: pb, goodMs: goodMs, badMs: badMs}
}

func (g *timeGE) set(nowNs int64) {
	g.now = nowNs
	for g.now >= g.until {
		g.bad = !g.bad
		m := g.goodMs
		if g.bad {
			m = g.badMs
		}
		g.until += int64(g.rng.ExpFloat64() * m * 1e6)
	}
}

func (g *timeGE) drop() bool {
	if g.bad {
		return g.rng.Float64() < g.pb
	}
	return g.rng.Float64() < g.pg
}
