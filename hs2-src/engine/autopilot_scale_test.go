package engine

import (
	"testing"
	"time"
)

// At up to 64 links the scaled controller is exactly the one that ran before:
// spares cap at 4, probe steps are a quarter (a half when chaining) of the
// pattern, a probe gets 15 s plus its links' turns at the dial gate.
func TestAutopilotScaledParamsMatchSmallPools(t *testing.T) {
	old := func(p int) int { // spare before the scaling
		if p == 0 {
			return 0
		}
		return min(max((p+3)/4, 1), 4)
	}
	for p := 0; p <= 64; p++ {
		if spare(p) != old(p) {
			t.Fatalf("spare(%d)=%d, was %d", p, spare(p), old(p))
		}
	}
	for _, c := range []struct{ p, want int }{{65, 5}, {160, 10}, {300, 19}} {
		if got := spare(c.p); got != c.want {
			t.Errorf("spare(%d)=%d, want %d", c.p, got, c.want)
		}
	}
	tun := defaultTunables()
	if got := tun.armTimeoutFor(64, 80); got != 15*time.Second+16*armPerLink {
		t.Errorf("armTimeoutFor(64,80)=%s", got)
	}
	if a := newAutopilot(2, 64, 8); a.tun.capMax != 256 {
		t.Errorf("capMax at max 64 = %d, want 256", a.tun.capMax)
	}
	if a := newAutopilot(2, 300, 8); a.tun.capMax != 1200 {
		t.Errorf("capMax at max 300 = %d, want 1200", a.tun.capMax)
	}
}

// Pressure-driven growth (beyond the floor) reaches hundreds of links: 600
// steady 1 Mbit/s flows (floor 75) on links each throttled to 2 Mbit/s need
// about 300 links. With a fixed spare of 4 the controller stalled near 165
// (direct) / 197 (reverse) links carrying about half the demand; scaled, it
// reaches about 230 (the rest is flows pinned to links that were already full).
func TestSimPressureGrowthReachesHundreds(t *testing.T) {
	if testing.Short() {
		t.Skip("long simulation")
	}
	for _, rev := range []bool{false, true} {
		s := newSim(t, simCfg{min: 2, max: 300, perLink: 8, statsOK: true, seed: 11, linkCap: 2 * mbit, pathCap: 2000 * mbit,
			reverse: rev, exitDials: rev})
		s.seedIdle(2000)
		s.gens = append(s.gens, genIdlePool(2000), genFlows(constN(600), 1*mbit, 0.1, 45*time.Second))
		var carried, demand float64
		s.onTick = func(s *sim) {
			if s.elapsed() >= 25*time.Minute {
				carried += s.carried
				demand += s.demand
			}
		}
		s.at(30 * time.Minute)
		share := carried / demand
		t.Logf("reverse=%v: max pattern %d links, last 5 min carried %.0f%% of demand", rev, s.maxT, 100*share)
		if s.maxT < 210 || share < 0.55 {
			t.Fatalf("reverse=%v: growth stalled at %d links carrying %.0f%% of demand", rev, s.maxT, 100*share)
		}
	}
}

// A shrink from hundreds closes more than 2 empty links per tick (2 up to 64
// retiring, a 32nd above, at most 8).
func TestClosesPerTickScales(t *testing.T) {
	for _, c := range []struct{ r, want int }{{0, 2}, {10, 2}, {64, 2}, {65, 3}, {150, 5}, {298, 8}, {1000, 8}} {
		if got := closesPerTick(c.r); got != c.want {
			t.Errorf("closesPerTick(%d)=%d, want %d", c.r, got, c.want)
		}
	}
}

// A link retiring for retireForce is no longer held by trickling connections
// (app keepalives under drainIdle): those are closed; a flowing one is not.
func TestReclaimForcedClosesTricklingNotFlowing(t *testing.T) {
	m, _, _ := newV2Manager(nil, 1, 8, false)
	pl := newV2PipeLink(t)
	trickle, flowing := pl.open(t), pl.open(t)
	pl.flowMu.Lock()
	trickle.ewma, trickle.lastActive = 300, time.Now() // a keepalive every so often
	flowing.ewma, flowing.lastActive = 50<<10, time.Now()
	pl.flowMu.Unlock()
	ml := addManaged(m, pl)
	ml.retiring = true

	ml.reclaiming.Store(true)
	m.reclaimIdle(ml, time.Now(), drainIdleDefault, false) // not yet retireForce
	if trickle.done.Load() || flowing.done.Load() {
		t.Fatal("closed a connection before retireForce")
	}
	ml.reclaiming.Store(true)
	m.reclaimIdle(ml, time.Now(), drainIdleDefault, true)
	if !trickle.done.Load() {
		t.Fatal("the trickling connection still holds the link after retireForce")
	}
	if flowing.done.Load() {
		t.Fatal("closed a flowing connection")
	}
	if m.forced.Load() != 1 {
		t.Fatalf("forced=%d, want 1", m.forced.Load())
	}
}
