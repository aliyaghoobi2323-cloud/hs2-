package engine

import (
	"fmt"
	"math"
	"sync/atomic"
	"time"
)

// γ2, the in-flight budget of a BULK link (R1 round 3, decision T19; plan B
// of REDESIGN §3.4, with the round-2 judge's lessons): in place of α's rate
// cap, each end bounds what its BULK link socket holds unacknowledged —
// in flight and unsent, SIOCOUTQ — to a budget B, and the link's writer
// holds a bulk frame that would take the socket past it (smux
// Config.BulkGate). Control and latency-lane frames go past a waiting bulk
// frame (the scheduler's urgent lanes); the frame keeps its place.
//
// Why a budget and not a rate: a window clocks itself. A sender held at B
// in flight sends B per round trip, so when a queue grows its rate falls
// that same round trip, before any controller reads anything; a rate cap
// keeps pushing at its rate until its next reading (α's 100 ms, then 3 to
// 10% a step). And a byte that goes out only once one came back never
// stacks a queue past B less the path's bytes.
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
//	       there waits in), and, while the link carries data that way too,
//	       at least min(S/2, T) (budgetTwoWay);
//	qFwd = max(S − qRev, the QUIET link's one-way delay of this direction);
//	base = min_rtt + qRev: the time a byte of a link at its target spends
//	       in flight less its own queue.
//
// At the target a link of delivery rate d holds d × (base + T) in flight.
// Budget-limited (the gate held a frame since the reading before), B moves
// by (base + T) / (base + qFwd), FAST's step: up while qFwd is under T, down
// past it, wholly past 2T, half of the way under it. Its queue so settles
// at T whatever the rate and the round trip. A step up is that ratio (T /
// base: 3.4% at a 72 ms round trip), doubled for each reading in a row the
// queue stayed under T/2 (room on the path), at most budgetGrowMax.
// Not budget-limited (the sender had less to send, or its own window held
// it), B comes down to d × (base + T) only when qFwd passed T (BBR's own
// standing queue, cwnd_gain 2, is no longer the link's), and is held within
// budgetCapK × the 2 s delivery maximum × (base + T) otherwise.
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
//     uploads held 65 and 9. Here the queue is the socket's own srtt,
//     refreshed by every ack and never sparse, and a link under its share
//     sees no queue of its own on fq_codel (its flow queue drains), so it
//     grows, doubling its step a reading, until it does: each link's
//     queue settles at T, and with each flow backlogged at T the bottleneck
//     is full. No floor below a quarter of the 2 s delivery maximum × the
//     round trip lets a noisy reading starve a link.
//   - srtt holds the queue of the way back too: the acks of a download wait
//     behind the upload's data on the same five-tuple (fq_codel's same flow
//     queue, or the FIFO), and α on srtt held a 100 Mbit/s download beside a
//     20 Mbit/s upload at 17. Here the way back's queue is in base (a
//     budget at d × (min_rtt + qRev + T) keeps the download's rate whatever
//     the upload's queue), and only what is left of S is this direction's.
//     On a shared FIFO the QUIET link's one-way delays say which direction
//     queues; on fq_codel, where they see no BULK queue, a two-way link's
//     ends both run this budget, so the way back holds about T, and each end
//     takes min(S/2, T) of S as the other's. A spike of the other direction
//     past T reads as some of this one's for a reading or two (it cuts a
//     little and grows back in a few), never as a floor under the rate.

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

