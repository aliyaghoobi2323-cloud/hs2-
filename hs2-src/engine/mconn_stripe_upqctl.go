package engine

import (
	"log"
	"os"
	"sync/atomic"
	"time"
)

// hl220: Iran's upload stripe gate under the edge queue controller's cap
// (HS2_QUEUE_CTL_UP with HS2_QUEUE_CTL_KEEP; HS2_QUEUE_CTL_STRIPE=0 turns it
// off).
//
// Two controllers read the same upload path queue: the stripe gate (target
// 3 ms: cut the width in half above it, grow under 1.5 ms) and the edge's
// queue controller, which holds the tunnel's upload at a cap with a shaper
// (htb + cake) and counts anything under qLow (8 ms, cake's own target is
// 5 ms) as no queue. Under KEEP the cap stays on the path, and the cap's own
// probing toward the path's rate (+2 %/tick) leaves the 3-10 ms the
// controller allows. The gate read each of those as the stripe's queue and
// halved: lab hl220 K1 (HS2_MCONN_STRIPE_LOG) u1b width 9 -> 2 -> 0 in two
// seconds with the gate's 1-s q at 0-1 ms, 30 cuts in 15 s (22 of them under
// 13 ms), the upload 523 -> 194 Mbit/s; u1a never wider than 2 (87 cuts).
// A narrow stripe does not lower the path's queue there (the cap does: the
// shaper drops/queues at the cap whatever the width), it only makes the upload
// window-bound (2 MiB smux window per sub / 90 ms ~ 170 Mbit/s each).
//
// So while an edge cap is on, the gate's target is the controller's: qLow.
// A queue the controller would call a queue (and lower the cap for) still
// cuts the stripe at once.
var stripeUpQCtlOff = os.Getenv("HS2_QUEUE_CTL_STRIPE") == "0"

// stripeUpQCtlTarget: the upload gate's target while the edge controller caps
// (0: not in force).
func stripeUpQCtlTarget() time.Duration {
	if stripeUpQCtlOff || !queueCtlKeep || qCtlStatEdge.capped.Load() <= 0 {
		return 0
	}
	return qLow
}

// hl222: one halving per queue spike under an edge cap
// (HS2_QUEUE_CTL_STRIPE_SPIKE=0 turns it off; in force only with
// stripeUpQCtlTarget).
//
// hl220 F1 u1b: when the stripe grew from 9 to 12 subs the pool dialed 5
// new reverse links, their start burst made one 8.1 ms sample, and the gate
// halved on two 100 ms samples of that one spike (7 -> 3 -> 1), then held
// every stripe for 2 s and grew back one sub per 500 ms: 373 Mbit/s where
// B had 437. Under a cap the controller's shaper owns the path's queue; the
// gate only has to step back once per spike. So, while capped:
//   - a sub that starts sending (first chunk, or after a second idle) starts
//     a warm-up window (stripeUpWarm, about one RTT plus a sample): its burst
//     shows in the socket RTTs for that long and does not cut;
//   - after a cut, the next cut needs a sample at or under the threshold in
//     between (a new spike) or stripeQGrow of queue that stayed;
//   - a sample under half the target after the cut (the queue is gone)
//     ends the cut's hold: growth may resume stripeQGrow after the cut
//     instead of stripeCutHold.
//
// A queue at twice the threshold and at least the controller's qHigh
// (15 ms: a queue it lowers the cap for) cuts at once, but (hl223) at most
// once per stripeUpBigGap below qFastDelay (40 ms); 40 ms and more cuts every
// sample. Not
// 2 x the threshold alone: with HS2_MCONN_STRIPE_UP_SPLIT the cut threshold
// is 0.67 x 8 = 5.4 ms, and lab hl222 qS1 u1b saw one 11-13 ms spike of the
// cap's own +2 % probe past the path take the width 11 -> 0 in 400 ms.
var stripeUpSpikeOff = os.Getenv("HS2_QUEUE_CTL_STRIPE_SPIKE") == "0"

const (
	stripeUpWarm = 250 * time.Millisecond
	stripeUpIdle = time.Second
	// stripeUpBigGap (hl223): under a cap, a queue of qHigh..qFastDelay
	// halves the width at most once per this.
	stripeUpBigGap = 200 * time.Millisecond
)

// stripeUpSpikeStat: counters (HS2_MCONN_STRIPE_LOG prints them per cut).
var stripeUpSpikeStat struct {
	warm, spike, release, cuts, probe, onset, first atomic.Int64
}

func stripeUpSpikeOn() bool { return !stripeUpSpikeOff && stripeUpQCtlTarget() > 0 }

// stripeUpNotePushLocked (under m.mu): best takes a stripe chunk at now.
func (m *mConn) stripeUpNotePushLocked(best *msub, now time.Time) {
	if m.iran && (best.stPushed.IsZero() || now.Sub(best.stPushed) >= stripeUpIdle) {
		m.stStartAt = now
	}
}

