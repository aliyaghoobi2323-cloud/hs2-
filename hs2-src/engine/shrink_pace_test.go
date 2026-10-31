package engine

import (
	"io"
	"math"
	"testing"
	"time"
)

// R1 fix 3: the pool's shrink pace (SlowShrink) and what a retiring link
// closes (ReclaimIdleOnly), from the main server under real traffic.

// paceSample is one tick of a pool of n serving links carrying `flowing`
// active connections spread over them, nothing pressed.
func paceSample(now time.Time, n, flowing int) apSample {
	s := apSample{now: now, flowing: flowing, open: flowing, growable: true}
	for i := 0; i < n; i++ {
		f := flowing / n
		if i < flowing%n {
			f++
		}
		s.links = append(s.links, apLink{id: i, serving: true, servingSince: now.Add(-time.Hour),
			flowing: f, open: f, rate: float64(f) * 0.5 * mbit, rate10: float64(f) * 0.5 * mbit})
	}
	return s
}

// SlowShrink: a demand that halves comes down at most a fifth of the pattern
// at a time, steps at least 5 min apart and never sooner than 5 min after the
// last growth; it reaches what demand needs (100 → 80 → 64 → 52 → 50). The
// old pace (SlowShrink 0) halves the gap every 30 s.
func TestAutopilotSlowShrinkPace(t *testing.T) {
	type step struct {
		at   time.Duration
		from int
		to   int
	}
	run := func(fast bool) []step {
		a := newAutopilot(2, 300, 8)
		if fast {
			a.tun.fastShrink()
		}
		t0 := time.Unix(1_700_000_000, 0)
		now := t0
		// 800 active connections: the floor raises the pattern to 100 (growth).
		var grewAt time.Time
		for i := 0; i < 10; i++ {
			now = now.Add(healthTick)
			if d := a.decide(paceSample(now, a.T, 800)); d.note != "" {
				grewAt = now
			}
		}
		if a.T != 100 || grewAt.IsZero() {
			t.Fatalf("fast=%v: T=%d after 800 active connections, want 100", fast, a.T)
		}
		var steps []step
		for now.Sub(t0) < 40*time.Minute {
			now = now.Add(healthTick)
			old := a.T
			d := a.decide(paceSample(now, a.T, 400))
			if d.target < old {
				steps = append(steps, step{now.Sub(grewAt), old, d.target})
			}
		}
		return steps
	}
	slow := run(false)
	want := []int{80, 64, 52, 50}
	if len(slow) != len(want) {
		t.Fatalf("SlowShrink: %d steps %v, want %v", len(slow), slow, want)
	}
	for i, s := range slow {
		if s.to != want[i] {
			t.Fatalf("SlowShrink step %d: %d → %d, want → %d (%v)", i, s.from, s.to, want[i], slow)
		}
		if s.from-s.to > int(0.2*float64(s.from)) {
			t.Fatalf("SlowShrink step %d: %d → %d is more than a fifth", i, s.from, s.to)
		}
		if s.at < slowShrinkGap {
			t.Fatalf("SlowShrink step %d at %s after the growth, want >= %s", i, s.at, slowShrinkGap)
		}
		if i > 0 && s.at-slow[i-1].at < slowShrinkGap {
			t.Fatalf("SlowShrink steps %d and %d %s apart, want >= %s", i-1, i, s.at-slow[i-1].at, slowShrinkGap)
		}
	}
	fast := run(true)
	if len(fast) < 5 || fast[len(fast)-1].at > 6*time.Minute {
		t.Fatalf("old pace: %v, want 5+ steps within 6 min", fast)
	}
	t.Logf("SlowShrink: %v; old pace: %v", slow, fast)
}

// oscDemand is the main server's demand, swinging between ~300 and ~700
// active connections (150–350 Mbit/s at 0.5 Mbit/s each): two swings of 4
// and 11 min, so a rise follows a dip within minutes.
func oscDemand(s *sim) float64 {
	m := s.elapsed().Minutes()
	return 500 + 130*math.Sin(2*math.Pi*m/4) + 70*math.Sin(2*math.Pi*m/11+1)
}

