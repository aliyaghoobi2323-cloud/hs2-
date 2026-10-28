package engine

// R1 round 4, the call in the home mix (decision T21 item 2), the second
// approach: a realtime lane inside each link's scheduler, for steady flows,
// ahead of the bursts.
//
// On a QUIET link of the home mix the latency lane (FIFO, sched.go) carries
// every new connection's first bytes (C1 queues an unjudged flow's first
// classNewBytes as light) and the light flows; a 1.5 Mbit/s call is bulk to
// C1 (more than 64 KiB in 2 s), so it waits in the bulk lane behind all of
// them, at its floor among the users with demand there: the round-3 judge
// saw the QUIET lane's p99 at 47 to 57 ms and the call delivered at 0.68.
//
// TuneRT 1 (HS2_TUNE_RT=1): LaneRT, served after the control lane and before
// the latency lane, takes the frames of
//
//   - a confirmed light flow (two light verdicts in a row: a game, a probe),
//     up to rtLightAllow bytes a classTick;
//   - a steady flow: over C1's 2 s window it sent in at least rtSlotsMin of
//     its 20 ticks, more than classLightSpan in all and at most RTMax (bit/s)
//     on average, and no tick near the slot's ceiling (a burst): a call, a
//     live stream; up to twice its mean tick, or its largest tick and a mean
//     one more (a key frame), a classTick;
//
// what a flow sends past its allowance in a tick goes in its usual lane,
// and a moved connection (on a BULK link for its backlog) never. In the
// scheduler each user has rtBurst of realtime credit refilled at RTMax: a
// user cannot take the link with steady flows. A download stuck on a QUIET
// link at a low rate looks steady; it is held to its own allowance and
// RTMax, and its writer keeps its backlog in big writes, so the exit's hint
// moves it as before. Each end judges what it sends; nothing on the wire.

// TuneRT: 1 on (HS2_TUNE_RT), 0 off.
var TuneRT = 1 // the default since R1 round 4 (round4/REPORT.fa.md)

// RTMax: a steady flow's most, and each user's realtime credit rate, bit/s
// (HS2_TUNE_RT_MAX).
var RTMax = 4_000_000

const (
	// rtLightAllow: a confirmed light flow's realtime bytes a classTick
	rtLightAllow = classLightBurst / 2
	// rtSlotsMin: the ticks of 20 a steady flow sent in at least
	rtSlotsMin = 16
	// rtAllowMax: the most realtime bytes of a flow a classTick
	rtAllowMax = 64 << 10
	// rtBurst: each user's realtime credit at most (sched.go)
	rtBurst = 64 << 10
	// rtFrameMax: a realtime frame at most (a larger one goes as usual)
	rtFrameMax = 16 << 10
)

// rtAllowance is a flow's realtime bytes for the next classTick from its
// classifier just ticked (confirmed: its two light verdicts in a row): 0
// when it is neither confirmed light nor steady.
func rtAllowance(c *flowClassifier, confirmed bool) int64 {
	if confirmed && c.d[0].cls == ClassLight {
		return rtLightAllow
	}
	tok, tmean, tmx := rtSteady(&c.d[0])
	if tok {
		// (never rtLightAllow itself: that value marks a light flow)
		return int64(max(min(max(2*tmean, tmx+tmean), rtAllowMax), rtLightAllow+1))
	}
	// TwoWay: the other way is the user's steady stream (a call) and what
	// this end sends stays under RTMax, whatever its own pattern (a call
	// held back on a full QUIET link reads as bursts: twoway.go)
	if TwoWay != 0 {
		// a call held back keeps a backlog of its own (seconds of frames at
		// the start of a busy link, the round-4 rig): it may take the most
		// a tick, so it drains it, its user's realtime credit (RTMax) the
		// bound
		if rok, _, _ := rtSteady(&c.d[1]); rok && float64(c.d[0].sum) <= rtWindowMax() {
			return rtAllowMax
		}
	}
	return 0
}

// rtWindowMax is RTMax over C1's 2 s window, bytes.
func rtWindowMax() float64 { return float64(RTMax) / 8 * classSlots * classTick.Seconds() }

// rtSteady says whether one direction of a flow is steady over C1's window
// (rtSlotsMin ticks of 20 with bytes, more than classLightSpan in all, at
// most RTMax, no tick at the slot's ceiling), with its mean and largest
// tick.
func rtSteady(d *classDir) (ok bool, mean, mx int) {
	nz := 0
	for _, v := range d.slots {
		if v > 0 {
			nz++
		}
		mx = max(mx, int(v))
	}
	sum := int(d.sum)
	ok = nz >= rtSlotsMin && sum > classLightSpan && float64(sum) <= rtWindowMax() && mx < classSlotMax
	return ok, sum / classSlots, mx
}

// rtLane is the realtime lane e's next frame goes in (LaneRT for a light
// flow, LaneRTSteady for a steady one; 0: none): its allowance since its
// latest tick is not used up.
func (e *flowEnt) rtLane() Lane {
	if TuneRT == 0 || e.moved {
		return 0
	}
	a := e.rtAllow.Load()
	if a <= 0 || int64(e.stats.tx.Load())-e.rtBase.Load() >= a {
		return 0
	}
	if a == rtLightAllow {
		return LaneRT
	}
	return LaneRTSteady
}
