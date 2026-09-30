package engine

import (
	"fmt"
	"time"
)

// autopilot is the pure decision core of the adaptive link pool — the "engine
// that measures everything and sizes the pattern from it". It is fed one sample
// per health tick and returns how many parallel links the pool SHOULD have right
// now, clamped to [min,max]. It holds no locks, opens no sockets and never
// touches a link, so its entire behaviour is driven by a deterministic simulator
// in autopilot_test.go. The LinkManager (direct edge) and the exit pool (reverse)
// are only actuators: they feed a sample in and move the real link count toward
// the target the autopilot returns.
//
// Two independent pressures set the size, and the larger always wins:
//
//  1. Connection floor — ceil(users / perLink), so no single link is ever asked
//     to carry a crowd. It reacts the instant users connect or leave.
//
//  2. Throughput demand — the tunnel exists to defeat per-connection throttling:
//     N links carry up to N× a single flow's policed cap. Whether one more link
//     would actually move more bytes is answered by experiment, not a guess:
//     while the live links are SATURATED (pushing as hard as the path allows)
//     and there is room, it speculatively adds one link and, a tick later, keeps
//     growing only if aggregate goodput really rose by probeGain. The moment an
//     added link stops helping — the path's own bandwidth, not per-flow
//     throttling, is now the ceiling — it settles there and remembers the size.
//
// It calibrates at start: until it has ever found a ceiling it is in the
// "calibrating" phase and, under load, probes every tick, so a fresh tunnel
// climbs to the right size in a few seconds rather than one link per re-probe.
// Afterwards it holds steady, re-probes only when demand climbs past the
// remembered ceiling (reprobeGain), and shrinks one link at a time when the pool
// has sat idle above its floor for scaleDownAfter — never yanking a busy link.
type autopilot struct {
	min, max, perLink int

	// Tunables, copied from package constants so a test can shorten the clocks.
	probeGain      float64       // a probe link must raise agg goodput this much to justify more
	reprobeGain    float64       // re-probe at once when demand climbs this far past the ceiling
	probeCooldown  time.Duration // quiet time after a plateau before re-probing
	reProbeEvery   time.Duration // even without a demand jump, re-probe this often while saturated
	scaleDownAfter time.Duration // idle-above-floor time before retiring one link

	// State, mutated only inside decide (single-goroutine, no locks).
	probing       bool
	preProbeAgg   float64   // aggregate goodput just before the current probe add
	ceilingAgg    float64   // aggregate goodput at the last plateau (0 = none found yet)
	ceilingSize   int       // pool size at that plateau (0 = never plateaued)
	coolUntil     time.Time // no new probe before this
	lastPlateauAt time.Time // when the pool last settled at a ceiling
	lowSince      time.Time // load has been at/under the floor since this
	lastPhase     apPhase
}

type apPhase int

const (
	apSteady      apPhase = iota // holding the size the measurements chose
	apCalibrating                // first ramp: no ceiling found yet, probing fast under load
	apProbing                    // speculatively grown by one, measuring the effect
	apShrinking                  // retiring an idle link
	apFloor                      // growing to meet the connection-count floor
)

func (p apPhase) String() string {
	switch p {
	case apCalibrating:
		return "calibrating"
	case apProbing:
		return "probing"
	case apShrinking:
		return "shrinking"
	case apFloor:
		return "scaling"
	default:
		return "steady"
	}
}

// apSample is one health tick's worth of measurements.
type apSample struct {
	now        time.Time
	users      int     // active user connections across the pool
	have       int     // live links right now
	aggGoodput float64 // EWMA aggregate goodput, bytes/sec
	saturated  bool    // the live links are network-limited (want to push more)
}

// apDecision is what the pool should do about its size.
type apDecision struct {
	target int     // desired link count, already clamped to [min,max]
	phase  apPhase // why, for the live monitor
	note   string  // one human sentence worth logging, or "" when unremarkable
}

func newAutopilot(min, max, perLink int) *autopilot {
	if min < 1 {
		min = 1
	}
	if max < min {
		max = min
	}
	if perLink < 1 {
		perLink = 8
	}
	return &autopilot{
		min: min, max: max, perLink: perLink,
		probeGain:      probeGain,
		reprobeGain:    reprobeGain,
		probeCooldown:  probeCooldownDur,
		reProbeEvery:   reProbeEveryDur,
		scaleDownAfter: scaleDownAfter,
	}
}

