package engine

import (
	"math"
	"os"
	"time"
)

// hl223 (HS2_QUEUE_CTL_KEEP; HS2_QUEUE_CTL_SEEN=0 turns it off): the path's
// rate as what it delivered, not as the cap that overshot it.
//
// Lab hl222 u1b: every upload started at a kept cap of 0.95-0.99 of the path
// (452-470 in cap units; the links delivered 475-479 on every tick with a
// queue) and rose +2 %/tick: 462 -> 471 -> 480.6, a 13-17 ms queue, and the
// stripe gate halved the upload's width to 0 — 8 % under B. The kept cap was
// low because the path was learned from the onset cap (0.95 x what the links
// delivered) through min(0.97 x cap, good).
//
// So the controller keeps q.seen, the most the links delivered on a tick
// with a queue (qHigh) the cap bound (what they deliver never exceeds the path; with a queue it is
// the path): the cap rises qUpFast up to qSeenFrac of it, then only qUpSlow;
// a cap that overshoots goes back to qSeenFrac of it; a kept cap is
// at least qSeenFrac of it. A tick that delivers qSeenGrow x seen showed the path grew:
// the rises before hl223 (qUpFast, the regrow) apply.
var qSeenOff = os.Getenv("HS2_QUEUE_CTL_SEEN") == "0"

// qSeenFrac: the cap's place under seen. What the links deliver runs ~1 %
// over the cap (lab hl223 tS1: 476-478 under a 470.4 cap, and that cap still
// queued 11-23 ms), so qNearPath (0.985) of seen is the path itself in the
// cap's units.
const qSeenFrac = 0.975

// qSeenGrow: a binding tick that delivers this much over seen shows the path
// grew (tS1: 477.6 over a 476.1 seen took the cap +10 % to 526).
const qSeenGrow = 1.02

func qSeenOn() bool { return queueCtlKeep && !qSeenOff }

// seenFresh: seen is known and not older than qPathMemory.
func (q *qCtl) seenFresh(now time.Time) bool {
	return qSeenOn() && q.seen > 0 && now.Sub(q.seenAt) < qPathMemory
}

// noteSeen: the links delivered in.g on a tick the cap bound (or a cap's
// onset); prev is seen before it (0: none fresh).
func (q *qCtl) noteSeen(in qIn, prev float64) {
	if !qSeenOn() || in.g < qCapMinRate || in.delay < qHigh {
		return // hl223 tS1/hl216 tests: no-queue ticks run under the path (a cap that does not fill), not at it
	}
	if !q.seenFresh(in.now) || q.seenGuess {
		prev = 0 // hl226: the first binding queue tick replaces the onset's guess
	}
	q.seenClear = false // hl232: a queue tick measures the path itself
	if in.g > prev {
		q.seen, q.seenAt, q.seenGuess = in.g, in.now, false
	} else {
		q.seenAt = in.now // a queue at it again: still the path
	}
}

// qProbeTicks, qProbeFill (hl224): the growth probe. A cap held at
// qSeenFrac x seen cannot show a path that grew: the links deliver at most the
// cap, never qSeenGrow x seen (lab hl223 uRS: after 250 -> 500 the cap stayed at
// ~0.975 x 250, u1hi 267 vs 354). After qProbeTicks binding ticks in a row with
// no queue, the links filling qProbeFill of a cap at/above qSeenFrac x seen,
// the cap rises qUpFast a tick until a queue (qHigh) stops it — seen is then
// the most the probe delivered — or the links deliver qSeenGrow x seen (the
// path grew: the rises before hl223 apply).
//
// A probe whose links deliver qProbeGrow x seen with no queue found the path
// grew: qUpFirst at once (lab hl224 vRS1 with 3 ticks and the 1.02 gate: the
// +10 % steps started 12 s into u1hi, 317 vs hl222 354).
const (
	qProbeTicks = 2
	qProbeFill  = 0.95 // a single upload runs ~0.95-1.0 of its cap
	qProbeGrow  = 1.01 // over seen's own noise (tS1: 477.6 delivered over a 476.1 seen)
	// hl225: each probe that overshoots the same path doubles the clear
	// ticks before the next one, up to qProbeWaitMax (~32 s); a new burst
	// (links under half the cap) or a path that grew starts again at
	// qProbeTicks.
	qProbeWaitMax = 16
	// hl226: a new burst (qBurstQuiet ticks under half the cap, then the
	// links at it) probes on its first full tick within qBurstTicks;
	// qBurstUp when they fill qNearFill of the cap.
	qBurstQuiet = 2
	qBurstTicks = 3
	qBurstUp    = 1.03
	// hl226: a probe step from a cap at/over seen with no queue is
	// qProbeUp (a single upload runs ~0.96 of its cap: at 2 % steps it showed
	// qProbeGrow x seen only ~5 ticks after the burst, rate19 u1hi); a
	// probe tick delivering qProbeJump x seen is growth at once.
	qProbeUp   = 1.04
	qProbeJump = 1.03
)

