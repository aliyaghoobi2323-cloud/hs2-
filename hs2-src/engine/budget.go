package engine

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sync/atomic"
	"time"
)

// γ2, the in-flight budget of a BULK link (R1 round 3, decision T19; plan B
// of REDESIGN §3.4, with the round-2 judge's lessons): in place of α's rate
// cap, each end bounds what its BULK link socket holds unacknowledged —
// in flight and unsent, SIOCOUTQ — to a budget B, and the link's writer
// holds a bulk frame that would take the socket past it (smux
// Config.BulkGate). Control and latency-lane frames go past a waiting bulk
// frame (the scheduler's urgent lanes); the frame keeps its place, and so
// the scheduler, not the socket, holds what waits (its floors decide).
//
// Why a budget and not a rate: a window clocks itself. A sender held at B
// in flight sends B per round trip, so when a queue grows its rate falls
// that same round trip, before any controller reads anything; a rate cap
// keeps pushing at its rate until its next reading (α's 100 ms, then 3 to
// 10% a step). And a byte that goes out only once one came back never
// stacks a queue past B less the path's bytes. The socket's pacing cap is
// BudgetPaceK of the budget's own rate, so a window goes out spread over its
// round trip, not as a burst at BBR's rate.
//
// B follows the queue this direction makes, each reading of the socket
// (the cores' runner, coreTick), toward the target T (BudgetTargetUs, 2.5
// ms a direction: under CoDel's 5 ms on fq_codel; half of the deep FIFO's
// bound on the round trip of a light flow, which crosses both directions):
//
//	S    = srtt − min_rtt of the socket: this direction's queue and the one
//	       its acks wait in on the way back;
//	qRev = the queue of the way back: the QUIET link's one-way delay of the
//	       other direction (owd.go: on a shared FIFO, the queue every flow
//	       there waits in), and on a link that carries data that way too
//	       S × LIn / (LOut + LIn), the share of S the link's own one-way
//	       delays (owd.go's own beat: each the middle of 200 ms of the
//	       other end's readings) give the way back, or, without them, the
//	       guess min(S/2, T);
//	qFwd = max(S − qRev, the QUIET link's one-way delay of this direction);
//	base = min_rtt + qRev: the time a byte of a link at its target spends
//	       in flight less its own queue.
//
// At the target a link of delivery rate d holds d × (base + T) in flight.
// Budget-limited (the gate held a frame since the reading before), B moves
// by FAST's step (base + T) / (base + qFwd): half of the way (γ 1/2) up
// while qFwd is under T and down past it, wholly past 4T. Its queue so
// settles at T whatever the rate and the round trip. A queue under T/2 two
// readings in a row or more is room on the path: the step up doubles each
// reading (T / base: 3.4% at a 72 ms round trip, then 7, 14%), at most
// budgetGrowMax. Not budget-limited (the sender had less to send, or its
// own window held it), B comes down to d × (base + T) only when qFwd passed
// T (BBR's own standing queue, cwnd_gain 2, is no longer the link's), and
// is held within budgetCapK × the 2 s delivery maximum × (base + T)
// otherwise. Never under budgetKMin of the most it acked a second over the
// last 10 s × min_rtt (nor two segments, two bulk frames): a run of noisy
// or misread readings cannot starve a link.
//
// Start: from C4-lite's first sample that is not app-limited (sendcap.go),
// B is budgetStartGain × d × (base + T) — BBR's start-up doubles in a round
// trip, and so may the budget — until the first queue of its own past T
// once the delivery stopped growing by budgetStartGrow over the latest
// readings (the round-2 judge's fix 1: a start that ended in BBR's start-up
// held a link at an eighth of the path); B is then d × (base + T). Idle
// (C4-lite's lift), the budget is lifted.
//
// The two traps of round 2 (round2-judge.fa.md):
//
//   - Version 4 cut each link for its own fq_codel flow queue with a rate
//     cap, on its own sparse one-way-delay messages (one each 0.5 s on a link
//     of 1.5 Mbit/s), at 3% (0.5% near the rate it was cut from) a step up
//     and 10% down: 17 flows on 64 links fell from 89 to 50 Mbit/s, two
//     uploads held 65 and 9. Here the queue of a one-way link is the
//     socket's own srtt, refreshed by every ack and never sparse, and a link
//     under its share sees no queue of its own on fq_codel (its flow queue
//     drains), so it grows, doubling its step a reading, until it does:
//     each link's queue settles at T, and with each flow backlogged at T the
//     bottleneck is full (TestBudgetManyLinksFQ, 1 to 300 links).
//   - srtt holds the queue of the way back too: the acks of a download wait
//     behind the upload's data on the same five-tuple (fq_codel's same flow
//     queue, or the FIFO), and α on srtt held a 100 Mbit/s download beside a
//     20 Mbit/s upload at 17. Here the way back's queue is in base (a
//     budget at d × (min_rtt + qRev + T) keeps the download's rate whatever
//     the upload's queue), and only what is left of S is this direction's.
//     Which direction queues: on a shared FIFO the QUIET link's one-way
//     delays say; on fq_codel, where they see no BULK queue, the link's own
//     — each end sends its own messages only while it sends data on the
//     link, so they ride its data and come only on a two-way link, the one
//     where srtt cannot tell the two queues apart (γ2's first smoke, M1,
//     with the guess alone: the upload's start-up queue read as the
//     download's, and the download fell to 7 Mbit/s of 100). Without them
//     (an older peer, HS2_TUNE_BUDGET_LINKOWD=0) each end takes min(S/2, T)
//     of S as the other's; both ends read the same S, so a split of it that
//     left one direction under its path would stay (TestBudgetTwoWayAsym's
//     model held a download at 0.966 of its path that way): a two-way link
//     budget-limited at its target that acks less than budgetRoom of its
//     most of the last budgetLong seconds grows a FAST step at least.