// floor is the connection-count baseline: enough links that no link carries
// more than perLink users, clamped to the envelope.
func (a *autopilot) floor(users int) int {
	want := (users + a.perLink - 1) / a.perLink // ceil
	if want < a.min {
		want = a.min
	}
	if want > a.max {
		want = a.max
	}
	return want
}

// calibrating reports whether the autopilot has yet to find any throughput
// ceiling — during which it probes aggressively so a new tunnel sizes itself
// quickly.
func (a *autopilot) calibrating() bool { return a.ceilingSize == 0 }

func (a *autopilot) decide(s apSample) apDecision {
	if a.lowSince.IsZero() {
		a.lowSince = s.now
	}
	floor := a.floor(s.users)

	// 1) Meet the connection floor first — fast and unconditional. A jump in
	// users restarts the throughput search from here.
	if s.have < floor {
		a.probing = false
		a.lowSince = s.now
		return a.decide2(floor, apFloor, "")
	}

	canGrow := s.have < a.max && s.users > 0 && s.saturated

	// 2) Evaluate an in-flight probe: did the extra link move more bytes?
	if a.probing {
		if s.aggGoodput > a.preProbeAgg*(1+a.probeGain) && canGrow {
			a.preProbeAgg = s.aggGoodput // it helped and there is room — keep climbing
			a.lowSince = s.now
			return a.decide2(s.have+1, a.rampPhase(), "")
		}
		// Growth ended — either the extra link stopped helping, or the path is now
		// full (saturated cleared) or we hit max. Settle here and remember the
		// ceiling so we hold this size until demand or the path changes.
		a.settle(s.now, s.have, s.aggGoodput)
		return a.decide2(s.have, apSteady,
			fmt.Sprintf("sized to %d links at ~%.1f Mbit/s — more links stopped helping", s.have, mbitps(s.aggGoodput)))
	}

	// 3) Start a probe when the pool is saturated and there is room, once the
	// cooldown has elapsed and either no ceiling is known, demand has climbed
	// past it (react fast), or enough time has passed to re-check whether the
	// path has since widened (slow background exploration, BBR-style).
	if canGrow && !s.now.Before(a.coolUntil) &&
		(a.ceilingSize == 0 ||
			s.aggGoodput > a.ceilingAgg*(1+a.reprobeGain) ||
			(!a.lastPlateauAt.IsZero() && s.now.Sub(a.lastPlateauAt) >= a.reProbeEvery)) {
		a.probing = true
		a.preProbeAgg = s.aggGoodput
		a.lowSince = s.now
		return a.decide2(s.have+1, a.rampPhase(), "")
	}

	// 4) Shrink slowly: only above the floor, only after the pool has stayed
	// idle-or-unsaturated for scaleDownAfter, and only by one link (the actuator
	// retires a link with no users, so a busy link is never dropped).
	if s.have > floor && !s.saturated {
		if s.now.Sub(a.lowSince) > a.scaleDownAfter {
			a.lowSince = s.now
			return a.decide2(s.have-1, apShrinking, "")
		}
		return a.decide2(s.have, apSteady, "")
	}
	a.lowSince = s.now
	return a.decide2(s.have, apSteady, "")
}

// settle ends a probe: record the current size as the throughput ceiling and
// start the cooldown before the next probe.
func (a *autopilot) settle(now time.Time, size int, agg float64) {
	a.probing = false
	a.ceilingAgg, a.ceilingSize = agg, size
	a.coolUntil = now.Add(a.probeCooldown)
	a.lastPlateauAt = now
	a.lowSince = now
}

// rampPhase labels a growth step calibrating until a ceiling has been found.
func (a *autopilot) rampPhase() apPhase {
	if a.calibrating() {
		return apCalibrating
	}
	return apProbing
}

func (a *autopilot) decide2(target int, phase apPhase, note string) apDecision {
	if target < a.min {
		target = a.min
	}
	if target > a.max {
		target = a.max
	}
	a.lastPhase = phase
	return apDecision{target: target, phase: phase, note: note}
}

// mbitps converts bytes/sec to Mbit/s for human-readable notes.
func mbitps(bytesPerSec float64) float64 { return bytesPerSec * 8 / 1e6 }