// probeUp: the probe's step — qUpFast, or qProbeUp from a cap at/over seen
// whose links do not fill qNearFill of it (a single flow: it shows a path
// that grew only once the cap is ~5 % past it). Links that fill the cap
// (many flows) keep qUpFast: on a fixed path they queue at ~1.0 x seen, and
// a bigger step would be a bigger queue.
func (q *qCtl) probeUp(in qIn, seen float64) float64 {
	if seen > 0 && q.rate >= seen && in.g < qNearFill*q.rate {
		return qProbeUp
	}
	return qUpFast
}

// qPostBurstTicks (hl228): the ticks after a burst probe in which a clear
// tick delivering qSeenGrow x seen is growth at once.
const qPostBurstTicks = 3

// qProbeQuiet (hl228): how long after a probe step the edge's stripe gate
// leaves a queue under qHigh to the controller: the tick the probe's cap is
// in force and one more (the controller drains its own probe's queue).
const qProbeQuiet = 2*healthTick + healthTick/4

// probeStep (hl228): r, the cap after a probe step from q.rate. The edge's
// (upload) controller stops a step that would cross qSeenGrow x seen at it
// for one tick (lab hl227 zS1 u1b: a kept 463.8 probed to 487.4 past the
// ~470-475 the path delivers; its 6-14 ms queue cut the upload stripe
// 11 -> 4 every ~7 s, -5.2 %). A cap already there steps on (a path that
// grew must still show qProbeJump x seen). The step is recorded (probeAt).
func (q *qCtl) probeStep(in qIn, r, seen float64) float64 {
	if q.up && seen > 0 {
		if lim := qSeenGrow * seen; q.rate < lim && r > lim {
			r = lim
		}
	}
	if r > q.rate {
		q.probeAt = in.now
	}
	return r
}

// probing (hl228): a probe step raised the cap within qProbeQuiet of now.
func (q *qCtl) probing(now time.Time) bool {
	return q.rate > 0 && !q.probeAt.IsZero() && now.Sub(q.probeAt) < qProbeQuiet
}

func (q *qCtl) probeNeed() int {
	if q.probeWait > qProbeTicks {
		return q.probeWait
	}
	return qProbeTicks
}

// seenRise (a binding tick with no queue): the rise near seen; false: the
// rules before hl223 decide (seen unknown, or the links delivered more than
// it: the path grew).
// burstProbe: a new burst under a cap with a fresh (not guessed) seen — its
// first full tick probes (seenRise), so hl216's "a kept cap's first binding
// tick does not rise" stays out of it (lab hl226 yRS2 u1hi: the kept cap at
// 0.985 x path got no rise on the burst's first full tick, growth 14 s late).
func (q *qCtl) burstProbe(in qIn, prev float64) bool {
	return q.burst > 0 && prev > 0 && !q.seenGuess && q.seenFresh(in.now)
}

