package engine

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The net's parameters for an experiment (HS2_PN_*: k, loss, ratio, span
// and clean in seconds, cleanshare, back, recur in seconds, win in seconds),
// read once by the test binary.
func init() {
	f := func(k string, set func(float64)) {
		if v, err := strconv.ParseFloat(os.Getenv(k), 64); err == nil {
			set(v)
		}
	}
	sec := func(v float64) time.Duration { return time.Duration(v * float64(time.Second)) }
	f("HS2_PN_K", func(v float64) { wcNetK = v })
	f("HS2_PN_LOSS", func(v float64) { wcNetLoss = v })
	f("HS2_PN_RATIO", func(v float64) { wcNetRatio = v })
	f("HS2_PN_SPAN", func(v float64) { wcNetSpan = sec(v) })
	f("HS2_PN_WIN", func(v float64) { wcNetWin = sec(v) })
	f("HS2_PN_CLEAN", func(v float64) { wcNetClean = sec(v) })
	f("HS2_PN_CLEANSHARE", func(v float64) { wcNetCleanShare = v })
	f("HS2_PN_BACK", func(v float64) { wcNetBackMax = int(v) })
	f("HS2_PN_RECUR", func(v float64) { wcNetRecur = sec(v) })
	f("HS2_PN_OFF", func(v float64) { PaceNet = 1 - int(v) })
	f("HS2_PN_QUEUE", func(v float64) { wcNetQueue = time.Duration(v * float64(time.Millisecond)) })
	f("HS2_PN_PAID", func(v float64) { wcNetPaid = v })
	f("HS2_PN_JUDGE", func(v float64) { wcNetJudge = sec(v) })
	f("HS2_PN_WARM", func(v float64) { wcNetWarm = sec(v) })
	f("HS2_PN_WARMLOSS", func(v float64) { wcNetWarmLoss = v })
	f("HS2_PN_GROW", func(v float64) { wcNetGrow = v })
}

// The pacing net (pacenet.go) on the link model: each cell of the red team's
// round-2 sweeps four ways, base and the core, each with the exit's net off
// and on.

// wnOutcome is one cell four ways.
type wnOutcome struct {
	cell             wm2Cell
	bOff, bOn        wmResult
	kOff, kOn        wmResult
	drOff, drOn      bool // the loss rule drains under the core and not under base (wmDrainedByCore)
	bDrOff, bDrOn    bool // ... drains base on its own (a streak of degradeStreak)
	kDrOffA, kDrOnA  bool // ... drains the core on its own
	bSlower, kSlower bool // the net's run under 95% of the same without it
}

// wmDrains says whether the loss rule drains r's link on its own (a streak
// of degradeStreak windows past lossFrac, 2 or 3 s windows).
func wmDrains(r wmResult) bool {
	for _, w := range []int{2, 3} {
		if s, _ := wmLossStreak(wmLossWins(r, w), lossFrac); s >= degradeStreak {
			return true
		}
	}
	return false
}

func wnRun(cell wm2Cell) wnOutcome {
	c := cell.c
	off, on := c, c
	off.netOff = true
	o := wnOutcome{cell: cell}
	o.bOff, o.bOn = wmRun(off.with(wmBase)), wmRun(on.with(wmBase))
	o.kOff, o.kOn = wmRun(off.with(wmCore)), wmRun(on.with(wmCore))
	o.drOff, o.drOn = wmDrainedByCore(o.bOff, o.kOff), wmDrainedByCore(o.bOn, o.kOn)
	o.bDrOff, o.bDrOn = wmDrains(o.bOff), wmDrains(o.bOn)
	o.kDrOffA, o.kDrOnA = wmDrains(o.kOff), wmDrains(o.kOn)
	o.bSlower, o.kSlower = o.bOn.mbit < 0.95*o.bOff.mbit, o.kOn.mbit < 0.95*o.kOff.mbit
	return o
}

func (o wnOutcome) String() string {
	b := func(v bool) int {
		if v {
			return 1
		}
		return 0
	}
	return fmt.Sprintf("%s/s%d | base %.1f rs%.2f%% → %.1f rs%.2f%% n%d %.1fs dr%d→%d | core %.1f rs%.2f%% → %.1f rs%.2f%% n%d %.1fs dr%d→%d | coreonly %d→%d",
		o.cell.c.name, o.cell.c.seed,
		o.bOff.mbit, 100*o.bOff.resend, o.bOn.mbit, 100*o.bOn.resend, o.bOn.netN, float64(o.bOn.netMs)/1000, b(o.bDrOff), b(o.bDrOn),
		o.kOff.mbit, 100*o.kOff.resend, o.kOn.mbit, 100*o.kOn.resend, o.kOn.netN, float64(o.kOn.netMs)/1000, b(o.kDrOffA), b(o.kDrOnA),
		b(o.drOff), b(o.drOn))
}