// Real, swinging demand on a 300-link pool (per_link 8): with the old pace
// the pattern changed about once a minute, a shrink in every dip and growth
// in the next rise (seed 1: 84 changes, 15 shrinks in 30 min). With
// SlowShrink the pattern keeps what the peaks need: a fifth of the shrinks,
// about half the changes (seed 1: 36, 3; what is left is growth, tick by
// tick in a rise, which retires nothing), nothing cut, as much carried.
func TestAutopilotSlowShrinkOscillatingDemand(t *testing.T) {
	type res struct {
		changes, shrinks, cuts, maxT, minT int
		share                              float64
	}
	run := func(fast bool, seed uint64) res {
		cfg := simCfg{min: 2, max: 300, statsOK: true, linkCap: 40 * mbit, seed: seed}
		if fast {
			cfg.tun = (*apTunables).fastShrink
		}
		s := newSim(t, cfg)
		s.gens = append(s.gens, genFlows(oscDemand, 0.5*mbit, 0.5, 60*time.Second), genIdlePool(400))
		s.at(10 * time.Minute) // warm up: the pool grows to the demand
		s.tChanges, s.shrinks, s.cuts = 0, 0, 0
		var dem, car float64
		r := res{minT: math.MaxInt}
		s.onTick = func(s *sim) {
			dem += s.demand
			car += s.carried
			r.minT = min(r.minT, s.a.T)
			r.maxT = max(r.maxT, s.a.T)
		}
		s.at(40 * time.Minute)
		r.changes, r.shrinks, r.cuts, r.share = s.tChanges, s.shrinks, s.cuts, car/dem
		return r
	}
	for seed := uint64(1); seed <= 3; seed++ {
		o, n := run(true, seed), run(false, seed)
		t.Logf("seed %d, 30 min of swinging demand: old pace %d pattern changes (%d shrinks), T %d..%d, carried %.1f%%; SlowShrink %d changes (%d shrinks), T %d..%d, carried %.1f%%",
			seed, o.changes, o.shrinks, o.minT, o.maxT, 100*o.share, n.changes, n.shrinks, n.minT, n.maxT, 100*n.share)
		if n.shrinks > 6 { // at most one per 5 min
			t.Fatalf("seed %d: SlowShrink shrank %d times in 30 min", seed, n.shrinks)
		}
		// The changes left are growth steps (the floor rising tick by tick
		// in a rise): they retire nothing.
		if 3*n.shrinks > o.shrinks || 3*n.changes > 2*o.changes {
			t.Fatalf("seed %d: SlowShrink %d shrinks, %d changes; the old pace %d, %d: want at most a third of the shrinks, two thirds of the changes",
				seed, n.shrinks, n.changes, o.shrinks, o.changes)
		}
		if n.share < o.share-0.005 {
			t.Fatalf("seed %d: SlowShrink carried %.1f%% of demand, the old pace %.1f%%", seed, 100*n.share, 100*o.share)
		}
		if n.cuts != 0 || o.cuts != 0 {
			t.Fatalf("seed %d: active connections cut: %d (SlowShrink), %d (old)", seed, n.cuts, o.cuts)
		}
	}
}