func (q *qCtl) seenRise(in qIn, prev float64) bool {
	if q.guessStep(in, prev) {
		return true
	}
	if q.postBurst > 0 && prev > 0 && q.seenFresh(in.now) && in.delay >= 0 && in.delay < qLow &&
		in.g >= qProbeFill*q.rate && in.g > qSeenGrow*prev {
		// hl228: right after a burst probe a clear tick over qSeenGrow x
		// seen is growth at once: on a fixed path the links queue at the
		// probe's cap, they do not deliver 1.02 x seen with no queue.
		q.probe, q.probeMax, q.probeHi, q.path, q.probeWait, q.postBurst = 0, 0, 0, 0, 0, 0
		return false
	}
	if q.burstProbe(in, prev) && in.delay >= 0 && in.delay < qLow &&
		in.g >= qProbeFill*q.rate && in.g <= qSeenGrow*prev {
		// hl226: a new burst's first full tick probes at once (lab hl225
		// d8: the kept cap sat under the path while the links climbed to
		// it; regrow 250 -> 500 started 12-16 s after the burst, two creep
		// ticks and the probe wait). Links that fill qNearFill of the cap:
		// qBurstUp. Then the probe goes on as one that waited.
		up := qUpFast
		if in.g >= qNearFill*q.rate {
			up = qBurstUp
		}
		q.burst, q.probeHi = 0, 0
		q.burstFrom, q.burstAt = q.rate, in.now // hl233
		q.probe = q.probeNeed()
		q.probeMax = math.Max(q.probeMax, in.g)
		// hl228: from qSeenFrac x seen at least (lab hl227 zRS3 d1hi: a
		// cap kept at 224.3 under a 238.7 seen probed to 231.0, still under
		// 0.975 x seen; the next tick rose to it with the probe reset and
		// growth came 14 s after the burst).
		q.rate = q.probeStep(in, math.Max(q.rate, qSeenFrac*prev)*up, prev)
		q.postBurst = qPostBurstTicks + 1
		return true
	}
	if q.probe >= q.probeNeed() && prev > 0 && in.delay >= 0 && in.delay < qLow &&
		in.g >= qProbeFill*q.rate && in.g > qProbeGrow*prev {
		// hl225: on two such ticks in a row — one is a fixed path's noise
		// (lab hl224 wS2 d1b: 467.1 over a ~462 seen took the cap +10 %
		// to 514.6, a 113 ms queue); the first rises qUpFast.
		// hl226: one tick qProbeJump over seen is no noise: at once.
		if q.probeHi++; q.probeHi >= 2 || in.g > qProbeJump*prev {
			q.probe, q.probeMax, q.probeHi, q.path, q.probeWait = 0, 0, 0, 0, 0 // hl224: the probe found room: rise as before any overshoot
			q.dropGuess(in)
			return false
		}
		q.probeMax = math.Max(q.probeMax, in.g)
		if q.burstProbe(in, prev) {
			// hl227: inside a new burst this tick is the burst probe
			// (yRS2: +2 % here, then two wait ticks): qBurstUp, and the
			// probe goes on as one that waited.
			q.burst = 0
			q.probe = q.probeNeed()
			q.burstFrom, q.burstAt = q.rate, in.now // hl233
			q.rate = q.probeStep(in, q.rate*qBurstUp, prev)
			q.postBurst = qPostBurstTicks + 1
			return true
		}
		q.probe++
		q.rate = q.probeStep(in, q.rate*q.probeUp(in, prev), prev)
		return true
	}
	q.probeHi = 0
	if prev <= 0 || !q.seenFresh(in.now) || in.g > qSeenGrow*prev {
		q.probe, q.probeMax, q.probeWait = 0, 0, 0
		if prev > 0 && in.g > qSeenGrow*prev {
			q.dropGuess(in)
		}
		return false
	}
	t := qSeenFrac * q.seen
	if q.rate < t {
		if q.postBurst == 0 {
			q.probe, q.probeMax = 0, 0 // hl228: right after a burst probe the probe goes on
		}
		up := qUpFast
		if q.path == 0 && !q.seenGuess {
			up = qUpFirst
		}
		q.rate = math.Min(q.rate*up, t)
		return true
	}
	if in.g >= qProbeFill*q.rate && in.delay >= 0 && in.delay < qLow {
		q.probe++
		q.probeMax = math.Max(q.probeMax, in.g)
	} else {
		q.probe, q.probeMax = 0, 0
	}
	if q.probe > q.probeNeed() {
		q.rate = q.probeStep(in, q.rate*q.probeUp(in, prev), prev) // hl224: probe for a path that grew
		return true
	}
	// hl225: no creep past qNearPath x seen while a probe waits (it waits
	// up to qProbeWaitMax ticks now: 0.5 % a tick would be the overshoot).
	q.rate = math.Min(q.rate*qUpSlow, math.Max(q.rate, qNearPath*q.seen))
	return true
}