// TuneBudget 0 (HS2_TUNE_BUDGET=0): BULK links keep α's rate cap (round 2's
// final candidate); 1, the default: the in-flight budget, and C4-lite's
// pacing cap as its outer one.
var TuneBudget = 1

// BudgetOff says whether the in-flight budget is off.
func BudgetOff() bool { return TuneBudget == 0 }

// BudgetTargetUs (HS2_TUNE_BUDGET_US): the queue target of a direction, µs.
var BudgetTargetUs = 2500

// BudgetTwoWay 0 (HS2_TUNE_BUDGET_TWOWAY=0, lab): no share of S is taken
// for the way back on a link that carries data both ways (qRev is the QUIET
// link's alone).
var BudgetTwoWay = 1

// BudgetLinkOWD 0 (HS2_TUNE_BUDGET_LINKOWD=0, lab): no one-way delays of a
// BULK link's own (owd.go's own beat); a two-way link's ends split S by the
// guess min(S/2, T) alone.
var BudgetLinkOWD = 1

// budgetLinkOWD says whether BULK links trade one-way delays of their own.
func budgetLinkOWD() bool { return BudgetLinkOWD != 0 && !BudgetOff() && !OWDOff() }

// BudgetPaceK (HS2_TUNE_BUDGET_PACE, ×100; 0: none): the pacing cap of a
// budget link is this share of B over base + T, its rate at the target,
// the lesser of it and C4-lite's. A window let out at the sender's own
// pacing rate (BBR's, whose bandwidth filter app-limited samples never
// bring down) arrives at a fq_codel bottleneck as a burst its flow's share
// drains slowly: on the rig 17 flows on 64 links read 4 ms of queue a
// window of 12 segments could not make at 2 Mbit/s, and cut and grew in
// turn each reading (M1).
var BudgetPaceK = 125

// BudgetQuietCheck 0 (HS2_TUNE_BUDGET_QCHECK=0): the budget takes the
// QUIET link's one-way queues whatever its own round trip says. 1, the
// default (R1 round 3, the judge): only while they are within the link's
// own srtt − min_rtt and the target (step).
var BudgetQuietCheck = 1

// BudgetC4 0 (HS2_TUNE_BUDGET_C4=0): on a link with an in-flight budget
// the pacing cap is the budget's alone (BudgetPaceK of its rate at the
// target), not the lesser of it and C4-lite's 1.25 × the 2 s delivery
// maximum. The R1 round-3 judge's diagnosis (D3 on the rig): 400
// connections moved onto one BULK link held it at 9 Mbit/s of 100 in five
// runs of six: C4-lite's cap, a step ahead of a delivery it held itself,
// kept the link's flight under B, so the gate never held a frame and B
// never grew (gate waits 0), while with C4-lite off the link took 73.
var BudgetC4 = 1

