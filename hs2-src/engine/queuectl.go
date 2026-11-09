package engine

import (
	"fmt"
	"math"
	"os"
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
//     cap — the loss, not the queue, holds the users there.
type qCtl struct {
	rate       float64   // the cap, bytes/s; 0 = none
	high       int       // consecutive ticks with a queue, uncapped
	stuck      int       // capped ticks the queue stayed while the cap was the limit
	hold       int       // ticks before the cap may rise again (after a step down)
	idle       int       // capped ticks with demand well under the cap
	grace      int       // ticks the first cap gets to drain the queue it found
	lossy      int       // capped ticks with loss over qMaxLoss
	path       float64   // the cap at the last overshoot: the path's rate, in the cap's units
	pathAt     time.Time // ... when it was seen (kept across caps for qPathMemory)
	good       float64   // the last cap that raised no queue
	release    time.Time
	releaseFor time.Duration
}

const (
	qHigh       = 15 * time.Millisecond // a queue
	qLow        = 8 * time.Millisecond  // no queue (cake's own target is 5 ms)
	qStartFrac  = 0.95                  // the first cap, of what the links delivered: drains the queue in seconds
	qBackFrac   = 0.97                  // a later overshoot: what they delivered then is the path's rate
	qDown       = 0.93
	qUpFirst    = 1.10 // a rise before any overshoot (the path's rate not known)
	qUpFast     = 1.02 // a rise well under the last overshoot (the path's rate)
	qPathMemory = 10 * time.Minute
	qUpSlow     = 1.005 // ... and near it: each overshoot is a queue every user waits
	qNearPath   = 0.985
	qGraceTicks = 2 // ~4 s for the first cap to drain the queue it found
	qHoldTicks  = 5 // ~10 s after a step down before it rises again
	qFullFrac   = 0.90
	qStuckTicks = 5 // ~10 s of a queue that does not drain under a binding cap
	qIdleTicks  = 15
	qMinRate    = 125000 // 1 Mbit/s: below this there is nothing to shape
	qReleaseMin = time.Minute
	qReleaseMax = 10 * time.Minute
	// qMaxLoss: a path that loses more than this of what the links send
	// limits them by its loss, not by its queue: capping it cost 8% of the
	// throughput at 3% loss in the lab, for no significant gain in the median
	// echo (the queue it removes is not what holds the users there). No cap
	// then — the tunnel as without the controller.
	qMaxLoss   = 0.02
	qLossTicks = 3
)

// qLossHold: HS2_QUEUE_CTL_LOSSHOLD=1 (default off). A cap removed for loss
// is not set again for releaseFor (doubling from qReleaseMin up to
// qReleaseMax, as the not-its-own-queue rule). Without it the controller
// cycles: uncapped, the path's delay (jitter, or a queue that is not the
// tunnel's) sets a cap; cake's own drops under the cap read as retransmits
// over qMaxLoss, so ~6 s later the cap goes; ~4 s later the delay sets it
// again. While capped, cake shares the cap evenly over the ~150 link flows
// (all one host pair), so one download's link gets 1/N of it: in the
// production-like lab (crowd 3000, pool 150, 1% loss, delay 30-70 ms) a
// single-stream download fell from 50-90 to 1-3 Mbit/s for exactly the
// capped seconds — the occasional slow test.
var qLossHold = os.Getenv("HS2_QUEUE_CTL_LOSSHOLD") == "1"

// qIn is one tick's measurement.
type qIn struct {
	now   time.Time
	delay time.Duration // median queueing delay of the busy links; <0: no busy link
	g     float64       // bytes/s the links delivered (acknowledged) this tick
	loss  float64       // share of the segments sent that were retransmits
}

// step returns the new cap (0: none) and a log line when it changed in kind.
func (q *qCtl) step(in qIn) (float64, string) {
	busy := in.delay >= 0 && in.g >= qMinRate
	if q.rate == 0 {
		if !busy || in.now.Before(q.release) || in.delay < qHigh || in.loss > qMaxLoss {
			q.high = 0
			return 0, ""
		}
		if q.high++; q.high < 2 {
			return 0, ""
		}
		// A queue forms while the downloads' BBR is still ramping up, so what
		// the links deliver now can be well under the path. A path rate seen in
		// the last qPathMemory starts the cap just under it; otherwise it is
		// not known yet and the cap rises fast (qUpFirst) to the first
		// overshoot, which tells it. No hold: in the lab a hold and 2% steps
		// cost a 20 s download ~9% of its throughput.
		q.high, q.stuck, q.idle, q.hold, q.grace, q.good = 0, 0, 0, 0, qGraceTicks, 0
		start := qStartFrac * in.g
		if q.path > 0 && in.now.Sub(q.pathAt) < qPathMemory {
			start = math.Max(start, qBackFrac*q.path)
		} else {
			q.path = 0
		}
		q.rate = q.floorOf(start)
		return q.rate, fmt.Sprintf("queue: the path's queue holds the links %s over their lowest round trip at %.1f Mbit/s — tunnel egress capped at %.1f Mbit/s so it drains (HS2_QUEUE_CTL)",
			in.delay.Round(time.Millisecond), mbitps(in.g), mbitps(q.rate))
	}
	if q.hold > 0 {
		q.hold--
	}
	if q.grace > 0 && in.delay >= qHigh {
		q.grace-- // the queue the cap found is still draining
		return q.rate, ""
	}
	q.grace = 0
	if in.loss > qMaxLoss {
		if q.lossy++; q.lossy >= qLossTicks {
			q.rate, q.lossy, q.stuck, q.idle = 0, 0, 0, 0
			if qLossHold {
				// HS2_QUEUE_CTL_LOSSHOLD: no new cap for a while, as after a
				// queue that is not the tunnel's own (see qLossHold)
				q.releaseFor = min(max(2*q.releaseFor, qReleaseMin), qReleaseMax)
				q.release = in.now.Add(q.releaseFor)
				return 0, fmt.Sprintf("queue: the path loses %.1f%% of what the links send — it limits them by its loss, not its queue; cap removed for %s", 100*in.loss, q.releaseFor)
			}
			return 0, fmt.Sprintf("queue: the path loses %.1f%% of what the links send — it limits them by its loss, not its queue; cap removed", 100*in.loss)
		}
	} else {
		q.lossy = 0
	}
	switch {
	case in.g < qFullFrac*q.rate/2:
		q.stuck = 0
		if q.idle++; q.idle >= qIdleTicks {
			q.rate, q.idle = 0, 0
			q.releaseFor = 0
			return 0, "queue: demand is well under the cap — cap removed"
		}
		return q.rate, ""
	case in.delay >= qHigh:
		q.idle = 0
		binding := in.g >= qFullFrac*q.rate
		if binding {
			q.stuck++
		} else {
			q.stuck = 0
		}
		if q.stuck >= qStuckTicks {
			q.rate, q.stuck = 0, 0
			q.releaseFor = min(max(2*q.releaseFor, qReleaseMin), qReleaseMax)
			q.release = in.now.Add(q.releaseFor)
			return 0, fmt.Sprintf("queue: capping did not drain it (still %s at %.1f Mbit/s, all the path delivered) — the queue is not the tunnel's own; cap removed for %s",
				in.delay.Round(time.Millisecond), mbitps(in.g), q.releaseFor)
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
			q.rate = q.floorOf(back)
		} else {
			// The links deliver well under the cap: the path lost rate (a
			// throttle): follow what it delivers now, at once.
			q.rate = q.floorOf(math.Min(q.rate*qDown, qBackFrac*in.g))
			q.path, q.pathAt = q.rate, in.now
		}
		q.hold = qHoldTicks
	case in.delay < qLow && in.g >= qFullFrac*q.rate && q.hold == 0:
		q.idle, q.stuck = 0, 0
		q.good = q.rate
		switch {
		case q.path == 0:
			q.rate *= qUpFirst
		case q.rate < qNearPath*q.path:
			q.rate *= qUpFast
		default:
			q.rate *= qUpSlow
		}
	default:
		q.idle, q.stuck = 0, 0
	}
	return q.rate, ""
}

// floorOf keeps a cap at qMinRate or more.
func (q *qCtl) floorOf(r float64) float64 { return math.Max(r, qMinRate) }
