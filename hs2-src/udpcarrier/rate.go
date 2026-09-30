package udpcarrier

import (
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// rateControl paces the carrier's send rate. It is delay-bounded and never
// loss-based: on a path with 26% base loss a loss-based controller (what TCP
// uses) would read every burst as congestion and collapse; this one ignores
// loss for pacing, lets FEC repair it, and instead keeps the QUEUE it builds at
// the bottleneck small. That is what keeps latency and jitter flat — the point
// of this carrier — and what keeps it from overflowing the bottleneck's buffer
// (loss FEC would then have to pay for) or bloating it for everything else.
//
// The signal is the standing one-way queue on the FORWARD path. Every data
// datagram carries its send time; the peer reports, each feedback interval,
// the minimum of (its receive time − that stamp) over the datagrams it got.
// The two clocks' offset is constant, so that minimum minus its own minimum
// over owdWindow is the queueing delay our data met — fresh every report,
// immune to a single late packet (it is a minimum over the whole interval),
// blind to the peer's own traffic on the return path, and still delivered
// when the bottleneck is tail-dropping most of what we send (Copa's standing
// RTT, measured one way). A peer too old to stamp falls back to the round-trip
// time over its minimum, taken as the smaller of the last two fresh samples.
//
// Control, per report, scaled per smoothed RTT so it behaves alike at 20 ms and
// 300 ms:
//
//   - startup: pace at startupGain × the delivered rate (BBR), so the rate
//     about doubles per round trip but never runs away from what the path
//     carries; a queue above lowQueue, or delivery that stops growing, ends it.
//   - queue above highQueue: cut below the delivered rate — deeper the deeper
//     the queue, so even a full buffer drains in about half a second — at most
//     once per measurement lag, and remember the delivered rate as the path's
//     capacity. Delivery is scaled up by the loss seen while no queue stood
//     (the path's own random loss), so random loss is not mistaken for a lack
//     of capacity.
//   - queue between lowQueue and highQueue: hold.
//   - no queue and the sender using its allowance: grow — 25% per RTT up to
//     the capacity estimate, 6% per RTT past it to probe for more.
//   - no queue but idle (application-limited): hold, so an idle carrier's
//     allowance never inflates past what the path was last shown to take.
type rateControl struct {
	mu sync.Mutex

	btlBw  float64 // bytes/sec, windowed max of delivered rate
	rtProp float64 // seconds, windowed min RTT
	srtt   float64 // seconds, smoothed RTT
	rate   float64 // bytes/sec, current pacing rate

	bwWindow []bwSample
	round    int
	rttMin   minFilter

	lastRxBytes uint64
	lastRxAt    time.Time
	haveRx      bool
	lastSent    uint64

	owdMin      minFilter // stamped one-way delay (+ clock offset), seconds
	owdRef      uint32    // first report's raw value, to unwrap the 32-bit ticks
	owdSet      bool
	haveOWD     bool       // the peer stamps: one-way samples replace RTT
	lastEcho    int64      // RTT fallback: echo of the last sample used
	rttQ        [2]float64 // RTT fallback: the last two fresh queue samples
	rttQN       int
	queue       float64   // last queue estimate, seconds (diagnostic)
	baseLoss    float64   // long-run average loss (see adjustLocked)
	probeLoss   float64   // loss measured inside base probes (at a lower rate)
	lossProbed  bool      // probeLoss has a value
	fullLoss    float64   // EWMA loss outside probes (at the full rate)
	policed     bool      // loss grows with our rate and no queue stands
	lastQueueAt time.Time // last report with a standing queue

	startup     bool
	plateauRuns int
	plateauAt   time.Time // next startup delivery-growth check (once per RTT)
	startRounds int       // reports seen in startup (hard exit backstop)
	lastDRate   float64
	capEst      float64   // bytes/sec: what the path gives us, learned while a queue stands
	emptyRuns   int       // consecutive reports with no queue while rate-limited
	probeAt     time.Time // next base probe
	probeStart  time.Time // current/last base probe began
	probeEnd    time.Time // ... and ends
	lastLossPPM uint32

	minRate float64
	maxRate float64

	// sent counts the data bytes the pacer put on the wire; written by the
	// pacer goroutine without the lock.
	sent atomic.Uint64
}

type bwSample struct {
	rate    float64
	round   int
	limited bool // taken while the sender used its allowance (a real capacity sample)
}

const (
	bwWindowRounds = 8
	owdWindow      = 10 * time.Second

	// The bottleneck queue the controller holds: it steers toward
	// targetQueue, correcting a deviation in about queueTau. Below lowQueue
	// the path counts as not full (capacity unknown, probe; loss is random).
	lowQueue    = 5 * time.Millisecond
	targetQueue = 10 * time.Millisecond
	queueTau    = 250 * time.Millisecond

	startupPlateau = 3
	startCap       = 100   // reports (~10 s): hard startup exit on a signal-less path
	startupGain    = 2.885 // BBR startup: 2/ln2 × the delivered rate
	deliveryCap    = 1.1   // with rate-dependent loss: pace ≤ this × (compensated) delivery
	looseCap       = 2.5   // otherwise: ≤ this × delivery (starved delay signal)
	rateLossExcess = 0.05  // full-rate loss this far above the probe's = rate-dependent
	fullLossAlpha  = 0.1   // per report: loss at full rate
	noQueueFor     = 2 * time.Second
	capAlpha       = 0.25 // capacity EWMA weight per report while a queue stands
	capFloorFrac   = 0.4  // capEst never below this x windowed-max delivery (anti-collapse)
	minDrainGain   = 0.5  // deepest drain: half the capacity
	maxQueueGain   = 1.1  // most the queue term paces above capacity
	probeGrow      = 1.04 // capacity probe per RTT while no queue stands

	// Base probe (BBR's ProbeRTT, one way): the base delay is a windowed
	// minimum, so a queue that never empties would, one window later, become
	// the base — and the target queue would then sit on top of it, creeping
	// up window after window. Every baseProbeEvery the carrier paces at
	// baseProbeGain of capacity for baseProbeDur (or an RTT and a report),
	// long enough for the queue to empty and a report to see the true base —
	// and, sending less, to measure how much of the loss is the path's own
	// (see adjustLocked). Cost ~2%.
	baseProbeEvery = 4 * time.Second
	baseProbeDur   = 300 * time.Millisecond
	maxProbeDur    = 1 * time.Second // an inflated RTT must not stretch a probe
	baseProbeGain  = 0.75
	limitedShare   = 0.8  // sent >= this share of the allowance = rate-limited
	maxLossComp    = 0.5  // loss compensation never assumes more than this
	baseLossAlpha  = 0.02 // per report, before the first probe: a ~5 s average
	probeLossAlpha = 0.5  // per report inside a probe
	overflowQueue  = 3 * targetQueue
	minRTTScale    = 0.03             // floor for the per-RTT growth scale, seconds
	rttSaneMax     = 30 * time.Second // an RTT sample above this is a clock glitch
)

// minFilter is a windowed minimum over roughly [window, 2×window): two buckets
// that rotate every window, so the minimum ages out without ever being
// "refreshed" by a single sample taken while a queue stood (the trap of
// resetting a min to the current value when it expires).
type minFilter struct {
	window    time.Duration
	cur, prev float64
	curAt     time.Time
	set       bool
}

func (f *minFilter) add(v float64, now time.Time) {
	// After an idle gap longer than the whole two-bucket window, both buckets
	// are stale (a low value from before the gap would otherwise linger in the
	// minimum for another window and understate the base): start fresh.
	if !f.set || now.Sub(f.curAt) >= 2*f.window {
		f.cur, f.prev, f.curAt, f.set = v, v, now, true
		return
	}
	if now.Sub(f.curAt) >= f.window {
		f.prev, f.cur, f.curAt = f.cur, v, now
		return
	}
	if v < f.cur {
		f.cur = v
	}
}

func (f *minFilter) min() float64 { return math.Min(f.cur, f.prev) }

func newRateControl() *rateControl {
	return &rateControl{
		rtProp:  0.05,
		srtt:    0.05,
		rate:    125_000, // ~1 Mbit/s to get going
		startup: true,
		rttMin:  minFilter{window: owdWindow / 2},
		owdMin:  minFilter{window: owdWindow / 2}, // > baseProbeEvery: always holds a probe
		minRate: 32_000,
		maxRate: 4e9,
	}
}

// onSent records data bytes put on the wire (pacer goroutine).
func (r *rateControl) onSent(n int) { r.sent.Add(uint64(n)) }

// onFeedback folds one receiver report into the model and adjusts the rate.
// echoNanos identifies which of our reports the RTT sample belongs to (a
// repeat is stale). owdTicks is the minimum stamped one-way delay the peer saw
// over its interval, in stampTick units with the clocks' offset folded in and
// wrapping at 32 bits; haveOWD is false when the peer saw no stamped data.
func (r *rateControl) onFeedback(now time.Time, rxDataBytes uint64, rttSampleSec float64, lossPPM uint32, echoNanos int64, owdTicks uint32, haveOWD bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.lastLossPPM = lossPPM
	fresh := echoNanos != 0 && echoNanos != r.lastEcho
	if fresh {
		r.lastEcho = echoNanos
	}
	if rttSampleSec > 0 && fresh {
		// Reject an absurd RTT sample (a wall-clock step while a report is in
		// flight can make (now - echo) huge or negative): it would poison srtt,
		// the base delay and the queue estimate. Anything over rttSaneMax or
		// more than 8x the smoothed RTT is dropped for pacing.
		if rttSampleSec <= rttSaneMax.Seconds() && (r.srtt == 0 || rttSampleSec <= 8*r.srtt) {
			r.rttMin.add(rttSampleSec, now)
			r.rtProp = r.rttMin.min()
			r.srtt += 0.25 * (rttSampleSec - r.srtt)
		} else {
			rttSampleSec, fresh = 0, false // do not use it for the queue either
		}
	}

	// Queue sample for this report.
	q, haveQ := 0.0, false
	if haveOWD {
		if !r.owdSet {
			r.owdRef, r.owdSet = owdTicks, true
		}
		v := float64(int32(owdTicks-r.owdRef)) * stampTick.Seconds()
		r.owdMin.add(v, now)
		q, haveQ, r.haveOWD = v-r.owdMin.min(), true, true
	} else if !r.haveOWD && rttSampleSec > 0 && fresh {
		r.rttQ[r.rttQN%2] = math.Max(0, rttSampleSec-r.rtProp)
		r.rttQN++
		q, haveQ = r.rttQ[0], true
		if r.rttQN > 1 {
			q = math.Min(r.rttQ[0], r.rttQ[1])
		}
	}
	q = math.Max(0, q)

	if !r.haveRx {
		r.haveRx, r.lastRxBytes, r.lastRxAt, r.lastSent = true, rxDataBytes, now, r.sent.Load()
		return
	}
	dt := now.Sub(r.lastRxAt).Seconds()
	// Guard against reordered/duplicated feedback: a too-small interval or a
	// non-increasing byte count would produce a bogus (often huge) sample.
	if dt < 0.02 || rxDataBytes < r.lastRxBytes {
		return
	}
	dRate := float64(rxDataBytes-r.lastRxBytes) / dt
	sent := r.sent.Load()
	sendRate := float64(sent-r.lastSent) / dt
	r.lastRxBytes, r.lastRxAt, r.lastSent = rxDataBytes, now, sent

	// A delivery sample only says what the path can carry when we were
	// sending all we were allowed to (or it beats the estimate anyway): an
	// application-limited interval (an idle tunnel) would otherwise drag the
	// estimate — and with it the delivery cap below — down to the idle rate,
	// and the next burst would have to climb back from there (BBR's rule).
	limited := sendRate >= limitedShare*r.rate
	if limited || dRate > r.btlBw {
		r.round++
		r.bwWindow = append(r.bwWindow, bwSample{rate: dRate, round: r.round, limited: limited})
		i := 0
		for i < len(r.bwWindow) && r.bwWindow[i].round <= r.round-bwWindowRounds {
			i++
		}
		r.bwWindow = r.bwWindow[i:]
		r.btlBw = 0
		for _, s := range r.bwWindow {
			r.btlBw = math.Max(r.btlBw, s.rate)
		}
	}

	r.adjustLocked(now, dt, dRate, limited, q, haveQ)
}

func (r *rateControl) adjustLocked(now time.Time, dt, dRate float64, limited bool, q float64, haveQ bool) {
	if haveQ {
		r.queue = q
	}
	loss := math.Min(float64(r.lastLossPPM)/1e6, 1)
	// The path's own random loss, which the pacing rate compensates for so
	// random loss is not mistaken for a lack of capacity: the long-run
	// average over every report (a loss burst itself empties the queue, so
	// sampling only queue-free reports would overstate it), leaving out
	// reports with a queue deep enough that a small buffer might overflow.
	//
	// That average cannot tell the path's loss from a per-flow policer's,
	// which grows with the rate and never queues. So the loss is also
	// measured while the base probe has the rate turned down: loss that does
	// not fall when we send less is the path's own; loss that does, with no
	// queue standing, is a policer's — ours to stay under, not to compensate.
	lagged := func(t time.Time, d float64) time.Time {
		return t.Add(time.Duration((r.srtt + d) * float64(time.Second)))
	}
	inProbe := !r.probeStart.IsZero() && !now.Before(lagged(r.probeStart, feedbackEvery.Seconds())) && now.Before(lagged(r.probeEnd, 0))
	switch {
	case inProbe && !r.lossProbed:
		r.probeLoss, r.lossProbed = loss, true
	case inProbe:
		r.probeLoss += probeLossAlpha * (loss - r.probeLoss)
	default:
		r.fullLoss += fullLossAlpha * (loss - r.fullLoss)
	}
	if !haveQ || q < overflowQueue.Seconds() {
		r.baseLoss += baseLossAlpha * (loss - r.baseLoss)
	}
	r.policed = r.lossProbed && r.fullLoss > r.probeLoss+rateLossExcess && now.Sub(r.lastQueueAt) > noQueueFor
	randomLoss := r.baseLoss
	if r.policed {
		randomLoss = math.Min(randomLoss, r.probeLoss)
	}
	comp := 1 / (1 - math.Min(randomLoss, maxLossComp))
	dComp := dRate * comp                       // what the path carried before its random loss
	scale := dt / math.Max(r.srtt, minRTTScale) // fraction of an RTT this report covers

	if r.startup {
		// Delivery growth is judged once per round trip: a rate change takes
		// that long to show at the receiver, so per report (every 100 ms) a
		// long path would look flat and end startup at a fraction of its
		// capacity.
		//
		// The backstop counts only reports where we used the allowance: it is
		// there for a saturated path that shows no queue, not for a carrier
		// that simply had little to send. Counting idle reports too ended
		// startup after ~10 s of light load (a pool carrier with only ACKs or
		// keepalives on it), and the next flow hashed onto it then crawled up
		// from the floor instead of ramping.
		if limited {
			r.startRounds++
		}
		if !now.Before(r.plateauAt) {
			if r.btlBw > r.lastDRate*1.25 {
				r.plateauRuns = 0
			} else if limited {
				r.plateauRuns++
			}
			r.lastDRate = math.Max(r.lastDRate, r.btlBw)
			r.plateauAt = now.Add(time.Duration(math.Max(r.srtt, feedbackEvery.Seconds()) * float64(time.Second)))
		}
		// Exit when a queue we built stands (only meaningful while we are
		// using the allowance — an app-limited queue is someone else's), when
		// delivery has plateaued, or, as a backstop, after startCap reports so
		// a slow path with no clean queue signal can never ramp to maxRate.
		if (limited && haveQ && q > lowQueue.Seconds()) ||
			(r.plateauRuns >= startupPlateau && dRate > 4*r.minRate) ||
			r.startRounds >= startCap {
			r.startup = false
			// capEst is what the path gives us; never below the current rate
			// on an app-limited exit (delivery understates capacity then) nor
			// below the floor, so the controller never starts out stuck.
			r.capEst = math.Max(math.Max(dComp, r.btlBw*comp), 2*r.minRate)
			if !limited {
				r.capEst = math.Max(r.capEst, r.rate)
			}
			r.rate = math.Min(r.rate, r.capEst)
			r.probeAt = now.Add(baseProbeEvery)
		} else if limited {
			r.rate = math.Max(startupGain*r.btlBw*comp, r.rate*math.Pow(1.25, math.Min(scale, 1)))
		}
		r.clampRateLocked()
		return
	}

	// Samples taken during a base probe (and until its effect has come back)
	// show an emptied queue on purpose: they refresh the base but say nothing
	// about spare capacity.
	probing := now.Before(r.probeEnd.Add(time.Duration((r.srtt + 2*feedbackEvery.Seconds()) * float64(time.Second))))
	if haveQ && q >= lowQueue.Seconds() {
		r.lastQueueAt = now
	}
	if haveQ {
		switch {
		case q >= lowQueue.Seconds():
			// A queue stands, so the bottleneck is busy and what the peer
			// receives is what the path can give us: learn it. But a queue we
			// did NOT build (cross traffic filling the buffer) also drops our
			// delivery, and EWMA-ing capEst down to that would spiral the rate
			// toward the floor. Floor capEst at a fraction of the windowed-max
			// delivery (btlBw is robust to a transient dip), so a shared path
			// costs throughput but never collapses; real capacity drops of more
			// than that fraction still track down through btlBw.
			r.capEst += capAlpha * (dComp - r.capEst)
			r.capEst = math.Max(r.capEst, capFloorFrac*r.btlBw*comp)
			r.emptyRuns = 0
		case probing:
			r.emptyRuns = 0
		case limited:
			// No queue for a while although we use the allowance: there may
			// be more capacity (cross traffic left, a faster path) — probe
			// gently, at most probeGrow per report however short the RTT.
			if r.emptyRuns++; r.emptyRuns >= 2 {
				r.capEst *= math.Pow(probeGrow, math.Min(scale, 1))
			}
		}
		// Proportional control of the queue around targetQueue: above it,
		// pace below capacity to drain it in about tau; below it, a little
		// above capacity to build it. The signal lags by about a report
		// interval plus a round trip; tau is kept well above that, so the
		// loop settles instead of oscillating.
		tau := math.Max(queueTau.Seconds(), 2.5*r.srtt)
		f := 1 + (targetQueue.Seconds()-q)/tau
		r.rate = r.capEst * math.Max(minDrainGain, math.Min(f, maxQueueGain))
	}
	if r.probeAt.IsZero() {
		r.probeAt = now.Add(baseProbeEvery)
	}
	if !now.Before(r.probeAt) && limited {
		// Bounded: a single inflated RTT (e.g. a wall-clock step while a
		// feedback frame is in flight) must not stretch the probe — during
		// which the rate is turned down — to seconds or minutes.
		d := math.Max(baseProbeDur.Seconds(), r.srtt+feedbackEvery.Seconds())
		d = math.Min(d, maxProbeDur.Seconds())
		r.probeStart = now
		r.probeEnd = now.Add(time.Duration(d * float64(time.Second)))
		r.probeAt = now.Add(baseProbeEvery)
	}
	// Delivery caps. When the loss at full rate clearly exceeds the path's
	// own (a per-flow policer, which drops without ever queueing so the delay
	// signal never fires), pace at most deliveryCap × what actually gets
	// through (mean delivery, loss-compensated): the policer's rate, not a
	// multiple of it. That also makes the sender wait on the pacer, which is
	// how the link pool sees the link as full and adds another. Otherwise a
	// loose cap only guards against a starved delay signal.
	//
	// Both are multiples of MEASURED delivery, so they apply only once the
	// window holds a sample taken at full allowance: delivery measured while
	// the carrier was nearly idle says nothing about the path, and capping at
	// a multiple of it would crush the allowance to the floor.
	if r.btlBw > 0 && r.hasLimitedSample() {
		c := looseCap * r.btlBw * comp
		if r.policed {
			c = deliveryCap * r.meanDelivery() * comp
		}
		r.rate = math.Min(r.rate, c)
		r.capEst = math.Min(r.capEst, c)
	}
	r.clampRateLocked()
}

// hasLimitedSample: the bandwidth window holds at least one sample taken while
// the sender used its allowance.
func (r *rateControl) hasLimitedSample() bool {
	for _, s := range r.bwWindow {
		if s.limited {
			return true
		}
	}
	return false
}

// meanDelivery is the mean delivered rate over the bandwidth window: the
// sustained rate, where btlBw (the max) is the peak a burst allowance or a
// lucky interval can reach.
func (r *rateControl) meanDelivery() float64 {
	if len(r.bwWindow) == 0 {
		return r.btlBw
	}
	sum := 0.0
	for _, s := range r.bwWindow {
		sum += s.rate
	}
	return sum / float64(len(r.bwWindow))
}

func (r *rateControl) clampRateLocked() {
	// A NaN compares false to everything, so it would slip past a plain
	// min/max and make the pacer send unpaced. Catch it explicitly and fall
	// back to the floor. (capEst feeding the rate is guarded the same way.)
	if math.IsNaN(r.rate) || r.rate < r.minRate {
		r.rate = r.minRate
	}
	if r.rate > r.maxRate {
		r.rate = r.maxRate
	}
	if math.IsNaN(r.capEst) || r.capEst < 0 {
		r.capEst = r.minRate
	}
}

// pacingRate returns the current send rate in bytes/sec.
func (r *rateControl) pacingRate(now time.Time) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now.Before(r.probeEnd) {
		// Probe at a fraction of the DELIVERED rate, not the pacing rate: if
		// the rate has run away above a policer (which drops the excess
		// without ever queueing, so the delay signal is silent), a fraction
		// of the pacing rate is still above the policer and loss stays high;
		// a fraction of what actually gets through drops below it, and the
		// loss fall is what marks it a policer (adjustLocked). It also drains
		// any real standing queue. btlBw is 0 until the first delivery.
		ref := r.rate
		if r.btlBw > 0 {
			ref = math.Min(ref, r.btlBw)
		}
		return math.Max(r.minRate, ref*baseProbeGain)
	}
	return r.rate
}

// snapshot returns the current model for logging and the transport selector.
func (r *rateControl) snapshot() (btlBwBytes, rtPropSec float64, lossPPM uint32, startup bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.btlBw, r.rtProp, r.lastLossPPM, r.startup
}

// queueSec is the last standing-queue estimate (for logging/tests).
func (r *rateControl) queueSec() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.queue
}

// srttSec exposes the smoothed RTT (for logging/tests).
func (r *rateControl) srttSec() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.srtt
}