// BudgetDrainMs (HS2_TUNE_BUDGET_DRAIN, ms; 0: none): about this often
// (a uniform draw in [0.75, 1.25) of it) a budget-limited BULK link holds
// its in-flight at BudgetDrainK of its delivery rate times min_rtt for two
// min_rtt: its queue at the bottleneck empties, so its own BBR sees the
// path's round trip again and refreshes min_rtt before its 10 s filter
// expires. Without it BBR's PROBE_RTT (4 segments for 200 ms) came every
// 10 to 13 s on a lone BULK link (N=1), a second at 59-75 of ~91 Mbit/s
// each time, ~2 % of P10 (c) (round 4's cdip: (c) 0.960-0.965, 0.970-0.980
// without the dips); the drain's cost is its 8 % for ~150 ms each time.
var BudgetDrainMs = 0

// BudgetDrainK (HS2_TUNE_BUDGET_DRAIN_K, ×100): the drain's in-flight, of
// the delivery rate times min_rtt.
var BudgetDrainK = 92

// BudgetInflight 0 (HS2_TUNE_BUDGET_INFLIGHT=0, lab): the budget counts a
// socket's SIOCOUTQ (all not acknowledged, the bytes past a hole the peer
// has too) in place of what Linux counts in flight and its unsent bytes
// (budget_linux.go).
var BudgetInflight = 1

const (
	budgetStartGain  = 2.0
	budgetStartGrow  = 1.25
	budgetGrowMax    = 0.125
	budgetKMin       = 0.5 // of the 10 s most acked a second (long) × min_rtt, the floor
	budgetCapK       = 2.0
	budgetMinSegs    = 2
	budgetRecent     = 3
	budgetRevShare   = 0.05     // the way back's data rate, of this one's, that makes a link two-way
	budgetRevMin     = 64 << 10 // ... and at least this, bytes/s
	budgetWaitMin    = 100 * time.Microsecond
	budgetWaitMax    = 5 * time.Millisecond
	budgetWaitBeyond = 16 // a wait is for the deficit and 1/budgetWaitBeyond of B more
	// budgetRoom, budgetLong: a two-way link budget-limited at its target
	// that acks less than budgetRoom of its most over budgetLong seconds
	// grows as if it had no queue (see step)
	budgetRoom = 0.97
	budgetLong = 10
	// budgetSplitMin: the link's own one-way delays split S only when they
	// sum to this much or more (else half each)
	budgetSplitMin = 500 * time.Microsecond
)

// budgetTarget is T.
func budgetTarget() time.Duration {
	return time.Duration(max(BudgetTargetUs, 100)) * time.Microsecond
}

type budgetMode uint8

const (
	budgetIdle budgetMode = iota
	budgetStart
	budgetRun
)

func (m budgetMode) String() string {
	switch m {
	case budgetStart:
		return "start"
	case budgetRun:
		return "run"
	}
	return "idle"
}

// budgetIn is one reading of a BULK link socket for its budget.
type budgetIn struct {
	Now            time.Duration // the reading's clock
	On             bool          // C4-lite is armed (sendCap.on): not idle since its first full sample
	Delivery       float64       // tcpi_delivery_rate, bytes/s (0: none)
	SRTT, MinRTT   time.Duration // the socket's (0: unknown)
	QOut, QIn      time.Duration // the QUIET link's one-way queues: this end's sending direction, the other
	QOutOK, QInOK  bool
	LOut, LIn      time.Duration // the link's own one-way queues (owd.go's own beat), the same two
	LOutOK, LInOK  bool
	TxRate, RxRate float64 // bytes/s acknowledged and received since the reading before
	Blocked        bool    // the gate held a bulk frame since the reading before
	MSS, Frame     int     // the socket's segment, the link's bulk frame
	DlvMax         float64 // the largest delivery rate of the last 2 s (sendCap.maxDlv)
}

// budgetCore is a link's budget: pure (its readings in, B out).
type budgetCore struct {
	mode budgetMode
	b    float64 // bytes; 0: none (the gate is open)
	dlv  [budgetRecent]float64
	di   int
	noQ  int // readings in a row budget-limited with qFwd under T/2
	// long: the most acked a second over the last budgetLong seconds (a
	// ring from sec)
	long [budgetLong]float64
	sec  int64
	// the latest reading's, for the status line
	q, qRev, s, base time.Duration
	d                float64
	two, own         bool
	cuts, grows      uint64
	rooms            uint64 // two-way growth for room of its own
	// drainAt, drainEnd: the next drain's time and the current one's end
	// (BudgetDrainMs); drains: how many
	drainAt, drainEnd time.Duration
	drains            uint64
}

