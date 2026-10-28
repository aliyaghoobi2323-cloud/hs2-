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
var TuneRT = 0

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
	d := &c.d[0]
	if confirmed && d.cls == ClassLight {
		return rtLightAllow
	}
	nz, mx := 0, 0
	for _, v := range d.slots {
		if v > 0 {
			nz++
		}
		mx = max(mx, int(v))
	}
	sum := int(d.sum)
	if nz < rtSlotsMin || sum <= classLightSpan || float64(sum) > float64(RTMax)/8*classSlots*float64(classTick)/1e9 || mx >= classSlotMax {
		return 0
	}
	mean := sum / classSlots
	return int64(min(max(2*mean, mx+mean), rtAllowMax))
}

// rtLane says whether e's next frame goes in the realtime lane: its
// allowance since its latest tick is not used up.
func (e *flowEnt) rtLane() bool {
	if TuneRT == 0 || e.moved {
		return false
	}
	a := e.rtAllow.Load()
	return a > 0 && int64(e.stats.tx.Load())-e.rtBase.Load() < a
}
