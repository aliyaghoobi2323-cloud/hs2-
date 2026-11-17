package engine

import (
	"fmt"
	"math"
	"time"
)

// The path's queue (HS2_QUEUE_CTL, the exit side). Each link runs its own BBR,
// and many of them together keep a standing queue at the path's bottleneck —
// a router between the servers that hs2 cannot manage: in the lab every
// link's round trip went from 82 ms to ~176 ms under load, and every user,
// a game beside the downloads included, waits that queue. fq_codel on this
// server does nothing about it: the server's own interface is faster than
// the path, so nothing queues here.
//
// Sending slightly under the path's rate moves the bottleneck onto this
// server, where a flow-queueing AQM (cake) keeps each flow's queue short and
// a game's packets apart from a download's: with the tunnel's egress capped
// at 92% of the path in the lab, the echo under 8 downloads went from 176 to
// 81 ms (the empty path), p95 from 254 to 81 ms at 1% loss, for 8% of the
// throughput. The path's rate is not known and moves, so it is found the way
// cake-autorate does on variable links: from the delay the links see.
//
// qCtl is that controller, pure (no I/O): each tick it gets the links'
// queueing delay (smoothed RTT over the lowest, the median of the busy links)
// and what they delivered, and says the rate to cap the tunnel's egress at,
// 0 for no cap. It caps only while the delay shows a queue it causes:
//   - a queue builds (delay above qHigh on two ticks) while the links carry
//     traffic: cap at qStartFrac of what they delivered — the queue drains;
//   - capped, a queue still above qHigh: down to qDown of the cap or
//     qBackFrac of what the links delivered, whichever is lower (a path
//     that lost half its rate is followed at once, not in steps), then no rise for
//     qHoldTicks. No floor: the cap follows what the links really delivered,
//     and a path throttled to half must be followed to half;
//   - capped, no queue and the cap is the limit (the links deliver at least
//     qFullFrac of it): up a step a tick, to find the path's rate again —
//     qUpFast while well under the last overshoot's rate, qUpSlow near it
//     (each overshoot is a queue every user waits);
//   - demand fell well under the cap for qIdleTicks: no cap (it costs nothing
//     then, and the next queue sets a fresh one);
//   - not its own queue: for qStuckTicks the cap was the limit (the path
//     delivered all the tunnel sent) and the queue stayed — others fill the
//     bottleneck, and holding back only gives them the tunnel's share: no
//     cap, and none for a while (doubling, up to qReleaseMax);
//   - a lossy path (retransmits over qMaxLoss of what the links send): no
//     cap — the loss, not the queue, holds the users there. The loss counted
//     is the path's, not the cap's own: while capped, the shaper's own drops
//     (cake drops what the links send over the cap, and each comes back as a
//     retransmit) are taken out of the retransmits by the runner (qLossOf).
//     In production on 2026-11-09 they were not: a cap at 64 Mbit/s "saw"
//     2–26% loss that the cap itself made, was removed 10–14 s after it was
//     set, the queue came back, the cap with it: 73 caps in 31 minutes.
//
// Against flapping (the same day): a cap is set only after the queue shows
// on qStartTicks ticks in a row (~10 s — not a burst, not a path's natural
// jitter: srtt-min_rtt there has a median of ~2 ms, p75 3–4 ms); one is kept
// at least qMinCapTicks before loss can take it off; and every removal but
// the idle one (demand gone) keeps it off for a while, doubling while it
// keeps coming back (qReleaseMin..qReleaseMax).
type qCtl struct {
	rate       float64   // the cap, bytes/s; 0 = none
	high       int       // consecutive ticks with a queue, uncapped
	stuck      int       // capped ticks the queue stayed while the cap was the limit
	hold       int       // ticks before the cap may rise again (after a step down)
	idle       int       // capped ticks with demand well under the cap
	grace      int       // ticks the first cap gets to drain the queue it found
	lossy      int       // capped ticks with loss over qMaxLoss
	age        int       // ticks the current cap has been on
	path       float64   // the cap at the last overshoot: the path's rate, in the cap's units
	pathAt     time.Time // ... when it was seen (kept across caps for qPathMemory)
	good       float64   // the last cap that raised no queue
	limbo      int       // capped ticks the cap neither bound nor went idle
	rev        int       // capped ticks the links received over qRevFrac of what they sent
	release    time.Time
	fitAt      time.Time // HS2_QUEUE_CTL_KEEP: the cap last bound with no queue (it fits the path)
	fitRate    float64   // ... and that cap: a kept cap is never above it (a cap that rose past the path after it fit is not kept there)
	kept       bool      // HS2_QUEUE_CTL_KEEP: the cap is held through idle (logged once until it binds again)
	revOff     bool      // the back-off (release) was set by the reverse rule (the other direction's queue)
	lostQ      int       // HS2_QUEUE_CTL_KEEP (hl219): consecutive ticks with a queue under a cap that does not bind
	clearG     float64   // HS2_QUEUE_CTL_KEEP (hl219): what the links delivered the last tick, if it had no queue (else 0)
	seen       float64   // HS2_QUEUE_CTL_KEEP (hl223): the most the links delivered on a binding tick: the path's delivery ceiling (qseen.go)
	seenAt     time.Time // ... last raised or set
	probe      int       // HS2_QUEUE_CTL_KEEP (hl224): binding no-queue ticks in a row at/above qSeenFrac x seen (qseen.go)
	probeMax   float64   // ... the most the links delivered on them
	probeHi    int       // ... of them in a row delivering qProbeGrow x seen (hl225: two before qUpFirst)
	probeWait  int       // hl225: clear ticks before the next probe (0: qProbeTicks); doubles on each probe that finds the same path
	onsetG     float64   // hl226: what the links delivered at the first cap's onset, no path or seen known (the guess's source)
	seenGuess  bool      // hl226: seen is the onset's rate, not yet a binding queue tick's (the first real one replaces it)
	quiet      int       // hl226: capped ticks in a row with the links under half the cap
	burst      int       // hl226: ticks left in which a new burst's first full tick probes at once
	burstFrom  float64   // hl233: the cap a burst probe stepped up from (onsetTarget)
	burstAt    time.Time // hl233: ... when
	postBurst  int       // hl228: ticks left after a burst probe in which a clear tick over qSeenGrow x seen is growth at once
	probeAt    time.Time // hl228: the last tick a probe step raised the cap (probing)
	up         bool      // hl228: the edge's (upload) controller: a probe step stops at qSeenGrow x seen for one tick
	onset      int       // hl229: ticks left in which a new burst's single small queue tick under a cap at seen takes no hold (qseen.go onsetQueue)
	onsetUntil time.Time // hl229: until when the edge's stripe gate leaves an onset queue under qOnsetGate to the controller
	onsetAt    time.Time // hl229: the tick onsetUntil was last set (the runner publishes it)
	clearRun   int       // hl229: binding ticks in a row with no queue at a cap at qSeenFrac x seen (kept at qKeepSure x seen after qKeepSureTicks)
	releaseFor time.Duration

	onsetQ     time.Duration // hl231: the onset tick's queue (onsetQueue; 0: the window from onsetStart): the edge's stripe gate skips samples up to it
	onsetGuess bool          // hl231: the onset window was opened under a cap near an unconfirmed guess (seenGuess)
	seenClear  bool          // hl232: seen is clearMax, set when the cap was kept (a floor of the path, not a queue tick's ceiling)
	clearMax   float64       // hl232: the most the links delivered on a loaded clear tick since the last queue (qseen.go)
	clearAt    time.Time     // ... when
	seenDropAt time.Time     // hl232: when growth last dropped an unconfirmed guess (dropGuess)
}

