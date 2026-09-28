package fec

import (
	"math"
	"sync"
	"time"
)

// Adapter turns the loss reports a receiver sends back into the loss estimate
// the encoder sizes parity for.
//
// A raw per-interval loss figure is far too noisy to drive redundancy
// directly on a bursty path (it jumps between 0% and 70% from one 100 ms
// report to the next), and following it would flap. So:
//
//   - an asymmetric EWMA rises fast (RiseAlpha) and decays slowly
//     (FallAlpha): protection goes up within a report or two of a loss
//     increase and comes down over seconds, not on the first quiet report;
//   - a recent-peak hold keeps the estimate at no less than HoldFrac of the
//     worst report in the last Hold window, so a path that has just shown a
//     burst stays protected against the next one;
//   - Floor is the smallest estimate ever used, so even a path that looks
//     clean carries some parity for the burst it has not shown yet.
type Adapter struct {
	cfg AdapterConfig

	mu     sync.Mutex
	ewma   float64
	primed bool
	peaks  []peak
}

type peak struct {
	t    time.Time
	loss float64
}

// AdapterConfig tunes Adapter. Zero fields take DefaultAdapterConfig values.
type AdapterConfig struct {
	RiseAlpha float64       // EWMA weight of a report above the estimate
	FallAlpha float64       // EWMA weight of a report below the estimate
	Hold      time.Duration // how long a peak keeps protection up
	HoldFrac  float64       // fraction of the held peak the estimate keeps
	Floor     float64       // lowest loss estimate ever used
	Margin    float64       // added to the estimate for estimation error
	Max       float64       // highest loss estimate (the ceiling on overhead)
}

// DefaultAdapterConfig holds the values chosen by simulation and the lab
// (see BUILD.md). Reports arrive every 100 ms, so FallAlpha 0.08 decays with
// a ~1.2 s time constant while RiseAlpha 0.5 reaches most of a step in two
// reports. The peak hold is moderate: on memoryless bursts holding at 0.75 of
// the peak cost ~30% more overhead for ~1% less residual loss, but real paths
// cluster their bursts (seconds of 70% loss), where a short hold pays off.
// Max 0.5 caps parity at the ceiling ratio of the encoder.
func DefaultAdapterConfig() AdapterConfig {
	return AdapterConfig{
		RiseAlpha: 0.5,
		FallAlpha: 0.08,
		Hold:      2 * time.Second,
		HoldFrac:  0.5,
		Floor:     0.03,
		Margin:    0.02,
		Max:       0.5,
	}
}

func (c *AdapterConfig) fill() {
	d := DefaultAdapterConfig()
	if c.RiseAlpha == 0 {
		c.RiseAlpha = d.RiseAlpha
	}
	if c.FallAlpha == 0 {
		c.FallAlpha = d.FallAlpha
	}
	if c.Hold == 0 {
		c.Hold = d.Hold
	}
	if c.HoldFrac == 0 {
		c.HoldFrac = d.HoldFrac
	}
	if c.Floor == 0 {
		c.Floor = d.Floor
	}
	if c.Margin == 0 {
		c.Margin = d.Margin
	}
	if c.Max == 0 {
		c.Max = d.Max
	}
}

// NewAdapter builds an Adapter; zero config fields take defaults.
func NewAdapter(cfg AdapterConfig) *Adapter {
	cfg.fill()
	return &Adapter{cfg: cfg}
}

// Observe feeds one loss report (0..1) and returns the new estimate.
func (a *Adapter) Observe(loss float64, now time.Time) float64 {
	if loss < 0 || math.IsNaN(loss) {
		loss = 0
	}
	if loss > 1 {
		loss = 1
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.primed {
		a.ewma, a.primed = loss, true
	} else if loss > a.ewma {
		a.ewma += a.cfg.RiseAlpha * (loss - a.ewma)
	} else {
		a.ewma += a.cfg.FallAlpha * (loss - a.ewma)
	}
	// Keep a short list of peaks: drop expired ones and any older peak that
	// the new report dominates, so the list stays tiny.
	keep := a.peaks[:0]
	for _, p := range a.peaks {
		if now.Sub(p.t) < a.cfg.Hold && p.loss > loss {
			keep = append(keep, p)
		}
	}
	a.peaks = append(keep, peak{now, loss})
	return a.estimateLocked(now)
}

// Estimate returns the current loss estimate without a new report.
func (a *Adapter) Estimate(now time.Time) float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.estimateLocked(now)
}

// Smoothed returns the EWMA alone (for logs and the transport selector).
func (a *Adapter) Smoothed() float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ewma
}

func (a *Adapter) estimateLocked(now time.Time) float64 {
	e := a.ewma
	for _, p := range a.peaks {
		if now.Sub(p.t) < a.cfg.Hold {
			if h := a.cfg.HoldFrac * p.loss; h > e {
				e = h
			}
		}
	}
	e += a.cfg.Margin
	if e < a.cfg.Floor {
		e = a.cfg.Floor
	}
	if e > a.cfg.Max {
		e = a.cfg.Max
	}
	return e
}

// Residual is the expected fraction of data shards still missing after
// decoding an RS(k data, r parity) group when each packet is lost
// independently with probability p. A group decodes when at most r of its
// k+r packets are lost; otherwise the lost data shards stay lost (on
// average x·k/n of x losses fall on data).
func Residual(k, r int, p float64) float64 {
	if p <= 0 {
		return 0
	}
	if p >= 1 {
		return 1
	}
	n := k + r
	q := 1 - p
	// pmf(x) iteratively: pmf(0) = q^n, pmf(x+1) = pmf(x)·(n-x)/(x+1)·p/q.
	pmf := math.Pow(q, float64(n))
	res := 0.0
	for x := 0; x <= n; x++ {
		if x > r {
			res += pmf * float64(x) * float64(k) / float64(n)
		}
		pmf *= float64(n-x) / float64(x+1) * p / q
	}
	return res / float64(k)
}

// ParityFor returns the smallest r in [1, maxR] with Residual(k, r, p) at
// most target, or maxR if none reaches it. The loss is rounded up to the next
// 0.5% first, so callers can cache by that step (see Encoder).
func ParityFor(k int, p, target float64, maxR int) int {
	if k < 1 {
		return 0
	}
	return parityForStep(k, lossStep(p), target, maxR)
}

// lossStep rounds a loss estimate up to the next 0.5% step.
func lossStep(p float64) int {
	if p > 0.9 {
		p = 0.9 // beyond this the binomial tail underflows; parity is capped anyway
	}
	if p < 0 {
		p = 0
	}
	return int(math.Ceil(p * 200))
}

func parityForStep(k, step int, target float64, maxR int) int {
	if maxR < 1 {
		maxR = 1
	}
	if maxR > MaxShards-k {
		maxR = MaxShards - k
	}
	pq := float64(step) / 200
	for r := 1; r < maxR; r++ {
		if Residual(k, r, pq) <= target {
			return r
		}
	}
	return maxR
}
