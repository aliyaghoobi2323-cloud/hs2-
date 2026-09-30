package engine

import (
	"testing"
	"time"
)

// simPath models the thing the pool fights: a per-connection policer plus a
// shared path cap. Each link gets its own flow with cap flowCap bytes/s; the
// whole path cannot exceed pathCap. So goodput(have) = min(have*flowCap,
// pathCap): more links help until the path itself is full, exactly the regime
// where a multi-link tunnel beats a single throttled connection. saturated is
// true while another link could still move more bytes.
type simPath struct{ flowCap, pathCap float64 }

func (p simPath) goodput(have int) float64 {
	g := float64(have) * p.flowCap
	if g > p.pathCap {
		g = p.pathCap
	}
	return g
}
func (p simPath) saturated(have int) bool { return float64(have)*p.flowCap < p.pathCap }

// ceilingLinks is the smallest link count whose marginal gain drops below
// probeGain — where the autopilot is designed to settle.
func (a *autopilot) ceilingLinks(p simPath) int {
	for n := a.min; n < a.max; n++ {
		if p.goodput(n+1) <= p.goodput(n)*(1+a.probeGain) {
			return n
		}
	}
	return a.max
}

// drive runs the autopilot against a path for a number of ticks, moving the
// real link count one dial toward the target each tick (as the actuator does),
// and returns the final size and the peak it reached.
func drive(a *autopilot, p simPath, users, ticks int) (final, peak int) {
	have := a.min
	peak = have
	now := time.Unix(1_000_000, 0)
	for i := 0; i < ticks; i++ {
		now = now.Add(2 * time.Second)
		d := a.decide(apSample{now: now, users: users, have: have, aggGoodput: p.goodput(have), saturated: p.saturated(have)})
		switch {
		case d.target > have:
			have++
		case d.target < have:
			have--
		}
		if have > peak {
			peak = have
		}
	}
	return have, peak
}

// Under per-connection throttling the pool climbs from min to the path ceiling
// and settles there — not at max, not at min.
func TestAutopilotConvergesToPathCeiling(t *testing.T) {
	a := newAutopilot(2, 32, 50) // users below floor so throughput drives it
	p := simPath{flowCap: 10e6 / 8, pathCap: 85e6 / 8}
	want := a.ceilingLinks(p)
	final, _ := drive(a, p, 4, 120)
	if final != want {
		t.Fatalf("settled at %d links, want the path ceiling %d", final, want)
	}
	if a.probing {
		t.Fatal("left the autopilot probing forever")
	}
}

// A single un-throttled fast flow (no per-connection cap below the path) needs
// no aggregation: the pool stays near the floor instead of fanning out.
func TestAutopilotStaysSmallWhenOneLinkSaturatesPath(t *testing.T) {
	a := newAutopilot(2, 32, 50)
	p := simPath{flowCap: 100e6 / 8, pathCap: 90e6 / 8} // one link already exceeds the path
	final, peak := drive(a, p, 3, 60)
	if final > 3 {
		t.Fatalf("fanned out to %d links when one saturates the path", final)
	}
	if peak > 4 {
		t.Fatalf("probed too far (peak %d) before settling", peak)
	}
}

// The connection floor dominates regardless of throughput: many users force a
// proportional number of links even when the path is not saturated.
func TestAutopilotConnectionFloor(t *testing.T) {
	a := newAutopilot(2, 32, 8)
	p := simPath{flowCap: 100e6 / 8, pathCap: 90e6 / 8} // not throughput-bound
	final, _ := drive(a, p, 100, 60)                    // ceil(100/8)=13
	if final != 13 {
		t.Fatalf("floor not honored: %d links for 100 users, want 13", final)
	}
}

// The envelope is respected: the pool never exceeds max even under unlimited
// demand, and never drops below min when idle.
func TestAutopilotRespectsEnvelope(t *testing.T) {
	a := newAutopilot(3, 6, 50)
	hi := simPath{flowCap: 5e6 / 8, pathCap: 10e9 / 8} // effectively unlimited path
	final, peak := drive(a, hi, 10, 80)
	if peak > 6 || final > 6 {
		t.Fatalf("exceeded max: final=%d peak=%d", final, peak)
	}
	// Now go idle: it must shrink back to min, not below.
	a2 := newAutopilot(3, 6, 50)
	// prime it high, then let it sit idle
	have := 6
	now := time.Unix(1, 0)
	for i := 0; i < 200; i++ {
		now = now.Add(2 * time.Second)
		d := a2.decide(apSample{now: now, users: 0, have: have, aggGoodput: 0, saturated: false})
		if d.target < have {
			have--
		} else if d.target > have {
			have++
		}
	}
	if have != 3 {
		t.Fatalf("idle pool settled at %d, want min 3", have)
	}
}

// When the path later widens (per-flow cap unchanged) the background re-probe
// discovers it and the pool grows again, so the pattern keeps adapting "over
// the hour" instead of freezing at the first ceiling.
func TestAutopilotReprobesWhenPathWidens(t *testing.T) {
	a := newAutopilot(2, 32, 50)
	a.reProbeEvery = 30 * time.Second
	narrow := simPath{flowCap: 10e6 / 8, pathCap: 45e6 / 8} // ceiling ~4
	first, _ := drive(a, narrow, 4, 60)
	wide := simPath{flowCap: 10e6 / 8, pathCap: 200e6 / 8} // ceiling ~20
	// continue on the same autopilot with the wider path
	have := first
	now := time.Unix(2_000_000, 0)
	for i := 0; i < 200; i++ {
		now = now.Add(2 * time.Second)
		d := a.decide(apSample{now: now, users: 4, have: have, aggGoodput: wide.goodput(have), saturated: wide.saturated(have)})
		if d.target > have {
			have++
		} else if d.target < have {
			have--
		}
	}
	if have <= first+2 {
		t.Fatalf("did not re-probe into the widened path: stayed at %d (was %d)", have, first)
	}
}

// Calibrating means "no ceiling found yet": true on a fresh autopilot, false
// once it has settled once.
func TestAutopilotCalibratingFlag(t *testing.T) {
	a := newAutopilot(2, 32, 50)
	if !a.calibrating() {
		t.Fatal("fresh autopilot should be calibrating")
	}
	drive(a, simPath{flowCap: 10e6 / 8, pathCap: 45e6 / 8}, 4, 80)
	if a.calibrating() {
		t.Fatal("should have found a ceiling and left calibration")
	}
}
