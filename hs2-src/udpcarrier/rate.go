package udpcarrier

import (
	"math"
	"sync"
	"time"
)

// rateControl paces the UDP send rate from a bandwidth/RTT model, never from
// loss. On a path with 26% base loss a loss-based controller (what TCP uses)
// would read every burst as congestion and collapse; this one ignores loss for
// pacing and lets FEC repair it. It is delay-bounded, in the spirit of BBR and
// Copa/Vegas:
//
//   - btlBw   the delivered bandwidth the receiver reports, windowed-max over
//     a few round trips: the path's real bottleneck rate.
//   - rtProp  the minimum RTT over a long window: the base propagation delay,
//     free of queueing.
//   - queue   srtt - rtProp: how much standing queue our sending has built.
//
// The controller holds the queueing delay near a small target. Pacing exactly
// at btlBw fills the bottleneck buffer and bloats latency; pacing to keep a
// few-millisecond queue keeps latency and jitter flat — the whole point of this
// carrier — while still tracking the path's capacity. It ramps fast at startup
// (rate doubles each feedback while the queue stays empty and delivery keeps
// rising) and then trims the rate up or down to sit at the target queue.
type rateControl struct {
	mu sync.Mutex

	btlBw  float64 // bytes/sec, windowed max of delivered rate
	rtProp float64 // seconds, windowed min RTT
	srtt   float64 // seconds, smoothed RTT
	rate   float64 // bytes/sec, current pacing rate

	bwWindow []bwSample
	round    int
	rttMin   rttSample

	lastRxBytes uint64
	lastRxAt    time.Time
	haveRx      bool

	startup     bool
	lastBtlBw   float64
	plateauRuns int
	startRounds int
	prevRate    float64
	gainIdx     int
	lastLossPPM uint32

	minRate float64
	maxRate float64
}

type bwSample struct {
	rate  float64
	round int
}

type rttSample struct {
	rtt float64
	at  time.Time
}

const (
	bwWindowRounds = 8
	rtPropWindow   = 10 * time.Second
	startupPlateau = 3

	// queueing-delay band the controller holds. Below lowQueue it probes up,
	// above highQueue it drains. Small, so latency stays close to the base RTT.
	lowQueue  = 5 * time.Millisecond
	highQueue = 25 * time.Millisecond
)

func newRateControl() *rateControl {
	return &rateControl{
		rtProp:   0.05,
		srtt:     0.05,
		rate:     125_000, // ~1 Mbit/s to get going
		prevRate: 125_000,
		startup:  true,
		minRate:  32_000,
		maxRate:  4e9,
	}
}

// onFeedback folds one receiver report into the model and adjusts the rate.
func (r *rateControl) onFeedback(now time.Time, rxDataBytes uint64, rttSampleSec float64, lossPPM uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.lastLossPPM = lossPPM

	if rttSampleSec > 0 {
		if r.rttMin.rtt == 0 || rttSampleSec < r.rttMin.rtt || now.Sub(r.rttMin.at) > rtPropWindow {
			r.rttMin = rttSample{rtt: rttSampleSec, at: now}
		}
		r.rtProp = r.rttMin.rtt
		if r.srtt == 0 {
			r.srtt = rttSampleSec
		} else {
			r.srtt += 0.25 * (rttSampleSec - r.srtt)
		}
	}

	if r.haveRx {
		dt := now.Sub(r.lastRxAt).Seconds()
		// Guard against reordered/duplicated feedback: a too-small interval or a
		// non-increasing byte count would produce a bogus (often huge) rate
		// sample that poisons the windowed-max bandwidth estimate.
		if dt >= 0.02 && rxDataBytes >= r.lastRxBytes {
			sample := float64(rxDataBytes-r.lastRxBytes) / dt
			r.round++
			r.bwWindow = append(r.bwWindow, bwSample{rate: sample, round: r.round})
			cut := r.round - bwWindowRounds
			i := 0
			for ; i < len(r.bwWindow); i++ {
				if r.bwWindow[i].round > cut {
					break
				}
			}
			r.bwWindow = r.bwWindow[i:]
			max := 0.0
			for _, s := range r.bwWindow {
				if s.rate > max {
					max = s.rate
				}
			}
			r.btlBw = max
			r.adjustLocked()
		}
	}
	// Advance the delivered-bytes baseline only forward, so a reordered older
	// report cannot create a negative/huge next sample.
	if !r.haveRx || rxDataBytes >= r.lastRxBytes {
		r.lastRxBytes = rxDataBytes
		r.lastRxAt = now
	}
	r.haveRx = true
}

