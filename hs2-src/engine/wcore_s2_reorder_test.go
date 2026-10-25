package engine

import (
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// Reordering read as loss (S2): on the real 72 ms Iran↔abroad path the
// edge's core cut a fast download's windows to the 2 MiB floor again and
// again, "packets arrived behind holes on a clean path", while the exit's
// socket resent 0.000 to 0.002% of its segments. Direct TCP on the same
// path showed why: the path reorders, in bursts, more the faster the flow
// (wmReo), and Linux's RACK waits that out (resends near none), so the only
// trace of it is the receiver's Rcv_ooopack. The core's fastStop takes a
// wake's jump of it past its level (wcOooCongAbs) for a loss when the holes
// held packets T_base/4 or more (lossHole), and cut takes B to the floor
// when no queue shows (a policer's loss, as it reads it).
//
// This file is the harness: the field's profile against its samples
// (TestWcoreS2ReorderProfile), the model's RACK sender against Linux's
// (TestWcoreS2ReorderRack), the reproduction as a report
// (TestWcoreS2Reorder, HS2_WCORE_REORDER=1) and a trace of one run
// (HS2_WCORE_REORDER_TRACE), and the gate a fix has to pass
// (TestWcoreS2ReorderCore).

// wmS2Path is the field's path: 72 ms, a 50 ms queue, nothing lost.
func wmS2Path(rate float64) wmCfg {
	c := wmPath(rate, 72*time.Millisecond, 50*time.Millisecond)
	c.dur = 40 * time.Second
	return c
}

// wmS2Field is c on the field's reordering with the RACK sender.
func wmS2Field(c wmCfg) wmCfg {
	c.reoField, c.rack = true, true
	c.name += " field"
	return c
}

// wmReoBins is each 500 ms sample's share of segments that arrived behind
// a hole, from a run's cumulative series (wmResult.oooHalf) over [from, to)
// seconds; a sample with no segment in it is left out.
func wmReoBins(h [][2]uint64, from, to float64) []float64 {
	var v []float64
	for i := int(from * 2); i+1 < len(h) && float64(i+1) <= to*2; i++ {
		ooo, in := h[i+1][0]-h[i][0], h[i+1][1]-h[i][1]
		if in > 0 {
			v = append(v, float64(ooo)/float64(in))
		}
	}
	return v
}

// wmReoStat is what the field reported of a flow's samples.
type wmReoStat struct {
	all, any, p50, p90, max float64
}

func (s wmReoStat) String() string {
	return fmt.Sprintf("all %5.2f%% any %3.0f%% p50 %5.2f%% p90 %5.2f%% max %5.2f%%", 100*s.all, 100*s.any, 100*s.p50, 100*s.p90, 100*s.max)
}

// wmReoStats takes the samples' shares (each sample of the same weight, as
// at a steady rate; all is their mean).
func wmReoStats(v []float64) wmReoStat {
	var s wmReoStat
	if len(v) == 0 {
		return s
	}
	w := slices.Clone(v)
	slices.Sort(w)
	n := 0
	for _, x := range w {
		s.all += x
		if x > 0 {
			n++
		}
	}
	s.all /= float64(len(w))
	s.any = float64(n) / float64(len(w))
	s.p50 = w[len(w)/2]
	s.p90 = w[min(len(w)-1, len(w)*9/10)]
	s.max = w[len(w)-1]
	return s
}

// wmReoStream runs the profile on a connection of mbit Mbit/s for secs
// seconds, with no window and no sender (packets of the model's unit at that
// rate, sent evenly each ms, through wmReo as wmModel.serve takes them), and
// returns its 500 ms samples. The receiver is the model's (arrive): the line
// delivers in order and the held packets come wmReo.ms late, after the
// line's of their ms, so a packet on time arrives behind a hole if one was
// held within the wmReo.ms before it; the held ones never do (each is as late
// as the others).
func wmReoStream(mbit float64, secs int, seed uint64, scale float64) []float64 {
	r := newWmReo(seed, 0, scale)
	k := max(1, int(math.Ceil(mbit*wmMbit/1000/(25*wmMSS))))
	unit := k * wmMSS
	per := mbit * wmMbit / 1000
	credit := 0.0
	heldAt := int64(math.MinInt32)
	var ooo, in uint64
	var v []float64
	for t := int64(0); t < int64(secs)*1000; t++ {
		credit += per
		for credit >= float64(unit) {
			credit -= float64(unit)
			in += uint64(k)
			if r.hold(unit) > 0 {
				heldAt = t
			} else if t-heldAt <= r.ms {
				ooo += uint64(k)
			}
		}
		r.tick(t)
		if (t+1)%500 == 0 {
			if t >= 1000 { // (the rate's average settles)
				v = append(v, float64(ooo)/float64(max(in, 1)))
			}
			ooo, in = 0, 0
		}
	}
	return v
}

// wmReoFieldRows is what the field measured (wmReo); -1: not reported.
var wmReoFieldRows = []struct {
	mbit float64
	n    int // samples
	s    wmReoStat
}{
	{300, 29, wmReoStat{all: 0.020, any: 5.0 / 29, p50: 0, p90: 0.066, max: 0.38}},
	{600, 28, wmReoStat{all: 0.061, any: 13.0 / 28, p50: 0, p90: 0.22, max: 0.35}},
	{1700, 30, wmReoStat{all: 0.147, any: -1, p50: 0.20, p90: 0.42, max: 0.49}},
}

// TestWcoreS2ReorderProfile holds wmReo to the field's samples: on a steady
// connection at each rate the field measured, 2000 samples (the field had
// ~29 each: its p90 and max are a few samples' worth) read within a third of
// its overall share, its share of samples with any within 0.15, its p50 and
// p90 within a half to one and a half times plus 2 points, no sample past
// 55% (none of the field's was past half); at a few Mbit/s at most 0.5%. The
// field's draws of 29 samples are in the log too, as a model of what one
// measurement of the field gives.
func TestWcoreS2ReorderProfile(t *testing.T) {
	for _, f := range wmReoFieldRows {
		var all []float64
		for seed := uint64(1); seed <= 4; seed++ {
			all = append(all, wmReoStream(f.mbit, 251, seed, 1)...)
		}
		s := wmReoStats(all)
		var draws []string
		for i := 0; i+f.n <= len(all) && len(draws) < 4; i += 6 * f.n {
			draws = append(draws, wmReoStats(all[i:i+f.n]).String())
		}
		t.Logf("%5.0f Mbit/s: model %s\n\t\tfield %s\n\t\tdraws of %d: %s", f.mbit, s, f.s, f.n, strings.Join(draws, "\n\t\t             "))
		near := func(got, want, rel, abs float64) bool {
			return want < 0 || got >= want*(1-rel)-abs && got <= want*(1+rel)+abs
		}
		if !near(s.all, f.s.all, 1.0/3, 0) || !near(s.any, f.s.any, 0, 0.15) ||
			!near(s.p50, f.s.p50, 0.5, 0.02) || !near(s.p90, f.s.p90, 0.5, 0.02) || s.max > 0.55 {
			t.Errorf("%.0f Mbit/s: the profile reads %s, the field %s", f.mbit, s, f.s)
		}
	}
	for _, mbit := range []float64{2, 5, 10} {
		if s := wmReoStats(wmReoStream(mbit, 251, 1, 1)); s.all > 0.005 {
			t.Errorf("%.0f Mbit/s: the profile reads %s, the field 0-0.5%%", mbit, s)
		}
	}
}

// TestWcoreS2ReorderRack holds the model's RACK sender (cfg.rack) to
// Linux's:
//   - on a path that keeps order it is the sender of before, run for run
//     (it calls a packet lost as soon as one sent after it came in, until it
//     has seen reordering);
//   - on the field's reordering the exit resends next to nothing (the field:
//     3 to 26 segments in 15 s, all of them before Linux saw reordering) and
//     the receiver reads the reordering as packets behind holes; the sender of
//     before resends what the path held, and its record reads it as loss;
//   - a path that holds packets past min_rtt/4 (30 ms on 72) costs a resend
//     until a DSACK took the window a step up (36 ms), and none after.
func TestWcoreS2ReorderRack(t *testing.T) {
	for _, seed := range []uint64{1, 2} {
		c := wmS2Path(1000)
		c.seed = seed
		for _, mode := range []wmMode{wmBase, wmCore} {
			old := wmRun(c.with(mode))
			c.rack = true
			r := wmRun(c.with(mode))
			c.rack = false
			if r.mbit != old.mbit || r.resend != old.resend || r.drains != old.drains || r.probes != old.probes {
				t.Errorf("%s %s s%d keeps order: RACK %s, the sender of before %s", c.name, mode, seed, r, old)
			}
		}
	}
	for _, seed := range []uint64{1, 2, 3} {
		c := wmS2Field(wmS2Path(1000))
		c.seed = seed
		r := wmRun(c.with(wmBase))
		c.rack = false
		old := wmRun(c.with(wmBase))
		t.Logf("%s s%d base: RACK %s\n\t\t\t\t      the sender of before %s", c.name, seed, wmS2Line(r), wmS2Line(old))
		if r.resend > 0.0005 || r.spurious > 200 || r.ooo < 0.002 {
			t.Errorf("%s s%d: RACK resent %.3f%% (%d segments spurious), %.2f%% behind holes: want ≤ 0.05%%, ≤ 200, ≥ 0.2%%",
				c.name, seed, 100*r.resend, r.spurious, 100*r.ooo)
		}
		if old.resend < 0.002 {
			t.Errorf("%s s%d: the sender of before resent %.3f%%: it no longer resends reordered packets?", c.name, seed, 100*old.resend)
		}
	}
	for _, x := range []struct {
		ms     int
		maxSpu uint64 // segments
	}{{10, 64}, {30, 64}} {
		c := wmS2Path(1000)
		c.reorder, c.reorderMs, c.rack, c.name = 0.002, x.ms, true, fmt.Sprintf("1000/72 q50 ro0.2%%/%dms", x.ms)
		r := wmRun(c.with(wmBase))
		c.rack = false
		old := wmRun(c.with(wmBase))
		t.Logf("%s base: RACK %s\n\t\t\t\t      the sender of before %s", c.name, wmS2Line(r), wmS2Line(old))
		if r.spurious > x.maxSpu || r.resend > 0.0002 {
			t.Errorf("%s: RACK resent %.3f%% after the warm-up, %d segments spurious in all: want ≤ 0.02%%, ≤ %d",
				c.name, 100*r.resend, r.spurious, x.maxSpu)
		}
	}
}

// TestWcoreS2ReorderCore is the gate a fix has to pass, and the core of
// before does not: on the field's path at 1000 Mbit/s, with its reordering
// and the RACK sender (under base's windows the exit resends next to nothing
// and packets do arrive behind holes: checked first, as the harness's own;
// the core's own resends are its business, a queue it overflows), the core
// keeps at least two thirds of the gain over base it has on the same path
// without reordering (medians of three seeds). Before a fix the
// reordering's bursts are cuts to the floor ("packets arrived behind holes
// on a clean path"): 17% of the gain.
func TestWcoreS2ReorderCore(t *testing.T) {
	const seeds, keep = 3, 2.0 / 3
	c := wmS2Path(1000)
	b0, k0 := wmRunN(c.with(wmBase), seeds), wmRunN(c.with(wmCore), seeds)
	f := wmS2Field(c)
	b, k := wmRunN(f.with(wmBase), seeds), wmRunN(f.with(wmCore), seeds)
	t.Logf("%s:       base %s\n\t\t\t\t      core %s", c.name, wmS2Line(b0), wmS2Line(k0))
	t.Logf("%s: base %s\n\t\t\t\t      core %s", f.name, wmS2Line(b), wmS2Line(k))
	if b.resend > 0.0005 || b.ooo < 0.002 {
		t.Fatalf("the harness: under base's windows the exit resent %.3f%% and %.2f%% arrived behind holes: want ≤ 0.05%%, ≥ 0.2%%",
			100*b.resend, 100*b.ooo)
	}
	gain, gain0 := k.mbit-b.mbit, k0.mbit-b0.mbit
	if gain0 < 2*b0.mbit {
		t.Fatalf("the harness: the core's gain on the clean path is %.0f Mbit/s over base's %.0f", gain0, b0.mbit)
	}
	if gain < keep*gain0 {
		t.Errorf("the field's reordering (exit resent %.3f%%) took the core's gain from %.0f to %.0f Mbit/s (%.0f%%; want ≥ %.0f%%): %d drains for loss, %d cuts",
			100*k.resend, gain0, gain, 100*gain/gain0, 100*keep, k.drains[1], k.cuts)
	}
}

// wmS2Line is a run of the report.
func wmS2Line(r wmResult) string {
	s := wmReoStats(wmReoBins(r.oooHalf, r.cfg.warm.Seconds(), r.cfg.dur.Seconds()))
	return fmt.Sprintf("%6.1f Mbit rs %6.3f%% (spur %3d) ooo %5.2f%% p90 %5.1f%% max %4.1f%% eps %2d | d%v/%d cut%d p%v B%4.1f",
		r.mbit, 100*r.resend, r.spurious, 100*r.ooo, 100*s.p90, 100*s.max, r.reoEps, r.drains, r.holdDrains, r.cuts, r.probes, float64(r.peakB)/(1<<20))
}

// TestWcoreS2Reorder (HS2_WCORE_REORDER=1, a report): one download on the
// field's clean path at 1000 and 300 Mbit/s, base against the core, three
// seeds each: without reordering, with the field's (wmReo) and the RACK
// sender, and with the field's and the model's sender of before (it resends
// every reordered packet, as Linux before it has seen reordering: the exit's
// record shows the reordering as loss there). At 300 Mbit/s behind 50 ms the
// core never leaves OFF, reordering or not (base's 2 MiB windows already
// queue 8 to 17 ms at the exit: harm), so 300 runs behind 20 ms too.
// HS2_WCORE_REORDER_SEEDS sets the seeds, HS2_WCORE_REORDER_SCALE the
// episodes' share of the time, HS2_WCORE_REORDER_Q one queue (ms) for all.
func TestWcoreS2Reorder(t *testing.T) {
	if os.Getenv("HS2_WCORE_REORDER") != "1" {
		t.Skip("HS2_WCORE_REORDER=1")
	}
	seeds := 3
	fmt.Sscan(os.Getenv("HS2_WCORE_REORDER_SEEDS"), &seeds)
	scale := 0.0
	fmt.Sscan(os.Getenv("HS2_WCORE_REORDER_SCALE"), &scale)
	qOne := 0
	fmt.Sscan(os.Getenv("HS2_WCORE_REORDER_Q"), &qOne)
	for _, p := range []struct {
		rate float64
		qms  int
	}{{1000, 50}, {300, 50}, {300, 20}} {
		rate, qms := p.rate, p.qms
		if qOne > 0 {
			if p.qms != 50 {
				continue
			}
			qms = qOne
		}
		path := fmt.Sprintf("%.0f/72 q%d", rate, qms)
		type sum struct{ b, k, n float64 }
		sums := map[string]*sum{}
		var order []string
		for _, v := range []struct {
			tag       string
			field     bool
			rack, old bool
		}{{"clean", false, true, false}, {"field rack", true, true, false}, {"field old sender", true, false, true}} {
			for seed := 1; seed <= seeds; seed++ {
				c := wmS2Path(rate)
				c.queue, c.name = wmMS(qms), path
				c.seed = uint64(seed)
				c.rack = v.rack
				c.reoField, c.reoScale = v.field, scale
				b, k := wmRun(c.with(wmBase)), wmRun(c.with(wmCore))
				t.Logf("%s %-16s s%d base %s\n\t\t\t\t      core %s", c.name, v.tag, seed, wmS2Line(b), wmS2Line(k))
				if sums[v.tag] == nil {
					sums[v.tag] = &sum{}
					order = append(order, v.tag)
				}
				s := sums[v.tag]
				s.b += b.mbit
				s.k += k.mbit
				s.n++
			}
		}
		for _, tag := range order {
			s := sums[tag]
			t.Logf("%s %-16s mean base %6.1f core %6.1f Mbit (gain %+6.1f)", path, tag, s.b/s.n, s.k/s.n, (s.k-s.b)/s.n)
		}
	}
}

// TestWcoreS2ReorderTrace traces one run of the reproduction:
// HS2_WCORE_REORDER_TRACE=rate/seed[/base][/old] (old: the sender of before).
func TestWcoreS2ReorderTrace(t *testing.T) {
	spec := os.Getenv("HS2_WCORE_REORDER_TRACE")
	if spec == "" {
		t.Skip("HS2_WCORE_REORDER_TRACE=rate/seed[/base][/old]")
	}
	p := strings.Split(spec, "/")
	var rate float64
	var seed int
	fmt.Sscan(p[0], &rate)
	if len(p) > 1 {
		fmt.Sscan(p[1], &seed)
	}
	c := wmS2Field(wmS2Path(rate))
	c.seed, c.trace, c.mode = uint64(max(seed, 1)), true, wmCore
	for _, x := range p[2:] {
		switch x {
		case "base":
			c.mode = wmBase
		case "old":
			c.rack = false
		}
	}
	r := wmRun(c)
	for _, l := range r.trace {
		t.Log(l)
	}
	bins := wmReoBins(r.oooHalf, 0, c.dur.Seconds())
	var b strings.Builder
	for i, x := range bins {
		fmt.Fprintf(&b, " %.0f", 100*x)
		if i%20 == 19 {
			b.WriteString("\n\t")
		}
	}
	t.Logf("ooo %% each 0.5 s:\n\t%s", b.String())
	t.Log(r, wmS2Line(r))
}