// stripeUpCapSkipLocked (Iran, under m.mu, a sample over the cut threshold):
// whether this sample does not cut (the warm-up or the same spike).
func (m *mConn) stripeUpCapSkipLocked(now time.Time, cutQ, cutAt time.Duration) bool {
	if !m.iran || !stripeUpSpikeOn() {
		return false
	}
	onsetGate := qCtlStatEdge.onsetGate(now)
	if cutQ >= qFastDelay && cutQ > onsetGate {
		return false
	}
	why := ""
	switch {
	case cutQ < qHigh && qCtlStatEdge.probing(now):
		// hl228: the controller's own probe (its cap stepped past the
		// path's rate this tick or the last) queues 6-14 ms; it drains that
		// itself (lab hl227 zS1 u1b: "skip spike/warm" then cuts, width
		// 11 -> 5 -> 4 every ~7 s, -5.2 %).
		stripeUpSpikeStat.probe.Add(1)
		why = "probe"
	case cutQ <= onsetGate:
		// hl229: a new upload's onset under a cap at seen (lab hl228 aS3
		// u1b: a 23 ms onset tick made 2 cuts, dips 299/291); the controller
		// drops to qSeenFrac x seen with no hold and drains it. hl231: after
		// that drop, samples up to the onset tick's own queue (any size: lab
		// hl230 u8 77-91 ms) for qProbeQuiet — a queue that grows past it cuts.
		stripeUpSpikeStat.onset.Add(1)
		why = "onset"
	case cutQ < qFastDelay && qCtlStatEdge.probing(now) && !m.stCapCutAt.IsZero() && now.Sub(m.stCapCutAt) < qProbeQuiet:
		// hl232 (lab hl231 eS1/eS2 u1a): a probe step past the path queued
		// 20-36 ms for ~0.5 s; "big" below skips only within stripeUpBigGap
		// of a cut, so the width went 10 -> 5 -> 2 -> 1 (237/275 Mbit/s that
		// second vs B ~460) and the queue was gone before the controller's
		// tick (6 ms): it never saw its probe overshoot. Under a probe, one
		// cut, then the queue (under qFastDelay) is the controller's.
		stripeUpSpikeStat.probe.Add(1)
		why = "probecut"
	case m.stripeUpFirstLocked(now):
		stripeUpSpikeStat.first.Add(1)
		why = "first"
	case cutQ >= max(2*cutAt, qHigh):
		// hl223: a queue the controller lowers the cap for, under its fast
		// threshold, halves at most once per stripeUpBigGap: the cap's own
		// probe past the path (13-17 ms) took the width 10 -> 0 in 400 ms
		// (lab hl222 rS1 u1b) while the controller drains it within a tick.
		if m.stCapCutAt.IsZero() || now.Sub(m.stCapCutAt) >= stripeUpBigGap {
			return false
		}
		stripeUpSpikeStat.spike.Add(1)
		why = "big"
	case !m.stStartAt.IsZero() && now.Sub(m.stStartAt) < stripeUpWarm:
		stripeUpSpikeStat.warm.Add(1)
		why = "warm"
	case m.stCapCut && now.Sub(m.stCapCutAt) < stripeQGrow:
		stripeUpSpikeStat.spike.Add(1)
		why = "spike"
	default:
		return false
	}
	if stripeLog {
		log.Printf("stripe upcap %x: skip %s q=%v at=%v w=%d", m.id&0xffffff, why, cutQ.Round(100*time.Microsecond), cutAt, m.stW)
	}
	return true
}

// stripeUpFirstLocked (hl235, under a cap): the gate's first samples of a
// fresh upload stripe do not cut. Lab hl234 hS2 u1b: the stripe's first gate
// decision came in the same millisecond as its first sub (before any stripe
// chunk, so no warm-up window yet) and read 6.6 ms — the raw first link's own
// start, not the stripe's — over the 5.4 ms cut threshold: a cut at width 0
// that held growth 528 ms. A cut now needs at least stripeUpFirstN samples
// and stripeUpWarm (about an RTT of the stripe's data) since its first one.
// A stripe whose gate was not started here (stGate0 zero) is not affected.
func (m *mConn) stripeUpFirstLocked(now time.Time) bool {
	if m.stGate0.IsZero() {
		return false
	}
	return m.stGateN < stripeUpFirstN || now.Sub(m.stGate0) < stripeUpWarm
}

const stripeUpFirstN = 2

// stripeUpCapCutLocked: the gate cut at now (under a cap: remember it).
func (m *mConn) stripeUpCapCutLocked(now time.Time) {
	if !m.iran || !stripeUpSpikeOn() {
		m.stCapCut = false
		return
	}
	stripeUpSpikeStat.cuts.Add(1)
	m.stCapCut, m.stCapCutAt, m.stCapHeld = true, now, true
}

// stripeUpCapClearLocked: a sample at or under the cut threshold. Under half
// the target after a capped cut, the cut's hold ends (stripeQGrow after it).
func (m *mConn) stripeUpCapClearLocked(now time.Time, q, target time.Duration) {
	m.stCapCut = false
	if !m.stCapHeld || q >= target/2 {
		return
	}
	m.stCapHeld = false
	if !m.iran || !stripeUpSpikeOn() {
		return
	}
	if m.stGrowAt.After(m.stCapCutAt) {
		m.stGrowAt = m.stCapCutAt
	}
	stripeCutRelease(m.stCapCutAt)
	stripeUpSpikeStat.release.Add(1)
	if stripeLog {
		log.Printf("stripe upcap %x: release after %v w=%d", m.id&0xffffff, now.Sub(m.stCapCutAt).Round(time.Millisecond), m.stW)
	}
}

// stripeCutRelease: the global hold of the cut at cutAt (if no later cut)
// ends stripeQGrow after it.
func stripeCutRelease(cutAt time.Time) {
	g := &stripeGlobal
	g.mu.Lock()
	if g.cutAt.Equal(cutAt) {
		g.cutAt = cutAt.Add(stripeQGrow - stripeCutHold)
	}
	g.mu.Unlock()
}