const (
	qHigh        = 15 * time.Millisecond // a queue
	qLow         = 8 * time.Millisecond  // no queue (cake's own target is 5 ms)
	qStartFrac   = 0.95                  // the first cap, of what the links delivered: drains the queue in seconds
	qBackFrac    = 0.97                  // a later overshoot: what they delivered then is the path's rate
	qDown        = 0.93
	qUpFirst     = 1.10 // a rise before any overshoot (the path's rate not known)
	qUpFast      = 1.02 // a rise well under the last overshoot (the path's rate)
	qPathMemory  = 10 * time.Minute
	qUpSlow      = 1.005 // ... and near it: each overshoot is a queue every user waits
	qNearPath    = 0.985
	qRegrowFrac  = 1.05 // HS2_QUEUE_CTL_KEEP: a binding cap this far past the path with no queue: the path grew
	qNearFill    = 0.97 // HS2_QUEUE_CTL_KEEP (hl216): links that fill this much of a cap near the path: qUpFast there too
	qStartTicks  = 5    // ~10 s of a queue, uncapped, before a cap: not a burst, not jitter
	qMinCapTicks = 15   // ~30 s a cap is kept at least before loss takes it off
	qGraceTicks  = 2    // ~4 s for the first cap to drain the queue it found
	qHoldTicks   = 5    // ~10 s after a step down before it rises again
	qFullFrac    = 0.90
	qStuckTicks  = 5 // ~10 s of a queue that does not drain under a binding cap
	qIdleTicks   = 15
	qMinRate     = 125000 // 1 Mbit/s: below this there is nothing to shape
	qLostTicks   = 2      // HS2_QUEUE_CTL_KEEP (hl219): queue ticks under a non-binding cap before it follows the path down
	qReleaseMin  = time.Minute
	qReleaseMax  = 10 * time.Minute
	// qMaxLoss: a path that loses more than this of what the links send
	// limits them by its loss, not by its queue: capping it cost 8% of the
	// throughput at 3% loss in the lab, for no significant gain in the median
	// echo (the queue it removes is not what holds the users there). No cap
	// then — the tunnel as without the controller.
	qMaxLoss   = 0.02
	qLossTicks = 3
)

