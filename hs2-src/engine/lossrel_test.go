package engine

import (
	"context"
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

// withLossRelative runs f with HS2_LOSS_RELATIVE set to on.
func withLossRelative(on bool, f func()) {
	old := lossRelative
	lossRelative = on
	defer func() { lossRelative = old }()
	f()
}

// runUniformLoss drives 20 busy links resending 9-13% of their download
// (median ~11%, the main tunnel's path at peak) for 8 samples, plus extra
// links at the given fractions, and returns the uniform links and the extra
// ones.
func runUniformLoss(t *testing.T, extra ...float64) (r *stuckRig, uni, ext []*managedLink) {
	r = newStuckRig(t, 0)
	var fs, xs []*meteredFakeLink
	for i := 0; i < 20; i++ {
		f, ml := r.add()
		fs, uni = append(fs, f), append(uni, ml)
	}
	for range extra {
		f, ml := r.add()
		xs, ext = append(xs, f), append(ext, ml)
	}
	for i := 0; i < 8; i++ {
		for j, f := range fs {
			downLossy(f, 200<<10, 0.09+0.04*float64(j)/19)
			f.pong(int64(healthTick), 0)
		}
		for j, f := range xs {
			downLossy(f, 200<<10, extra[j])
			f.pong(int64(healthTick), 0)
		}
		r.tick()
	}
	return r, uni, ext
}

func degradedCount(mls []*managedLink) int {
	n := 0
	for _, ml := range mls {
		if ml.degraded {
			n++
		}
	}
	return n
}

// A path losing 9-13% on every link: without the key the links over lossFrac
// (12.x-13%) are drained one by one; with HS2_LOSS_RELATIVE none is — they
// are within lossMargin of the path's median.
func TestLossRelativeUniformPathDrainsNone(t *testing.T) {
	withLossRelative(false, func() {
		r, uni, _ := runUniformLoss(t)
		if n := degradedCount(uni); n == 0 {
			t.Fatalf("without the key no link at 12-13%% was drained — the case this key is for:\n%s", r.lg)
		}
	})
	withLossRelative(true, func() {
		before := obs.lossRelKept.Load()
		r, uni, _ := runUniformLoss(t)
		if n := degradedCount(uni); n != 0 {
			t.Fatalf("HS2_LOSS_RELATIVE: %d links drained on a path losing 9-13%% everywhere:\n%s", n, r.lg)
		}
		if obs.lossRelKept.Load() == before {
			t.Fatal("no verdict counted as withheld by the relative rule")
		}
	})
}

// The same path with one link at 25%: with the key only that one is drained.
func TestLossRelativeStillDrainsTheOutlier(t *testing.T) {
	withLossRelative(true, func() {
		r, uni, ext := runUniformLoss(t, 0.25)
		if n := degradedCount(uni); n != 0 {
			t.Fatalf("%d links at 9-13%% drained beside the 25%% one:\n%s", n, r.lg)
		}
		if !ext[0].degraded {
			t.Fatalf("the link at 25%% (median ~11%%) was not drained:\n%s", r.lg)
		}
	})
}

// The throttle cases keep their verdicts with the key on: the links at the
// path's rate are left out of the median, so the lossy ones among them are
// still judged by lossFrac (fewer than lossPathMin left).
func TestLossRelativeKeepsThrottleVerdicts(t *testing.T) {
	withLossRelative(true, func() {
		t.Run("among-throttled", TestLossFindsTheLossyAmongThrottledLinks)
		t.Run("at-paths-rate", TestLossKeepsALinkAtThePathsRate)
		t.Run("headroom", TestLossDrainsAtMostHeadroom)
		t.Run("path-wide", TestLossPathWideDrainsNone)
		t.Run("few-busy", TestLossFewBusyLinksJudgedAlone)
		t.Run("quiet", TestLossStreakSurvivesShortQuietSamples)
	})
}

// lossBar with the key off is lossFrac whatever the links do; with it on and
// too few links judged, lossFrac too.
func TestLossBar(t *testing.T) {
	never := func(float64) bool { return false }
	j := []lossJudged{{frac: 0.10}, {frac: 0.11}, {frac: 0.12}, {frac: 0.13}}
	withLossRelative(false, func() {
		if b := lossBar(j, never); b != lossFrac {
			t.Fatalf("off: bar %.3f, want lossFrac", b)
		}
	})
	withLossRelative(true, func() {
		if b := lossBar(j[:3], never); b != lossFrac {
			t.Fatalf("3 judged: bar %.3f, want lossFrac", b)
		}
		if b, want := lossBar(j, never), 0.115+lossMargin; b < want-1e-9 || b > want+1e-9 {
			t.Fatalf("4 judged: bar %.3f, want %.3f", b, want)
		}
		low := []lossJudged{{frac: 0}, {frac: 0}, {frac: 0.01}, {frac: 0.02}}
		if b := lossBar(low, never); b != lossFrac {
			t.Fatalf("clean path: bar %.3f, want lossFrac", b)
		}
	})
}

// A link that recovered on probation and is degraded again: with
// HS2_PROBATION_ONCE it drains for good; without it, it recovers again.
func TestProbationOnce(t *testing.T) {
	run := func(once bool) *managedLink {
		oldP, oldO := linkProbation, probationOnce
		linkProbation, probationOnce = true, once
		defer func() { linkProbation, probationOnce = oldP, oldO }()
		m := NewLinkManager(&fakeDialer{}, 4, 8, 50, nil)
		m.gate = instantGate()
		good := []*meteredFakeLink{newMeteredFake(), newMeteredFake(), newMeteredFake()}
		bad := newMeteredFake()
		for _, g := range good {
			ml := &managedLink{link: g, mtr: g.m}
			ml.users.Store(5)
			m.links = append(m.links, ml)
		}
		badML := &managedLink{link: bad, mtr: bad.m}
		badML.users.Store(5)
		m.links = append(m.links, badML)
		step := func(badRetrans uint64) {
			for _, g := range good {
				g.active(200<<10, 0)
			}
			bad.active(200<<10, badRetrans)
			m.sampleHealth()
		}
		degrade := func(round int) {
			for i := 0; i < degradeStreak+1; i++ {
				step(30) // ~20% loss
			}
			if !badML.degraded {
				t.Fatalf("round %d: the lossy link was not degraded", round)
			}
			m.heal(context.Background())
			settle(m)
			if !badML.draining {
				t.Fatalf("round %d: the degraded link is not draining", round)
			}
			for i := 0; i < probationTicks+1; i++ {
				step(0) // the burst is over
			}
		}
		step(30) // seed
		degrade(1)
		if badML.degraded || badML.draining || !badML.recovered {
			t.Fatalf("first probation: degraded=%v draining=%v recovered=%v; want it recovered",
				badML.degraded, badML.draining, badML.recovered)
		}
		degrade(2)
		return badML
	}
	before := obs.probationOnceDenied.Load()
	if ml := run(true); !ml.degraded || !ml.draining {
		t.Fatalf("HS2_PROBATION_ONCE: a link degraded a second time recovered again (degraded=%v draining=%v)", ml.degraded, ml.draining)
	}
	if d := obs.probationOnceDenied.Load() - before; d != 1 {
		t.Fatalf("second probations refused: %d, want 1", d)
	}
	before = obs.probationOnceDenied.Load()
	if ml := run(false); ml.degraded || ml.draining {
		t.Fatal("without HS2_PROBATION_ONCE a link degraded a second time must recover as before")
	}
	if obs.probationOnceDenied.Load() != before {
		t.Fatal("refusals counted with the key off")
	}
}

// simUniformLoss runs 30 busy links for 30 simulated minutes on a path losing
// 8-11% (each link its own base, plus the sampling noise of ~150 segments a
// tick) and returns how many were degraded (each drained and replaced in
// place, on the same path: its 5 connections cut), and how many of those were
// link 0 while it lost 25% (once drained, its replacement loses what the path
// does).
func simUniformLoss(t *testing.T, seed uint64) (drains, badDrains int) {
	r := newStuckRig(t, 0)
	rng := rand.New(rand.NewPCG(seed, 7))
	type sl struct {
		f    *meteredFakeLink
		ml   *managedLink
		base float64
	}
	var ls []sl
	for i := 0; i < 30; i++ {
		f, ml := r.add()
		base := 0.08 + 0.03*rng.Float64()
		if i == 0 {
			base = 0.25
		}
		ls = append(ls, sl{f, ml, base})
	}
	const segs = 150.0
	for tick := 0; tick < int(30*time.Minute/healthTick); tick++ {
		for _, l := range ls {
			sd := math.Sqrt(l.base * (1 - l.base) / segs)
			frac := min(0.9, max(0, l.base+sd*rng.NormFloat64()))
			downLossy(l.f, 200<<10, frac)
			l.f.pong(int64(healthTick), 0)
		}
		r.tick()
		for i, l := range ls {
			if l.ml.degraded {
				drains++
				if i == 0 && l.base > 0.2 {
					badDrains++
					ls[0].base = 0.08 + 0.03*rng.Float64() // its replacement is on the path
				}
				l.ml.degraded, l.ml.upStreak, l.ml.dnStreak, l.ml.lowStreak = false, 0, 0, 0
			}
		}
	}
	return drains, badDrains
}

// The simulated path at 8-11% (plus one link at 25%): drains and connections
// cut with and without HS2_LOSS_RELATIVE.
func TestLossRelativeSim(t *testing.T) {
	var onOthers, offOthers int
	defer func() {
		if offOthers == 0 || onOthers*10 > offOthers {
			t.Errorf("drains of links at 8-11%%: %d with the key, %d without; want a tenth or less", onOthers, offOthers)
		}
	}()
	for _, on := range []bool{false, true} {
		withLossRelative(on, func() {
			total, totalBad := 0, 0
			for seed := uint64(1); seed <= 3; seed++ {
				d, b := simUniformLoss(t, seed)
				total, totalBad = total+d, totalBad+b
			}
			t.Logf("HS2_LOSS_RELATIVE=%v: 3 runs x 30 links x 30 min at 8-11%% loss (+1 link at 25%%): %d drains (%d of the 25%% link, %d of the others), %d connections cut",
				on, total, totalBad, total-totalBad, 5*total)
			if totalBad != 3 {
				t.Errorf("the 25%% link drained in %d of 3 runs", totalBad)
			}
			if on {
				onOthers = total - totalBad
			} else {
				offOthers = total - totalBad
			}
		})
	}
}