// longMax notes the acked rate of the reading and is the most of the last
// budgetLong seconds.
func (c *budgetCore) longMax(now time.Duration, r float64) float64 {
	sec := int64(now / time.Second)
	switch d := sec - c.sec; {
	case d >= budgetLong || d < 0:
		c.long = [budgetLong]float64{}
	default:
		for i := int64(1); i <= d; i++ {
			c.long[(c.sec+i)%budgetLong] = 0
		}
	}
	c.sec = sec
	if k := sec % budgetLong; r > c.long[k] {
		c.long[k] = r
	}
	mx := 0.0
	for _, v := range c.long {
		mx = math.Max(mx, v)
	}
	return mx
}

// step takes a reading and says B (bytes; 0: no budget), lowered while a
// drain runs (BudgetDrainMs).
func (c *budgetCore) step(in budgetIn) float64 {
	b := c.step0(in)
	if BudgetDrainMs <= 0 || b <= 0 || c.mode != budgetRun || in.MinRTT <= 0 || c.d <= 0 {
		c.drainAt, c.drainEnd = 0, 0
		return b
	}
	period := time.Duration(BudgetDrainMs) * time.Millisecond
	switch {
	case c.drainAt == 0:
		c.drainAt = in.Now + time.Duration(float64(period)*(0.75+0.5*rand.Float64()))
	case in.Now < c.drainEnd:
	case in.Now >= c.drainAt && in.Blocked:
		c.drainEnd = in.Now + 2*in.MinRTT
		c.drainAt = in.Now + time.Duration(float64(period)*(0.75+0.5*rand.Float64()))
		c.drains++
	default:
		return b
	}
	if in.Now >= c.drainEnd {
		return b
	}
	return math.Min(b, math.Max(float64(BudgetDrainK)/100*c.d*in.MinRTT.Seconds(), float64(budgetMinSegs*max(in.MSS, 536))))
}