// Growth is as fast with SlowShrink: after a shrink, demand jumping from
// ~300 to ~700 active connections (arrivals ramp up over about a minute)
// raises the pattern tick by tick with the active-flow floor, as with the
// old pace: never more than 10 s behind it, and to 75 links as soon.
func TestAutopilotSlowShrinkGrowthStillFast(t *testing.T) {
	var took [2]time.Duration
	for i, fast := range []bool{true, false} {
		lvl := 700.0
		cfg := simCfg{min: 2, max: 300, statsOK: true, linkCap: 40 * mbit, seed: 7}
		if fast {
			cfg.tun = (*apTunables).fastShrink
		}
		s := newSim(t, cfg)
		s.gens = append(s.gens, genFlows(func(*sim) float64 { return lvl }, 0.5*mbit, 0.3, 60*time.Second))
		s.at(10 * time.Minute)
		high := s.a.T
		lvl = 300
		s.at(25 * time.Minute) // long enough for a shrink at either pace
		low := s.a.T
		if s.shrinks == 0 || low >= high {
			t.Fatalf("fast=%v: no shrink after the drop (T %d → %d)\n%s", fast, high, low, s.dump())
		}
		lvl = 700
		from := s.elapsed()
		const need = 75 // ~600 active connections at per_link 8
		var fl []int    // flowing per tick
		s.onTick = func(s *sim) {
			f := 0
			for _, l := range s.links {
				f += l.flowing
			}
			fl = append(fl, f)
			if len(fl) >= 5 { // the floor: the least of the last 5 ticks (10 s)
				lo := slicesMin(fl[len(fl)-5:])
				if want := min(ceilDiv(lo, 8), 300); s.a.T < want {
					t.Fatalf("fast=%v at %s after the rise: T=%d below the floor %d of %d active\n%s",
						fast, s.elapsed()-from, s.a.T, want, lo, s.dump())
				}
			}
		}
		for s.a.T < need && s.elapsed()-from < 3*time.Minute {
			s.step()
		}
		took[i] = s.elapsed() - from
		t.Logf("fast=%v: T %d at 700 active, %d at 300; back to %d in %s", fast, high, low, need, took[i])
		if s.a.T < need {
			t.Fatalf("fast=%v: T=%d 3 min after demand rose, want >= %d\n%s", fast, s.a.T, need, s.dump())
		}
	}
	if took[1] > took[0]+2*healthTick {
		t.Fatalf("SlowShrink grew back to 75 links in %s, the old pace in %s", took[1], took[0])
	}
}

func slicesMin(xs []int) int {
	m := xs[0]
	for _, x := range xs[1:] {
		m = min(m, x)
	}
	return m
}

// A connection that still moves data, but slowly, is not closed on a link
// retiring past retireForce: only one silent for drain_idle is. Without
// ReclaimIdleOnly (the old rule) the slow one is cut at the first pass.
func TestReclaimSlowLiveStreamOnRetiringLink(t *testing.T) {
	for _, only := range []int{1, 0} {
		func() {
			defer ReclaimIdleOnly.Set(ReclaimIdleOnly.Get())
			ReclaimIdleOnly.Set(only)
			m, clk, lg := newV2Manager(nil, 1, 8, false)
			pl := newV2PipeLink(t)
			slow, idle := pl.open(t), pl.open(t)
			ml := addManaged(m, pl)
			ml.retiring, ml.retireSince = true, clk.Now().Add(-retireForce-time.Minute)
			m.sampleHealth()
			slowCut := 0
			runDrainTicks(t, m, clk, ml, 170, func(tick int) { // 340 s
				if slow.done.Load() {
					if slowCut == 0 {
						slowCut = tick
					}
					return
				}
				// A chat or a throttled reader: 600 B every 3 ticks (~100 B/s):
				// under flowingRate, and never steady.
				if tick%3 == 0 {
					if _, err := slow.Write(make([]byte, 600)); err != nil {
						t.Fatalf("ReclaimIdleOnly=%d tick %d: the slow stream broke: %v", only, tick, err)
					}
				}
			})
			if only == 0 {
				if slowCut == 0 || slowCut > 2 {
					t.Fatalf("old rule: the slow stream was cut at tick %d, want at the first passes", slowCut)
				}
				return
			}
			if slowCut != 0 {
				t.Fatalf("a slow stream still moving data was closed at tick %d (%s)", slowCut, time.Duration(slowCut)*healthTick)
			}
			found := false
			for _, c := range pl.slowStreams(idleReclaimMax) {
				found = found || c.cs == slow
			}
			if !found {
				t.Fatal("the slow stream does not read as not flowing: the test does not test the rule")
			}
			if ended, err := pl.exitEnded(idle, 2*time.Second); !ended || err != io.EOF {
				t.Fatalf("idle stream: ended=%v err=%v, want closed with a FIN after %s", ended, err, drainIdleDefault)
			}
			if _, err := slow.Write([]byte("still here")); err != nil {
				t.Fatalf("slow stream: %v", err)
			}
			if !inPool(m, pl) || pl.Active() != 1 || m.forced.Load() != 0 {
				t.Fatalf("link pooled=%v active=%d forced=%d, want it kept for the slow stream, nothing forced",
					inPool(m, pl), pl.Active(), m.forced.Load())
			}
			if lg.count("closed 1 connection(s) idle for over 5m") != 1 {
				t.Fatalf("idle reclaim not logged:\n%s", lg)
			}
		}()
	}
}