// adjustLocked updates the pacing rate from the delivery estimate. It grows
// exponentially in startup until the bottleneck bandwidth stops rising, then
// paces at btlBw with a BBR-style probe/drain gain cycle. Loss never enters
// here. A large standing queue (RTT well above the base) forces a drain, but a
// single noisy RTT sample cannot, so the estimator stays robust on a bursty
// path where the odd feedback is late.
func (r *rateControl) adjustLocked() {
	if r.startup {
		r.startRounds++
		if r.btlBw > r.lastBtlBw*1.25 {
			r.plateauRuns = 0
		} else {
			r.plateauRuns++
		}
		r.lastBtlBw = r.btlBw
		// Exit only once delivery has climbed to a meaningful level and then
		// plateaued, or after a hard cap, so noisy early feedback on a very
		// lossy path cannot end startup while the rate is still tiny.
		if (r.plateauRuns >= startupPlateau && r.btlBw > 4*r.minRate) || r.startRounds > 30 {
			r.startup = false
		}
		// Ramp off btlBw, but guarantee the rate climbs each round so it cannot
		// get stuck at the floor before delivery has a chance to grow.
		r.rate = startupGain * r.btlBw
		if grow := r.prevRate * 1.25; r.rate < grow {
			r.rate = grow
		}
		r.clampRateLocked()
		r.prevRate = r.rate
		return
	}
	// steady state: pace around btlBw, cycling a probe and a drain. The gain
	// cycle's own 0.75 phase drains any standing queue; we deliberately do NOT
	// add an RTT-triggered cut, because on a bursty path the odd late feedback
	// would misfire it and collapse the rate.
	r.gainIdx = (r.gainIdx + 1) % len(pacingGains)
	r.rate = pacingGains[r.gainIdx] * r.btlBw
	r.clampRateLocked()
}

const startupGain = 2.885 // ~2x per round (2/ln2), as in BBR startup

// pacingGains cycles a probe (>1) and a drain (<1) around btlBw.
var pacingGains = []float64{1.25, 0.75, 1, 1, 1, 1, 1, 1}

func (r *rateControl) clampRateLocked() {
	if r.rate < r.minRate {
		r.rate = r.minRate
	}
	if r.rate > r.maxRate {
		r.rate = r.maxRate
	}
}

// pacingRate returns the current send rate in bytes/sec.
func (r *rateControl) pacingRate(now time.Time) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.btlBw == 0 && r.startup {
		return r.rate // initial rate until the first delivery sample
	}
	return r.rate
}

// snapshot returns the current model for logging and the transport selector.
func (r *rateControl) snapshot() (btlBwBytes, rtPropSec float64, lossPPM uint32, startup bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.btlBw, r.rtProp, r.lastLossPPM, r.startup
}

// srttSec exposes the smoothed RTT (for logging/tests).
func (r *rateControl) srttSec() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.srtt
}

// bdpBytes is the bandwidth-delay product, used to size the pacer queue.
func (r *rateControl) bdpBytes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	bdp := r.btlBw * r.rtProp
	if bdp < 16*1500 || math.IsNaN(bdp) {
		return 16 * 1500
	}
	return int(bdp)
}