// hl229: the onset queue under a cap at seen (lab hl228 aS3 u1b). A kept cap
// at 464.5 (seen 461.4) took a new upload's first full tick at 3 ms, crept to
// 466.8, and the next tick queued 23 ms: the cap went to 0.975 x seen (449.9)
// and qHoldTicks (10 s) held it there to the end of the phase (447-450
// delivered vs B ~449-475), and the stripe gate cut twice on that queue. A
// single queue tick under qFastDelay within a new burst's first ticks, with
// the cap within qOnsetNear of seen, is the burst meeting the path, not a
// throttle: the cap goes to qSeenFrac x seen with no hold (the probe goes on
// as usual from the next tick), once per burst. The edge's stripe gate leaves
// a queue up to qOnsetGate to the controller over that window and the
// tick after the drop (qProbeQuiet), like a probe's.
const (
	qOnsetNear = 0.03
	qOnsetGate = 25 * time.Millisecond
)

// nearSeen: a fresh, measured seen and the cap within qOnsetNear of it.
func (q *qCtl) nearSeen(now time.Time) bool {
	return q.seenFresh(now) && !q.seenGuess && q.rate >= (1-qOnsetNear)*q.seen && q.rate <= (1+qOnsetNear)*q.seen
}

// nearGuess (hl231): a fresh, unconfirmed onset guess and the cap within
// qOnsetNear of it.
func (q *qCtl) nearGuess(now time.Time) bool {
	return q.seenFresh(now) && q.seenGuess && q.rate >= (1-qOnsetNear)*q.seen && q.rate <= (1+qOnsetNear)*q.seen
}

// onsetStart: a new burst starts under the cap (step, q.burst set).
func (q *qCtl) onsetStart(now time.Time) {
	q.onsetGuess = false
	if !q.onsetSeen(now) {
		if !q.nearGuess(now) {
			q.onset = 0
			return
		}
		q.onsetGuess = true // hl231
	}
	q.onset = qBurstTicks + 1
	q.onsetQ = 0
	q.onsetUntil, q.onsetAt = now.Add(qBurstTicks*healthTick+healthTick/4), now
}

// onsetQueue (a binding queue tick): the burst's single onset queue tick —
// to qSeenFrac x seen, no hold. False: the usual rules.
//
// hl231 (lab hl230 dS1-3 u8, dRS2 u1hi): of ANY size. A kept cap at the path
// (468.2 / 469.5 / 479.4 after the burst probe, seen ~475) took 8 new
// uploads' first full tick at 77 / 91 / 78 ms; at >= qFastDelay that was
// hl216's drain (0.93 x cap) with qHoldTicks — 435-445 delivered for the rest
// of the phase (u8 -5.1 %), while the next tick was clear at 2-3 ms (the
// burst meeting the path, not a cap over it). dRS2 u1hi: one upload's BBR
// startup queued 56 ms at a kept 239.4 (seen 236, the path already 500):
// 0.93 + hold, growth 30 s late. A second queue tick in the window (a queue
// that stays) takes the usual rules. Under an unconfirmed guess (the window
// opened by nearGuess) noteSeen has just set seen to what this tick
// delivered: the cap goes to qSeenFrac of that.
func (q *qCtl) onsetQueue(in qIn) bool {
	if q.onset == 0 || !q.seenFresh(in.now) {
		return false
	}
	if in.g < qFullFrac*q.rate {
		// hl232: the burst's first, partial tick (onsetPartial): no
		// follow-down, no hold; the cap goes to qSeenFrac x a measured
		// seen only from over it
		q.onset, q.onsetGuess = 0, false
		if !q.seenGuess && q.rate > qSeenFrac*q.seen {
			q.path, q.pathAt = q.seen, in.now
			q.rate = q.floorOf(qSeenFrac * q.seen)
		}
		q.onsetQ = in.delay
		q.onsetUntil, q.onsetAt = in.now.Add(qProbeQuiet), in.now
		return true
	}
	if q.rate <= qSeenFrac*q.seen {
		return false
	}
	if !q.onsetGuess && !q.onsetSeen(in.now) {
		return false
	}
	if q.onsetGuess && q.seenGuess {
		// noteSeen did not take this tick (under qCapMinRate): the guess stands unconfirmed
		return false
	}
	q.onset, q.onsetGuess = 0, false
	q.path, q.pathAt = q.seen, in.now
	q.rate = q.floorOf(q.onsetTarget(in))
	q.burstFrom = 0
	q.onsetQ = in.delay
	q.onsetUntil, q.onsetAt = in.now.Add(qProbeQuiet), in.now
	return true
}