// wnClean says whether a cell's path is clean: no random loss, no policer,
// nothing changing during the run.
func wnClean(c wmCfg) bool {
	return c.loss == 0 && c.policer == 0 && len(c.lossAt) == 0 && len(c.changes) == 0 && len(c.polAt) == 0 &&
		len(c.rttAt) == 0 && c.reorder == 0 && !c.reoField && c.compete == 0 && len(c.recOff) == 0 && len(c.recZero) == 0 && c.recEvery == 0
}

// wnSweep runs the cells four ways, logs a line per cell that drains or
// slows down (all with HS2_PACENET_ALL=1; every line to HS2_PACENET_OUT)
// and the sums per category.
func wnSweep(t *testing.T, cells []wm2Cell) {
	type sum struct {
		n, drOff, drOn, bDrOff, bDrOn, kDrOff, kDrOn, bSlow, kSlow, engB, engK int
		bOff, bOn, kOff, kOn                                                   float64
		cn                                                                     int
		cbOff, cbOn, ckOff, ckOn                                               float64
	}
	sums := map[string]*sum{}
	tot := &sum{}
	var out *os.File
	if p := os.Getenv("HS2_PACENET_OUT"); p != "" {
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		out = f
	}
	for _, cell := range cells {
		o := wnRun(cell)
		for _, s := range []*sum{tot, sums[cell.cat]} {
			if s == nil {
				s = &sum{}
				sums[cell.cat] = s
			}
			s.n++
			add := func(v bool, p *int) {
				if v {
					*p++
				}
			}
			add(o.drOff, &s.drOff)
			add(o.drOn, &s.drOn)
			add(o.bDrOff, &s.bDrOff)
			add(o.bDrOn, &s.bDrOn)
			add(o.kDrOffA, &s.kDrOff)
			add(o.kDrOnA, &s.kDrOn)
			add(o.bSlower, &s.bSlow)
			add(o.kSlower, &s.kSlow)
			add(o.bOn.netN > 0, &s.engB)
			add(o.kOn.netN > 0, &s.engK)
			s.bOff += o.bOff.mbit
			s.bOn += o.bOn.mbit
			s.kOff += o.kOff.mbit
			s.kOn += o.kOn.mbit
			if wnClean(cell.c) {
				s.cn++
				s.cbOff += o.bOff.mbit
				s.cbOn += o.bOn.mbit
				s.ckOff += o.kOff.mbit
				s.ckOn += o.kOn.mbit
			}
		}
		if out != nil {
			fmt.Fprintf(out, "%s | %s\n", cell.cat, o)
		}
		if o.drOff || o.drOn || o.bDrOff != o.bDrOn || o.kDrOffA != o.kDrOnA || o.bSlower || o.kSlower || os.Getenv("HS2_PACENET_ALL") == "1" {
			t.Logf("CELL %s", o)
		}
	}
	var cats []string
	for c := range sums {
		cats = append(cats, c)
	}
	sort.Strings(cats)
	line := func(name string, s *sum) {
		cm := func(a float64) float64 {
			if s.cn == 0 {
				return 0
			}
			return a / float64(s.cn)
		}
		t.Logf("SUM %-26s %4d runs: core-only drains %d → %d; base drains %d → %d; core drains %d → %d; engaged base %d core %d; slower base %d core %d; mean base %.1f → %.1f core %.1f → %.1f; clean %d: base %.1f → %.1f core %.1f → %.1f",
			name, s.n, s.drOff, s.drOn, s.bDrOff, s.bDrOn, s.kDrOff, s.kDrOn, s.engB, s.engK, s.bSlow, s.kSlow,
			s.bOff/float64(s.n), s.bOn/float64(s.n), s.kOff/float64(s.n), s.kOn/float64(s.n),
			s.cn, cm(s.cbOff), cm(s.cbOn), cm(s.ckOff), cm(s.ckOn))
	}
	for _, c := range cats {
		line(c, sums[c])
	}
	line("ALL", tot)
}

// TestPaceNetSweep (HS2_PACENET_SWEEP=1): the round-2 sweeps with the net
// off and on. HS2_PACENET_SET: short (the grid at 20–300 ms), long (450 ms
// to 2 s), 16 (16 streams), dyn (route, policer, records, reordering, rate
// changes; HS2_WCORE_RED2_CAT picks categories), named (the cells of
// HS2_WCORE_RED2_CELLS), red1 (round 1's grid and loss-rule scenarios, the
// downloads: the net is the exit's, the downloads' sender).
func TestPaceNetSweep(t *testing.T) {
	if os.Getenv("HS2_PACENET_SWEEP") != "1" {
		t.Skip("HS2_PACENET_SWEEP=1")
	}
	var cells []wm2Cell
	switch os.Getenv("HS2_PACENET_SET") {
	case "short", "":
		cells = wm2Grid([]int{20, 50, 100, 150, 300}, []int{1, 4})
	case "long":
		cells = wm2Grid([]int{450, 500, 600, 800, 1200, 2000}, []int{1, 4})
	case "16":
		cells = wm2Grid([]int{50, 150, 300}, []int{16})
	case "dyn":
		for _, c := range wm2Dyn() {
			if p := os.Getenv("HS2_WCORE_RED2_CAT"); p == "" || strings.HasPrefix(c.cat, p) {
				cells = append(cells, c)
			}
		}
	case "named":
		cells = wm2Named(t)
	case "red1":
		cells = wnRed1()
	default:
		t.Fatalf("HS2_PACENET_SET=%q", os.Getenv("HS2_PACENET_SET"))
	}
	wnSweep(t, cells)
}

