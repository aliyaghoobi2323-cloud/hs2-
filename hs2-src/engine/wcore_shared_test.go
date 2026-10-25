package engine

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// Several links on one bottleneck (red team, round 3, finding 1): the edge's
// pool of links to one exit shares the path's queue, and each link's window
// core sees the queue the others build. The model (wcore_model_test.go) runs
// one wmModel per link, each with its own sender (the exit's socket of that
// link), receiver and core; the first link's path (bn) carries all of their
// packets through one tail-drop queue, and the cores share what the runner's
// registry shares between the links to one peer.

// wmLinkCfg sets one link of a shared run apart from the others.
type wmLinkCfg struct {
	start time.Duration // its download starts then (its streams' start)
	idle  bool          // no download: only the interactive replies on it
}

// wmShared is a shared run's result.
type wmShared struct {
	cfg        wmCfg
	links      []wmResult
	mbit       float64 // all the links' downloads over [warm, dur]
	qMean, q99 float64 // the bottleneck's queue over [warm, dur], ms
	qSec       []float64
	iP50, iP99 float64 // every link's interactive replies together, ms
	// sec: each link's download Mbit/s per second; off: each link's time in
	// OFF after its download started and the core first left OFF (s)
	sec   [][]float64
	off   []float64
	never []bool // the link's core never left OFF while its download ran
	trace []string
}

func (r wmShared) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-30s %4s %6.1f Mbit q %5.1f/%5.1f ms i %5.0f/%5.0f |", r.cfg.name, r.cfg.mode, r.mbit, r.qMean, r.q99, r.iP50, r.iP99)
	for i, l := range r.links {
		fmt.Fprintf(&b, " L%d %5.1f %4.2f%% d%v p%v", i, l.mbit, 100*l.resend, l.drains, l.probes)
		if r.never[i] {
			b.WriteString(" OFF!")
		}
	}
	return b.String()
}

