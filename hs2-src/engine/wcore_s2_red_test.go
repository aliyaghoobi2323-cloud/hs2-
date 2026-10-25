package engine

import (
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

// Red team of S2's "loss confirmed by the sender" (wcore.go: provSnap,
// judgeCut, undoCut, roLevel): on the edge, where the exit's records come, a
// reaction to holes alone is provisional until the exit's next record judges
// it, and each cut taken back teaches the path's reordering level oooRo,
// within which holes cut no more. These are the attacks of the judge's list
// on the link model, as reports (HS2_WCORE_S2RED=1; HS2_WCORE_S2RED_CAT picks
// one by prefix). They run as well on a tree without the angle (with
// wcore_s2_red_find_test.go left out wmS2RedProbe is unset; the reports use
// nothing else of the angle) for the A/B against the core of before.
//
// What they found (the red team's round, 1000 Mbit/s unless named, 3 seeds):
//   - Policer (TestWcoreS2RedPolicer): a policer at 30/60/90% of the path
//     coming 20–40 s into a download, lifted 15 s on, at 20–300 ms. On the
//     field's path the loss rule drained the link under the core and not
//     under base in 11 of 135 runs (the core before the angle 2: it never ran
//     fast there; the core on the same path without reordering 6 of 135);
//     with the pacing net's stale cap following the delivery down
//     (pacenet.go) 6 and 4. Short paths keep the most: 1000/20 at 60%
//     resends 9.95% against 1.48% without reordering.
//   - Loss bursts (TestWcoreS2RedBursts, 192 runs): no drain, resends as
//     base's; 7 cuts taken back around drops of the link's own packets (a
//     record whose resends sit within the level's spread, sndNoLoss).
//   - Records stopping 1–6 s around a cut (TestWcoreS2RedRecords), a route
//     change (TestWcoreS2RedRoute), ten minutes (TestWcoreS2RedSoak: the
//     level between 0.57 and 0.75, 700–816 Mbit/s, resends 0.02–0.03%), 2 and
//     4 links (TestWcoreS2RedShared): no drain and no cut taken back around
//     drops of the link's own packets; past wcSndQuiet (5 s) the edge is
//     blind and holes cut as before. A route to a shorter path keeps the
//     level (no route event; it expires after wcQcapTTL).
//   - The autopilot's pool probe (TestWcoreS2RedApProbing): 62–69% of the
//     rate without it in 3 of 12 runs (the freeze holds B where a cut and
//     its taking back left it as the probe began).

// wmS2RedSee is what the probe reads of a core: its reordering level now,
// its cuts taken back so far, and when its pending provisional reaction
// began (zero: none).
type wmS2RedSee struct {
	ro     float64
	undo   int
	provAt time.Time
}

// wmS2RedProbe reads a core (nil on a tree without the angle).
var wmS2RedProbe func(c *wcore, now time.Time) wmS2RedSee

// wmS2RedOut is one run with what the core did along it.
type wmS2RedOut struct {
	r     wmResult
	m     *wmModel
	roAt  float64 // the level as the impairment came (mark)
	roMax float64
	roSec []float64 // the level each second
	undo  int
	// poison: cuts taken back while the link's own packets were dropped
	// around them (from a round trip and 300 ms before the provisional
	// reaction to 300 ms after it): a real loss's holes read as reordering
	poison []string
}

// wmS2RedRun runs c and watches its core (mark: when the impairment comes).
func wmS2RedRun(c wmCfg, mark time.Duration) wmS2RedOut {
	m := newModel(c)
	var o wmS2RedOut
	drops := []uint64{0} // the link's own drops, cumulative, at each 100 ms
	var provAt time.Time
	undo := 0
	m.cfg.check = func(t time.Duration, core *wcore, in wcIn, out wcOut) {
		k := int(t / (100 * time.Millisecond))
		for len(drops) <= k {
			drops = append(drops, drops[len(drops)-1])
		}
		drops[k] = m.tailDrops + m.polDrops + m.randDrops
		if wmS2RedProbe == nil {
			return
		}
		s := wmS2RedProbe(core, in.now)
		if t >= mark && o.roAt == 0 && t < mark+100*time.Millisecond {
			o.roAt = s.ro
		}
		o.roMax = math.Max(o.roMax, s.ro)
		if sec := int(t / time.Second); sec >= len(o.roSec) {
			o.roSec = append(o.roSec, s.ro)
		}
		if s.undo > undo && !provAt.IsZero() {
			at := provAt.Sub(m.epoch)
			lo := int((at - c.rtt - 300*time.Millisecond) / (100 * time.Millisecond))
			hi := int((at + 300*time.Millisecond) / (100 * time.Millisecond))
			lo, hi = max(lo, 0), min(hi, len(drops)-1)
			if d := drops[hi] - drops[lo]; hi > lo && d > 0 {
				o.poison = append(o.poison, fmt.Sprintf("undo at %.1fs of a reaction at %.1fs: %d segments of the link dropped around it (level now %.2f)",
					t.Seconds(), at.Seconds(), d, s.ro))
			}
		}
		undo = s.undo
		if !s.provAt.IsZero() {
			provAt = s.provAt
		}
	}
	o.r, o.m = m.run(), m
	o.undo = undo
	return o
}

// wmRtxOver is the sender's resend share from second a to second b.
func wmRtxOver(r wmResult, a, b int) float64 {
	b = min(b, len(r.rtxSec)-1)
	if a < 0 || a >= b {
		return 0
	}
	x, y := r.rtxSec[a], r.rtxSec[b]
	if y[1] <= x[1] {
		return 0
	}
	return float64(y[0]-x[0]) / float64(y[1]-x[1])
}

// wmRtxWorst is the largest resend share of a 2 s window from second a to
// second b.
func wmRtxWorst(r wmResult, a, b int) float64 {
	w := 0.0
	for s := a; s+2 <= b; s++ {
		w = math.Max(w, wmRtxOver(r, s, s+2))
	}
	return w
}

func wmS2RedOn(t *testing.T, cat string) {
	if os.Getenv("HS2_WCORE_S2RED") != "1" {
		t.Skip("HS2_WCORE_S2RED=1")
	}
	if p := os.Getenv("HS2_WCORE_S2RED_CAT"); p != "" && !strings.HasPrefix(cat, p) {
		t.Skip("HS2_WCORE_S2RED_CAT=" + p)
	}
	wmS2RedKnobs(t)
}

// wmS2RedKnobs: what each part adds. HS2_WCORE_S2RED_ROMAX sets wcRoMax, the
// level's ceiling, 0 none; HS2_WCORE_S2RED_NET=0 runs without the
// pacing net.
func wmS2RedKnobs(t *testing.T) {
	if v := os.Getenv("HS2_WCORE_S2RED_ROMAX"); v != "" {
		old := wcRoMax
		fmt.Sscan(v, &wcRoMax)
		t.Cleanup(func() { wcRoMax = old })
	}
	if os.Getenv("HS2_WCORE_S2RED_NET") == "0" {
		old := PaceNet
		PaceNet = 0
		t.Cleanup(func() { PaceNet = old })
	}
}

// wmS2RedSeeds: HS2_WCORE_S2RED_SEEDS seeds (3).
func wmS2RedSeeds() int {
	n := 3
	fmt.Sscan(os.Getenv("HS2_WCORE_S2RED_SEEDS"), &n)
	return n
}

// TestWcoreS2RedPolicer (attack 1, 3): a per-connection policer at 30, 60
// and 90% of the path comes 20, 30 or 40 s into a download on the field's
// path (its reordering and the RACK sender: the level oooRo was learned by
// then) and on the same path without reordering, at 20 to 300 ms; it is
// lifted 15 s later. The policed phase's rate and resends (all of it, its
// first 5 s, its worst 2 s window), the lifted phase's rate, and whether the
// loss rule drains the link under the core and not under base.
// HS2_WCORE_S2RED_RTTS sets the round trips (20,40,72,150,300).
func TestWcoreS2RedPolicer(t *testing.T) {
	wmS2RedOn(t, "policer")
	rtts := wm2Ints("HS2_WCORE_S2RED_RTTS", []int{20, 40, 72, 150, 300})
	seeds := wmS2RedSeeds()
	type acc struct {
		n                                         int
		bM, kM, bR, kR, kR5, kW, bL, kL, ro, undo float64
		drained, poison                           int
	}
	sums := map[string]*acc{}
	var keys []string
	for _, rtt := range rtts {
		for _, reo := range []bool{true, false} {
			for _, pol := range []float64{0.3, 0.6, 0.9} {
				for _, at := range []int{20, 30, 40} {
					for seed := 1; seed <= seeds; seed++ {
						c := wmPath(1000, wmMS(rtt), wmMS(50))
						c.rack, c.reoField, c.seed = true, reo, uint64(seed)
						c.polAt = []wmChange{{time.Duration(at) * time.Second, pol * 1000 * wmMbit}, {time.Duration(at+15) * time.Second, 0}}
						c.dur = time.Duration(at+30) * time.Second
						tag := "clean"
						if reo {
							tag = "field"
						}
						c.name = fmt.Sprintf("1000/%d q50 %s pol%.0f@%ds", rtt, tag, 100*pol, at)
						b := wmS2RedRun(c.with(wmBase), 0)
						k := wmS2RedRun(c.with(wmCore), time.Duration(at)*time.Second)
						pa, pb := at, at+15
						kb := fmt.Sprintf("%d %s pol%.0f", rtt, tag, 100*pol)
						s := sums[kb]
						if s == nil {
							s = &acc{}
							sums[kb] = s
							keys = append(keys, kb)
						}
						s.n++
						s.bM += wmMbitOver(b.r, b.m, time.Duration(pa+2)*time.Second, time.Duration(pb)*time.Second)
						s.kM += wmMbitOver(k.r, k.m, time.Duration(pa+2)*time.Second, time.Duration(pb)*time.Second)
						s.bR += wmRtxOver(b.r, pa, pb)
						s.kR += wmRtxOver(k.r, pa, pb)
						s.kR5 += wmRtxOver(k.r, pa, pa+5)
						s.kW = math.Max(s.kW, wmRtxWorst(k.r, pa, pb))
						s.bL += wmMbitOver(b.r, b.m, time.Duration(pb+5)*time.Second, c.dur)
						s.kL += wmMbitOver(k.r, k.m, time.Duration(pb+5)*time.Second, c.dur)
						s.ro += k.roAt
						s.undo += float64(k.undo)
						dr := wmDrainedByCore(b.r, k.r)
						if dr {
							s.drained++
						}
						s.poison += len(k.poison)
						flag := ""
						if dr {
							flag = " DRAINED"
						}
						if os.Getenv("HS2_WCORE_S2RED_ALL") == "1" || dr || len(k.poison) > 0 {
							t.Logf("%-34s s%d: policed base %6.1f Mbit rs %5.2f%% | core %6.1f rs %5.2f%% first5 %5.2f%% worst2s %5.2f%% | lifted base %6.1f core %6.1f | ro@%.2f undo %d cuts %d d%v [%s]%s",
								c.name, seed, wmMbitOver(b.r, b.m, time.Duration(pa+2)*time.Second, time.Duration(pb)*time.Second), 100*wmRtxOver(b.r, pa, pb),
								wmMbitOver(k.r, k.m, time.Duration(pa+2)*time.Second, time.Duration(pb)*time.Second), 100*wmRtxOver(k.r, pa, pb), 100*wmRtxOver(k.r, pa, pa+5),
								100*wmRtxWorst(k.r, pa, pb), wmMbitOver(b.r, b.m, time.Duration(pb+5)*time.Second, c.dur), wmMbitOver(k.r, k.m, time.Duration(pb+5)*time.Second, c.dur),
								k.roAt, k.undo, k.r.cuts, k.r.drains, wmLossNote(k.r), flag)
							for _, p := range k.poison {
								t.Logf("    POISON %s", p)
							}
						}
					}
				}
			}
		}
	}
	for _, kb := range keys {
		s := sums[kb]
		n := float64(s.n)
		t.Logf("SUM %-22s %3d runs: policed base %6.1f Mbit rs %5.2f%% core %6.1f rs %5.2f%% (first 5 s %5.2f%%, worst 2 s %5.2f%%) | lifted base %6.1f core %6.1f | ro@ %.2f undo %.1f | drained %d poison %d",
			kb, s.n, s.bM/n, 100*s.bR/n, s.kM/n, 100*s.kR/n, 100*s.kR5/n, 100*s.kW, s.bL/n, s.kL/n, s.ro/n, s.undo/n, s.drained, s.poison)
	}
}

// TestWcoreS2RedTrace traces one run of TestWcoreS2RedPolicer:
// HS2_WCORE_S2RED_TRACE=rtt/field(0|1)/policer share/at s/seed[/base]
// (from at−3 s to at+15 s).
func TestWcoreS2RedTrace(t *testing.T) {
	spec := os.Getenv("HS2_WCORE_S2RED_TRACE")
	if spec == "" {
		t.Skip("HS2_WCORE_S2RED_TRACE=rtt/field/share/at/seed[/base]")
	}
	wmS2RedKnobs(t)
	p := strings.Split(spec, "/")
	var rtt, field, at, seed int
	var pol float64
	fmt.Sscan(p[0], &rtt)
	fmt.Sscan(p[1], &field)
	fmt.Sscan(p[2], &pol)
	fmt.Sscan(p[3], &at)
	fmt.Sscan(p[4], &seed)
	c := wmPath(1000, wmMS(rtt), wmMS(50))
	c.rack, c.reoField, c.seed, c.trace, c.mode = true, field == 1, uint64(seed), true, wmCore
	c.polAt = []wmChange{{time.Duration(at) * time.Second, pol * 1000 * wmMbit}, {time.Duration(at+15) * time.Second, 0}}
	c.dur = time.Duration(at+30) * time.Second
	for _, x := range p[5:] {
		switch x {
		case "base":
			c.mode = wmBase
		case "ap":
			// (TestWcoreS2RedApProbing's spell: the autopilot probes the
			// pool from at to at+20 s; no policer)
			c.polAt = nil
			c.apProbing = func(t time.Duration) bool {
				return t >= time.Duration(at)*time.Second && t < time.Duration(at+20)*time.Second
			}
		}
	}
	o := wmS2RedRun(c, time.Duration(at)*time.Second)
	w := wmLossWins(o.r, 1)
	sec := 0.0
	for _, l := range o.r.trace {
		x := strings.TrimSpace(l)
		ev := strings.HasPrefix(x, "event") || strings.HasPrefix(x, "net")
		if !ev {
			fmt.Sscanf(x, "%fs", &sec)
		}
		if sec < float64(at-3) || sec > float64(at+15) {
			continue
		}
		if s := int(sec); s < len(w) && !ev {
			l += fmt.Sprintf(" | rs1s %.1f%%", 100*w[s])
		}
		t.Log(l)
	}
	t.Log(o.r, wmLossNote(o.r))
}

// TestWcoreS2RedBursts (attack 3, 4, 5): short bursts of real loss on the
// field's path (reordering, RACK): 200 to 500 ms of 1 to 5% random loss at
// 8 s (before or as the level is learned) and every 10 s after, at 20 to
// 150 ms behind queues of 50 to 200 ms (the exit's socket queue holds its
// record back). A cut for such holes taken back is poison: the level learns a
// real loss's holes. Rate and resends against the core without reordering's
// rules (the A/B tree) and base.
func TestWcoreS2RedBursts(t *testing.T) {
	wmS2RedOn(t, "bursts")
	seeds := wmS2RedSeeds()
	type acc struct {
		n                       int
		bM, kM, bR, kR, ro, und float64
		poison, drained         int
	}
	sums := map[string]*acc{}
	var keys []string
	for _, rtt := range []int{20, 40, 72, 150} {
		for _, q := range []int{50, 200} {
			for _, rate := range []float64{300, 1000} {
				for _, burst := range []struct {
					p  float64
					ms int
				}{{0.01, 200}, {0.05, 200}, {0.01, 500}, {0.03, 300}} {
					for seed := 1; seed <= seeds; seed++ {
						c := wmPath(rate, wmMS(rtt), wmMS(q))
						c.rack, c.reoField, c.seed, c.dur = true, true, uint64(seed), 60*time.Second
						for at := 8 * time.Second; at < c.dur; at += 10 * time.Second {
							c.lossAt = append(c.lossAt, wmChange{at, burst.p}, wmChange{at + time.Duration(burst.ms)*time.Millisecond, 0})
						}
						c.name = fmt.Sprintf("%.0f/%d q%d field burst%g%%/%dms", rate, rtt, q, 100*burst.p, burst.ms)
						b := wmS2RedRun(c.with(wmBase), 0)
						k := wmS2RedRun(c.with(wmCore), 8*time.Second)
						kb := fmt.Sprintf("%.0f/%d q%d", rate, rtt, q)
						s := sums[kb]
						if s == nil {
							s = &acc{}
							sums[kb] = s
							keys = append(keys, kb)
						}
						s.n++
						s.bM += b.r.mbit
						s.kM += k.r.mbit
						s.bR += b.r.resend
						s.kR += k.r.resend
						s.ro += k.roMax
						s.und += float64(k.undo)
						s.poison += len(k.poison)
						dr := wmDrainedByCore(b.r, k.r)
						if dr {
							s.drained++
						}
						if os.Getenv("HS2_WCORE_S2RED_ALL") == "1" || dr || len(k.poison) > 0 {
							t.Logf("%-40s s%d: base %6.1f Mbit rs %5.2f%% | core %6.1f rs %5.2f%% [%s] | ro max %.2f undo %d cuts %d d%v",
								c.name, seed, b.r.mbit, 100*b.r.resend, k.r.mbit, 100*k.r.resend, wmLossNote(k.r), k.roMax, k.undo, k.r.cuts, k.r.drains)
							for _, p := range k.poison {
								t.Logf("    POISON %s", p)
							}
						}
					}
				}
			}
		}
	}
	for _, kb := range keys {
		s := sums[kb]
		n := float64(s.n)
		t.Logf("SUM %-14s %3d runs: base %6.1f Mbit rs %5.2f%% | core %6.1f rs %5.2f%% | ro max %.2f undo %.1f | poison %d drained %d",
			kb, s.n, s.bM/n, 100*s.bR/n, s.kM/n, 100*s.kR/n, s.ro/n, s.und/n, s.poison, s.drained)
	}
}

// TestWcoreS2RedRecords (attack 6): the exit's records stop for 1 to 6 s
// around a cut: a policer at 60% (or a loss burst) comes at 20 s on the
// field's path and no record comes from 19.5 s on for the gap; the next one
// covers it. Past wcSndQuiet (5 s) the edge is blind and holes cut as before.
func TestWcoreS2RedRecords(t *testing.T) {
	wmS2RedOn(t, "records")
	seeds := wmS2RedSeeds()
	for _, rtt := range []int{40, 72, 150} {
		for _, imp := range []string{"pol60", "burst3%"} {
			for _, gap := range []int{0, 1, 2, 3, 4, 6} {
				var bM, kM, bR, kR float64
				var notes []string
				for seed := 1; seed <= seeds; seed++ {
					c := wmPath(1000, wmMS(rtt), wmMS(50))
					c.rack, c.reoField, c.seed, c.dur = true, true, uint64(seed), 50*time.Second
					if imp == "pol60" {
						c.polAt = []wmChange{{20 * time.Second, 600 * wmMbit}, {35 * time.Second, 0}}
					} else {
						c.lossAt = []wmChange{{20 * time.Second, 0.03}, {20*time.Second + 300*time.Millisecond, 0}}
					}
					if gap > 0 {
						c.recOff = [][2]time.Duration{{19500 * time.Millisecond, 19500*time.Millisecond + time.Duration(gap)*time.Second}}
					}
					c.name = fmt.Sprintf("1000/%d q50 field %s recoff%ds", rtt, imp, gap)
					b := wmS2RedRun(c.with(wmBase), 0)
					k := wmS2RedRun(c.with(wmCore), 20*time.Second)
					bM += wmMbitOver(b.r, b.m, 20*time.Second, 35*time.Second)
					kM += wmMbitOver(k.r, k.m, 20*time.Second, 35*time.Second)
					bR += wmRtxOver(b.r, 20, 35)
					kR += wmRtxOver(k.r, 20, 35)
					dr := ""
					if wmDrainedByCore(b.r, k.r) {
						dr = " DRAINED"
					}
					notes = append(notes, fmt.Sprintf("s%d core %.0f rs %.2f%% worst2s %.2f%% undo %d poison %d%s", seed,
						wmMbitOver(k.r, k.m, 20*time.Second, 35*time.Second), 100*wmRtxOver(k.r, 20, 35), 100*wmRtxWorst(k.r, 20, 35), k.undo, len(k.poison), dr))
					for _, p := range k.poison {
						notes = append(notes, "POISON "+p)
					}
				}
				n := float64(seeds)
				t.Logf("1000/%d %-7s recoff %ds: 20-35 s base %6.1f Mbit rs %5.2f%% | core %6.1f rs %5.2f%% | %s",
					rtt, imp, gap, bM/n, 100*bR/n, kM/n, 100*kR/n, strings.Join(notes, "; "))
			}
		}
	}
}

// TestWcoreS2RedRoute (attack 8): the route changes 30 s in, after the level
// was learned (72 → 150, 72 → 300: longer, a route change event resets the
// level; 72 → 40, 72 → 20: shorter, no event), and a policer at 60% comes
// at 45 s on the new path.
func TestWcoreS2RedRoute(t *testing.T) {
	wmS2RedOn(t, "route")
	seeds := wmS2RedSeeds()
	for _, to := range []int{150, 300, 40, 20} {
		for _, pol := range []bool{false, true} {
			var bM, kM, bR, kR, ro float64
			var notes []string
			for seed := 1; seed <= seeds; seed++ {
				c := wmPath(1000, 72*time.Millisecond, wmMS(50))
				c.rack, c.reoField, c.seed, c.dur = true, true, uint64(seed), 75*time.Second
				c.rttAt = []wmChange{{30 * time.Second, float64(to)}}
				if pol {
					c.polAt = []wmChange{{45 * time.Second, 600 * wmMbit}}
				}
				c.name = fmt.Sprintf("1000/72>%d q50 field pol%v", to, pol)
				b := wmS2RedRun(c.with(wmBase), 0)
				k := wmS2RedRun(c.with(wmCore), 45*time.Second)
				bM += wmMbitOver(b.r, b.m, 45*time.Second, 75*time.Second)
				kM += wmMbitOver(k.r, k.m, 45*time.Second, 75*time.Second)
				bR += wmRtxOver(b.r, 45, 75)
				kR += wmRtxOver(k.r, 45, 75)
				ro += k.roAt
				dr := ""
				if wmDrainedByCore(b.r, k.r) {
					dr = " DRAINED"
				}
				notes = append(notes, fmt.Sprintf("s%d core %.0f rs %.2f%% worst2s %.2f%% ro@45 %.2f undo %d%s", seed,
					wmMbitOver(k.r, k.m, 45*time.Second, 75*time.Second), 100*wmRtxOver(k.r, 45, 75), 100*wmRtxWorst(k.r, 45, 75), k.roAt, k.undo, dr))
			}
			n := float64(seeds)
			t.Logf("%-26s: 45-75 s base %6.1f Mbit rs %5.2f%% | core %6.1f rs %5.2f%% ro@45 %.2f | %s",
				fmt.Sprintf("72>%d pol60@45 %v", to, pol), bM/n, 100*bR/n, kM/n, 100*kR/n, ro/n, strings.Join(notes, "; "))
		}
	}
}

// TestWcoreS2RedSoak (attack 10): ten minutes of one download on the field's
// path (reordering, RACK) with a burst of real loss now and then (1% for
// 300 ms every 45 s): the level, B, the rate and the resends minute by
// minute.
func TestWcoreS2RedSoak(t *testing.T) {
	wmS2RedOn(t, "soak")
	for _, p := range []struct {
		rate   float64
		rtt, q int
		bursts bool
	}{{1000, 72, 50, true}, {1000, 72, 50, false}, {600, 72, 50, true}, {1000, 40, 50, true}} {
		for seed := 1; seed <= wmS2RedSeeds(); seed++ {
			c := wmPath(p.rate, wmMS(p.rtt), wmMS(p.q))
			c.rack, c.reoField, c.seed, c.dur = true, true, uint64(seed), 10*time.Minute
			if p.bursts {
				for at := 30 * time.Second; at < c.dur; at += 45 * time.Second {
					c.lossAt = append(c.lossAt, wmChange{at, 0.01}, wmChange{at + 300*time.Millisecond, 0})
				}
			}
			c.name = fmt.Sprintf("%.0f/%d q%d field bursts%v", p.rate, p.rtt, p.q, p.bursts)
			b := wmS2RedRun(c.with(wmBase), 0)
			k := wmS2RedRun(c.with(wmCore), 0)
			var mins []string
			for mi := 0; mi < 10; mi++ {
				a, e := mi*60, (mi+1)*60
				ro := 0.0
				if e-1 < len(k.roSec) {
					ro = k.roSec[e-1]
				}
				mins = append(mins, fmt.Sprintf("m%d %4.0f/%4.0f rs%5.2f%% ro%.2f", mi+1, wmMbitOver(b.r, b.m, time.Duration(a)*time.Second, time.Duration(e)*time.Second),
					wmMbitOver(k.r, k.m, time.Duration(a)*time.Second, time.Duration(e)*time.Second), 100*wmRtxOver(k.r, a, e), ro))
			}
			dr := ""
			if wmDrainedByCore(b.r, k.r) {
				dr = " DRAINED"
			}
			t.Logf("%s s%d: base %.1f core %.1f Mbit rs %.3f%% [%s] undo %d cuts %d d%v p%v ro max %.2f poison %d%s\n\t%s",
				c.name, seed, b.r.mbit, k.r.mbit, 100*k.r.resend, wmLossNote(k.r), k.undo, k.r.cuts, k.r.drains, k.r.probes, k.roMax, len(k.poison), dr, strings.Join(mins, " | "))
			for _, x := range k.poison {
				t.Logf("    POISON %s", x)
			}
		}
	}
}

// TestWcoreS2RedShared (attack 7): 2 and 4 links to one exit on one
// bottleneck, each connection with the field's reordering at its own rate
// and the RACK sender, the cores sharing the peer's registry. (No policer:
// wmRunShared takes the path's loss and rate changes, not polAt.)
func TestWcoreS2RedShared(t *testing.T) {
	wmS2RedOn(t, "shared")
	for _, n := range []int{2, 4} {
		for _, rtt := range []int{72, 150} {
			var lines []string
			var bs, ks float64
			for seed := 1; seed <= wmS2RedSeeds(); seed++ {
				c := wmPath(1000, wmMS(rtt), wmMS(50))
				c.rack, c.reoField, c.seed, c.dur = true, true, uint64(seed), 50*time.Second
				c.name = fmt.Sprintf("1000/%d q50 field x%d links", rtt, n)
				b := wmRunShared(c.with(wmBase), n, nil)
				k := wmRunShared(c.with(wmCore), n, nil)
				bs += b.mbit
				ks += k.mbit
				lines = append(lines, fmt.Sprintf("s%d %s", seed, k))
				for i := range k.links {
					if wmDrainedByCore(b.links[i], k.links[i]) {
						lines = append(lines, fmt.Sprintf("DRAINED link %d", i))
					}
				}
			}
			ns := float64(wmS2RedSeeds())
			t.Logf("%-20s base %.1f core %.1f Mbit\n\t%s", fmt.Sprintf("1000/%d x%d", rtt, n), bs/ns, ks/ns, strings.Join(lines, "\n\t"))
		}
	}
}

// TestWcoreS2RedApProbing (attack 9): the autopilot probes the pool size
// (apProbing) from 15 to 35 s on the field's path, with the window pool at
// 64 MiB and at 12 MiB.
func TestWcoreS2RedApProbing(t *testing.T) {
	wmS2RedOn(t, "approbing")
	for _, pool := range []int64{0, 12 << 20} {
		for _, rtt := range []int{72, 150} {
			for seed := 1; seed <= wmS2RedSeeds(); seed++ {
				c := wmPath(1000, wmMS(rtt), wmMS(50))
				c.rack, c.reoField, c.seed, c.dur, c.pool = true, true, uint64(seed), 45*time.Second, pool
				c.mode = wmCore
				k0 := wmS2RedRun(c, 0)
				c.apProbing = func(at time.Duration) bool { return at >= 15*time.Second && at < 35*time.Second }
				k1 := wmS2RedRun(c, 15*time.Second)
				t.Logf("1000/%d pool %d MiB s%d: 15-35 s %.0f with the probe, %.0f without (%.0f%%); after %.0f/%.0f; rs %.3f%%/%.3f%% undo %d/%d",
					rtt, pool>>20, seed, wmMbitOver(k1.r, k1.m, 15*time.Second, 35*time.Second), wmMbitOver(k0.r, k0.m, 15*time.Second, 35*time.Second),
					100*wmMbitOver(k1.r, k1.m, 15*time.Second, 35*time.Second)/wmMbitOver(k0.r, k0.m, 15*time.Second, 35*time.Second),
					wmMbitOver(k1.r, k1.m, 35*time.Second, 45*time.Second), wmMbitOver(k0.r, k0.m, 35*time.Second, 45*time.Second),
					100*k1.r.resend, 100*k0.r.resend, k1.undo, k0.undo)
			}
		}
	}
}