// wnRed1 is round 1's download cells: TestWcoreRedGrid's grid (2 seeds) and
// TestWcoreRedLossRuleSweep's scenarios (3 seeds).
func wnRed1() []wm2Cell {
	var cells []wm2Cell
	for _, rate := range []float64{100, 300, 1000} {
		for _, rtt := range []int{50, 100, 150, 300} {
			for _, q := range []int{10, 20, 50, 100} {
				for _, loss := range []float64{0, 0.0005, 0.001, 0.003, 0.01, -0.3, -0.6} {
					for seed := 1; seed <= 2; seed++ {
						c := wmPath(rate, wmMS(rtt), wmMS(q))
						tag := fmt.Sprintf("l%g", 100*loss)
						if loss < 0 {
							c.policer = -loss * rate * wmMbit
							tag = fmt.Sprintf("pol%g", -loss)
						} else {
							c.loss = loss
						}
						c.seed, c.dur = uint64(seed), 40*time.Second
						c.name += " " + tag
						cells = append(cells, wm2Cell{cat: "red1 grid " + tag, c: c})
					}
				}
			}
		}
	}
	for _, p := range []struct {
		rate     float64
		rtt, q   int
		loss     float64
		policer  float64
		lossAt   []wmChange
		changes  []wmChange
		n        int
		extraTag string
	}{
		{rate: 300, rtt: 150, q: 50}, {rate: 300, rtt: 150, q: 20}, {rate: 300, rtt: 150, q: 10}, {rate: 1000, rtt: 100, q: 10},
		{rate: 1000, rtt: 100, q: 50, loss: 0.001}, {rate: 1000, rtt: 100, q: 20, loss: 0.001}, {rate: 300, rtt: 150, q: 20, loss: 0.001},
		{rate: 300, rtt: 150, q: 20, loss: 0.003}, {rate: 300, rtt: 150, q: 50, loss: 0.01}, {rate: 100, rtt: 100, q: 50}, {rate: 100, rtt: 150, q: 50},
		{rate: 1000, rtt: 100, q: 50, policer: 300}, {rate: 1000, rtt: 100, q: 50, policer: 150}, {rate: 1000, rtt: 150, q: 50, policer: 200},
		{rate: 300, rtt: 150, q: 50, policer: 150},
		{rate: 300, rtt: 150, q: 50, lossAt: []wmChange{{12 * time.Second, 0.02}}, extraTag: " loss0>2%@12s"},
		{rate: 300, rtt: 150, q: 50, lossAt: []wmChange{{12 * time.Second, 0.05}}, extraTag: " loss0>5%@12s"},
		{rate: 1000, rtt: 100, q: 50, lossAt: []wmChange{{12 * time.Second, 0.02}}, extraTag: " loss0>2%@12s"},
		{rate: 300, rtt: 150, q: 50, changes: []wmChange{{12 * time.Second, 50 * wmMbit}, {22 * time.Second, 300 * wmMbit}}, extraTag: " 300>50>300"},
		{rate: 1000, rtt: 100, q: 20, changes: []wmChange{{12 * time.Second, 100 * wmMbit}}, extraTag: " 1000>100"},
		{rate: 300, rtt: 150, q: 50, n: 4}, {rate: 1000, rtt: 100, q: 20, n: 4},
	} {
		for seed := 1; seed <= 3; seed++ {
			c := wmPath(p.rate, wmMS(p.rtt), wmMS(p.q))
			c.loss, c.lossAt, c.changes, c.policer = p.loss, p.lossAt, p.changes, p.policer*wmMbit
			if p.n > 1 {
				c.streams = make([]wmStreamCfg, p.n)
			}
			c.dur, c.seed = 40*time.Second, uint64(seed)
			name := fmt.Sprintf("dn %.0f/%d q%d", p.rate, p.rtt, p.q)
			if p.loss > 0 {
				name += fmt.Sprintf(" l%g", 100*p.loss)
			}
			if p.policer > 0 {
				name += fmt.Sprintf(" pol%.0f", p.policer)
			}
			if p.n > 1 {
				name += fmt.Sprintf(" x%d", p.n)
			}
			c.name = name + p.extraTag
			cells = append(cells, wm2Cell{cat: "red1 scenarios", c: c})
		}
	}
	return cells
}