// step0 is step without the drain.
func (c *budgetCore) step0(in budgetIn) float64 {
	if !in.On {
		*c = budgetCore{cuts: c.cuts, grows: c.grows, rooms: c.rooms, drains: c.drains}
		return 0
	}
	T := budgetTarget()
	S := time.Duration(0)
	if in.SRTT > 0 && in.MinRTT > 0 && in.SRTT > in.MinRTT {
		S = in.SRTT - in.MinRTT
	}
	long := c.longMax(in.Now, in.TxRate)
	qRev := time.Duration(0)
	two := BudgetTwoWay != 0 && in.RxRate > budgetRevMin && in.RxRate > budgetRevShare*in.TxRate
	// own: the link's own one-way delays, both ways (a two-way link whose
	// ends both send: owd.go's own beat), which split S as it is
	own := in.LOutOK && in.LInOK
	c.two, c.own = two || own, own
	switch {
	case own:
		// measured both ways: S, the smoothed round trip less min_rtt, split
		// by the link's own one-way delays (each the middle of 200 ms of the
		// other end's readings, owd.go): the least of them is a window's
		// trough (M3: 0.5 to 3.6 ms on a link whose S was 17 to 24 ms, so the
		// budget grew into a queue of 40), and S less one of them read the way
		// back's swings as this direction's (M2: a download cut to half)
		if sum := in.LOut + in.LIn; sum >= budgetSplitMin {
			qRev = time.Duration(float64(S) * float64(in.LIn) / float64(sum))
		} else {
			qRev = S / 2
		}
	case two:
		qRev = min(S/2, T)
		if BudgetSplitRate != 0 && in.RxRate > in.TxRate {
			// the heavier way holds the queue: S by the two rates, at least
			// the guess (BudgetSplitRate)
			qRev = max(qRev, time.Duration(float64(S)*in.RxRate/(in.RxRate+in.TxRate)))
		}
	}
	// the QUIET link's one-way queues count only while the link's own
	// round trip holds them (BudgetQuietCheck): a one-way queue of the path
	// this link shares (a FIFO) is within its srtt − min_rtt; one past it
	// waits where this link does not — the QUIET link's own resends or
	// backlog (under 1% loss 7 to 19 ms of it, on a link of 1 to 2: the
	// judge's smoke C4)
	qOK := func(q time.Duration) bool { return BudgetQuietCheck == 0 || q <= S+T }
	if in.QInOK && qOK(in.QIn) {
		qRev = max(qRev, in.QIn)
	}
	qFwd := max(S-qRev, 0)
	if in.QOutOK && qOK(in.QOut) {
		qFwd = max(qFwd, in.QOut)
	}
	two = two && !own // the guess (min(S/2, T)) and its room rule
	minRTT := in.MinRTT
	if minRTT <= 0 {
		minRTT = in.SRTT
	}
	base := minRTT + qRev
	c.q, c.qRev, c.s, c.base = qFwd, qRev, S, base
	if in.Delivery > 0 {
		c.di = (c.di + 1) % budgetRecent
		c.dlv[c.di] = in.Delivery
	}
	d := 0.0
	for _, v := range c.dlv {
		d = math.Max(d, v)
	}
	c.d = d
	if d <= 0 || base <= 0 {
		return c.b // nothing to judge by: it holds
	}
	floor := math.Max(float64(budgetMinSegs*max(in.MSS, 536)), float64(2*in.Frame))
	floor = math.Max(floor, budgetKMin*long*minRTT.Seconds())
	if k := BudgetDFloor.Get(); k > 0 {
		floor = math.Max(floor, float64(k)/100*d*minRTT.Seconds())
	}
	want := d * (base + T).Seconds() // in flight at the target, at this delivery rate
	switch c.mode {
	case budgetIdle:
		c.mode = budgetStart
		c.b = math.Max(budgetStartGain*want, floor)
		return c.b
	case budgetStart:
		// growing: the delivery rose by budgetStartGrow since the oldest of
		// the latest readings (BBR's start-up: its queue then says nothing of
		// the path's rate)
		growing, n, old := true, 0, 0.0
		for k := 1; k <= budgetRecent; k++ {
			if v := c.dlv[(c.di+k)%budgetRecent]; v > 0 {
				if n++; old == 0 {
					old = v
				}
			}
		}
		if n >= 2 {
			growing = in.Delivery >= budgetStartGrow*old
		}
		if qFwd <= T || growing {
			c.b = math.Max(budgetStartGain*want, floor)
			return c.b
		}
		c.mode = budgetRun
		c.b = math.Max(want, floor)
		c.cuts++
		return c.b
	}
	ratio := float64(base+T) / float64(base+qFwd)
	if qFwd >= T/2 {
		c.noQ = 0
	}
	switch {
	case two && in.Blocked && qFwd <= T+T/2 && in.TxRate < budgetRoom*long:
		// both ends read the same srtt, so a split of its queue that held
		// this direction under its path stays (the way back's queue past
		// what is taken for it, and this one's empty): budget-limited at
		// the target and acking less than it did lately, it grows a FAST
		// step at least. A path of its own that got slower makes a queue of
		// its own as it grows, and the cut below takes it back, until its
		// 10 s maximum ages.
		c.b *= 1 + math.Max(ratio-1, float64(T)/float64(base))
		c.rooms++
	case qFwd > T:
		w := c.b * ratio
		if !in.Blocked {
			w = math.Min(c.b, want) // B was not what held it: the target at its rate
		}
		if qFwd <= 4*T {
			w = c.b + (w-c.b)/2
		}
		if w < c.b {
			c.cuts++
		}
		c.b = w
	case in.Blocked:
		if qFwd < T/2 {
			c.noQ++
		}
		g := (ratio - 1) / 2 // FAST's step, half of the way (γ 1/2)
		if c.noQ > 1 {
			g = math.Min((ratio-1)*float64(int(1)<<min(c.noQ-1, 8)), budgetGrowMax)
		}
		if g > 0 {
			c.b *= 1 + g
			c.grows++
		}
	default:
		c.b = math.Min(c.b, math.Max(budgetCapK*in.DlvMax*(base+T).Seconds(), floor))
	}
	c.b = math.Max(c.b, floor)
	return c.b
}