// qFastDelay, qFastTicks (HS2_QUEUE_CTL_FAST, lat58): a queue of qFastDelay or
// more is capped after qFastTicks (~4 s) instead of qStartTicks (~10 s). Every
// user, games and calls included, waits that queue on the path (lab, 8
// uploads: the links' round trip 75 → 135-165 ms for the first ~10 s of every
// burst); a queue that big is not a path's jitter (production: srtt-min_rtt
// median ~2 ms, p75 3-4 ms), nor a burst once it lasts two ticks.
//
// qFastMinRate (lat59): the fast start only while the links deliver at least
// this much. The edge's links also see the download direction's queue (what
// they send is acknowledged behind it): in the lab under 8 downloads the
// edge's light upload (7 Mbit/s) saw 57-130 ms, FAST capped it at 6.8 Mbit/s
// after 2 ticks and the cap stayed (demand never under half of it, never the
// limit either): a call's p99 went to ~2 s. A queue the tunnel makes itself
// comes with real traffic; under this rate the slow start (qStartTicks) holds.
const (
	qFastDelay   = 40 * time.Millisecond
	qFastTicks   = 2
	qFastMinRate = qCapMinRate
)

// The stuck cap (qctl61). Under 8 downloads the edge's light upload
// (~7 Mbit/s) saw the download's queue (what it sends is acknowledged behind
// it), was capped at 6.7-9.1 Mbit/s, and the cap never came off: the shaped
// links delivered ~4.5 Mbit/s — under qFullFrac of the cap (no rise), over
// half of it (no idle release), the queue gone (no stuck release) — and the
// shaper held the downloads' own ACKs, which leave by the same links, under
// the cap: d8 throughput -18%, a call's p99 to seconds (lab, 6 of 13 runs).
// Three rules, each enough for that case:
//   - qCapMinRate: no cap starts while the links deliver under it — a tunnel
//     that sends a few Mbit/s does not make the queue of a path that carries
//     hundreds; the queue it sees is someone else's;
//   - qRevFrac: the links receive more than qRevFrac times what they deliver
//     — the queue they see is the other direction's (their ACKs wait behind
//     it): no cap starts, and one in place comes off after qRevTicks;
//   - qLimboTicks: a cap that for that long neither binds (the links deliver
//     under qFullFrac of it) nor goes idle comes off, as an idle one (no
//     back-off) — it shapes nothing the controller measures, only what it
//     does not (the reverse ACKs).
const (
	qCapMinRate = 25e6 / 8 // 25 Mbit/s, in bytes/s
	qRevFrac    = 4
	qRevTicks   = 2
	qLimboTicks = 8 // ~16 s
)

// qIn is one tick's measurement.
type qIn struct {
	now   time.Time
	delay time.Duration // median queueing delay of the busy links; <0: no busy link
	g     float64       // bytes/s the links delivered (acknowledged) this tick
	loss  float64       // share of the segments sent that were retransmits
	rx    float64       // bytes/s the links received (the other direction)
	own   time.Duration // hl234: the backlog in the tunnel's own shaper, as wait at the cap (part of delay)
}

// reverse: the links carry far more the other way — the delay they see is
// that direction's queue, not one this side's egress makes.
func (in qIn) reverse() bool { return in.rx > qRevFrac*in.g }