// hl229: the kept cap's confidence (lab hl228 d1b: S ran flat ~450 under a
// cap kept at 0.975 x seen, the path ~470-475; -5.1 %). A run that ended with
// qKeepSureTicks (>= 5 s) binding with no queue at qSeenFrac x seen or more
// shows the path carries it: that cap is kept at qKeepSure (qNearPath) x seen,
// the path itself in the cap's units. Otherwise qSeenFrac.
const (
	qKeepSure      = qNearPath
	qKeepSureTicks = 3
)

// noteClearRun (each capped tick): a binding clear tick at qSeenFrac x seen
// counts; a queue tick, or a binding tick under that level, ends the run.
// Ticks under the cap (the run's end, idle) leave it as it was.
func (q *qCtl) noteClearRun(in qIn) {
	binding := in.g >= qFullFrac*q.rate
	switch {
	case binding && in.delay >= 0 && in.delay < qHigh && q.seenFresh(in.now) && !q.seenGuess && q.rate >= 0.995*qSeenFrac*q.seen:
		q.clearRun++
	case in.delay >= qHigh && in.g >= q.rate/2, binding:
		q.clearRun = 0
	}
}

func (q *qCtl) keepSure() bool {
	return q.clearRun >= qKeepSureTicks && !q.seenGuess
}

// dropGuess (hl229, lab bS1): growth past seen while seen is only the onset's
// guess (seenGuess) shows the guess was BBR's ramp, not the path: forget it,
// so the cap rises as before any overshoot and the next binding queue tick
// sets a real seen. bS1 u1a: an onset tick at 431.8 (the path ~470) kept
// every later upload's cap at 0.975 x 431.8 = 421 although a probe ran clear
// at 494 (u1a -9 %, u1b -10 %, u8 -12 %).
func (q *qCtl) dropGuess(in qIn) {
	if q.seenGuess {
		q.seen, q.seenAt, q.seenGuess, q.seenClear = 0, time.Time{}, false, false
		q.seenDropAt = in.now // hl232
	}
}

// guessStep (hl230, lab hl229 cS1): while seen is only the onset's guess
// (seenGuess), a clear binding tick (the links fill qNearFill of the cap, no
// queue) at or over qSeenFrac x guess raises the cap qUpFast at once — no
// qUpSlow creep, no probe wait — until a binding queue tick sets a real seen
// (noteSeen) or the links deliver qSeenGrow x guess (dropGuess, below). cS1:
// a download onset tick at 412.4 (BBR's ramp; the path ~476) left the cap
// creeping 0.5 %/tick to 0.985 x guess with a probe only after 3 clear ticks:
// growth was found only in d8 (d1a/d1b/d8 375/373/400 vs B ~435). A fixed path
// costs one 2 % overshoot here, the same as a probe step.
func (q *qCtl) guessStep(in qIn, prev float64) bool {
	if !q.seenGuess || prev <= 0 || !q.seenFresh(in.now) || in.delay < 0 || in.delay >= qLow ||
		in.g < qNearFill*q.rate || q.rate < qSeenFrac*q.seen*0.995 || in.g > qSeenGrow*prev {
		return false
	}
	q.probe, q.probeMax, q.probeHi = 0, 0, 0
	q.rate *= qUpFast
	q.probeAt = in.now // the edge's stripe gate leaves its queue to the controller (qProbeQuiet)
	return true
}