// linkBudget is a BULK link socket's budget with its gate: gate is the link
// writer's (smux's BulkGate), the core the cores' runner's (under its
// sendCapLink's lock).
type linkBudget struct {
	core budgetCore
	outq func() (int, error) // SIOCOUTQ of the socket
	// b: B in force, bytes (0: none); rate: bytes/s a wait is reckoned at
	b, rate atomic.Int64
	// blocked: the gate held a bulk frame since the runner's reading
	blocked atomic.Bool
	// the gate's tallies: the waits it asked and their time, the most it saw
	// in flight (the status line's, since its previous one)
	held, waitNs, outqMax atomic.Int64
	// the runner's: acked and received at the reading before
	ack0, rx0 uint64
	at0       time.Duration
	// quiet: a QUIET link's (QuietBudget): no pacing of its own, its light
	// and realtime frames go past the gate at the socket's pace
	quiet bool
}

// QuietBudget (HS2_TUNE_QUIET_BUDGET; 0: off; R1 round 5): the in-flight
// budget on a QUIET link's bulk lane too (web responses and every download's
// start, what is past a connection's first bytes), not only on BULK links.
// A QUIET link holding a window of web responses in flight and in its flow
// queue at the bottleneck holds a call's next frame behind them, in the
// socket and in fq_codel's queue for that flow: the realtime lanes only
// order the frames not yet written (round 4's item 3: a call's downward p99
// +74 to +103 ms; Illinois, which CoDel's drops cut by an eighth, doubled it
// at N=4 and N=8). With the budget the bulk lane waits in the scheduler past
// the target queue, and the lanes go first (round-4 judge's open item).
var QuietBudget = newTuneInt(1) // the default since R1 round 5 (round5/REPORT.fa.md)

// BudgetDFloor (HS2_TUNE_BUDGET_DFLOOR, ×100; 0: off; R1 round 5): B stays at
// this share of the delivery rate × min_rtt. A socket whose flight is under
// its rate × its round trip holds no queue of its own, whatever srtt says, and
// a cut below it only slows the link under what it delivers. Without it a
// one-way link with few users fell into a spiral (round 5's LX: home at N=16,
// 0.03 %): the budget cut, the flight fell to a few segments, the peer's
// delayed acks (up to 40 ms on an odd last segment) lifted srtt 11 to 16 ms
// over min_rtt, which read as its own queue and cut again, down to B 5 to 8
// KiB, 0.6 Mbit/s, on a link that delivered 4 to 6: its users' pages took 13
// to 28 s or never loaded (5 to 8 failed a run, production none).
//
// The main server (R1 fix 2) showed the same spiral on BULK links next to
// links that took 72 Mbit/s: B at the floor of two segments (3 KiB), 0.8
// Mbit/s, srtt − min_rtt 11 ms against the target's 2.5, and nearly every cut
// of the tunnel on those links. Every other floor follows the link's own held
// rate (two segments, two frames, half the 10 s most acked × min_rtt), so a
// queue the link does not make (delayed acks of its small flight, the way
// back, other flows' backlog) cuts it at each reading down to two segments,
// and nothing there grows it again. This one recovers it: its delivery
// follows the budget's own pacing cap (BudgetPaceK × B / (base + T)), so
// delivery × min_rtt stands over B by BudgetPaceK × min_rtt / (base + T) and
// the floor lifts B a reading at a time, until the path, not the budget, sets
// the delivery; a queue of its own past the floor is cut to it as before
// (TestBudgetSpiralRecovers).
var BudgetDFloor = newTuneInt(100) // the default since R1 round 5

// BudgetSplitRate (HS2_TUNE_BUDGET_SPLIT; 0: off; R1 round 5): a two-way
// link without its own one-way delays (dirty while it resends: OWDGuardBulk)
// splits S by what each way carries when the other way carries more,
// instead of holding the way back to the guess min(S/2, T). In the home mix
// at N=1 one BULK link carries the downloads and ~90 Mbit/s of uploads; under
// random loss its own delays read none, and the exit took S (8 to 12 ms, most
// of it the uplink's queue, where the downloads' acks wait behind the uploads
// in one flow queue) for its own less 2.5 ms: it cut the downloads to 16 to 19
// Mbit/s at 0.3 % (round 5's screen S4; production 78).
var BudgetSplitRate = 0

// newLinkBudget is the budget of a socket read by outq (nil: none).
func newLinkBudget(outq func() (int, error)) *linkBudget {
	if outq == nil {
		return nil
	}
	return &linkBudget{outq: outq}
}

