package engine

import (
	"fmt"
	"math"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// Red team, round 2 (Phase Z5, part B): the rules the fix round added
// (long round trips, the policer's signature, reordering, the old exit's
// gate) and the loss rule, on the link model with the options of round 2
// (wmCfg.rttAt, polAt, recEvery, recOff, recZero).

// wm2Cell is one cell of the round-2 sweep.
type wm2Cell struct {
	cat string // the sweep's category (its summary line)
	c   wmCfg
}

// wm2Outcome is what one cell gave: base and core, and the verdict.
type wm2Outcome struct {
	cell    wm2Cell
	b, k    wmResult
	drained bool // the loss rule would drain the link under the core but not without it
	resent  bool // the core resent 3 points or more past base
	slower  bool // the core under 90% of base
}

func wm2Run(cell wm2Cell) wm2Outcome {
	b, k := wmRun(cell.c.with(wmBase)), wmRun(cell.c.with(wmCore))
	return wm2Outcome{cell: cell, b: b, k: k, drained: wmDrainedByCore(b, k),
		resent: k.resend > b.resend+0.03, slower: k.mbit < 0.9*b.mbit}
}

func (o wm2Outcome) String() string {
	return fmt.Sprintf("%-46s s%d: base %6.1f Mbit rs %5.2f%% [%s] | core %6.1f Mbit rs %5.2f%% [%s] d%v p%v B%.1f",
		o.cell.c.name, o.cell.c.seed, o.b.mbit, 100*o.b.resend, wmLossNote(o.b), o.k.mbit, 100*o.k.resend, wmLossNote(o.k),
		o.k.drains, o.k.probes, float64(o.k.peakB)/(1<<20))
}

// wm2Sweep runs the cells and logs every one the core drains (or resends
// past base + 3 points), and a summary per category.
func wm2Sweep(t *testing.T, cells []wm2Cell) []wm2Outcome {
	type sum struct {
		n, drained, resent, slower int
		b, k                       float64
	}
	sums := map[string]*sum{}
	var cats []string
	var out []wm2Outcome
	for _, cell := range cells {
		o := wm2Run(cell)
		out = append(out, o)
		s := sums[cell.cat]
		if s == nil {
			s = &sum{}
			sums[cell.cat] = s
			cats = append(cats, cell.cat)
		}
		s.n++
		s.b += o.b.mbit
		s.k += o.k.mbit
		flag := ""
		if o.drained {
			s.drained++
			flag += " DRAINED"
		}
		if o.resent {
			s.resent++
			flag += " RESENT"
		}
		if o.slower {
			s.slower++
		}
		if flag != "" {
			t.Logf("BAD%s %s", flag, o)
		} else if os.Getenv("HS2_WCORE_RED2_ALL") == "1" {
			t.Logf("ok %s", o)
		}
	}
	sort.Strings(cats)
	for _, c := range cats {
		s := sums[c]
		t.Logf("SUM %-28s %4d runs: drained by the core %d, resent past base+3 %d, slower than 90%% of base %d; mean base %.1f core %.1f Mbit",
			c, s.n, s.drained, s.resent, s.slower, s.b/float64(s.n), s.k/float64(s.n))
	}
	return out
}

func wm2Name(rate float64, rtt, q int) string { return fmt.Sprintf("%.0f/%d q%d", rate, rtt, q) }

// wm2Imp is an impairment of a grid cell.
type wm2Imp struct {
	tag     string
	loss    float64
	pol     float64 // share of the path's rate
	reorder float64
	roMs    int
}

var wm2Imps = []wm2Imp{
	{tag: "clean"}, {tag: "l0.1", loss: 0.001}, {tag: "l1", loss: 0.01}, {tag: "l3", loss: 0.03},
	{tag: "pol30", pol: 0.3}, {tag: "pol60", pol: 0.6}, {tag: "pol90", pol: 0.9},
}

func wm2Apply(c *wmCfg, rate float64, x wm2Imp) {
	c.loss = x.loss
	if x.pol > 0 {
		c.policer = x.pol * rate * wmMbit
	}
	c.reorder, c.reorderMs = x.reorder, x.roMs
	c.name += " " + x.tag
}

func wm2Ints(env string, def []int) []int {
	s := os.Getenv(env)
	if s == "" {
		return def
	}
	var v []int
	for _, f := range strings.Split(s, ",") {
		var x int
		fmt.Sscan(f, &x)
		v = append(v, x)
	}
	return v
}

// TestWcoreRed2Grid is the loss rule's grid of round 2 (HS2_WCORE_RED2=1):
// rates 50 to 1000 Mbit/s × round trips (HS2_WCORE_RED2_RTTS, default 20 to
// 300 ms) × queues 10 to 400 ms × clean, random loss 0.1 to 3%, a policer at
// 30 to 90% × 1 and 4 streams (HS2_WCORE_RED2_STREAMS) × 2 seeds.
func TestWcoreRed2Grid(t *testing.T) {
	if os.Getenv("HS2_WCORE_RED2") != "1" {
		t.Skip("HS2_WCORE_RED2=1")
	}
	wm2Sweep(t, wm2Grid(wm2Ints("HS2_WCORE_RED2_RTTS", []int{20, 50, 100, 150, 300}), wm2Ints("HS2_WCORE_RED2_STREAMS", []int{1, 4})))
}

// wm2Grid is the grid of TestWcoreRed2Grid for the given round trips and
// stream counts.
func wm2Grid(rtts, streams []int) []wm2Cell {
	var cells []wm2Cell
	for _, rtt := range rtts {
		for _, rate := range []float64{50, 100, 300, 1000} {
			for _, q := range []int{10, 50, 200, 400} {
				for _, x := range wm2Imps {
					for _, n := range streams {
						for seed := uint64(1); seed <= 2; seed++ {
							c := wmPath(rate, wmMS(rtt), wmMS(q))
							c.name = wm2Name(rate, rtt, q)
							wm2Apply(&c, rate, x)
							if n > 1 {
								c.streams = make([]wmStreamCfg, n)
								c.name += fmt.Sprintf(" x%d", n)
							}
							c.seed, c.dur = seed, 40*time.Second
							if rtt >= 450 {
								c.dur, c.warm = 90*time.Second, 20*time.Second
							}
							cat := fmt.Sprintf("rtt%d %s", rtt, x.tag)
							cells = append(cells, wm2Cell{cat: cat, c: c})
						}
					}
				}
			}
		}
	}
	return cells
}

// wm2Dyn is the round-2 sweep of what changes during a run
// (HS2_WCORE_RED2=1; HS2_WCORE_RED2_CAT picks categories by prefix):
// route changes across the long round trip's thresholds, a policer whose
// rate changes or that shares the path with a real queue, the exit's records
// stalling, slower or without TCP_INFO, reordering, path rate changes.
func wm2Dyn() []wm2Cell {
	var cells []wm2Cell
	add := func(cat string, c wmCfg) {
		for seed := uint64(1); seed <= 2; seed++ {
			c.seed = seed
			cells = append(cells, wm2Cell{cat: cat, c: c})
		}
	}
	// route changes (min_rtt keeps the old floor)
	for _, rc := range [][2]int{{150, 600}, {300, 600}, {300, 1000}, {100, 500}, {250, 450}, {400, 800}, {300, 2000}, {600, 150}, {1000, 300}} {
		for _, rate := range []float64{100, 300, 1000} {
			for _, q := range []int{20, 50, 200} {
				for _, loss := range []float64{0, 0.001} {
					c := wmPath(rate, wmMS(rc[0]), wmMS(q))
					c.loss = loss
					c.rttAt = []wmChange{{20 * time.Second, float64(rc[1])}}
					c.dur, c.warm = 90*time.Second, 10*time.Second
					c.name = fmt.Sprintf("%s rtt>%d l%g", wm2Name(rate, rc[0], q), rc[1], 100*loss)
					add(fmt.Sprintf("route %d>%d", rc[0], rc[1]), c)
				}
			}
		}
	}
	// a policer whose rate changes, or that shares the path with a queue
	for _, rate := range []float64{300, 1000} {
		for _, rtt := range []int{100, 150, 300} {
			for _, q := range []int{20, 50, 200} {
				for _, pc := range []struct {
					tag      string
					from, to float64
				}{{"pol60>none", 0.6, 0}, {"none>pol60", 0, 0.6}, {"pol60>pol30", 0.6, 0.3}, {"pol30>pol90", 0.3, 0.9}, {"pol90>pol40", 0.9, 0.4}} {
					c := wmPath(rate, wmMS(rtt), wmMS(q))
					c.policer = pc.from * rate * wmMbit
					c.polAt = []wmChange{{20 * time.Second, pc.to * rate * wmMbit}}
					c.dur = 60 * time.Second
					c.name = fmt.Sprintf("%s %s", wm2Name(rate, rtt, q), pc.tag)
					add("policer "+pc.tag, c)
				}
				c := wmPath(rate, wmMS(rtt), wmMS(q))
				c.policer, c.compete, c.dur = 0.6*rate*wmMbit, 10*time.Second, 60*time.Second
				c.name = fmt.Sprintf("%s pol60+compete", wm2Name(rate, rtt, q))
				add("policer pol60+compete", c)
				c = wmPath(rate, wmMS(rtt), wmMS(q))
				c.policer, c.loss, c.dur = 0.6*rate*wmMbit, 0.001, 60*time.Second
				c.name = fmt.Sprintf("%s pol60+l0.1", wm2Name(rate, rtt, q))
				add("policer pol60+l0.1", c)
			}
		}
	}
	// the exit's records: a stall (the stream reopens), a slower cadence,
	// records without TCP_INFO after a lossy start
	type rp struct {
		rate   float64
		rtt, q int
		loss   float64
		pol    float64
	}
	recPaths := []rp{{300, 150, 20, 0.001, 0}, {1000, 100, 50, 0.001, 0}, {300, 150, 50, 0.003, 0}, {1000, 100, 20, 0.001, 0},
		{300, 100, 20, 0, 0}, {1000, 150, 50, 0, 0.6}, {300, 300, 50, 0, 0.6}, {100, 150, 20, 0.001, 0}, {1000, 50, 20, 0.0005, 0}}
	for _, p := range recPaths {
		base := func(tag string) wmCfg {
			c := wmPath(p.rate, wmMS(p.rtt), wmMS(p.q))
			c.loss, c.policer, c.dur = p.loss, p.pol*p.rate*wmMbit, 60*time.Second
			c.name = fmt.Sprintf("%s l%g pol%g %s", wm2Name(p.rate, p.rtt, p.q), 100*p.loss, p.pol, tag)
			return c
		}
		for _, gap := range [][2]int{{15, 30}, {15, 45}, {10, 50}} {
			c := base(fmt.Sprintf("recoff%d-%d", gap[0], gap[1]))
			c.recOff = [][2]time.Duration{{time.Duration(gap[0]) * time.Second, time.Duration(gap[1]) * time.Second}}
			add("records stall", c)
		}
		for _, ev := range []int{3, 4, 6} {
			c := base(fmt.Sprintf("rec/%ds", ev))
			c.recEvery = time.Duration(ev) * time.Second
			add("records slower", c)
		}
		c := base("rec0@20s")
		c.recZero = [][2]time.Duration{{19 * time.Second, 21 * time.Second}}
		c.lossAt = []wmChange{{0, 0.03}, {8 * time.Second, p.loss}}
		add("records no tcp_info", c)
	}
	// reordering
	for _, rate := range []float64{300, 1000} {
		for _, rtt := range []int{20, 50, 100, 150} {
			for _, ro := range []struct {
				p  float64
				ms int
			}{{0.0005, 2}, {0.0005, 5}, {0.002, 2}, {0.01, 1}, {0.001, 10}} {
				c := wmPath(rate, wmMS(rtt), wmMS(50))
				c.reorder, c.reorderMs, c.dur = ro.p, ro.ms, 40*time.Second
				c.name = fmt.Sprintf("%s ro%g/%dms", wm2Name(rate, rtt, 50), 100*ro.p, ro.ms)
				add("reorder", c)
			}
		}
	}
	// path rate changes
	for _, rtt := range []int{50, 100, 150, 300} {
		for _, q := range []int{20, 50, 200} {
			for _, ch := range []struct {
				tag   string
				rate  float64
				steps []wmChange
			}{
				{"1000>100", 1000, []wmChange{{15 * time.Second, 100 * wmMbit}}},
				{"300>50>300", 300, []wmChange{{15 * time.Second, 50 * wmMbit}, {30 * time.Second, 300 * wmMbit}}},
				{"100>1000", 100, []wmChange{{15 * time.Second, 1000 * wmMbit}}},
				{"1000>300>1000", 1000, []wmChange{{15 * time.Second, 300 * wmMbit}, {30 * time.Second, 1000 * wmMbit}}},
			} {
				c := wmPath(ch.rate, wmMS(rtt), wmMS(q))
				c.changes, c.dur = ch.steps, 50*time.Second
				c.name = fmt.Sprintf("%s %s", wm2Name(ch.rate, rtt, q), ch.tag)
				add("rate "+ch.tag, c)
			}
		}
	}
	return cells
}

func TestWcoreRed2Dyn(t *testing.T) {
	if os.Getenv("HS2_WCORE_RED2") != "1" {
		t.Skip("HS2_WCORE_RED2=1")
	}
	var cells []wm2Cell
	for _, c := range wm2Dyn() {
		if p := os.Getenv("HS2_WCORE_RED2_CAT"); p == "" || strings.HasPrefix(c.cat, p) {
			cells = append(cells, c)
		}
	}
	wm2Sweep(t, cells)
}

// TestWcoreRed2Trace traces one cell of the round-2 sweeps whose name (and
// seed) is HS2_WCORE_RED2_TRACE="<name>/<seed>[/base]".
func TestWcoreRed2Trace(t *testing.T) {
	spec := os.Getenv("HS2_WCORE_RED2_TRACE")
	if spec == "" {
		t.Skip("HS2_WCORE_RED2_TRACE=<name>/<seed>[/base]")
	}
	p := strings.Split(spec, "/s")
	name, seed := p[0], uint64(1)
	base := strings.HasSuffix(spec, "/base")
	if len(p) > 1 {
		fmt.Sscan(strings.TrimSuffix(p[len(p)-1], "/base"), &seed)
		name = strings.Join(p[:len(p)-1], "/s")
	}
	var rtt, n int
	fmt.Sscanf(name[strings.Index(name, "/")+1:], "%d", &rtt)
	if i := strings.Index(name, " x"); i >= 0 {
		fmt.Sscanf(name[i+2:], "%d", &n)
	}
	all := append(wm2Dyn(), wm2Grid([]int{rtt}, []int{max(n, 1)})...)
	for _, c := range all {
		if c.c.name != name || c.c.seed != seed {
			continue
		}
		c.c.trace, c.c.mode = true, wmCore
		if base {
			c.c.mode = wmBase
		}
		r := wmRun(c.c)
		w := wmLossWins(r, 1)
		for _, l := range r.trace {
			if sec := 0; len(l) > 6 {
				fmt.Sscanf(strings.TrimSpace(l[:6]), "%d", &sec)
				if sec < len(w) && !strings.HasPrefix(strings.TrimSpace(l), "event") {
					l += fmt.Sprintf(" | rs1s %.1f%%", 100*w[sec])
				}
			}
			t.Log(l)
		}
		t.Log(r, wmLossNote(r))
		return
	}
	t.Fatalf("no cell %q seed %d", name, seed)
}

// TestWcoreRed2SlowedAfterCut (round 2): the path slows down (1000 → 100
// Mbit/s) under a full window; its loss comes first and a cut between
// evaluations halves B. HOLD's own test of a slower path compares with the
// rate of its first measured interval, which is after the cut, and R̂ keeps
// the faster rate for wcRhatWin: the loss drain's end took B back to a
// budget of the old rate (bHold within bCap) into a standing queue of 100 ms
// and more, lost again and was cut again, seven times in ten seconds (the
// model's 1000/100 q200 1000>100: the loss rule's streak 4, base 0%). With
// the queue standing and the rate past wcRateDrop under R̂, it is a drain
// for rate: R̂ starts over and B goes under the knee of the slower path.
func TestWcoreRed2SlowedAfterCut(t *testing.T) {
	for _, pending := range []bool{false, true} {
		c, in := wtHold()
		c.lossDrain, c.lossDrainN, c.refPending = !pending, 2, pending
		c.B, c.bPub = 4*wtMiB, 4*wtMiB
		c.bCap, c.bCapAt = 7*wtMiB, wtT0
		now := wtT0.Add(3 * time.Second)
		in.now = now
		in.streams[0].win = 4 * wtMiB
		m := wtMeas(1, 12e6, 4*wtMiB)
		m.at = now
		m.qOwn, m.qOwnOK = 140*time.Millisecond, true
		m.qTurn, m.qOK = 150*time.Millisecond, true
		var ev []wcEvent
		c.hold(in, m, &ev)
		if c.B > 4*wtMiB || c.state != wcDrain || c.cnt.drainRate != 1 {
			t.Errorf("pending %v: the path slowed (12 of 60 MB/s, q_own 140 ms) after a cut: %v B %.1f MiB, drains for rate %d; want a drain for rate under 4 MiB",
				pending, c.state, float64(c.B)/wtMiB, c.cnt.drainRate)
		}
		// a cut on a path that did not slow: the queue is empty, B stays
		c, in = wtHold()
		c.lossDrain, c.lossDrainN = true, 2
		c.B, c.bPub = 4*wtMiB, 4*wtMiB
		in.now = now
		m.qOwn, m.qTurn = 2*time.Millisecond, 6*time.Millisecond
		ev = nil
		c.hold(in, m, &ev)
		if c.cnt.drainRate != 0 {
			t.Errorf("pending %v: a cut with the queue empty (the rate fell with B): a drain for rate", pending)
		}
	}
}

// TestWcoreRed2RateDropModel: 1000 → 100 Mbit/s behind a 200 ms queue on
// 100 and 150 ms paths, where the windows without the core lose nothing:
// the core's one wave of loss at the drop must not become a streak the loss
// rule drains the link for.
func TestWcoreRed2RateDropModel(t *testing.T) {
	for _, rtt := range []int{100, 150} {
		for seed := uint64(1); seed <= 2; seed++ {
			c := wmPath(1000, wmMS(rtt), wmMS(200))
			c.changes, c.dur, c.seed = []wmChange{{15 * time.Second, 100 * wmMbit}}, 50*time.Second, seed
			o := wm2Run(wm2Cell{c: c})
			if o.drained {
				t.Errorf("drained by the core: %s", o)
			} else {
				t.Log(o)
			}
		}
	}
}

// TestWcoreRed2PaceCap (HS2_WCORE_RED2=1): the cells the round-2 sweeps
// found drained by the core (HS2_WCORE_RED2_CELLS: a file of "name/sseed"
// lines; default: a built-in set), each with the exit's pacing cap of the
// lab (WcorePaceCap, HS2_WCORE_RED2_PACE: its factor, default 2) on base
// and on the core.
func TestWcoreRed2PaceCap(t *testing.T) {
	if os.Getenv("HS2_WCORE_RED2") != "1" {
		t.Skip("HS2_WCORE_RED2=1")
	}
	k := 2.0
	if v := os.Getenv("HS2_WCORE_RED2_PACE"); v != "" {
		fmt.Sscan(v, &k)
	}
	var cells []wm2Cell
	for _, c := range wm2Named(t) {
		c.c.paceCap = k
		cells = append(cells, c)
	}
	wm2Sweep(t, cells)
}

// wm2Named is the cells of HS2_WCORE_RED2_CELLS (a file of "name/sseed"
// lines from the sweeps' BAD lines), found in the grid and the dynamic
// sweep.
func wm2Named(t *testing.T) []wm2Cell {
	path := os.Getenv("HS2_WCORE_RED2_CELLS")
	if path == "" {
		t.Skip("HS2_WCORE_RED2_CELLS=<file>")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	type key struct {
		name string
		seed uint64
	}
	want := map[key]bool{}
	var rtts, streams []int
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		i := strings.LastIndex(l, "/s")
		if i < 0 {
			continue
		}
		var seed uint64
		fmt.Sscan(l[i+2:], &seed)
		want[key{l[:i], seed}] = true
		var rtt, n int
		fmt.Sscanf(l[strings.Index(l, "/")+1:], "%d", &rtt)
		n = 1
		if j := strings.Index(l, " x"); j >= 0 {
			fmt.Sscanf(l[j+2:], "%d", &n)
		}
		rtts, streams = append(rtts, rtt), append(streams, n)
	}
	slices.Sort(rtts)
	slices.Sort(streams)
	var out []wm2Cell
	for _, c := range append(wm2Dyn(), wm2Grid(slices.Compact(rtts), slices.Compact(streams))...) {
		if want[key{c.c.name, c.c.seed}] {
			c.cat = "named"
			out = append(out, c)
			delete(want, key{c.c.name, c.c.seed})
		}
	}
	for k := range want {
		t.Logf("no cell %q s%d", k.name, k.seed)
	}
	return out
}

// TestWcoreRed2RecordsStop (round 2): an edge whose exit's records stop
// (the stats stream stalled on a congested link and is being reopened) has
// no unbiased loss signal left where packets wait behind holes: the core is
// blind there as without the records (round 1, finding 1), and goes OFF.
func TestWcoreRed2RecordsStop(t *testing.T) {
	c := wtCore(1)
	c.sndSeen, c.sndLastAt = true, wtT0
	m := wtMeas(1, 60e6, 4*wtMiB)
	m.ooo, m.oooOK, m.holey = 0.5, true, true
	m.at = wtT0.Add(2 * time.Second)
	if c.blind(m) {
		t.Errorf("a record 2 s ago: blind")
	}
	m.at = wtT0.Add(2*wcSndSpan + time.Second)
	if !c.blind(m) {
		t.Errorf("no record for %v on a path with holes: not blind", 2*wcSndSpan+time.Second)
	}
	m.holey = false
	if c.blind(m) {
		t.Errorf("no record on a clean path: blind (q_turn and Rcv_ooopack judge it)")
	}
	for _, x := range []struct {
		rate   float64
		rtt, q int
		loss   float64
		off    [2]int
	}{{1000, 100, 20, 0.001, [2]int{10, 50}}, {300, 150, 20, 0.001, [2]int{15, 45}}} {
		for seed := uint64(1); seed <= 2; seed++ {
			c := wmPath(x.rate, wmMS(x.rtt), wmMS(x.q))
			c.loss, c.seed, c.dur = x.loss, seed, 60*time.Second
			c.recOff = [][2]time.Duration{{time.Duration(x.off[0]) * time.Second, time.Duration(x.off[1]) * time.Second}}
			c.name = fmt.Sprintf("%s l%g recoff%d-%d", wm2Name(x.rate, x.rtt, x.q), 100*x.loss, x.off[0], x.off[1])
			o := wm2Run(wm2Cell{c: c})
			if o.drained || o.resent {
				t.Errorf("records stopped: %s", o)
			} else {
				t.Log(o)
			}
		}
	}
}

// TestWcoreRed2RouteChange (round 2, finding 4): after a route change (300
// → 600 ms) both ends' min_rtt keep the old floor (Linux: 5 minutes), and
// notePaths took the turnaround's new floor only on a record of the exit
// that showed no queue, which the exit's own stale min_rtt never shows
// (q_snd read the longer path as ~300 ms of queue): T_base stayed 300 ms to
// the end and the long round trip's rules (wcLongRTT, wcLongGear) did not
// apply. The longer path is taken where q_snd's floor rose with the
// turnaround's and stays there.
func TestWcoreRed2RouteChange(t *testing.T) {
	c := wmPath(300, wmMS(300), wmMS(50))
	c.rttAt = []wmChange{{20 * time.Second, 600}}
	c.dur, c.mode, c.seed = 120*time.Second, wmCore, 1
	var tb, qs time.Duration
	c.check = func(at time.Duration, k *wcore, in wcIn, o wcOut) { tb, qs = k.tBase, k.qSnd }
	wmRun(c)
	if tb < 500*time.Millisecond || qs > 100*time.Millisecond {
		t.Errorf("100 s after 300 → 600 ms: T_base %v (q_snd %v), want the new path's (and q_snd of its queue only)", tb, qs)
	}
}

// wtRoute feeds the core's notePaths a record and a turnaround every 2 s for
// d from *now: the edge's socket and the exit's at min_rtt minRTT with srtt,
// a stream's least turnaround turn; it returns the route events.
func wtRoute(c *wcore, now *time.Time, d, minRTT, srtt, sndSrtt, turn time.Duration) int {
	n := 0
	for end := now.Add(d); now.Before(end); *now = now.Add(2 * time.Second) {
		in := wcIn{now: *now, minRTT: minRTT, srtt: srtt, poolFree: -1,
			snd:     wcSnd{fresh: true, srtt: sndSrtt, minRTT: minRTT, segsOut: 1000},
			streams: []wcStream{{id: 1, turnMin: turn}}}
		var ev []wcEvent
		c.notePaths(in, &ev)
		for _, e := range ev {
			if e.kind == wcEvRoute {
				n++
			}
		}
	}
	return n
}

// notePaths after a route change: the longer path is T_base once the
// turnaround's floor and the exit's q_snd floor rose together (both ends'
// min_rtt stale) and q_snd then reads the queue past the new path only; a
// turnaround floor equal to min_rtt before the change is no trap (tbOff 0);
// a turnaround that rose while the exit's q_snd did not rise with it (a
// queue only the turnaround shows) or swings (a queue) is no route change.
func TestWcoreRed2RouteNotePaths(t *testing.T) {
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	for _, tc := range []struct {
		name             string
		turn0            time.Duration // the least turnaround before the change
		sndSrtt, turn    []time.Duration
		route            bool
		tBase, qSndUnder time.Duration
	}{
		{"300 → 600 ms, both min_rtt stale", ms(310), []time.Duration{ms(605)}, []time.Duration{ms(610)}, true, ms(600), ms(20)},
		{"... with a turnaround floor that was min_rtt (tbOff 0)", ms(300), []time.Duration{ms(605)}, []time.Duration{ms(600)}, true, ms(600), ms(20)},
		{"the turnaround rose, the exit's q_snd did not", ms(310), []time.Duration{ms(340)}, []time.Duration{ms(610)}, false, ms(300), 0},
		{"q_snd swings (a queue)", ms(310), []time.Duration{ms(420), ms(605), ms(780)}, []time.Duration{ms(610)}, false, ms(300), 0},
	} {
		c := wtCore(1)
		now := wtT0
		wtRoute(c, &now, time.Minute, ms(300), ms(305), ms(305), tc.turn0)
		if c.tBase != ms(300) {
			t.Fatalf("%s: before the change T_base %v", tc.name, c.tBase)
		}
		n := 0
		for end := now.Add(80 * time.Second); now.Before(end); {
			for i := range tc.sndSrtt {
				n += wtRoute(c, &now, 2*time.Second, ms(300), tc.sndSrtt[i]+5*time.Millisecond, tc.sndSrtt[i], tc.turn[min(i, len(tc.turn)-1)])
			}
		}
		if got := n > 0; got != tc.route || c.tBase != tc.tBase {
			t.Errorf("%s: route %v (T_base %v, q_snd %v), want %v (%v)", tc.name, got, c.tBase, c.qSnd, tc.route, tc.tBase)
		}
		if tc.route && c.qSnd > tc.qSndUnder {
			t.Errorf("%s: q_snd %v past the new path's floor, want under %v", tc.name, c.qSnd, tc.qSndUnder)
		}
		if !tc.route {
			continue
		}
		// the exit's min_rtt catches up (Linux renews it after 5 minutes):
		// q_snd reads its socket as it is again
		wtRoute(c, &now, 10*time.Second, ms(300), ms(605), ms(605), tc.turn[0])
		in := wcIn{now: now, minRTT: ms(300), srtt: ms(605), poolFree: -1, snd: wcSnd{fresh: true, srtt: ms(612), minRTT: ms(600), segsOut: 1000},
			streams: []wcStream{{id: 1, turnMin: tc.turn[0]}}}
		c.notePaths(in, new([]wcEvent))
		if c.qSnd != ms(12) || c.tBase != tc.tBase {
			t.Errorf("%s: the exit's min_rtt renewed: q_snd %v (want 12ms), T_base %v", tc.name, c.qSnd, c.tBase)
		}
	}
}

// TestWcoreRed2PolicerLifted (report, HS2_WCORE_RED2=1): a policer at 60% of
// the path for the first 20 s, then none: a policer's loss point (bCap)
// holds for wcQcapTTL (10 minutes) and is not cleared below it (policed).
// What the core carries after the policer is gone, against the same path
// with no policer at all and the windows without the core.
func TestWcoreRed2PolicerLifted(t *testing.T) {
	if os.Getenv("HS2_WCORE_RED2") != "1" {
		t.Skip("HS2_WCORE_RED2=1")
	}
	for _, p := range []struct {
		rate   float64
		rtt, q int
	}{{300, 150, 50}, {1000, 100, 50}, {1000, 150, 200}, {300, 300, 50}} {
		for seed := uint64(1); seed <= 2; seed++ {
			mk := func(pol bool, mode wmMode) (*wmModel, wmResult) {
				c := wmPath(p.rate, wmMS(p.rtt), wmMS(p.q))
				c.seed, c.dur, c.mode = seed, 200*time.Second, mode
				if pol {
					c.policer = 0.6 * p.rate * wmMbit
					c.polAt = []wmChange{{20 * time.Second, 0}}
				}
				m := newModel(c)
				return m, m.run()
			}
			mp, rp := mk(true, wmCore)
			mn, rn := mk(false, wmCore)
			mb, rb := mk(true, wmBase)
			var parts []string
			for _, w := range [][2]int{{20, 40}, {40, 80}, {80, 140}, {140, 200}} {
				from, to := time.Duration(w[0])*time.Second, time.Duration(w[1])*time.Second
				parts = append(parts, fmt.Sprintf("%d-%ds %.0f/%.0f/%.0f", w[0], w[1],
					wmMbitOver(rp, mp, from, to), wmMbitOver(rn, mn, from, to), wmMbitOver(rb, mb, from, to)))
			}
			t.Logf("%s s%d policed then lifted / never policed / base: %s | drains %v probes %v", wm2Name(p.rate, p.rtt, p.q), seed, strings.Join(parts, " | "), rp.drains, rp.probes)
		}
	}
}

// TestWcoreRed2RecordWithoutTCPInfo (round 2): when the exit's tcpStats fails
// for a poll, serveStats sends a record whose extended fields are all 0. The
// builder took its counters as the base of the next record, whose resend
// share was then the link's lifetime one (all of its resends over all of
// its segments), judged as one record's: a lossy start long ago read as a
// jump of loss now (a policer's signature too, with q_snd under wcPoliceQ).
func TestWcoreRed2RecordWithoutTCPInfo(t *testing.T) {
	var b wcBuilder
	s := wcLinkSnap{now: wtT0, poolFree: -1}
	s.sndAt, s.sndSrtt, s.sndMinRTT, s.sndRetrans, s.sndSegsOut = wtT0, 120*time.Millisecond, 100*time.Millisecond, 50000, 100000
	b.build(s)
	s.now, s.sndAt, s.sndRetrans, s.sndSegsOut = wtT0.Add(2*time.Second), wtT0.Add(2*time.Second), 50010, 110000
	if in := b.build(s); in.snd.retrans != 10 || in.snd.segsOut != 10000 {
		t.Fatalf("a record: %+v", in.snd)
	}
	// no TCP_INFO at the exit for one poll
	s.now, s.sndAt = wtT0.Add(4*time.Second), wtT0.Add(4*time.Second)
	s.sndSrtt, s.sndMinRTT, s.sndRetrans, s.sndSegsOut = 0, 0, 0, 0
	if in := b.build(s); in.snd.retrans != 0 || in.snd.segsOut != 0 || in.snd.srtt != 0 {
		t.Fatalf("a record without TCP_INFO: %+v", in.snd)
	}
	s.now, s.sndAt = wtT0.Add(6*time.Second), wtT0.Add(6*time.Second)
	s.sndSrtt, s.sndMinRTT, s.sndRetrans, s.sndSegsOut = 120*time.Millisecond, 100*time.Millisecond, 50020, 130000
	if in := b.build(s); in.snd.retrans != 10 || in.snd.segsOut != 20000 {
		t.Errorf("the record after one without TCP_INFO: %d resent of %d (%.1f%%), want 10 of 20000 since the last that had it",
			in.snd.retrans, in.snd.segsOut, 100*float64(in.snd.retrans)/float64(max(in.snd.segsOut, 1)))
	}
}

// wm2RatchetCells are one drained cell of each class the round-2 sweeps
// found where a drain to the floor does not give back the behaviour without
// the core (without the exit's pacing net): the exit's BBR, held by the windows, stays in its STARTUP and
// its pacing (3.9 times the largest rate it delivered) only rises, so once
// the core's windows took the rate up, each window update's credit at the
// floor (1 MiB at need) goes out faster than a shallow queue or a policer's
// bucket takes it.
var wm2RatchetCells = []string{
	"300/450 q50 pol60/s1",          // a policer past wcLongRTT (×2 STARTUP gear under wcLongGear)
	"300/800 q50 pol30/s1",          // ... and past wcLongGear (×1.25)
	"1000/300 q10 clean x4/s1",      // four streams, shallow queue (the ramp's B/8 a wake)
	"1000/100 q20 1000>300>1000/s2", // the path slows to 300 under a raised pacing
	"300/300 q50 none>pol60/s2",     // a policer comes mid-transfer
	"1000/20 q10 clean/s1",          // a short path whose base windows nearly fill it
	"1000/150 q50 pol90>pol40/s1",   // a policer gets stricter
}

// TestWcoreRed2Ratchet (round 2, finding 1): the loss rule must not drain a
// link under the core that it leaves alone without it. On the cells of
// wm2RatchetCells it did, as long as nothing held the sender's pace: the
// exit's pacing net (pacenet.go) holds it once its socket resends (each
// cell is run with the net off too, to show the finding is still there).
func TestWcoreRed2Ratchet(t *testing.T) {
	f := t.TempDir() + "/cells"
	if err := os.WriteFile(f, []byte(strings.Join(wm2RatchetCells, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HS2_WCORE_RED2_CELLS", f)
	cells := wm2Named(t)
	if len(cells) != len(wm2RatchetCells) {
		t.Fatalf("%d cells of %d", len(cells), len(wm2RatchetCells))
	}
	off := 0
	for _, c := range cells {
		o := wm2Run(c)
		c.c.netOff = true
		p := wm2Run(c)
		if p.drained {
			off++
		}
		msg := fmt.Sprintf("%s\n    without the exit's pacing net: %s", o, p)
		if o.drained {
			t.Errorf("drained by the core: %s", msg)
		} else {
			t.Log(msg)
		}
	}
	if off < len(cells)/2 {
		t.Errorf("only %d of %d cells drain without the pacing net: the finding is gone, the test proves nothing", off, len(cells))
	}
}

// TestWcoreRed2LossHoleLongRTT (round 2, finding 5): a wake's hole time is
// at most its own length (wakeHoles: the wake's length times the share of
// its segments behind a hole), and an active core wakes every wcRampTick:
// two wakes held at most ~200 ms, so past a T_base of ~800 ms no hole was
// ever a loss's (lossHole asks for T_base/4) however long it stayed open:
// Rcv_ooopack never cut B between evaluations (fastStop) nor counted as
// congestion (oooPast) there, and only the exit's records (every ~2 s) told
// the core of its losses. The holes are now summed over as many wakes as
// T_base/4 takes (two at least); reordering still opens none.
func TestWcoreRed2LossHoleLongRTT(t *testing.T) {
	for _, tb := range []time.Duration{400 * time.Millisecond, 800 * time.Millisecond, 1500 * time.Millisecond, 2 * time.Second} {
		for _, tc := range []struct {
			name string
			ooo  uint64 // of 1000 segments a wake
			hole bool
		}{{"every segment behind a hole", 1000, true}, {"reordering: 2% of each wake", 20, false}, {"a short hole: 30% of each wake", 300, false}} {
			c := wtCore(1)
			c.tBase = tb
			now := wtT0
			c.wakeAt = now
			wakes := int(math.Ceil(float64(tb)/4/float64(wcRampTick))) + 1
			for i := 0; i < wakes; i++ {
				now = now.Add(wcRampTick)
				c.step(wtWake(c, now, 1000, tc.ooo))
			}
			if c.holeSeen != tc.hole {
				t.Errorf("T_base %v, %s for %d wakes: a loss's hole %v, want %v", tb, tc.name, wakes, c.holeSeen, tc.hole)
			}
		}
	}
	// two wakes past ~800 ms are still no loss's hole: T_base/4 of waiting
	c := wtCore(1)
	c.tBase = 1500 * time.Millisecond
	now := wtT0
	c.wakeAt = now
	for i := 0; i < 2; i++ {
		now = now.Add(wcRampTick)
		c.step(wtWake(c, now, 1000, 1000))
	}
	if c.holeSeen {
		t.Errorf("T_base 1.5 s: two wakes (200 ms) behind a hole taken for a loss's (375 ms)")
	}
}

// TestWcoreRed2CarryKeepsCleanRecs (round 2, finding 7): a record of the exit
// that came in the settle part of an interval is carried to the next
// evaluation (sndCarry) with no srtt or min_rtt in it; it was counted as it
// came, and taken again as a record that is "not clean" it reset the
// clean records in a row (cleanRecs) that leaving OFF past wcLongRTT waits
// for. (Its time still counts as the latest record's, sndLastAt, as before:
// taking that away too made the core blind an evaluation sooner when the
// records stop, and TestWcoreRed2RecordsStop's 300/150 q20 l0.1 s1 resent
// 4.8% against base 1.3%.)
func TestWcoreRed2CarryKeepsCleanRecs(t *testing.T) {
	c := wtCore(1)
	c.tBase = 500 * time.Millisecond
	c.cleanRecs, c.sndSeen, c.sndLastAt = 2, true, wtT0
	c.sndCarry, c.sndCarryAt = wcSnd{fresh: true, retrans: 0, segsOut: 900}, wtT0
	c.lastAt = wtT0
	in := wtIn(wtT0.Add(2*time.Second), 1, 2*wtMiB)
	in.minRTT, in.srtt = 500*time.Millisecond, 505*time.Millisecond
	c.step(in)
	if c.cleanRecs != 2 {
		t.Errorf("an evaluation with only the carried record: cleanRecs %d, want 2", c.cleanRecs)
	}
	// a clean record next to a carried one that had resends: one more
	c.sndCarry, c.sndCarryAt = wcSnd{fresh: true, retrans: 30, segsOut: 900}, wtT0.Add(3*time.Second)
	in = wtIn(wtT0.Add(4*time.Second), 1, 2*wtMiB)
	in.minRTT, in.srtt = 500*time.Millisecond, 505*time.Millisecond
	in.snd = wcSnd{fresh: true, srtt: 502 * time.Millisecond, minRTT: 500 * time.Millisecond, segsOut: 800}
	c.step(in)
	if c.cleanRecs != 3 {
		t.Errorf("a clean record with a carried one's resends: cleanRecs %d, want 3", c.cleanRecs)
	}
}