// hl232 (lab hl231 eS3): the onset window and the cap kept after growth.
//
// eS3 u1b: a kept cap of 438 took a burst probe (446.8), then two growth
// steps with no queue (+10 %: 491.4, 540.6) under an unconfirmed guess that
// growth dropped: no seen, so the cap kept at the end was 491.4 (the last
// that fit), ~3 % over the path (~475). u8's onset window then did not open
// (the cap not within qOnsetNear of a seen), and its first tick — a burst
// half-way in, 49 ms at 434.8 under 0.9 x 491.4 — was a non-binding queue:
// follow-down 0.97 x 434.8 + hold, 413 for the phase (B 461-467).
//
// So (1) the most the links delivered on loaded clear ticks since the last
// queue (clearMax: no queue, so not over the path) becomes a measured seen
// when the cap is kept after growth (a seen it passed by qSeenGrow, or a
// guess growth dropped) — seenClear, a floor of the path — and that cap is kept at most
// qKeepSure (qNearPath) x it. The rises while the cap is on are unchanged.
// (2) The onset window opens for any cap at/above (1-qOnsetNear) x a measured
// seen (a kept cap above seen is the case that needs it most), and (3) the
// burst's first tick with a queue that does not fill the cap but would bind
// the onset's cap (qFullFrac x qSeenFrac x seen) is the onset too.

// noteClearMax (each capped tick): a loaded clear tick raises clearMax; a
// queue under load clears it (the path may be lower now).
func (q *qCtl) noteClearMax(in qIn) {
	switch {
	case in.delay >= qHigh && in.g >= q.rate/2:
		q.clearMax = 0
	case in.delay >= 0 && in.delay < qHigh && in.g >= q.rate/2 && in.g > q.clearMax:
		q.clearMax, q.clearAt = in.g, in.now
	}
}

// keepClearSeen (the cap is kept): clearMax becomes seen (seenClear) when
// it passed a fresh seen (measured or guessed) by qSeenGrow, or growth
// dropped the guess (dropGuess) within qPathMemory.
func (q *qCtl) keepClearSeen(now time.Time) {
	if !qSeenOn() || q.clearMax < qCapMinRate || now.Sub(q.clearAt) >= qPathMemory {
		return
	}
	if q.seenFresh(now) {
		if q.clearMax <= qSeenGrow*q.seen {
			return
		}
	} else if q.seenDropAt.IsZero() || now.Sub(q.seenDropAt) >= qPathMemory {
		return // no seen (measured or guessed) to clamp to: the rules before hl232
	}
	q.seen, q.seenAt, q.seenGuess, q.seenClear = q.clearMax, now, false, true
}

// onsetSeen: a fresh, measured seen and the cap at or over
// (1-qOnsetNear) x it.
func (q *qCtl) onsetSeen(now time.Time) bool {
	return q.seenFresh(now) && !q.seenGuess && q.rate >= (1-qOnsetNear)*q.seen
}

// onsetPartial: the burst's first tick (the window just opened) did not fill
// the cap but would bind the onset's cap: a burst that started mid-tick.
func (q *qCtl) onsetPartial(in qIn) bool {
	return q.onset == qBurstTicks+1 && q.seenFresh(in.now) && in.g >= qFullFrac*qSeenFrac*q.seen
}

// hl233 (lab hl232 fS1-3 d8, -4.0 %): a cap kept at qKeepSure x seen (470,
// seen ~477, the path ~478) took the new burst's probe to 484 (qBurstUp);
// the next tick queued 18-20 ms and the onset rule took the cap to qSeenFrac
// x seen (465), under the kept cap that had fit, then crept back 0.5 %/tick
// (467.8, 470.1) for ~6 s of the 15 s phase. The queue was the probe's step
// past the path, not the kept cap's: an onset tick within qBurstFromTicks of
// a burst probe, with a queue under qFastDelay, goes back to the cap the probe
// left (at most qKeepSure x seen), not lower.
const qBurstFromTicks = 2

// onsetTarget: the cap an onset queue tick goes to.
func (q *qCtl) onsetTarget(in qIn) float64 {
	t := qSeenFrac * q.seen
	if q.burstFrom > t && q.burstFrom <= qKeepSure*q.seen*1.0005 && in.delay < qFastDelay &&
		!q.burstAt.IsZero() && in.now.Sub(q.burstAt) <= qBurstFromTicks*healthTick+healthTick/4 {
		return q.burstFrom
	}
	return t
}
