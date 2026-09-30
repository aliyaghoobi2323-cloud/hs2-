package engine

import (
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

// Scenario tests for the autopilot against the flow-level plant in
// autopilot_sim_test.go. Rates are bytes/s; mbit converts.

// at runs the sim until t (from its start) and returns it.
func (s *sim) at(t time.Duration) *sim {
	for time.Duration(s.tick)*healthTick < t {
		s.step()
	}
	return s
}

func (s *sim) elapsed() time.Duration { return time.Duration(s.tick) * healthTick }

// productionTraffic is the reported deployment: ~250 open xray connections
// (idle, closed by the panel 300 s after their last byte, reconnected by
// clients), ~17 short-lived active flows averaging 0.25 Mbit/s with bursty
// demand, and one video that wants 3 Mbit/s.
func productionTraffic(s *sim) {
	s.seedIdle(250)
	s.gens = append(s.gens, genIdlePool(250), genFlows(constN(17), 0.25*mbit, 0.6, 60*time.Second))
	s.rate(3*mbit, 0.1, 2*time.Hour)
}

// 1. The reported bug, replayed: the pool must never climb past its warm size
// and must come down to what the traffic needs, with a reason every tick.
func TestSimProductionReplay(t *testing.T) {
	t.Run("capped video", func(t *testing.T) {
		s := newSim(t, simCfg{min: 2, max: 32, reverse: true, exitDials: true, statsOK: true,
			linkCap: 2.4 * mbit, pathCap: 100 * mbit, seed: 1})
		productionTraffic(s)
		s.at(3 * time.Minute)
		if s.a.T > 5 {
			t.Fatalf("T=%d at 3 min, want <= 5\n%s", s.a.T, s.dump())
		}
		s.at(8 * time.Minute)
		if s.serving() > 5 || len(s.links) > 6 {
			t.Fatalf("at 8 min: %d serving, %d up; want <= 5 and <= 6\n%s", s.serving(), len(s.links), s.dump())
		}
		s.at(30 * time.Minute)
		if s.maxT > 8 || s.probes != 0 || s.emptyReason != 0 {
			t.Fatalf("maxT=%d probes=%d emptyReason=%d; want <=8, 0, 0\n%s", s.maxT, s.probes, s.emptyReason, s.dump())
		}
		if s.cuts != 0 {
			t.Fatalf("%d active connections cut", s.cuts)
		}
	})
	t.Run("no cap", func(t *testing.T) {
		s := newSim(t, simCfg{min: 2, max: 32, reverse: true, exitDials: true, statsOK: true,
			pathCap: 100 * mbit, seed: 2})
		productionTraffic(s)
		s.at(30 * time.Minute)
		if s.maxT > 8 || s.probes != 0 || s.a.T > 5 {
			t.Fatalf("maxT=%d probes=%d T=%d; want <=8, 0, <=5\n%s", s.maxT, s.probes, s.a.T, s.dump())
		}
	})
}

// 2. An edge upgraded while stuck at 32 comes down, and no connection that is
// still moving data is ever closed.
func TestSimProductionReplayFromStuck32(t *testing.T) {
	s := newSim(t, simCfg{min: 2, max: 32, reverse: true, exitDials: true, statsOK: true,
		linkCap: 2.4 * mbit, pathCap: 100 * mbit, seed: 3, startT: 32})
	productionTraffic(s)
	s.at(4 * time.Minute)
	if s.a.T > 6 { // bursts reach ~9.5 Mbit/s at 2.4 Mbit/s per link
		t.Fatalf("T=%d at 4 min, want <= 6\n%s", s.a.T, s.dump())
	}
	// Retiring links close once the panel has closed their idle connections
	// (300 s after each one's last byte).
	s.at(12 * time.Minute)
	if len(s.links) > 7 {
		t.Fatalf("%d links up at 12 min, want <= 7\n%s", len(s.links), s.dump())
	}
	if s.cuts != 0 {
		t.Fatalf("%d active connections cut", s.cuts)
	}
}

// tracksTraffic: 1 Mbit/s flows (e.g. streams) on a path that throttles each
// connection to 2 Mbit/s; n(s) of them active at a time.
func tracksTraffic(n func(*sim) float64) func(*sim) {
	return genFlows(n, 1*mbit, 0.1, 45*time.Second)
}

func stepDemand(lo, hi float64, from, to time.Duration) func(*sim) float64 {
	return func(s *sim) float64 {
		if e := s.elapsed(); e >= from && e < to {
			return hi
		}
		return lo
	}
}

// 3. Demand 6 → 25 → 6 Mbit/s under per-connection throttling: the pool grows
// to carry the peak and comes back down after it. Five seeds; the flows arrive
// and end as a Poisson process, so "6" and "25" are averages with real bursts.
func TestSimTracksUpAndDown(t *testing.T) {
	for _, stats := range []bool{true, false} {
		name := "exit stats"
		if !stats {
			name = "older exit"
		}
		t.Run(name, func(t *testing.T) {
			var sumPct float64
			for seed := uint64(1); seed <= 5; seed++ {
				s := newSim(t, simCfg{min: 2, max: 32, reverse: true, exitDials: true, statsOK: stats,
					linkCap: 2 * mbit, pathCap: 100 * mbit, seed: seed})
				s.gens = append(s.gens, tracksTraffic(stepDemand(6, 25, 5*time.Minute, 15*time.Minute)), genIdlePool(60))
				var dem, car float64
				s.onTick = func(s *sim) {
					if e := s.elapsed(); e >= 7*time.Minute && e < 15*time.Minute {
						dem += s.demand
						car += s.carried
					}
				}
				s.at(15 * time.Minute)
				pct := 100 * car / dem
				sumPct += pct
				if !stats {
					// No download pressure: growth only from the active-flow floor.
					if s.probes != 0 || s.maxT > 8 {
						t.Fatalf("seed %d: probes=%d maxT=%d, want 0 and <= 8\n%s", seed, s.probes, s.maxT, s.dump())
					}
				} else if pct < 82 || s.a.T < 12 {
					t.Fatalf("seed %d high phase: carried %.0f%% of demand with T=%d; want >= 82%% and T >= 12\n%s",
						seed, pct, s.a.T, s.dump())
				}
				prev := s.a.T
				s.onTick = func(s *sim) {
					if s.elapsed() < 20*time.Minute && s.a.T > prev {
						t.Fatalf("seed %d: T rose %d → %d during the descent at %s\n%s", seed, prev, s.a.T, fmtDur(s.elapsed()), s.dump())
					}
					prev = s.a.T
				}
				s.at(21 * time.Minute)
				if s.a.T > 9 {
					t.Fatalf("seed %d: T=%d 6 min after the drop, want <= 9\n%s", seed, s.a.T, s.dump())
				}
				s.at(27 * time.Minute)
				if len(s.links) > 8 {
					t.Fatalf("seed %d: %d links up 12 min after the drop, want <= 8\n%s", seed, len(s.links), s.dump())
				}
			}
			if stats && sumPct/5 < 88 {
				t.Fatalf("mean carried %.1f%% of demand in the high phase, want >= 88%%", sumPct/5)
			}
		})
	}
}

// 4. Bursty traffic that no cap ever binds must never trigger growth.
func TestSimNoGrowthUnderBurstyNoise(t *testing.T) {
	d := 2 * time.Hour
	if testing.Short() {
		d = 30 * time.Minute
	}
	s := newSim(t, simCfg{min: 2, max: 32, reverse: true, exitDials: true, statsOK: true,
		pathCap: 1000 * mbit, seed: 5})
	s.gens = append(s.gens, genIdlePool(200), genFlows(constN(20), 0.5*mbit, 0.6, 60*time.Second),
		func(s *sim) {
			for i := s.poisson(2); i > 0; i-- {
				s.web(50e3 + s.rnd.Float64()*450e3)
			}
		})
	s.run(d)
	if s.probes != 0 || s.maxT > 8 {
		t.Fatalf("probes=%d maxT=%d, want 0 and <= 8\n%s", s.probes, s.maxT, s.dump())
	}
}

// pathFull: 20 users downloading back to back (each file a new connection)
// on a path whose total is 8 Mbit/s with 2 Mbit/s per connection.
func pathFull(t *testing.T, seed uint64, stats bool) *sim {
	s := newSim(t, simCfg{min: 2, max: 32, reverse: true, exitDials: true, statsOK: stats,
		linkCap: 2 * mbit, pathCap: 8 * mbit, seed: seed})
	for i := 0; i < 20; i++ {
		s.bulk(2e6, 3*time.Hour)
	}
	s.gens = append(s.gens, genIdlePool(50))
	return s
}

// 5. When the path itself is full, more links do not help: every probe fails,
// the backoff grows to its cap, and the pool does not ratchet up.
func TestSimPathFullNoRatchet(t *testing.T) {
	d := 2 * time.Hour
	if testing.Short() {
		d = 45 * time.Minute
	}
	s := pathFull(t, 6, true)
	maxK, t15 := 0, 0
	s.onTick = func(s *sim) {
		maxK = max(maxK, s.a.k)
		if t15 == 0 && s.elapsed() >= 15*time.Minute && s.a.pr == nil {
			t15 = s.a.T
		}
	}
	s.run(d)
	for s.a.pr != nil {
		s.step()
	}
	if s.succ != 0 || s.probes > 20 || maxK < 5 && d == 2*time.Hour || s.a.T > t15 {
		t.Fatalf("succ=%d probes=%d maxK=%d T(end)=%d T(15m)=%d; want 0, <=20, >=5, T(end) <= T(15m)\n%s",
			s.succ, s.probes, maxK, s.a.T, t15, s.dump())
	}
	t.Run("older exit", func(t *testing.T) {
		s := pathFull(t, 7, false)
		s.run(30 * time.Minute)
		if s.probes != 0 || s.maxT > 8 {
			t.Fatalf("older exit: probes=%d maxT=%d\n%s", s.probes, s.maxT, s.dump())
		}
	})
}

// 6. Constant pressure on a full path: no creep between probes.
func TestSimConstantPressedNoCreep(t *testing.T) {
	s := pathFull(t, 8, true)
	t10 := 0
	s.onTick = func(s *sim) {
		if t10 == 0 && s.elapsed() >= 10*time.Minute && s.a.pr == nil {
			t10 = s.a.T
		}
	}
	s.run(30 * time.Minute)
	for s.a.pr != nil {
		s.step()
	}
	if s.a.T != t10 {
		t.Fatalf("T(10m)=%d T(30m)=%d, want equal\n%s", t10, s.a.T, s.dump())
	}
}

// 7. One capped flow pinned to its link never triggers growth: another link
// could not help it, and there is a free link for new flows.
func TestSimSinglePinnedCappedFlowNoProbe(t *testing.T) {
	for S := 2; S <= 8; S++ {
		s := newSim(t, simCfg{min: 2, max: 32, reverse: true, exitDials: true, statsOK: true,
			linkCap: 2 * mbit, pathCap: 100 * mbit, seed: uint64(10 + S), startT: S})
		s.bulk(1e15, 2*time.Hour)
		s.gens = append(s.gens, genIdlePool(50))
		s.run(time.Hour)
		if s.probes != 0 {
			t.Fatalf("S=%d: %d probes, want 0\n%s", S, s.probes, s.dump())
		}
	}
}

// 8. A probe that no new flow reaches is inconclusive: its links are kept as
// spares, and it does not repeat.
func TestSimInconclusiveKeepsSpare(t *testing.T) {
	s := newSim(t, simCfg{min: 3, max: 8, reverse: true, exitDials: true, statsOK: true,
		linkCap: 2 * mbit, pathCap: 100 * mbit, seed: 18, startT: 3})
	for i := 0; i < 3; i++ {
		s.bulk(1e15, 2*time.Hour)
	}
	for s.inconcl == 0 && s.elapsed() < 10*time.Minute {
		s.step()
	}
	dials := s.dials
	s.run(30 * time.Minute)
	if s.probes != 1 || s.inconcl != 1 || s.a.T != 4 || s.dials != dials {
		t.Fatalf("probes=%d inconclusive=%d T=%d dials after verdict=%d; want 1, 1, 4, 0\n%s",
			s.probes, s.inconcl, s.a.T, s.dials-dials, s.dump())
	}
}

// 9. Per-connection throttling with arriving bulk users: the pool grows so
// that (nearly) every flow gets a link of its own.
func TestSimPerFlowGrowthFollowsArrivals(t *testing.T) {
	s := newSim(t, simCfg{min: 2, max: 32, reverse: true, exitDials: true, statsOK: true,
		linkCap: 1 * mbit, pathCap: 100 * mbit, seed: 19})
	started := 0
	s.gens = append(s.gens, func(s *sim) {
		if started < 24 && s.tick%1 == 0 && s.elapsed() <= time.Minute {
			// ~one arrival per 2.5 s over the first minute
			for ; started < 24 && float64(started) < s.elapsed().Seconds()/2.5; started++ {
				s.bulk(8e6, 3*time.Hour)
			}
		}
	})
	s.at(3 * time.Minute)
	alone := 0
	for _, l := range s.links {
		act := 0
		for _, c := range l.conns {
			if c.bulk {
				act++
			}
		}
		if act == 1 {
			alone++
		}
	}
	if alone < 22 {
		t.Fatalf("%d of 24 flows on their own link at 3 min (T=%d), want >= 22\n%s", alone, s.a.T, s.dump())
	}
}

// 10. A reconnect storm (hundreds of connections doing a handshake) is not
// traffic: the pool does not grow.
func TestSimReconnectStormIgnored(t *testing.T) {
	s := newSim(t, simCfg{min: 2, max: 32, reverse: true, exitDials: true, statsOK: true,
		linkCap: 2 * mbit, pathCap: 100 * mbit, seed: 20})
	s.run(10 * time.Minute)
	T := s.a.T
	for i := 0; i < 5; i++ { // 300 connections within 10 s
		for j := 0; j < 60; j++ {
			s.idle()
		}
		s.step()
	}
	s.run(2 * time.Minute)
	if s.maxT > 8 || s.a.T > T || s.probes != 0 {
		t.Fatalf("T %d → %d (max %d), probes=%d; want unchanged\n%s", T, s.a.T, s.maxT, s.probes, s.dump())
	}
}

// 11. Severe throttling (400 kbit/s per connection, 8 flows per link): every
// flow still counts as active, and the pool grows.
func TestSimSevereThrottling(t *testing.T) {
	s := newSim(t, simCfg{min: 2, max: 32, reverse: true, exitDials: true, statsOK: true,
		linkCap: 400 * kbit, pathCap: 100 * mbit, seed: 21})
	for i := 0; i < 64; i++ {
		s.bulk(400e3, 3*time.Hour)
	}
	maxFlowing := 0
	s.onTick = func(s *sim) {
		n := 0
		for _, l := range s.links {
			n += l.flowing
		}
		maxFlowing = max(maxFlowing, n)
	}
	s.run(5 * time.Minute)
	if maxFlowing < 60 || s.a.T < 16 {
		t.Fatalf("max flowing %d of 64, T=%d; want >= 60 and T >= 16\n%s", maxFlowing, s.a.T, s.dump())
	}
}

// 12. A shrink that turns out too deep is undone at once (the retiring links
// are still up) and held; each further undo doubles the hold, so shrink/undo
// cycles are bounded. Driven with crafted samples: in the plant, real pressure
// samples correct a stale capacity estimate before a shrink can overshoot.
func TestAutopilotOvershootRestore(t *testing.T) {
	a := newAutopilot(2, 32, 8)
	now := time.Unix(1e9, 0)
	const G = 1e6
	sample := func(S int, pressed bool) apDecision {
		now = now.Add(healthTick)
		smp := apSample{now: now, G: G, flowing: 10, open: 100, growable: true}
		for id := 0; id < 8; id++ {
			l := apLink{id: id, serving: id < S, retiring: id >= S, rate: G / 8, rate10: G / 8, flowing: 1}
			if id < S {
				l.pressed, l.sustained = pressed, G/8
			}
			smp.links = append(smp.links, l)
		}
		return a.decide(smp)
	}
	var restores []time.Time
	var ttls []time.Duration
	for i := 0; i < 1800; i++ { // one hour
		T := a.T
		short := false
		if T < 8 && now.Sub(a.lastShrinkAt) < 20*time.Second {
			short = true // every shrink leaves the remaining links at their limit
		}
		d := sample(T, short)
		if strings.Contains(d.note, "undone") {
			if d.target != 8 {
				t.Fatalf("restored to %d, want 8", d.target)
			}
			restores = append(restores, now)
			ttls = append(ttls, a.hold.until.Sub(now))
		}
		if len(restores) > 0 && now.Sub(restores[len(restores)-1]) < ttls[len(ttls)-1] && a.T < 8 {
			t.Fatalf("shrank to %d %s after a restore held for %s", a.T, now.Sub(restores[len(restores)-1]), ttls[len(ttls)-1])
		}
	}
	if len(restores) < 2 || len(restores) > 3 {
		t.Fatalf("%d restores in 1 h, want 2..3 (hold doubles: 10, 20, 40 min)", len(restores))
	}
	for i, ttl := range ttls {
		if want := 10 * time.Minute << i; ttl != want {
			t.Fatalf("hold %d = %s, want %s", i, ttl, want)
		}
	}
}

// 13. Square-wave demand: bounded changes and almost no real closes.
func TestSimOscillationBound(t *testing.T) {
	for _, period := range []time.Duration{60 * time.Second, 120 * time.Second} {
		s := newSim(t, simCfg{min: 2, max: 32, reverse: true, exitDials: true, statsOK: true,
			linkCap: 2 * mbit, pathCap: 100 * mbit, seed: uint64(period / time.Second)})
		s.gens = append(s.gens, genIdlePool(60), func(s *sim) {
			ph := s.elapsed() % period
			if ph < healthTick { // high half starts: 16 flows for half a period
				for i := 0; i < 16; i++ {
					s.rate(1*mbit, 0.1, period/2)
				}
			}
		})
		for i := 0; i < 4; i++ {
			s.rate(1*mbit, 0.1, 2*time.Hour)
		}
		s.run(10 * time.Minute)
		for w := 0; w < 2; w++ {
			ch, cl := s.tChanges, s.closes
			s.run(10 * time.Minute)
			if s.tChanges-ch > 6 || s.closes-cl > 2 {
				t.Fatalf("period %s window %d: %d target changes, %d closes; want <= 6 and <= 2\n%s",
					period, w, s.tChanges-ch, s.closes-cl, s.dump())
			}
		}
	}
}

// 14. Whatever happened before, a tunnel with no traffic ends at min links.
func TestSimPropertyIdleSettlesAtMin(t *testing.T) {
	n := 200
	if testing.Short() {
		n = 30
	}
	for i := 0; i < n; i++ {
		r := rand.New(rand.NewPCG(uint64(i), 99))
		cfg := simCfg{min: 1 + r.IntN(4), max: 8 + r.IntN(25), reverse: r.IntN(2) == 0, exitDials: true,
			statsOK: r.IntN(5) > 0, pathCap: (5 + r.Float64()*195) * mbit, seed: uint64(1000 + i)}
		if r.IntN(3) > 0 {
			cfg.linkCap = (0.5 + r.Float64()*4.5) * mbit
		}
		s := newSim(t, cfg)
		nf, rf := float64(r.IntN(40)), (0.1+r.Float64()*2.9)*mbit
		idle := r.IntN(300)
		s.seedIdle(idle)
		s.gens = append(s.gens, genIdlePool(idle), genFlows(constN(nf), rf, 0.4, 60*time.Second))
		for j := r.IntN(10); j > 0; j-- {
			s.bulk(4e6, 3*time.Hour)
		}
		s.run(time.Duration(5+r.IntN(16)) * time.Minute)
		// Traffic stops.
		s.gens = nil
		for c := range s.conns {
			c.until, c.bulk, c.remain, c.hb = s.now, false, -1, 0
		}
		s.run(10*time.Minute + s.cfg.idleClose)
		if s.a.T != cfg.min || len(s.links) != cfg.min {
			t.Fatalf("history %d (%+v): T=%d links=%d conns=%d, want %d\n%s",
				i, cfg, s.a.T, len(s.links), len(s.conns), cfg.min, s.dump())
		}
	}
}

// 15. A probe is judged only once its links exist; if they never come up it
// is abandoned and not retried for a minute.
func TestSimProbeArmsOnlyWhenLinksUp(t *testing.T) {
	s := newSim(t, simCfg{min: 2, max: 32, reverse: true, exitDials: false, statsOK: true,
		linkCap: 1 * mbit, pathCap: 100 * mbit, seed: 23})
	for i := 0; i < 16; i++ {
		s.bulk(8e6, 3*time.Hour)
	}
	var aborts []time.Time
	var starts []time.Time
	had := false
	s.onTick = func(s *sim) {
		if s.a.pr != nil && !had {
			starts = append(starts, s.now)
		}
		had = s.a.pr != nil
		if len(aborts) < s.aborts {
			aborts = append(aborts, s.now)
			if s.a.T != 8 {
				t.Fatalf("after the abort T=%d, want back to 8", s.a.T)
			}
		}
	}
	s.run(10 * time.Minute)
	if s.aborts < 2 || s.succ+s.fails+s.inconcl != 0 {
		t.Fatalf("aborts=%d verdicts=%d; want >= 2 aborts and no verdict\n%s", s.aborts, s.succ+s.fails+s.inconcl, s.dump())
	}
	for i := 1; i < len(starts); i++ {
		if gap := starts[i].Sub(aborts[i-1]); gap < 60*time.Second {
			t.Fatalf("probe %d started %s after the previous abort, want >= 60 s", i, gap)
		}
	}
}

// A total outage (no links in the sample) is not zero demand: the pool keeps
// its size instead of decaying to min while nothing can be measured.
func TestAutopilotOutageKeepsSize(t *testing.T) {
	a := newAutopilot(2, 32, 8)
	a.T = 12
	now := time.Unix(1e9, 0)
	for i := 0; i < 300; i++ { // 10 minutes without a single link
		now = now.Add(healthTick)
		if d := a.decide(apSample{now: now, growable: true}); d.target != 12 {
			t.Fatalf("tick %d: target %d during an outage, want 12 kept", i, d.target)
		}
	}
	if len(a.hist) != 0 {
		t.Fatalf("the outage entered the history (%d ticks)", len(a.hist))
	}
}

// With an exit that cannot add links (no pool control), the active-flow floor
// is capped at what is up and does not re-fire (and log) every tick.
func TestAutopilotFloorNotRepeatedWhenNotGrowable(t *testing.T) {
	a := newAutopilot(2, 32, 8)
	now := time.Unix(1e9, 0)
	notes := 0
	for i := 0; i < 30; i++ {
		now = now.Add(healthTick)
		smp := apSample{now: now, flowing: 200, open: 300, growable: false}
		for id := 0; id < 4; id++ {
			smp.links = append(smp.links, apLink{id: id, serving: true, rate: 1e5, flowing: 50})
		}
		d := a.decide(smp)
		if d.note != "" {
			notes++
		}
		if d.target > 4 {
			t.Fatalf("target %d with 4 links and no pool control", d.target)
		}
	}
	if notes > 1 {
		t.Fatalf("%d decision notes in 30 ticks, want at most 1", notes)
	}
}

// 17. The envelope holds: unlimited demand never exceeds max, and an idle
// pool settles at exactly min.
func TestSimEnvelope(t *testing.T) {
	s := newSim(t, simCfg{min: 3, max: 12, reverse: false, exitDials: true, statsOK: true,
		linkCap: 500 * kbit, pathCap: 1000 * mbit, seed: 24})
	for i := 0; i < 100; i++ {
		s.bulk(2e6, 20*time.Minute)
	}
	s.run(20 * time.Minute)
	if s.maxT > 12 || s.maxPhys > 12 {
		t.Fatalf("maxT=%d maxPhys=%d, want <= 12\n%s", s.maxT, s.maxPhys, s.dump())
	}
	s.gens = nil
	s.run(20 * time.Minute)
	if s.a.T != 3 || len(s.links) != 3 {
		t.Fatalf("idle: T=%d links=%d, want 3\n%s", s.a.T, len(s.links), s.dump())
	}
}

// ---- 18. the probe verdict, unit-level --------------------------------------

// probeRig drives a bare autopilot through one probe: `from` serving links, all
// pressed (so the pool is short of free links), carrying gb bytes/s in total
// with lognormal noise cv. Once the probe starts, the new links carry rNew in
// total (pressed too if newPressed: the path is full) and the pool total
// becomes gb+dG. It returns the verdict note.
func probeRig(a *autopilot, now *time.Time, rnd *rand.Rand, from int, gb, cv, rNew, dG float64, newPressed bool) string {
	noise := func() float64 {
		if cv == 0 {
			return 1
		}
		sig := math.Sqrt(math.Log(1 + cv*cv))
		return math.Exp(rnd.NormFloat64()*sig - sig*sig/2)
	}
	for i := 0; i < 1000; i++ {
		*now = now.Add(healthTick)
		smp := apSample{now: *now, flowing: 60, open: 200, growable: true}
		per := gb / float64(from)
		for id := 0; id < from; id++ {
			smp.links = append(smp.links, apLink{id: id, serving: true, pressed: true, rate: per, rate10: per, sustained: per, flowing: 4})
		}
		if pr := a.pr; pr != nil {
			per = (gb + dG - rNew) / float64(from)
			for id := range smp.links {
				smp.links[id].rate = per
			}
			for id := from; id < pr.to; id++ {
				smp.links = append(smp.links, apLink{id: id, serving: true, servingSince: pr.start,
					rate: rNew / float64(pr.to-from), pressed: newPressed && rNew > 0})
			}
		}
		k := noise()
		smp.G = 0
		for j := range smp.links {
			smp.links[j].rate *= k
			smp.G += smp.links[j].rate
		}
		had := a.pr != nil
		d := a.decide(smp)
		if had && a.pr == nil {
			return d.note
		}
	}
	return ""
}

// relievedRig: like probeRig, but once the probe starts the old links are no
// longer pressed and the total changes by dG (demand was nearly met).
func relievedRig(a *autopilot, now *time.Time, from int, gb, rNew, dG float64) string {
	for i := 0; i < 200; i++ {
		*now = now.Add(healthTick)
		smp := apSample{now: *now, flowing: 60, open: 200, growable: true, G: gb}
		pr := a.pr
		per := gb / float64(from)
		if pr != nil {
			per = (gb + dG - rNew) / float64(from)
			smp.G = gb + dG
		}
		for id := 0; id < from; id++ {
			smp.links = append(smp.links, apLink{id: id, serving: true, pressed: pr == nil, rate: per, rate10: per, sustained: per, flowing: 4})
		}
		if pr != nil {
			for id := from; id < pr.to; id++ {
				smp.links = append(smp.links, apLink{id: id, serving: true, servingSince: pr.start, rate: rNew / float64(pr.to-from)})
			}
		}
		had := a.pr != nil
		d := a.decide(smp)
		if had && a.pr == nil {
			return d.note
		}
	}
	return ""
}

func verdict(note string) string {
	switch {
	case strings.Contains(note, "links kept:"):
		return "success"
	case strings.Contains(note, "kept as headroom"):
		return "relieved"
	case strings.HasPrefix(note, "sized to"):
		return "fail"
	case strings.Contains(note, "kept as spares"):
		return "inconclusive"
	}
	return "none: " + note
}

func TestProbeVerdictTable(t *testing.T) {
	gb := 8 * 250e3 // 8 links × 2 Mbit/s
	cases := []struct {
		name       string
		rNew, dG   float64
		newPressed bool
		want       string
	}{
		{"additive (per-connection throttling)", 500e3, 500e3, true, "success"},
		{"substitutive (path full)", 500e3, 0, true, "fail"},
		{"pressure relieved, total rose a little (demand nearly met)", 500e3, 0.08 * gb, false, "relieved"},
		{"pressure relieved but the total did not rise", 500e3, 0, false, "fail"},
		{"no new flow reached the links", 0, 0, false, "inconclusive"},
	}
	for _, c := range cases {
		a := newAutopilot(2, 32, 8)
		now := time.Unix(1e9, 0)
		if !c.newPressed && c.rNew > 0 {
			// Demand met: once the probe links take flows, the old links
			// stop being pressed. Model it by a rig variant below.
			got := verdict(relievedRig(a, &now, 8, gb, c.rNew, c.dG))
			if got != c.want {
				t.Errorf("%s: verdict %q, want %q", c.name, got, c.want)
			}
			continue
		}
		got := verdict(probeRig(a, &now, rand.New(rand.NewPCG(1, 2)), 8, gb, 0.05, c.rNew, c.dG, c.newPressed))
		if got != c.want {
			t.Errorf("%s: verdict %q, want %q", c.name, got, c.want)
		}
	}
}

// Under noisy traffic (CV 0.3 per tick) a probe whose links only took share
// from the others must almost never be judged a success.
func TestProbeVerdictNoiseFalsePass(t *testing.T) {
	n := 10000
	if testing.Short() {
		n = 2000
	}
	pass := 0
	gb := 8 * 250e3
	for seed := 0; seed < n; seed++ {
		a := newAutopilot(2, 32, 8)
		rnd := rand.New(rand.NewPCG(uint64(seed), 7))
		a.rnd = rnd.Float64
		now := time.Unix(1e9, 0)
		if verdict(probeRig(a, &now, rnd, 8, gb, 0.3, 2*gb/8, 0, true)) == "success" {
			pass++
		}
	}
	if rate := float64(pass) / float64(n); rate >= 0.03 {
		t.Fatalf("false-pass rate %.2f%%, want < 3%%", 100*rate)
	}
}

// Failed probes back off 30 s, 1 m, 2 m, … capped at 8 min, each ±20%; the
// backoff resets when demand clearly outgrows the last ceiling, or 30 min
// after the last failure.
func TestProbeBackoffAndReset(t *testing.T) {
	a := newAutopilot(2, 32, 8)
	rnd := rand.New(rand.NewPCG(3, 4))
	a.rnd = rnd.Float64
	now := time.Unix(1e9, 0)
	gb := 8 * 250e3
	for k := 1; k <= 7; k++ {
		// probeRig idles through the previous backoff, then runs one probe.
		note := probeRig(a, &now, rnd, 8, gb, 0.02, 500e3, 0, true)
		if verdict(note) != "fail" {
			t.Fatalf("probe %d: %q, want fail", k, note)
		}
		base := min(30*time.Second<<(k-1), 8*time.Minute)
		got := a.next.Sub(now)
		if got < time.Duration(0.8*float64(base))-time.Second || got > time.Duration(1.2*float64(base))+time.Second {
			t.Fatalf("backoff after fail %d = %s, want %s ±20%%", k, got, base)
		}
	}
	// Demand clearly and steadily above the ceiling: re-check at once.
	for i := 0; i < 5; i++ {
		now = now.Add(healthTick)
		smp := apSample{now: now, G: 2 * gb, flowing: 60, open: 200, growable: true}
		for id := 0; id < 8; id++ {
			smp.links = append(smp.links, apLink{id: id, serving: true, rate: 2 * gb / 8, flowing: 8})
		}
		a.decide(smp)
	}
	if a.k != 0 || a.next.After(now.Add(a.tun.backoffBase)) {
		t.Fatalf("after demand rose 2×: k=%d next in %s, want reset (next check within %s)", a.k, a.next.Sub(now), a.tun.backoffBase)
	}
	// And 30 min after a failure, even without that.
	b := newAutopilot(2, 32, 8)
	b.rnd = rnd.Float64
	now2 := time.Unix(2e9, 0)
	probeRig(b, &now2, rnd, 8, gb, 0.02, 500e3, 0, true)
	if b.k != 1 {
		t.Fatalf("k=%d after one failure", b.k)
	}
	now2 = now2.Add(31 * time.Minute)
	b.decide(apSample{now: now2, G: gb / 2, flowing: 10, open: 100, growable: true,
		links: []apLink{{id: 0, serving: true, rate: gb / 2}}})
	if b.k != 0 {
		t.Fatalf("k=%d 31 min after the failure, want 0", b.k)
	}
}

// Lab finding (full 20 Mbit/s path): right after a failed probe its links are
// retiring but still carry the flows that landed on them. A probe that starts
// again brings exactly those links back, so little of their traffic is new —
// that must be judged "path full", not "no flow reached them, keep as spares"
// (which kept the links and let the pool creep on a full path).
func TestProbeUnretiredBusyLinksWithoutGainFails(t *testing.T) {
	a := newAutopilot(2, 32, 8)
	a.rnd = func() float64 { return 0.5 }
	now := time.Unix(1e9, 0)
	const G = 2.2e6 // bytes/s, the whole path
	var probeStart time.Time
	for i := 0; i < 400; i++ {
		now = now.Add(healthTick)
		smp := apSample{now: now, G: G, flowing: 20, open: 21, growable: true}
		// 10 links exist throughout: 8 original + 2 that a previous probe
		// added. While no probe runs, the extra 2 are retiring but busy.
		for id := 0; id < 10; id++ {
			serving := id < 8 || a.pr != nil
			l := apLink{id: id, serving: serving, retiring: !serving, pressed: serving,
				rate: G / 10, rate10: G / 10, sustained: G / 10, flowing: 2, open: 2}
			if id >= 8 && a.pr != nil {
				l.servingSince = probeStart
			}
			smp.links = append(smp.links, l)
		}
		had := a.pr != nil
		d := a.decide(smp)
		if !had && a.pr != nil {
			probeStart = a.pr.start
		}
		if had && a.pr == nil {
			if v := verdict(d.note); v != "fail" {
				t.Fatalf("verdict %q (%s), want fail", v, d.note)
			}
			if a.T != 8 {
				t.Fatalf("T=%d after the failed probe, want back to 8", a.T)
			}
			return
		}
	}
	t.Fatal("no probe was judged")
}

// A throughput burst (a queue draining) must not cancel the backoff after a
// failed probe; only demand that stays above the old ceiling does.
func TestBackoffNotResetBySpike(t *testing.T) {
	a := newAutopilot(2, 32, 8)
	a.rnd = func() float64 { return 0.5 }
	now := time.Unix(1e9, 0)
	gb := 8 * 250e3
	rnd := rand.New(rand.NewPCG(9, 9))
	if v := verdict(probeRig(a, &now, rnd, 8, gb, 0.02, 500e3, 0, true)); v != "fail" {
		t.Fatalf("setup: %s", v)
	}
	next := a.next
	sample := func(g float64) {
		now = now.Add(healthTick)
		smp := apSample{now: now, G: g, flowing: 60, open: 200, growable: true}
		for id := 0; id < 8; id++ {
			smp.links = append(smp.links, apLink{id: id, serving: true, pressed: true, rate: g / 8, rate10: g / 8, sustained: g / 8, flowing: 4})
		}
		a.decide(smp)
	}
	sample(gb)
	sample(1.4 * gb) // two-tick burst
	sample(1.4 * gb)
	sample(gb)
	if a.k == 0 || !a.next.Equal(next) {
		t.Fatalf("a 4 s burst reset the backoff (k=%d)", a.k)
	}
	for i := 0; i < 6; i++ { // demand really grew
		sample(1.5 * gb)
	}
	if a.k != 0 {
		t.Fatalf("sustained growth did not reset the backoff (k=%d)", a.k)
	}
}

// Many pressed links whose flows are too throttled to count as active: the
// useful-links cap must leave room for the pressed links and their spares, or
// the pool shrinks, is short at once, restores, and repeats every minute.
func TestAutopilotNoShrinkRestoreFlap(t *testing.T) {
	a := newAutopilot(2, 32, 8)
	a.T = 12
	now := time.Unix(1e9, 0)
	changes := 0
	for i := 0; i < 600; i++ { // 20 minutes
		now = now.Add(healthTick)
		smp := apSample{now: now, G: 12 * 50e3, flowing: 5, open: 100, growable: true}
		for id := 0; id < a.T; id++ {
			smp.links = append(smp.links, apLink{id: id, serving: true, pressed: id < 9,
				rate: 50e3, rate10: 50e3, sustained: 50e3})
		}
		if d := a.decide(smp); d.note != "" {
			changes++
		}
	}
	if changes > 0 || a.T != 12 {
		t.Fatalf("%d size changes, T=%d; want the pool left at 12", changes, a.T)
	}
}

// 5b. A full path whose capacity is noisy (cross traffic: ±20% per tick). A
// probe can pass by chance, and on a full path every link reads as pressed, so
// nothing else would take its links back: gains found after a path-full
// verdict must last a minute, and a path found full again at more links with
// no more throughput goes back to the smaller size. Over 8 hours the pool must
// not drift up (the reviewer's rig reached 32).
func TestSimNoisyFullPathNoDrift(t *testing.T) {
	d, seeds := 8*time.Hour, uint64(4)
	if testing.Short() {
		d, seeds = 2*time.Hour, 2
	}
	for seed := uint64(1); seed <= seeds; seed++ {
		s := newSim(t, simCfg{min: 2, max: 32, reverse: true, exitDials: true, statsOK: true,
			linkCap: 2 * mbit, pathCap: 8 * mbit, pathCV: 0.2, seed: seed})
		for i := 0; i < 20; i++ {
			s.bulk(2e6, 30*time.Hour)
		}
		s.run(d)
		for s.a.pr != nil || s.a.confirm.active {
			s.step()
		}
		if s.a.T > 10 || s.maxT > 17 {
			t.Fatalf("seed %d: T=%d after %s (max %d); want <= 10 (max <= 17)\n%s", seed, s.a.T, d, s.maxT, s.dump())
		}
	}
}