const (
	budgetStartGain  = 2.0
	budgetStartGrow  = 1.25
	budgetGrowMax    = 0.125
	budgetKMin       = 0.25
	budgetCapK       = 2.0
	budgetMinSegs    = 4
	budgetRecent     = 3
	budgetRevShare   = 0.05     // the way back's data rate, of this one's, that makes a link two-way
	budgetRevMin     = 64 << 10 // ... and at least this, bytes/s
	budgetWaitMin    = 100 * time.Microsecond
	budgetWaitMax    = 5 * time.Millisecond
	budgetWaitBeyond = 16 // a wait is for the deficit and 1/budgetWaitBeyond of B more
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
	On             bool          // C4-lite is armed (sendCap.on): not idle since its first full sample
	Delivery       float64       // tcpi_delivery_rate, bytes/s (0: none)
	SRTT, MinRTT   time.Duration // the socket's (0: unknown)
	QOut, QIn      time.Duration // the QUIET link's one-way queues: this end's sending direction, the other
	QOutOK, QInOK  bool
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
	// the latest reading's, for the status line
	q, qRev, s, base time.Duration
	d                float64
	cuts, grows      uint64
}

// step takes a reading and says B (bytes; 0: no budget).
func (c *budgetCore) step(in budgetIn) float64 {
	if !in.On {
		*c = budgetCore{cuts: c.cuts, grows: c.grows}
		return 0
	}
	T := budgetTarget()
	S := time.Duration(0)
	if in.SRTT > 0 && in.MinRTT > 0 && in.SRTT > in.MinRTT {
		S = in.SRTT - in.MinRTT
	}
	qRev := time.Duration(0)
	if BudgetTwoWay != 0 && in.RxRate > budgetRevMin && in.RxRate > budgetRevShare*in.TxRate {
		qRev = min(S/2, T)
	}
	if in.QInOK {
		qRev = max(qRev, in.QIn)
	}
	qFwd := max(S-qRev, 0)
	if in.QOutOK {
		qFwd = max(qFwd, in.QOut)
	}
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
	floor = math.Max(floor, budgetKMin*in.DlvMax*minRTT.Seconds())
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
	case qFwd > T:
		w := c.b * ratio
		if !in.Blocked {
			w = math.Min(c.b, want) // B was not what held it: the target at its rate
		}
		if qFwd <= 2*T {
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
		g := ratio - 1
		if c.noQ > 1 {
			g = math.Min(g*float64(int(1)<<min(c.noQ-1, 8)), budgetGrowMax)
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
}

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
// sendCapLink's lock): on is C4-lite's state, ts the socket's TCP_INFO.
func (lb *linkBudget) step(now time.Duration, on bool, ts tcpStat, dlvMax float64) {
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
	in := budgetIn{On: on, Delivery: float64(ts.deliveryRate), SRTT: usDur(uint64(ts.rtt)),
		MinRTT: usDur(uint64(ts.minRTT)), TxRate: tx, RxRate: rx, Blocked: lb.blocked.Swap(false),
		MSS: int(ts.sndMSS), Frame: BulkFrameSize(dlvMax), DlvMax: dlvMax}
	if o := quietOWD.Load(); o != nil {
		in.QOut, in.QOutOK = o.queue()
		in.QIn, in.QInOK = o.queueIn()
	}
	b := lb.core.step(in)
	lb.b.Store(int64(b))
	r := lb.core.d
	if lb.core.base > 0 {
		r = math.Max(r, b/(lb.core.base+budgetTarget()).Seconds())
	}
	lb.rate.Store(int64(r))
}

// line is the budget's status line (§3.4's sendcore line, in part), and
// starts the next one's tallies.
func (lb *linkBudget) line(n int) string {
	c := &lb.core
	return fmt.Sprintf("budget link %d: %s, B %.0f KiB (in flight at most %.0f KiB), queue %.1f ms (srtt-min %.1f, way back %.1f, target %.1f), delivery %.1f Mbit/s, cuts %d, grows %d, gate waits %d (%.0f ms)",
		n, c.mode, float64(lb.b.Load())/1024, float64(lb.outqMax.Swap(0))/1024, msOf(c.q), msOf(c.s), msOf(c.qRev),
		msOf(budgetTarget()), c.d*8/1e6, c.cuts, c.grows, lb.held.Swap(0), float64(lb.waitNs.Swap(0))/1e6)
}