// step returns the new cap (0: none) and a log line when it changed in kind.
func (q *qCtl) step(in qIn) (float64, string) {
	busy := in.delay >= 0 && in.g >= qMinRate
	if q.rate == 0 {
		blocked := in.now.Before(q.release)
		if blocked && q.ownQueue(in) {
			// hl205 (HS2_QUEUE_CTL_KEEP): a back-off the other direction's
			// queue set holds only while that direction dominates. When this
			// side's links carry the traffic and see a big queue themselves
			// (lab u8 8 s after a download: 280-350 ms, uncapped), the queue
			// is their own: the cap may start again.
			blocked = false
		}
		if !busy || blocked || in.delay < qHigh || in.loss > qMaxLoss ||
			in.g < qCapMinRate || in.reverse() {
			q.high = 0
			return 0, ""
		}
		need := qStartTicks
		if queueCtlFast && in.delay >= qFastDelay && in.g >= qFastMinRate {
			need = qFastTicks
		}
		if q.high++; q.high < need {
			return 0, ""
		}
		// A queue forms while the downloads' BBR is still ramping up, so what
		// the links deliver now can be well under the path. A path rate seen in
		// the last qPathMemory starts the cap just under it; otherwise it is
		// not known yet and the cap rises fast (qUpFirst) to the first
		// overshoot, which tells it. No hold: in the lab a hold and 2% steps
		// cost a 20 s download ~9% of its throughput.
		q.high, q.stuck, q.idle, q.hold, q.grace, q.good, q.lossy, q.age, q.limbo, q.rev = 0, 0, 0, 0, qGraceTicks, 0, 0, 0, 0, 0
		q.clearRun, q.onset = 0, 0
		start := qStartFrac * in.g
		if q.path > 0 && in.now.Sub(q.pathAt) < qPathMemory {
			start = math.Max(start, qBackFrac*q.path)
		} else {
			q.path = 0
		}
		q.rate = q.floorOf(start)
		q.onsetG = 0
		if q.path == 0 && !q.seenFresh(in.now) {
			q.onsetG = in.g // hl226: the path guess once the grace ends
		}
		// hl224: no seen from the onset tick — BBR is still ramping (lab
		// hl223 d1a: a 215 ms onset at 460 kept the cap at 448 for 30 s).
		q.probe, q.probeMax = 0, 0
		if q.revOff && in.now.Before(q.release) {
			q.release, q.revOff = time.Time{}, false // ownQueue lifted it
		}
		return q.rate, fmt.Sprintf("queue: the path's queue holds the links %s over their lowest round trip at %.1f Mbit/s — tunnel egress capped at %.1f Mbit/s so it drains (HS2_QUEUE_CTL)",
			in.delay.Round(time.Millisecond), mbitps(in.g), mbitps(q.rate))
	}
	q.age++
	if in.g < q.rate/2 {
		q.quiet++
	} else {
		if q.quiet >= qBurstQuiet {
			q.burst = qBurstTicks // hl226: a new burst under the (kept) cap
			q.onsetStart(in.now)  // hl229
		}
		q.quiet = 0
	}
	if in.delay >= qHigh {
		q.burst = 0
	}
	if in.delay >= qHigh || in.g < q.rate/2 {
		q.postBurst = 0 // hl228: the window after a burst probe ends at a queue or a pause
	}
	prevClear := q.clearG
	prevSeen := q.seen
	if !q.seenFresh(in.now) {
		prevSeen = 0
	}
	if in.delay >= qHigh && q.probe > q.probeNeed() && q.seenFresh(in.now) {
		// hl225: a probe ran into a queue. At the same path (it delivered no
		// more than qProbeGrow x seen) the next one waits twice as long: a
		// fixed path is not overshot every hold+3 ticks (lab hl224 wS2 d8).
		if q.probeMax <= qProbeGrow*q.seen {
			q.probeWait = min(2*q.probeNeed(), qProbeWaitMax)
		} else {
			q.probeWait = 0
		}
	}
	if in.g < q.rate/2 {
		q.probeWait = 0 // hl225: a new burst probes at once again (the path may have changed while idle)
	}
	if in.delay >= qHigh && q.probeMax > 0 && q.seenFresh(in.now) && q.probeMax > q.seen {
		// hl224: a growth probe ran into a queue: the path delivered the
		// most the probe saw.
		q.seen, q.seenAt, q.seenGuess, q.seenClear = q.probeMax, in.now, false, false
		prevSeen = q.seen
	}
	if in.g >= qFullFrac*q.rate && q.grace == 0 {
		// a binding tick: the path delivered this much. hl224: not while
		// the first cap drains the queue it found (the onset's ramp).
		q.noteSeen(in, prevSeen)
	}
	q.noteClearMax(in) // hl232
	defer func() {
		if q.burst > 0 {
			q.burst--
		}
		if q.postBurst > 0 {
			q.postBurst--
		}
		if q.onset > 0 {
			q.onset--
		}
		if in.delay >= 0 && in.delay < qHigh {
			q.clearG = in.g
		} else {
			q.clearG = 0
		}
		if in.delay < 0 || in.delay >= qLow {
			q.probe, q.probeMax, q.probeHi = 0, 0, 0 // hl224: a probe needs clear ticks in a row
		}
	}()
	if q.hold > 0 {
		q.hold--
	}
	if q.grace > 0 && in.delay >= qHigh {
		q.grace-- // the queue the cap found is still draining
		return q.rate, ""
	}
	q.grace = 0
	if q.onsetG > 0 {
		// hl226: the onset's rate is the path's guess (the links waited a
		// queue there: they delivered what the path carries, unless BBR still
		// ramped). With no path or seen known the cap rose qUpFirst from the
		// onset cap (0.95 x it): lab hl225 u1a 476 -> 452.5 -> 497.7 -> 547.5,
		// a 96-128 ms queue, or a 34 ms one that left the cap at 449.5, 5 %
		// under the path, for the rest of the burst (u1b -5.8 %). Now it rises
		// qUpFast to qSeenFrac of it, then probes; the first binding queue
		// tick replaces it, and a path past it shows as growth (qSeenGrow).
		if q.path == 0 && !q.seenFresh(in.now) && qSeenOn() && q.onsetG >= qCapMinRate {
			q.seen, q.seenAt, q.seenGuess, q.seenClear = q.onsetG, in.now, true, false
			prevSeen = q.seen
		}
		q.onsetG = 0
	}
	wasKept := q.kept
	if in.g >= qFullFrac*q.rate && in.delay >= 0 && in.delay < qHigh {
		q.fitAt, q.fitRate, q.kept = in.now, q.rate, false
	}
	q.noteClearRun(in)
	// HS2_QUEUE_CTL_KEEP: an idle kept cap (the edge's, while a download
	// runs) is not chased off by the other direction's queue: it holds back
	// nothing then, and a back-off would leave the next upload uncapped.
	if in.reverse() && in.g < q.rate && !(queueCtlKeep && in.g < qFullFrac*q.rate/2) {
		if q.rev++; q.rev >= qRevTicks {
			defer func() { q.revOff = true }()
			return q.drop(in.now, fmt.Sprintf("queue: the links receive %.1f Mbit/s and send %.1f — the queue they see is the other direction's; cap removed for %%s",
				mbitps(in.rx), mbitps(in.g)))
		}
	} else {
		q.rev = 0
	}
	if in.g >= qFullFrac*q.rate || in.g < qFullFrac*q.rate/2 {
		q.limbo = 0
	} else if q.limbo++; q.limbo >= qLimboTicks {
		// As idle, no back-off: the cap holds back nothing the links
		// deliver; a queue that comes back sets a fresh one (at the path's
		// rate, qPathMemory). In u8 the caps that rose past the path (1080
		// Mbit/s cap, 900 delivered) came off here, and a minute's back-off
		// would leave the next queue uncapped.
		n := q.limbo
		q.rate, q.stuck, q.idle, q.lossy, q.limbo, q.rev = 0, 0, 0, 0, 0, 0
		q.releaseFor = 0
		return 0, fmt.Sprintf("queue: for %d ticks the links delivered %.1f Mbit/s under a cap that neither binds nor idles — cap removed", n, mbitps(in.g))
	}
	if in.loss > qMaxLoss {
		if q.lossy++; q.lossy >= qLossTicks && q.age >= qMinCapTicks {
			q.rate, q.lossy, q.stuck, q.idle = 0, 0, 0, 0
			q.backOff(in.now)
			return 0, fmt.Sprintf("queue: the path loses %.1f%% of what the links send — it limits them by its loss, not its queue; cap removed for %s", 100*in.loss, q.releaseFor)
		}
	} else {
		q.lossy = 0
	}
	if !(in.delay >= qHigh && in.g >= qFullFrac*q.rate/2 && in.g < qFullFrac*q.rate) {
		q.lostQ = 0 // hl219: not a queue tick under a cap that does not bind
	}
	switch {
	case in.g < qFullFrac*q.rate/2:
		q.stuck = 0
		if q.idle++; q.idle >= qIdleTicks {
			pathFresh := q.path > 0 && in.now.Sub(q.pathAt) < qPathMemory
			if queueCtlKeep && (pathFresh || (!q.fitAt.IsZero() && in.now.Sub(q.fitAt) < qPathMemory)) {
				// HS2_QUEUE_CTL_KEEP: the path's rate is known and fresh (an
				// overshoot, or a cap that bound with no queue) — keep the cap
				// just under it, so the next burst finds no queue to fill. It
				// binds nothing while idle: it costs nothing then.
				q.idle = 0
				keep := q.rate
				if q.fitRate > 0 {
					keep = math.Min(keep, q.fitRate)
				}
				if pathFresh && !q.fitAt.After(q.pathAt) {
					// hl208: a cap that fit after the path's rate was
					// learned (it crept up with no queue) is kept as it
					// is; 0.97 of an older overshoot kept d8 ~4 % low.
					keep = math.Min(keep, q.floorOf(qBackFrac*q.path))
				} else if pathFresh {
					// hl216: ... but never above that rate: a single upload
					// fit 478.6 with no queue past a 465 path, and the next
					// 8 uploads found it over the path (onset 42-97 ms).
					keep = math.Min(keep, q.path)
				}
				// hl229: a guess (the onset's rate) no queue confirmed is
				// no ceiling: the cap that fit. hl230: nor a floor — an
				// unconfirmed guess is never what is kept (lab hl229 cS1: a
				// ramp guess of 412.4 kept the download at 406).
				q.keepClearSeen(in.now) // hl232
				if q.seenFresh(in.now) && !q.seenGuess {
					// hl223: never under qSeenFrac of what the path
					// delivered (u1b was kept at 0.95 of it: the onset
					// cap through 0.97 x min(cap, good)). hl224: and
					// never over it (uS1: a cap that crept +0.5 %/tick
					// with no queue to 481 was kept there; the next u8
					// onset queued 76 ms).
					keep = q.floorOf(math.Min(q.rate, qSeenFrac*q.seen))
					if q.seenClear {
						// hl232: seen from clear ticks (a floor of
						// the path): never kept over qNearPath of it
						// (lab hl231 eS3 u1b: kept at 491.4 with the
						// path ~475).
						keep = q.floorOf(math.Min(q.rate, qKeepSure*q.seen))
					} else if q.keepSure() {
						keep = q.floorOf(qKeepSure * q.seen) // hl229
					}
				}
				if keep != q.rate || !q.kept {
					q.rate, q.kept = keep, true
					return q.rate, fmt.Sprintf("queue: demand is well under the cap — cap kept at %.1f Mbit/s, the path's rate seen in the last %s (HS2_QUEUE_CTL_KEEP)",
						mbitps(q.rate), qPathMemory)
				}
				return q.rate, ""
			}
			q.rate, q.idle = 0, 0
			q.releaseFor = 0
			return 0, "queue: demand is well under the cap — cap removed"
		}
		return q.rate, ""
	case in.delay >= qHigh:
		q.idle = 0
		binding := in.g >= qFullFrac*q.rate
		if binding && q.ownShaperQueue(in) {
			// hl234: the queue is in our own shaper, not on the path: no cut, no hold
			if queueDebug {
				return q.rate, fmt.Sprintf("queue-dbg own: queue %s of which %s in the tunnel's own shaper at %.1f Mbit/s - cap %.1f kept, no hold (hl234)",
					in.delay.Round(time.Millisecond), in.own.Round(time.Millisecond), mbitps(in.g), mbitps(q.rate))
			}
			return q.rate, ""
		}
		// A queue that stays under the cap counts toward "not its own"
		// whether the cap binds or not: in the lab (2026-11-09) the edge's
		// links saw the download direction's queue — no cap on the upload
		// drains that — and following "what they deliver" chased the cap
		// from 16 down to 2.8 Mbit/s.
		q.stuck++
		if q.stuck >= qStuckTicks {
			q.rate, q.stuck = 0, 0
			q.backOff(in.now)
			return 0, fmt.Sprintf("queue: capping did not drain it (still %s at %.1f Mbit/s) — the queue is not the tunnel's own; cap removed for %s",
				in.delay.Round(time.Millisecond), mbitps(in.g), q.releaseFor)
		}
		if (binding || q.onsetPartial(in)) && q.onsetQueue(in) {
			if queueDebug {
				return q.rate, fmt.Sprintf("queue-dbg onset: a new burst's queue %s at the path's rate - cap %.1f Mbit/s, no hold (hl229)", in.delay.Round(time.Millisecond), mbitps(q.rate))
			}
			return q.rate, "" // hl229: no hold
		}
		if !binding && q.hold > 0 {
			return q.rate, "" // followed down already: wait for it to tell
		}
		if binding && queueCtlKeep && in.delay >= qFastDelay && q.hold == 0 {
			// hl216 (HS2_QUEUE_CTL_KEEP): a binding cap with a big queue is
			// well over the path (lab u8 onset under a kept cap: 66-96 ms
			// for 3 ticks of 0.97 steps). qDown drains it in about a tick;
			// the path's rate is learned as for any overshoot. A fixed
			// path's own overshoots (<= 2 % steps) stay under qFastDelay.
			back := qBackFrac * q.rate
			if q.good > 0 {
				back = math.Min(back, q.good)
			}
			q.path, q.pathAt = back/qBackFrac, in.now
			if q.seenFresh(in.now) && q.seen > q.path {
				q.path = q.seen // hl223: the rise stops near it
			}
			q.rate = q.floorOf(math.Min(back, qDown*q.rate))
			q.hold = qHoldTicks
			break
		}
		if binding {
			// The cap itself just went past the path (a rise found its rate):
			// back to just under that cap. In the cap's own units — what the
			// links deliver is less than the cap lets onto the wire by the
			// retransmits (3% loss: 3%), and taking it as the path left every
			// cap after an overshoot that much too low.
			// Never above the last cap that raised no queue: after a big rise
			// (qUpFirst) a few percent back is still over the path.
			back := qBackFrac * q.rate
			if q.good > 0 {
				back = math.Min(back, q.good)
			}
			q.path, q.pathAt = back/qBackFrac, in.now
			if q.seenFresh(in.now) {
				// hl223: back to qSeenFrac of what the path delivered
				// under the queue (the onset cap's 0.97 kept u1b 5 %
				// under it); a cap already under that: the path
				// shrank, it is what this tick delivered.
				if t := qSeenFrac * q.seen; t < q.rate {
					back = t
				} else {
					q.seen, q.seenAt, q.seenGuess, q.seenClear = in.g, in.now, false, false
				}
				q.path = q.seen
			}
			q.rate = q.floorOf(back)
		} else {
			// hl219 (HS2_QUEUE_CTL_KEEP): a single small queue tick under a
			// cap that does not bind is not a throttle (lab d8: one 16 ms
			// tick, 419 delivered under a 487 cap, set cap and path to 406
			// for the rest of the burst, -5 %). Follow down only on the
			// second such tick in a row, or at once on a big queue.
			if queueCtlKeep && in.delay < qFastDelay {
				if q.lostQ++; q.lostQ < qLostTicks {
					return q.rate, ""
				}
			}
			q.lostQ = 0
			// The links deliver well under the cap: the path lost rate (a
			// throttle): follow what it delivers now, at once.
			g := in.g
			if queueCtlKeep && prevClear > g {
				// hl219 (HS2_QUEUE_CTL_KEEP): the tick before delivered more
				// with no queue — the cap rose past the path (qUpFirst) and
				// this queue is its overshoot, not a throttle (lab u1a: 477
				// clear, then 408 at 44 ms under a 548 cap set cap and path
				// to 396 for every later upload, -12 %). That rate is the
				// path's; no hold, so a real throttle is followed next tick.
				g = prevClear
			}
			q.rate = q.floorOf(math.Min(q.rate*qDown, qBackFrac*g))
			q.path, q.pathAt = q.rate, in.now
			if qSeenOn() {
				q.seen, q.seenAt, q.seenGuess, q.seenClear = g, in.now, false, false // hl223: the path delivers this now
			}
			if g != in.g {
				return q.rate, ""
			}
		}
		q.hold = qHoldTicks
	case in.delay < qLow && in.g >= qFullFrac*q.rate && q.hold == 0:
		q.idle, q.stuck = 0, 0
		q.good = q.rate
		if queueCtlKeep && wasKept && q.path > 0 && q.rate >= qNearPath*q.path && !q.burstProbe(in, prevSeen) {
			// hl216: the first tick a kept cap at the path binds is a burst
			// starting (lab u8: 8 uploads at 0.96 of a 467.8 cap, 2 ms): a
			// 2 % rise then put the cap at the path and the next tick queued
			// 128 ms. It rises from the next tick on, if it still fits.
			// hl227: not when the burst probe owns the tick (fresh seen).
			return q.rate, ""
		}
		if queueCtlKeep && q.path > 0 && q.rate > qRegrowFrac*q.path && in.g > q.path {
			// hl205 (HS2_QUEUE_CTL_KEEP): the cap binds with no queue well
			// past the path's last rate — the path grew (lab: 250 → 500
			// Mbit/s under a kept 224 cap: 0.5 %/tick took 40 s for +9 %,
			// ~280 s to full rate). Forget it: rise as before any overshoot.
			// hl208: only when the links deliver more than that rate — 8
			// downloads run ~0.92 of a cap that does not bind (lab d8: a
			// false regrow, 498 -> 548, a 76 ms queue).
			q.path = 0
		}
		if q.seenRise(in, prevSeen) {
			break
		}
		switch {
		case q.path == 0:
			q.rate *= qUpFirst
		case queueCtlKeep && q.rate >= q.path && q.rate < qRegrowFrac*q.path:
			// hl208 (HS2_QUEUE_CTL_KEEP): binding with no queue at or past
			// the path's last rate — it has room (8 downloads under a kept
			// cap ran 0.92 of it, the cap at 0.5 %/tick stayed under the
			// path for the whole burst; a path that grew waited ~30 s for
			// the regrow rule). 2 %/tick up to qRegrowFrac; past it only
			// qUpSlow, and qUpFirst (the regrow above) once the links deliver
			// more than the path's last rate.
			// hl214: the 2 % step needs no g > path (ba0a45bb did): a single
			// upload runs ~0.95 of its cap, so a kept cap at the old path
			// never showed g > path and climbed 0.5 %/tick (lab u1hi 253 vs
			// 306 Mbit/s). The false jump ba0a45bb fixed was the qUpFirst
			// regrow (+10 %), which stays gated.
			q.rate *= qUpFast
		case q.rate < qNearPath*q.path:
			q.rate *= qUpFast
		case queueCtlKeep && in.g >= qNearFill*q.rate:
			// hl216 (HS2_QUEUE_CTL_KEEP): just under the path's last rate
			// with the links filling the cap (the htb burst fix, 7a23cd28,
			// makes them track it) and no queue: 2 %/tick, not 0.5 %. A
			// path that grew back (lab 250 -> 500) waited ~15 s in this
			// band before the regrow; a fixed path is overshot by the same
			// <= 2 % step the band past it already takes.
			q.rate *= qUpFast
		default:
			q.rate *= qUpSlow
		}
	default:
		q.idle, q.stuck = 0, 0
	}
	return q.rate, ""
}