// gate is smux's BulkGate: 0, a bulk frame of size bytes may go; else ask
// again after that long.
func (lb *linkBudget) gate(size int) time.Duration {
	b := lb.b.Load()
	if b <= 0 {
		return 0
	}
	q, err := lb.outq()
	if err != nil {
		return 0 // (the socket is gone: its write says so)
	}
	if int64(q) > lb.outqMax.Load() {
		lb.outqMax.Store(int64(q))
	}
	if q == 0 || int64(q+size) <= b {
		return 0
	}
	lb.blocked.Store(true)
	lb.held.Add(1)
	w := budgetWaitMax
	if r := lb.rate.Load(); r > 0 {
		deficit := float64(int64(q+size)-b) + float64(b)/budgetWaitBeyond
		w = time.Duration(deficit / float64(r) * float64(time.Second))
	}
	w = min(max(w, budgetWaitMin), budgetWaitMax)
	lb.waitNs.Add(int64(w))
	return w
}

// step is the runner's reading of the socket for the budget (under its
// sendCapLink's lock): on is C4-lite's state, ts the socket's TCP_INFO,
// own the link's own one-way delays (nil: none); it says the pacing cap
// (bytes/s; 0: none).
func (lb *linkBudget) step(now time.Duration, on bool, ts tcpStat, dlvMax float64, own *owdLink) uint64 {
	tx, rx := 0.0, 0.0
	if lb.at0 > 0 && now > lb.at0 {
		dt := (now - lb.at0).Seconds()
		if ts.bytesAcked >= lb.ack0 {
			tx = float64(ts.bytesAcked-lb.ack0) / dt
		}
		if ts.bytesReceived >= lb.rx0 {
			rx = float64(ts.bytesReceived-lb.rx0) / dt
		}
	}
	lb.ack0, lb.rx0, lb.at0 = ts.bytesAcked, ts.bytesReceived, now
	in := budgetIn{Now: now, On: on, Delivery: float64(ts.deliveryRate), SRTT: usDur(uint64(ts.rtt)),
		MinRTT: usDur(uint64(ts.minRTT)), TxRate: tx, RxRate: rx, Blocked: lb.blocked.Swap(false),
		MSS: int(ts.sndMSS), Frame: BulkFrameSize(dlvMax), DlvMax: dlvMax}
	if o := quietOWD.Load(); o != nil {
		in.QOut, in.QOutOK = o.queue()
		in.QIn, in.QInOK = o.queueIn()
	}
	if own != nil {
		in.LOut, in.LOutOK = own.queue()
		in.LIn, in.LInOK = own.queueIn()
	}
	b := lb.core.step(in)
	lb.b.Store(int64(b))
	r := lb.core.d
	pace := uint64(0)
	if lb.core.base > 0 && b > 0 {
		at := b / (lb.core.base + budgetTarget()).Seconds() // its rate at the target
		r = math.Max(r, at)
		if BudgetPaceK > 0 && !lb.quiet {
			pace = uint64(float64(BudgetPaceK) / 100 * at)
		}
	}
	lb.rate.Store(int64(r))
	return pace
}

// line is the budget's status line (§3.4's sendcore line, in part), and
// starts the next one's tallies.
func (lb *linkBudget) line(n int) string {
	c := &lb.core
	way := "one-way"
	switch {
	case c.own:
		way = "two-way (own delays)"
	case c.two:
		way = "two-way"
	}
	return fmt.Sprintf("budget link %d: %s %s, B %.0f KiB (in flight at most %.0f KiB), queue %.1f ms (srtt-min %.1f, way back %.1f, target %.1f), delivery %.1f Mbit/s, cuts %d, grows %d, room %d, gate waits %d (%.0f ms)",
		n, c.mode, way, float64(lb.b.Load())/1024, float64(lb.outqMax.Swap(0))/1024, msOf(c.q), msOf(c.s), msOf(c.qRev),
		msOf(budgetTarget()), c.d*8/1e6, c.cuts, c.grows, c.rooms, lb.held.Swap(0), float64(lb.waitNs.Swap(0))/1e6) + drainNote(c)
}

// drainNote is the budget line's drains (BudgetDrainMs), else nothing.
func drainNote(c *budgetCore) string {
	if BudgetDrainMs <= 0 {
		return ""
	}
	return fmt.Sprintf(", drains %d", c.drains)
}