// rateFrom is link i's download Mbit/s from second a to b.
func (r wmShared) rateFrom(i, a, b int) float64 {
	s := r.sec[i]
	sum, n := 0.0, 0
	for k := a; k < b && k < len(s); k++ {
		sum += s[k]
		n++
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// spread is the largest over the least download rate of the links with a
// download from second a to b.
func (r wmShared) spread(lc []wmLinkCfg, a, b int) float64 {
	lo, hi := math.Inf(1), 0.0
	for i := range r.links {
		if i < len(lc) && lc[i].idle {
			continue
		}
		v := r.rateFrom(i, a, b)
		lo, hi = math.Min(lo, v), math.Max(hi, v)
	}
	if lo <= 0 {
		return math.Inf(1)
	}
	return hi / lo
}

// wmRunShared runs n links of cfg on one bottleneck (cfg's rate, queue, loss
// and rate changes; each link its own sender, receiver, streams and core).
// lc sets links apart (nil: all alike); peer, when on, gives the cores one
// registry entry (the links to one peer) as the runner does.
func wmRunShared(cfg wmCfg, n int, lc []wmLinkCfg) wmShared {
	links := make([]*wmModel, n)
	for i := range links {
		c := cfg
		c.seed = cfg.seed*7919 + uint64(i)
		if i > 0 {
			c.compete = 0
		}
		c.trace = cfg.trace && i == wmTraceLink
		if i < len(lc) {
			if lc[i].idle {
				c.streams = []wmStreamCfg{{start: c.dur + time.Second}}
			} else if lc[i].start > 0 {
				c.streams = append([]wmStreamCfg(nil), c.streams...)
				if len(c.streams) == 0 {
					c.streams = []wmStreamCfg{{}}
				}
				for k := range c.streams {
					c.streams[k].start += lc[i].start
				}
			}
		}
		links[i] = newModel(c)
	}
	bn := links[0]
	every := int64(2000)
	if cfg.recEvery > 0 {
		every = cfg.recEvery.Milliseconds()
	}
	for i, l := range links {
		l.bn = bn
		l.recPhase = int64(i) * every / int64(n)
	}
	bn.links = links
	wmSharePeer(links)
	cfg = bn.cfg
	warm := cfg.warm.Milliseconds()
	ci, li := 0, 0
	end := bn.end
	res := wmShared{cfg: cfg, off: make([]float64, n), never: make([]bool, n)}
	left := make([]bool, n) // the core left OFF once
	offMs := make([]int64, n)
	running := make([]int64, n)
	order := make([]*wmModel, n)
	for t := int64(0); t < end; t++ {
		for _, l := range links {
			l.t = t
		}
		for li < len(cfg.lossAt) && t >= cfg.lossAt[li].at.Milliseconds() {
			bn.cfg.loss = cfg.lossAt[li].rate
			bn.lossUnit = 1 - math.Pow(1-bn.cfg.loss, float64(bn.unit/wmMSS))
			li++
		}
		for ci < len(cfg.changes) && t >= cfg.changes[ci].at.Milliseconds() {
			bn.rate = cfg.changes[ci].rate / 1000
			bn.qCap = int64(cfg.changes[ci].rate * cfg.queue.Seconds())
			ci++
		}
		if t == warm {
			for _, l := range links {
				l.at0.segs, l.at0.rtx = l.main.segsOut, l.main.retrans
				l.at0.tail, l.at0.pol, l.at0.rnd = l.tailDrops, l.polDrops, l.randDrops
			}
		}
		// (the order the links' senders reach the queue in turns each ms)
		for k := range links {
			order[k] = links[(int(t)+k)%n]
		}
		for _, l := range order {
			l.atSender()
			l.main.tailTimer()
			l.main.send()
		}
		if bn.comp != nil && t >= cfg.compete.Milliseconds() {
			bn.comp.tailTimer()
			bn.comp.send()
		}
		bn.serve()
		bn.arrive()
		for i, l := range links {
			l.demux()
			l.read()
			l.demux()
			l.periodic()
			l.runCore()
			if l.core != nil && t%100 == 0 && t >= l.rs[0].start {
				running[i] += 100
				if l.core.state != wcOff {
					left[i] = true
				} else if left[i] {
					offMs[i] += 100
				}
			}
		}
		if t >= warm {
			qms := float64(bn.qBytes) / bn.rate
			bn.qSum += qms
			bn.qN++
			bn.qHist[min(int(qms), len(bn.qHist)-1)]++
		}
		if t%1000 == 999 {
			res.qSec = append(res.qSec, float64(bn.qBytes)/bn.rate)
		}
	}
	var inter []float64
	for i, l := range links {
		r := l.result()
		res.links = append(res.links, r)
		if !(i < len(lc) && lc[i].idle) {
			res.mbit += r.mbit
		}
		inter = append(inter, l.inter...)
		var s []float64
		for b := int64(0); b+10 <= int64(len(l.readMs)); b += 10 {
			sum := uint64(0)
			for k := b; k < b+10; k++ {
				sum += l.readMs[k]
			}
			s = append(s, float64(sum)/wmMbit)
		}
		res.sec = append(res.sec, s)
		res.off[i] = float64(offMs[i]) / 1000
		res.never[i] = l.core != nil && running[i] > 0 && !left[i]
		res.trace = append(res.trace, r.trace...)
	}
	if bn.qN > 0 {
		res.qMean = bn.qSum / bn.qN
		res.q99 = histPct(bn.qHist, 0.99)
	}
	if len(inter) > 0 {
		sort.Float64s(inter)
		res.iP50 = inter[len(inter)/2]
		res.iP99 = inter[min(len(inter)-1, len(inter)*99/100)]
	}
	return res
}

// wmRunSharedN runs seeds from cfg.seed on and keeps the median of the
// totals (each run's links stay as they were).
func wmRunSharedN(cfg wmCfg, n int, lc []wmLinkCfg, seeds int) []wmShared {
	out := make([]wmShared, seeds)
	for i := range out {
		c := cfg
		c.seed = cfg.seed + uint64(i)
		out[i] = wmRunShared(c, n, lc)
	}
	return out
}

func wmMedShared(rs []wmShared, get func(wmShared) float64) float64 {
	v := make([]float64, len(rs))
	for i, r := range rs {
		v[i] = get(r)
	}
	sort.Float64s(v)
	return v[len(v)/2]
}

// TestWcoreSharedTrace prints one shared run (HS2_WCORE_SHARED=rate/rtt/queue/links[/mode[/dur s[/late s]]]).
func TestWcoreSharedTrace(t *testing.T) {
	spec := os.Getenv("HS2_WCORE_SHARED")
	if spec == "" {
		t.Skip("HS2_WCORE_SHARED=rate/rtt/queue/links[/mode[/dur s[/late s[/seed]]]]")
	}
	var rate, rtt, q, n, dur, late, seed float64
	mode := "core"
	parts := strings.Split(spec, "/")
	fmt.Sscan(parts[0], &rate)
	fmt.Sscan(parts[1], &rtt)
	fmt.Sscan(parts[2], &q)
	fmt.Sscan(parts[3], &n)
	if len(parts) > 4 {
		mode = parts[4]
	}
	dur = 60
	if len(parts) > 5 {
		fmt.Sscan(parts[5], &dur)
	}
	if len(parts) > 6 {
		fmt.Sscan(parts[6], &late)
	}
	seed = 1
	if len(parts) > 7 {
		fmt.Sscan(parts[7], &seed)
	}
	c := wmPath(rate, time.Duration(rtt)*time.Millisecond, time.Duration(q)*time.Millisecond)
	c.dur, c.seed = time.Duration(dur)*time.Second, uint64(seed)
	if v := os.Getenv("HS2_WCORE_SHARED_TRACE"); v != "" {
		c.trace = true
		fmt.Sscan(v, &wmTraceLink)
	}
	for i, s := range []string{"base", "n4", "fin", "core"} {
		if s == mode {
			c.mode = wmMode(i)
		}
	}
	var lc []wmLinkCfg
	if late > 0 {
		lc = make([]wmLinkCfg, int(n))
		lc[int(n)-1].start = time.Duration(late) * time.Second
	}
	r := wmRunShared(c, int(n), lc)
	for _, l := range r.trace {
		t.Log(l)
	}
	t.Log(r)
	for s := 0; s < int(dur); s += 5 {
		var b strings.Builder
		for i := range r.links {
			fmt.Fprintf(&b, " %5.0f", r.rateFrom(i, s, s+5))
		}
		qm := 0.0
		for k := s; k < s+5 && k < len(r.qSec); k++ {
			qm += r.qSec[k] / 5
		}
		t.Logf("%3ds q%5.1f |%s", s, qm, b.String())
	}
}

// wmSharePeer gives the links' cores what the runner's registry shares
// between the links to one peer.
func wmSharePeer(links []*wmModel) {
	if wmNoPeer {
		return
	}
	p := newWcPeer()
	for _, l := range links {
		if l.core != nil {
			l.core.peer = p
		}
	}
}

// wmTraceLink: the link a shared run traces
var wmTraceLink = 0

// wmNoPeer: the links' cores share nothing (as before the registry did)
var wmNoPeer = false

// wmSharedCell is a cell of the shared-bottleneck table.
type wmSharedCell struct {
	name string
	cfg  wmCfg
	n    int
	lc   []wmLinkCfg
	// oldMbit: the core before the fix (229f682), median of three seeds:
	// the total the fix must keep 80% of
	oldMbit float64
}

func wmSharedCells() []wmSharedCell {
	p := func(rate float64, rtt, q int) wmCfg {
		c := wmPath(rate, wmMS(rtt), wmMS(q))
		c.dur, c.warm = 120*time.Second, 20*time.Second
		return c
	}
	late := make([]wmLinkCfg, 4)
	late[3].start = 30 * time.Second
	return []wmSharedCell{
		{"1000/100 q200 x2", p(1000, 100, 200), 2, nil, 843.0},
		{"1000/100 q200 x4", p(1000, 100, 200), 4, nil, 853.2},
		{"1000/100 q50 x4", p(1000, 100, 50), 4, nil, 840.6},
		{"1000/100 q200 x4 late", p(1000, 100, 200), 4, late, 850.4},
		{"300/150 q50 x2", p(300, 150, 50), 2, nil, 218.7},
	}
}

// TestWcoreSharedBottleneck: several links' cores on one bottleneck hold its
// queue near qTarget, not n×qTarget, and share it fairly (red team, round 3,
// finding 1). Each cell is three seeds of base and the core; per seed:
//   - the bottleneck's mean queue at most qTarget + Δq (25 ms at 100 ms),
//     over the run and over its last 40 s (no creep);
//   - the interactive replies on the links within 1.3× base (p50) and
//     1.5× base (p99);
//   - every core that had a download left OFF, and from 60 s on the links'
//     download rates within 2× of each other;
//   - and the total at least 80% of the core's before the fix.
func TestWcoreSharedBottleneck(t *testing.T) {
	const seeds = 3
	for _, cell := range wmSharedCells() {
		bs := wmRunSharedN(cell.cfg.with(wmBase), cell.n, cell.lc, seeds)
		ks := wmRunSharedN(cell.cfg.with(wmCore), cell.n, cell.lc, seeds)
		bP50 := wmMedShared(bs, func(r wmShared) float64 { return r.iP50 })
		bP99 := wmMedShared(bs, func(r wmShared) float64 { return r.iP99 })
		dq := max(wcDqMin, time.Duration(float64(cell.cfg.rtt)*wcDqShare))
		lim := (wcQTarget + dq).Seconds() * 1000
		for i, k := range ks {
			t.Logf("%s seed %d\n\tbase %v\n\tcore %v", cell.name, i, bs[i], k)
			last := 0.0
			for s := len(k.qSec) - 40; s < len(k.qSec); s++ {
				last += k.qSec[s] / 40
			}
			if k.qMean > lim || last > lim {
				t.Errorf("%s seed %d: queue %.1f ms (last 40 s %.1f), want ≤ %.0f", cell.name, i, k.qMean, last, lim)
			}
			if k.iP50 > 1.3*bP50 || k.iP99 > 1.5*bP99 {
				t.Errorf("%s seed %d: interactive %.0f/%.0f ms, base %.0f/%.0f", cell.name, i, k.iP50, k.iP99, bP50, bP99)
			}
			for j, nv := range k.never {
				if nv {
					t.Errorf("%s seed %d: link %d's core never left OFF", cell.name, i, j)
				}
			}
			if sp := k.spread(cell.lc, 60, 120); sp > 2 {
				t.Errorf("%s seed %d: download rates from 60 s %.1f× apart", cell.name, i, sp)
			}
			if cell.oldMbit > 0 && k.mbit < 0.8*cell.oldMbit {
				t.Errorf("%s seed %d: %.0f Mbit/s, under 80%% of the core's before (%.0f)", cell.name, i, k.mbit, cell.oldMbit)
			}
		}
	}
}