// drop removes the cap and keeps it off for a while (backOff); note's %s is
// how long.
func (q *qCtl) drop(now time.Time, note string) (float64, string) {
	q.rate, q.stuck, q.idle, q.lossy, q.limbo, q.rev = 0, 0, 0, 0, 0, 0
	q.backOff(now)
	return 0, fmt.Sprintf(note, q.releaseFor)
}

// backOff keeps the cap off from now for a while: qReleaseMin, doubling each
// time it comes back to be removed again, up to qReleaseMax. Only a cap that
// ended because demand did (idle) starts the doubling over.
func (q *qCtl) backOff(now time.Time) {
	q.revOff = false
	q.releaseFor = min(max(2*q.releaseFor, qReleaseMin), qReleaseMax)
	q.release = now.Add(q.releaseFor)
}

// floorOf keeps a cap at qMinRate or more.
func (q *qCtl) floorOf(r float64) float64 { return math.Max(r, qMinRate) }

// ownQueue (HS2_QUEUE_CTL_KEEP, hl205): a back-off set by the reverse rule no
// longer blocks a cap once this side's links clearly make the queue: they are
// the busy direction (not reverse), deliver at least qCapMinRate and wait at
// least qFastDelay.
func (q *qCtl) ownQueue(in qIn) bool {
	return queueCtlKeep && q.revOff && !in.reverse() && in.g >= qCapMinRate && in.delay >= qFastDelay
}

// qOwnPath (hl234): the most of a binding queue tick's delay, once the
// shaper's own backlog is taken off, that is still "no queue on the path".
const qOwnPath = qHigh

// ownShaperQueue (hl234, HS2_QUEUE_CTL_KEEP; lab hl233 gS1/gS3 u8 -3.3 %): a
// binding queue tick whose delay is mostly the backlog in the tunnel's own
// shaper is not a path queue. 8 uploads' onset under a kept cap (465-483)
// left 102-142 ms in our cake while the links delivered >= the cap: hl216's
// big-queue rule took the cap to 0.93 with a 10 s hold (432-435 for the rest
// of the burst). The path had no queue: cutting only moves more of the
// burst into our own shaper. The cap stays, no hold, no stuck count; a queue
// on the path past it (delay - own >= qOwnPath) takes the usual rules, and
// so does one the backlog is not most of (lab hl234 hS1/hS2 kh: 15 ms ticks
// with 2-3 ms own, 12-13 on the path, were kept by the looser v2 rule).
func (q *qCtl) ownShaperQueue(in qIn) bool {
	return queueCtlKeep && in.own > 0 && 2*in.own >= in.delay && in.delay-in.own < qOwnPath
}
