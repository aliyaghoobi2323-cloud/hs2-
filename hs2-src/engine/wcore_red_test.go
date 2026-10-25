package engine

import (
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// Red team, round 1 (Phase Z5, part B: the controller and the runner).
// Helpers and attack scenarios on the link model (wcore_model_test.go).

// wmLossWins is the sender's resend share over consecutive windows of win
// seconds (the edge's loss rule judges the download per pong window, ~3 s;
// the upload per 2 s health tick), from the model's per-second series; a
// window that moved less than activeBytes a tick is quiet (-1): the loss
// rule neither judges it nor ends a streak on it.
func wmLossWins(r wmResult, win int) []float64 {
	var out []float64
	for i := win; i < len(r.rtxSec); i += win {
		a, b := r.rtxSec[i-win], r.rtxSec[i]
		segs := b[1] - a[1]
		if float64(segs*wmMSS) < float64(activeBytes)*float64(win)/healthTick.Seconds() {
			out = append(out, -1)
			continue
		}
		out = append(out, float64(b[0]-a[0])/float64(segs))
	}
	return out
}

// wmLossStreak is the longest run of windows past frac (the loss rule's
// lossFrac, degradeStreak of them in a row drain the link), quiet windows
// skipped.
func wmLossStreak(v []float64, frac float64) (streak int, worst float64) {
	n := 0
	for _, x := range v {
		if x < 0 {
			continue
		}
		worst = max(worst, x)
		if x > frac {
			n++
			streak = max(streak, n)
		} else {
			n = 0
		}
	}
	return streak, worst
}

// wmRedUpload sets the lab knob WcoreUpload for a red-team sweep run with
// HS2_WCORE_RED_UPLOAD=1, so its "up" side runs the exit's core; by default
// (step 1) the exit has none and that side is base (WINDOW_CORE §2.2).
func wmRedUpload(t *testing.T) {
	if os.Getenv("HS2_WCORE_RED_UPLOAD") != "1" {
		return
	}
	up := WcoreUpload
	WcoreUpload = 1
	t.Cleanup(func() { WcoreUpload = up })
}

func wmLossNote(r wmResult) string {
	var parts []string
	for _, w := range []int{2, 3} {
		s, worst := wmLossStreak(wmLossWins(r, w), lossFrac)
		parts = append(parts, fmt.Sprintf("%ds:streak%d worst%.1f%%", w, s, 100*worst))
	}
	return strings.Join(parts, " ")
}

// wmRedLossRule: on each scenario, would the loss rule (lossFrac over
// degradeStreak samples in a row) drain a link the core runs on where it
// would not drain the same link without the core? Report with
// HS2_WCORE_RED=1; the scenarios that fail it are their own tests below.
// The "up" side is base unless HS2_WCORE_RED_UPLOAD=1 (wmRedUpload).
func TestWcoreRedLossRuleSweep(t *testing.T) {
	if os.Getenv("HS2_WCORE_RED") != "1" {
		t.Skip("HS2_WCORE_RED=1")
	}
	wmRedUpload(t)
	type sc struct {
		c wmCfg
	}
	var cs []wmCfg
	add := func(c wmCfg, name string) {
		c.name = name
		cs = append(cs, c)
	}
	for _, exit := range []bool{false, true} {
		side := "dn"
		if exit {
			side = "up"
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
			{rate: 300, rtt: 150, q: 50},
			{rate: 300, rtt: 150, q: 20},
			{rate: 300, rtt: 150, q: 10},
			{rate: 1000, rtt: 100, q: 10},
			{rate: 1000, rtt: 100, q: 50, loss: 0.001},
			{rate: 1000, rtt: 100, q: 20, loss: 0.001},
			{rate: 300, rtt: 150, q: 20, loss: 0.001},
			{rate: 300, rtt: 150, q: 20, loss: 0.003},
			{rate: 300, rtt: 150, q: 50, loss: 0.01},
			{rate: 100, rtt: 100, q: 50},
			{rate: 100, rtt: 150, q: 50},
			{rate: 1000, rtt: 100, q: 50, policer: 300},
			{rate: 1000, rtt: 100, q: 50, policer: 150},
			{rate: 1000, rtt: 150, q: 50, policer: 200},
			{rate: 300, rtt: 150, q: 50, policer: 150},
			{rate: 300, rtt: 150, q: 50, lossAt: []wmChange{{12 * time.Second, 0.02}}, extraTag: " loss0>2%@12s"},
			{rate: 300, rtt: 150, q: 50, lossAt: []wmChange{{12 * time.Second, 0.05}}, extraTag: " loss0>5%@12s"},
			{rate: 1000, rtt: 100, q: 50, lossAt: []wmChange{{12 * time.Second, 0.02}}, extraTag: " loss0>2%@12s"},
			{rate: 300, rtt: 150, q: 50, changes: []wmChange{{12 * time.Second, 50 * wmMbit}, {22 * time.Second, 300 * wmMbit}}, extraTag: " 300>50>300"},
			{rate: 1000, rtt: 100, q: 20, changes: []wmChange{{12 * time.Second, 100 * wmMbit}}, extraTag: " 1000>100"},
			{rate: 300, rtt: 150, q: 50, n: 4},
			{rate: 1000, rtt: 100, q: 20, n: 4},
		} {
			c := wmPath(p.rate, wmMS(p.rtt), wmMS(p.q))
			c.loss, c.lossAt, c.changes, c.exit = p.loss, p.lossAt, p.changes, exit
			c.policer = p.policer * wmMbit
			if p.n > 1 {
				c.streams = make([]wmStreamCfg, p.n)
			}
			c.dur = 40 * time.Second
			name := fmt.Sprintf("%s %.0f/%d q%d", side, p.rate, p.rtt, p.q)
			if p.loss > 0 {
				name += fmt.Sprintf(" l%g", 100*p.loss)
			}
			if p.policer > 0 {
				name += fmt.Sprintf(" pol%.0f", p.policer)
			}
			if p.n > 1 {
				name += fmt.Sprintf(" x%d", p.n)
			}
			add(c, name+p.extraTag)
		}
	}
	seeds := 3
	for _, c := range cs {
		for s := 0; s < seeds; s++ {
			c.seed = uint64(1 + s)
			b, k := wmRun(c.with(wmBase)), wmRun(c.with(wmCore))
			bs, _ := wmLossStreak(wmLossWins(b, 3), lossFrac)
			ks, _ := wmLossStreak(wmLossWins(k, 3), lossFrac)
			bs2, _ := wmLossStreak(wmLossWins(b, 2), lossFrac)
			ks2, _ := wmLossStreak(wmLossWins(k, 2), lossFrac)
			flag := ""
			if (ks >= degradeStreak && bs < degradeStreak) || (ks2 >= degradeStreak && bs2 < degradeStreak) {
				flag = "  <<< DRAINED BY THE CORE"
			}
			t.Logf("%-36s s%d base %6.1f Mbit rs %5.2f%% [%s] | core %6.1f Mbit rs %5.2f%% [%s] d%v p%v B%.1f%s",
				c.name, c.seed, b.mbit, 100*b.resend, wmLossNote(b), k.mbit, 100*k.resend, wmLossNote(k), k.drains, k.probes, float64(k.peakB)/(1<<20), flag)
		}
	}
}

// TestWcoreRedTrace traces one red-team scenario:
// HS2_WCORE_RED_TRACE=rate/rtt/queue/loss%/seed/exit[/policerMbit[/streams]]
func TestWcoreRedTrace(t *testing.T) {
	spec := os.Getenv("HS2_WCORE_RED_TRACE")
	if spec == "" {
		t.Skip("HS2_WCORE_RED_TRACE=rate/rtt/queue/loss%/seed/exit[/policer[/streams]]")
	}
	var rate, rtt, q, loss, pol float64
	var seed, exit, n int
	p := strings.Split(spec, "/")
	fmt.Sscan(p[0], &rate)
	fmt.Sscan(p[1], &rtt)
	fmt.Sscan(p[2], &q)
	fmt.Sscan(p[3], &loss)
	fmt.Sscan(p[4], &seed)
	fmt.Sscan(p[5], &exit)
	if len(p) > 6 {
		fmt.Sscan(p[6], &pol)
	}
	if len(p) > 7 {
		fmt.Sscan(p[7], &n)
	}
	if exit == 1 {
		// (the exit's core runs only with the lab knob)
		up := WcoreUpload
		WcoreUpload = 1
		t.Cleanup(func() { WcoreUpload = up })
	}
	c := wmPath(rate, time.Duration(rtt)*time.Millisecond, time.Duration(q)*time.Millisecond)
	c.loss, c.seed, c.exit, c.policer, c.trace, c.dur = loss/100, uint64(seed), exit == 1, pol*wmMbit, true, 40*time.Second
	if rtt >= 1000 {
		c.dur, c.warm = 90*time.Second, 20*time.Second
	}
	if n > 1 {
		c.streams = make([]wmStreamCfg, n)
	}
	r := wmRun(c.with(wmCore))
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
}

// TestWcoreRedGrid: a grid of paths, each side, base against the core: how
// often the core takes a link the loss rule would leave alone into
// degradeStreak windows past lossFrac (HS2_WCORE_RED=1; HS2_WCORE_RED_SIDE
// up|dn|old|both). Since step 1's decision the "up" side (the exit's core)
// and "old" (an edge with an old exit) are base: the former runs the core
// only with HS2_WCORE_RED_UPLOAD=1 (wmRedUpload), the latter never leaves
// OFF (before: 8 of 96 runs each drained behind a policer at 60%).
func TestWcoreRedGrid(t *testing.T) {
	if os.Getenv("HS2_WCORE_RED") != "1" {
		t.Skip("HS2_WCORE_RED=1")
	}
	wmRedUpload(t)
	sides := []bool{false, true}
	old := false
	switch os.Getenv("HS2_WCORE_RED_SIDE") {
	case "up":
		sides = []bool{true}
	case "dn":
		sides = []bool{false}
	case "old":
		sides, old = []bool{false}, true
	}
	type key struct {
		exit bool
		loss float64
		pol  bool
	}
	bad, tot := map[key]int{}, map[key]int{}
	gain := map[key][2]float64{}
	for _, exit := range sides {
		for _, rate := range []float64{100, 300, 1000} {
			for _, rtt := range []int{50, 100, 150, 300} {
				for _, q := range []int{10, 20, 50, 100} {
					for _, loss := range []float64{0, 0.0005, 0.001, 0.003, 0.01, -0.3, -0.6} {
						for seed := 1; seed <= 2; seed++ {
							c := wmPath(rate, wmMS(rtt), wmMS(q))
							if loss < 0 {
								c.policer = -loss * rate * wmMbit
							} else {
								c.loss = loss
							}
							c.exit, c.seed, c.dur, c.oldExit = exit, uint64(seed), 40*time.Second, old
							b, k := wmRun(c.with(wmBase)), wmRun(c.with(wmCore))
							kk := key{exit, loss, loss < 0}
							tot[kk]++
							g := gain[kk]
							g[0] += b.mbit
							g[1] += k.mbit
							gain[kk] = g
							bs, _ := wmLossStreak(wmLossWins(b, 2), lossFrac)
							ks, _ := wmLossStreak(wmLossWins(k, 2), lossFrac)
							bs3, _ := wmLossStreak(wmLossWins(b, 3), lossFrac)
							ks3, _ := wmLossStreak(wmLossWins(k, 3), lossFrac)
							if (ks >= degradeStreak && bs < degradeStreak) || (ks3 >= degradeStreak && bs3 < degradeStreak) {
								bad[kk]++
								side := "dn"
								if exit {
									side = "up"
								} else if old {
									side = "dn-old"
								}
								t.Logf("DRAINED %s %.0f/%d q%d loss%g pol%g s%d: base %.0f Mbit rs %.2f%% [%s] core %.0f Mbit rs %.2f%% [%s] d%v p%v B%.1f",
									side, rate, rtt, q, max(loss, 0)*100, max(-loss, 0), seed, b.mbit, 100*b.resend, wmLossNote(b), k.mbit, 100*k.resend, wmLossNote(k), k.drains, k.probes, float64(k.peakB)/(1<<20))
							}
						}
					}
				}
			}
		}
	}
	for k, n := range tot {
		t.Logf("exit=%v loss=%g: %d of %d runs drained by the core; mean base %.0f core %.0f Mbit", k.exit, k.loss, bad[k], n, gain[k][0]/float64(n), gain[k][1]/float64(n))
	}
}

// wmDrainedByCore says whether the loss rule (lossFrac over degradeStreak
// samples in a row, 2 s ticks for uploads, ~3 s pong windows for downloads)
// would drain the link under the core but not without it.
func wmDrainedByCore(b, k wmResult) bool {
	for _, w := range []int{2, 3} {
		bs, _ := wmLossStreak(wmLossWins(b, w), lossFrac)
		ks, _ := wmLossStreak(wmLossWins(k, w), lossFrac)
		if ks >= degradeStreak && bs < degradeStreak {
			return true
		}
	}
	return false
}

// TestWcoreRedBlindHoley (finding 1): where packets wait behind holes
// (Rcv_ooopack past wcOooClean) and no record of the sender's socket comes
// (the exit's core for uploads; the edge's with an old exit), the core has
// no loss signal (ooo is no congestion signal there) and its only queue
// signal is the link socket's own srtt, which reads the queue's floor: its
// probes "paid" and it grew the windows into a queue they overflowed, for
// as long as it ran: resends past lossFrac for degradeStreak samples in a
// row, which the loss rule drains the link for, where the windows without
// the core resent 0.05-2%. Since step 1's decision the exit's core runs
// only with the lab knob (WcoreUpload, set here for its cases) and an edge
// with an old exit stays OFF: its cases are base.
func TestWcoreRedBlindHoley(t *testing.T) {
	up := WcoreUpload
	t.Cleanup(func() { WcoreUpload = up })
	cases := []struct {
		rate      float64
		rtt, q    int
		loss      float64
		seed      uint64
		exit, old bool
	}{
		{300, 150, 20, 0.001, 3, true, false},
		{300, 100, 20, 0, 2, true, false},
		{1000, 100, 10, 0.0005, 1, true, false},
		{300, 150, 20, 0.0005, 1, true, false},
		{300, 100, 20, 0.0005, 2, false, true},
		{300, 150, 50, 0.001, 1, false, true},
		{1000, 100, 10, 0.0005, 2, false, true},
	}
	for _, x := range cases {
		c := wmPath(x.rate, wmMS(x.rtt), wmMS(x.q))
		c.loss, c.seed, c.exit, c.oldExit, c.dur = x.loss, x.seed, x.exit, x.old, 40*time.Second
		WcoreUpload = 0
		if x.exit {
			WcoreUpload = 1
		}
		b, k := wmRun(c.with(wmBase)), wmRun(c.with(wmCore))
		side := map[bool]string{true: "up", false: "dn-old"}[x.exit]
		msg := fmt.Sprintf("%s %s l%g s%d: base %.0f Mbit resent %.2f%% [%s]; core %.0f Mbit resent %.2f%% [%s] drains %v probes %v",
			side, c.name, 100*x.loss, x.seed, b.mbit, 100*b.resend, wmLossNote(b), k.mbit, 100*k.resend, wmLossNote(k), k.drains, k.probes)
		if wmDrainedByCore(b, k) {
			t.Errorf("the loss rule would drain the link under the core: %s", msg)
		} else if x.old && (k.mbit != b.mbit || k.resend != b.resend || k.peakB != 0) {
			t.Errorf("an edge with an old exit is not base: %s", msg)
		} else {
			t.Log(msg)
		}
	}
}

// TestWcoreRedBlindOff: a core that cannot tell its own losses (blind) stays
// OFF and leaves for OFF: the windows as without it.
func TestWcoreRedBlindOff(t *testing.T) {
	c := newWcore(wcConfig{side: wcExit, stage: 1, need: wtNeed, maxWin: wtMax, minWin: wtMin}, func() float64 { return 0.5 })
	c.tBase = 100 * time.Millisecond
	m := wtMeas(1, 60e6, 2*wtMiB)
	m.ooo, m.oooOK, m.holey = 0.5, true, true
	if !c.harm(m) {
		t.Errorf("blind (holes, no record of the sender): harm false, want the core to stay OFF")
	}
	c.sndSeen = true
	if c.harm(m) {
		t.Errorf("holes with the sender's record: harm true, want the core free to start")
	}
	// in HOLD, wcOffAfter blind evaluations in a row: OFF
	c = newWcore(wcConfig{side: wcExit, stage: 1, need: wtNeed, maxWin: wtMax, minWin: wtMin}, func() float64 { return 0.5 })
	c.state, c.B, c.bPub, c.tBase = wcHold, 4*wtMiB, 4*wtMiB, 100*time.Millisecond
	c.oooRef = wcLossRef{v: 0.5, b: 4 * wtMiB, ok: true}
	now := wtT0
	for i := 0; i < 2*wcOffAfter && c.state != wcOff; i++ {
		in := wtIn(now, 1, 4*wtMiB)
		in.streams[0].read = 6e6
		in.streams[0].turnMin, in.streams[0].turnLast = 150*time.Millisecond, 150*time.Millisecond
		in.segsIn, in.ooo, in.oooOK, in.srtt, in.minRTT = 4000, 2000, true, 102*time.Millisecond, 100*time.Millisecond
		wtInF(c, in)
		c.evalAt = time.Time{}
		c.step(in)
		now = now.Add(time.Second)
	}
	if c.state != wcOff {
		t.Errorf("blind HOLD: %v after %d evaluations, want off", c.state, 2*wcOffAfter)
	}
}

// wtRedIn is an interval of the HOLD core of wtHold through step: its one
// fast stream reading at 60 MB/s, a clean socket, and (snd) the exit's record.
func wtRedIn(now time.Time, dt time.Duration, snd wcSnd) wcIn {
	in := wtIn(now, 1, 8*wtMiB)
	in.streams[0].read = uint64(60e6 * dt.Seconds())
	in.streams[0].turnMin, in.streams[0].turnLast = 105*time.Millisecond, 105*time.Millisecond
	in.segsIn, in.oooOK, in.srtt, in.minRTT = 4000, true, 101*time.Millisecond, 100*time.Millisecond
	in.linkBytes = in.streams[0].read
	in.snd = snd
	return in
}

// TestWcoreRedSettleDropsRecord (finding 2): the exit's record is the edge's
// only unbiased loss signal and comes once per ~2 s. One that lands in the
// settle part of a HOLD interval (each toHold, and each change of B by more
// than wcResettle) is merged into that interval and dropped with it:
// settled() judges loss in PROBE and STARTUP only. A record of 10% resent
// against a level of 0.01% at the budget in force was never judged.
func TestWcoreRedSettleDropsRecord(t *testing.T) {
	for _, settling := range []bool{false, true} {
		c, _ := wtHold()
		c.lastAt, c.settling = wtT0, settling
		lossy := wcSnd{fresh: true, srtt: 101 * time.Millisecond, minRTT: 100 * time.Millisecond, retrans: 1000, segsOut: 10000}
		now := wtT0.Add(400 * time.Millisecond)
		c.step(wtRedIn(now, 400*time.Millisecond, lossy))
		judged := func() bool { return c.lossTest != wcSrcNone || c.cnt.drainLoss > 0 || c.state == wcDrain }
		for i := 0; i < 3 && !judged(); i++ {
			c.evalAt = time.Time{}
			now = now.Add(400 * time.Millisecond)
			c.step(wtRedIn(now, 400*time.Millisecond, wcSnd{}))
		}
		if !judged() {
			t.Errorf("settling %v: a record of 10%% resent (level 0.01%%) was never judged: %v B %.1f MiB (from 8)", settling, c.state, float64(c.B)/wtMiB)
		} else {
			t.Logf("settling %v: judged: %v B %.1f MiB lossTest %d drains %d", settling, c.state, float64(c.B)/wtMiB, c.lossTest, c.cnt.drainLoss)
		}
	}
}

// BenchmarkWcoreRedStep: one runner wake of an active core (HOLD) with n
// large streams: the cost per link per 100 ms.
func BenchmarkWcoreRedStep(b *testing.B) {
	for _, n := range []int{4, 64, 1000} {
		b.Run(fmt.Sprintf("streams=%d", n), func(b *testing.B) {
			c := newWcore(wcConfig{side: wcEdge, stage: 1, need: wtNeed, maxWin: wtMax, minWin: wtMin}, func() float64 { return 0.5 })
			now := wtT0
			mk := func() wcIn {
				in := wcIn{now: now, minRTT: 100 * time.Millisecond, srtt: 102 * time.Millisecond, segsIn: 1000, oooOK: true, linkBytes: uint64(n) * 100000, poolFree: -1}
				for i := 0; i < n; i++ {
					in.streams = append(in.streams, wcStream{id: uint32(2*i + 1), win: 3 * wtMiB, upd: 2, hit: 1, read: 100000,
						turnMin: 101 * time.Millisecond, turnLast: 103 * time.Millisecond})
				}
				return in
			}
			// in HOLD with every stream in F
			c.state, c.B, c.bPub, c.bGood, c.tBase = wcHold, uint64(n)*3*wtMiB, uint64(n)*3*wtMiB, uint64(n)*3*wtMiB, 100*time.Millisecond
			c.sndSeen, c.lastAt = true, now
			c.noteRate(now, float64(n)*1e6)
			c.probeAt, c.refreshAt = now.Add(time.Hour), now.Add(time.Hour)
			for i := 0; i < n; i++ {
				c.inF[uint32(2*i+1)] = true
			}
			for i := 0; i < 40; i++ {
				now = now.Add(100 * time.Millisecond)
				c.step(mk())
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				now = now.Add(100 * time.Millisecond)
				c.step(mk())
			}
			b.StopTimer()
			b.ReportMetric(float64(len(c.inF)), "nF")
			b.Logf("state %v B %.1f MiB nF %d", c.state, float64(c.B)/wtMiB, len(c.inF))
		})
	}
}

// TestWcoreRedMinRTTUnset (finding 5): before a socket's first round-trip
// sample Linux reports tcpi_min_rtt as ~0U; read as 71 minutes it became
// T_base, and with it Δq (7 minutes), q_ctrl's threshold and the knee: no
// queue signal could ever fire.
func TestWcoreRedMinRTTUnset(t *testing.T) {
	if d := wcMinRTTOf(^uint32(0)); d != 0 {
		t.Errorf("min_rtt ~0U read as %v, want 0 (unknown)", d)
	}
	if d := wcMinRTTOf(150000); d != 150*time.Millisecond {
		t.Errorf("min_rtt 150000 µs read as %v", d)
	}
	c := wtCore(1)
	c.tBase = 0
	in := wtIn(wtT0, 1, 2*wtMiB)
	in.minRTT, in.srtt = wcMinRTTOf(^uint32(0)), 100*time.Millisecond
	c.notePaths(in, new([]wcEvent))
	if c.tBase > time.Minute {
		t.Errorf("T_base %v from a socket with no round-trip sample", c.tBase)
	}
}

// wmMbitOver is the link's goodput over [from, to).
func wmMbitOver(r wmResult, m *wmModel, from, to time.Duration) float64 {
	var sum uint64
	for b := from.Milliseconds() / 100; b < to.Milliseconds()/100 && b < int64(len(m.readMs)); b++ {
		sum += m.readMs[b]
	}
	return float64(sum) / (to - from).Seconds() / wmMbit
}

// TestWcoreRedApProbingSweep: the autopilot probes the pool size (apProbing)
// for 20 s in the middle of a transfer: what the link carries before,
// during and after, against a run with no probe (HS2_WCORE_RED=1).
func TestWcoreRedApProbingSweep(t *testing.T) {
	if os.Getenv("HS2_WCORE_RED") != "1" {
		t.Skip("HS2_WCORE_RED=1")
	}
	for _, p := range []struct {
		rate   float64
		rtt, q int
		loss   float64
	}{{300, 150, 50, 0}, {1000, 100, 50, 0}, {1000, 150, 50, 0}, {300, 100, 50, 0}, {1000, 100, 50, 0.001}, {300, 150, 200, 0}} {
		for seed := uint64(1); seed <= 3; seed++ {
			c := wmPath(p.rate, wmMS(p.rtt), wmMS(p.q))
			c.loss, c.seed, c.dur = p.loss, seed, 45*time.Second
			c.mode = wmCore
			m0 := newModel(c)
			r0 := m0.run()
			c.apProbing = func(at time.Duration) bool { return at >= 15*time.Second && at < 35*time.Second }
			m1 := newModel(c)
			r1 := m1.run()
			t.Logf("%s l%g s%d: before %.0f/%.0f | probe 15-35s %.0f vs %.0f (%.0f%%) | 20-35s %.0f vs %.0f (%.0f%%) | after %.0f/%.0f",
				c.name, 100*p.loss, seed,
				wmMbitOver(r0, m0, 8*time.Second, 15*time.Second), wmMbitOver(r1, m1, 8*time.Second, 15*time.Second),
				wmMbitOver(r1, m1, 15*time.Second, 35*time.Second), wmMbitOver(r0, m0, 15*time.Second, 35*time.Second),
				100*wmMbitOver(r1, m1, 15*time.Second, 35*time.Second)/wmMbitOver(r0, m0, 15*time.Second, 35*time.Second),
				wmMbitOver(r1, m1, 20*time.Second, 35*time.Second), wmMbitOver(r0, m0, 20*time.Second, 35*time.Second),
				100*wmMbitOver(r1, m1, 20*time.Second, 35*time.Second)/wmMbitOver(r0, m0, 20*time.Second, 35*time.Second),
				wmMbitOver(r0, m0, 35*time.Second, 45*time.Second), wmMbitOver(r1, m1, 35*time.Second, 45*time.Second))
		}
	}
}

// TestWcoreRedApTrace: HS2_WCORE_RED_APTRACE=rate/rtt/queue/seed traces an
// apProbing spell from 15 to 35 s.
func TestWcoreRedApTrace(t *testing.T) {
	spec := os.Getenv("HS2_WCORE_RED_APTRACE")
	if spec == "" {
		t.Skip("HS2_WCORE_RED_APTRACE=rate/rtt/queue/seed")
	}
	var rate, rtt, q float64
	var seed int
	p := strings.Split(spec, "/")
	fmt.Sscan(p[0], &rate)
	fmt.Sscan(p[1], &rtt)
	fmt.Sscan(p[2], &q)
	fmt.Sscan(p[3], &seed)
	c := wmPath(rate, time.Duration(rtt)*time.Millisecond, time.Duration(q)*time.Millisecond)
	c.seed, c.trace, c.dur, c.mode = uint64(seed), true, 45*time.Second, wmCore
	c.apProbing = func(at time.Duration) bool { return at >= 15*time.Second && at < 35*time.Second }
	r := wmRun(c)
	for _, l := range r.trace {
		t.Log(l)
	}
	t.Log(r)
}

// TestWcoreRedApProbingFreeze (finding 3): while the autopilot probes the
// pool size (apProbing) the core starts no PROBE and no STARTUP, so its
// budget stays within what was shown to pay; it also held every window's
// growth (growOK false), so a drain, a cut (the sender's PROBE_RTT burst) or
// a refresh in the spell kept the windows down for all of it: the link
// carried 16-26% of what it did without the spell (1000/100, 1000/150 behind
// 50 ms), which is what the autopilot measured its probe against.
func TestWcoreRedApProbingFreeze(t *testing.T) {
	for _, x := range []struct {
		rate float64
		rtt  int
		seed uint64
	}{{1000, 100, 2}, {1000, 100, 3}, {1000, 150, 2}, {300, 150, 2}} {
		c := wmPath(x.rate, wmMS(x.rtt), 50*time.Millisecond)
		c.seed, c.dur, c.mode = x.seed, 45*time.Second, wmCore
		m0 := newModel(c)
		r0 := m0.run()
		c.apProbing = func(at time.Duration) bool { return at >= 15*time.Second && at < 35*time.Second }
		m1 := newModel(c)
		r1 := m1.run()
		with, without := wmMbitOver(r1, m1, 15*time.Second, 35*time.Second), wmMbitOver(r0, m0, 15*time.Second, 35*time.Second)
		msg := fmt.Sprintf("%s s%d: %.0f Mbit/s while the autopilot probed, %.0f without", c.name, x.seed, with, without)
		if with < 0.7*without {
			t.Errorf("%s", msg)
		} else {
			t.Log(msg)
		}
	}
}

// TestWcoreRedExtremes: base against the core on round trips far from the
// table's (LAN, satellite, DPI-delayed) and with transfers that come and go
// (HS2_WCORE_RED=1).
func TestWcoreRedExtremes(t *testing.T) {
	if os.Getenv("HS2_WCORE_RED") != "1" {
		t.Skip("HS2_WCORE_RED=1")
	}
	type ex struct {
		rate   float64
		rtt, q int
		loss   float64
		n      int
		churn  bool
	}
	for _, x := range []ex{
		{1000, 2, 5, 0, 1, false}, {1000, 4, 10, 0, 4, false}, {100, 2, 20, 0, 1, false},
		{300, 1000, 50, 0, 1, false}, {100, 2000, 100, 0, 1, false}, {50, 1500, 200, 0.001, 1, false},
		{1000, 800, 50, 0, 1, false}, {300, 150, 50, 0, 6, true}, {1000, 100, 50, 0, 6, true},
	} {
		for seed := uint64(1); seed <= 2; seed++ {
			c := wmPath(x.rate, wmMS(x.rtt), wmMS(x.q))
			c.loss, c.seed, c.dur = x.loss, seed, 40*time.Second
			if x.rtt >= 1000 {
				c.dur, c.warm = 90*time.Second, 20*time.Second
			}
			if x.n > 1 {
				c.streams = make([]wmStreamCfg, x.n)
				if x.churn {
					// downloads that start a few seconds apart, each with
					// a source that stops: F changes every few intervals
					for i := range c.streams {
						c.streams[i].start = time.Duration(i) * 3 * time.Second
						c.streams[i].src = x.rate * wmMbit / float64(x.n) * float64(1+i%3)
					}
				}
			}
			b, k := wmRun(c.with(wmBase)), wmRun(c.with(wmCore))
			flag := ""
			if wmDrainedByCore(b, k) {
				flag = " <<< DRAINED"
			}
			if k.mbit < 0.9*b.mbit {
				flag += " <<< SLOWER"
			}
			t.Logf("%-22s n%d churn%v s%d: base %6.1f Mbit rs %5.2f%% | core %6.1f Mbit rs %5.2f%% q %.0f/%.0f d%v p%v B%.1f last %v%s",
				c.name, x.n, x.churn, seed, b.mbit, 100*b.resend, k.mbit, 100*k.resend, k.qMean, k.q99, k.drains, k.probes, float64(k.peakB)/(1<<20), k.lastState, flag)
		}
	}
}

// TestWcoreRedLongRTTSweep: base against the core on round trips of 600 ms
// to 2 s (DPI-held paths, satellite): HS2_WCORE_RED=1.
func TestWcoreRedLongRTTSweep(t *testing.T) {
	if os.Getenv("HS2_WCORE_RED") != "1" {
		t.Skip("HS2_WCORE_RED=1")
	}
	bad, n := 0, 0
	var sb, sk float64
	for _, rate := range []float64{50, 100, 300, 1000} {
		for _, rtt := range wmRedRTTs() {
			for _, q := range []int{50, 100, 200} {
				for seed := uint64(1); seed <= 2; seed++ {
					c := wmPath(rate, wmMS(rtt), wmMS(q))
					c.seed, c.dur, c.warm = seed, 90*time.Second, 20*time.Second
					b, k := wmRun(c.with(wmBase)), wmRun(c.with(wmCore))
					n++
					sb += b.mbit
					sk += k.mbit
					if wmDrainedByCore(b, k) || k.resend > b.resend+0.03 {
						bad++
						t.Logf("BAD %s s%d: base %.1f Mbit rs %.2f%% | core %.1f Mbit rs %.2f%% [%s] d%v p%v B%.1f",
							c.name, seed, b.mbit, 100*b.resend, k.mbit, 100*k.resend, wmLossNote(k), k.drains, k.probes, float64(k.peakB)/(1<<20))
					}
				}
			}
		}
	}
	t.Logf("%d of %d runs drained by the core or past base + 3 points resent; mean base %.1f core %.1f Mbit", bad, n, sb/float64(n), sk/float64(n))
}

// wmRedRTTs: the long round trips TestWcoreRedLongRTTSweep runs
// (HS2_WCORE_RED_RTTS=300,400,500 to change them).
func wmRedRTTs() []int {
	v := []int{600, 800, 1000, 1500, 2000}
	if s := os.Getenv("HS2_WCORE_RED_RTTS"); s != "" {
		v = nil
		for _, f := range strings.Split(s, ",") {
			var x int
			fmt.Sscan(f, &x)
			v = append(v, x)
		}
	}
	return v
}

// TestWcoreRedGarbage: the controller fed hostile inputs (round trips of 0
// to 71 minutes, counters that jump, ooo past segs_in, a record of the exit
// with srtt under min_rtt or resends past its segments, the clock stepping
// back or a day forward, thousands of streams appearing and leaving): no
// panic, no NaN, and the published budget and ceilings within their
// bounds.
func TestWcoreRedGarbage(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 11))
	durs := []time.Duration{0, time.Microsecond, time.Millisecond, 100 * time.Millisecond, 2 * time.Second, time.Hour, wcMinRTTOf(^uint32(0) - 1)}
	pick := func() time.Duration { return durs[r.IntN(len(durs))] }
	big := func() uint64 {
		switch r.IntN(4) {
		case 0:
			return 0
		case 1:
			return uint64(r.IntN(1000))
		case 2:
			return uint64(r.Int64N(1 << 40))
		}
		return ^uint64(0) >> uint(r.IntN(8))
	}
	for run := 0; run < 200; run++ {
		side := wcEdge
		if run%2 == 1 {
			side = wcExit
		}
		c := newWcore(wcConfig{side: side, stage: 1 + run%2, need: wtNeed, maxWin: wtMax, minWin: wtMin}, r.Float64)
		now := wtT0
		// stream counts of the steps (B follows them an evaluation late, and
		// stays while F is empty: the bound is the most of the last 40 steps
		// and of every step since the evaluation with F before the latest
		// one, when a clock that stepped back held the evaluations off)
		var nHist, evals []int
		for i := 0; i < 400; i++ {
			switch r.IntN(20) {
			case 0:
				now = now.Add(-time.Duration(r.IntN(5000)) * time.Millisecond) // a step back
			case 1:
				now = now.Add(24 * time.Hour)
			default:
				now = now.Add(100 * time.Millisecond)
			}
			in := wcIn{now: now, minRTT: pick(), srtt: pick(), segsIn: big(), ooo: big(), oooOK: r.IntN(2) == 0,
				linkBytes: big(), parks: uint32(r.IntN(3)), brakes: uint32(r.IntN(2)), poolFree: int64(r.IntN(1<<30)) - 1<<20,
				poolShort: uint32(r.IntN(2)), ctrlMin: pick(), apProbing: r.IntN(10) == 0}
			if r.IntN(3) == 0 {
				in.pongs = []time.Duration{pick(), pick()}
			}
			if r.IntN(4) == 0 {
				in.snd = wcSnd{fresh: true, srtt: pick(), minRTT: pick(), retrans: big(), segsOut: big()}
			}
			n := r.IntN(6)
			if r.IntN(50) == 0 {
				n = 2000
			}
			for k := 0; k < n; k++ {
				in.streams = append(in.streams, wcStream{id: uint32(1 + 2*r.IntN(n+3)), read: big(), upd: uint32(r.IntN(10)), hit: uint32(r.IntN(10)),
					lag: uint32(r.IntN(3)), turnMin: pick(), turnLast: pick(), win: uint32(r.Int64N(wtMax + 1)), braked: r.IntN(20) == 0})
			}
			func() {
				defer func() {
					if e := recover(); e != nil {
						t.Fatalf("run %d step %d: panic %v (state %v)", run, i, e, c.state)
					}
				}()
				o := c.step(in)
				for _, f := range []float64{c.tau.Seconds(), c.spread.Seconds(), c.last.rF, c.last.rLink, c.rhatF(now, c.last), c.cnt.peakR} {
					if math.IsNaN(f) || math.IsInf(f, 0) {
						t.Fatalf("run %d step %d: NaN/Inf in the core's estimators (tau %v rF %v)", run, i, c.tau, c.last.rF)
					}
				}
				nHist = append(nHist, max(len(in.streams), len(c.inF), 1))
				if c.lastAt.Equal(now) && len(c.last.f) > 0 {
					evals = append(evals, len(nHist)-1)
				}
				from := max(0, len(nHist)-40)
				if k := len(evals); k < 2 {
					from = 0
				} else {
					from = min(from, evals[k-2])
				}
				ns := uint64(slices.Max(nHist[from:]))
				if o.B > ns*wtMax {
					t.Fatalf("run %d step %d: B %d past %d streams × MaxStreamBuffer", run, i, o.B, ns)
				}
				sum := uint64(0)
				for _, x := range o.ceils {
					if x.ceil > wtMax {
						t.Fatalf("run %d step %d: ceiling %d", run, i, x.ceil)
					}
					sum += uint64(x.ceil)
				}
				// (the input's streams in F, a repeated id each time:
				// out() gives each at least c_min)
				nF := 0
				for _, x := range in.streams {
					if c.inF[x.id] {
						nF++
					}
				}
				if sum > max(o.B, uint64(max(nF, len(c.inF)))*uint64(c.cmin())) {
					t.Fatalf("run %d step %d: ceilings %d past B %d", run, i, sum, o.B)
				}
				if !o.next.After(now) {
					t.Fatalf("run %d step %d: next step at %v, not after now %v", run, i, o.next, now)
				}
			}()
		}
	}
}

// TestWcoreRedGrownDip (finding 6): the autopilot takes a pressed link's
// capacity for ordinary connections as sustained − grown, the least
// dominant rate of the last ticks less the grown windows' share of the
// latest one, and keeps the best of it over capWindow. One tick in which
// the core's latest interval read little grown (the sender's PROBE_RTT
// every ~10 s, a drain, a refresh) while the rate over the ticks stayed
// up took the whole rate as one link's capacity for ordinary connections:
// a capacity 6× too large, and the pool sized by it 6× too small.
func TestWcoreRedGrownDip(t *testing.T) {
	const mbit = 1e6 / 8
	run := func(dip bool) *autopilot {
		a := newAutopilot(2, 64, 8)
		now := time.Unix(1e9, 0)
		for tick := 0; tick < 10; tick++ {
			s := apSample{now: now.Add(time.Duration(tick) * healthTick), growable: true}
			for i := 0; i < 8; i++ {
				l := apLink{id: i, serving: true, rate: 100 * mbit, sustained: 100 * mbit, flowing: 1, open: 1}
				if i < 6 {
					l.pressed, l.rate, l.sustained, l.grown = true, 300*mbit, 300*mbit, 250*mbit
					if dip && tick == 4 {
						l.grown = 0 // this tick's latest evaluation of the core fell in a dip
					}
					// (the LinkManager's: the most grown of the ticks
					// sustained is taken over)
					l.grownSus = 250 * mbit
				}
				s.links = append(s.links, l)
				s.G += l.rate
				s.grown += l.grown
				s.flowing++
			}
			a.decide(s)
		}
		return a
	}
	steady, dipped := run(false), run(true)
	t.Logf("capacity for ordinary connections: steady %.0f, with one dip %.0f Mbit/s", steady.cCap/mbit, dipped.cCap/mbit)
	if dipped.cCap > 1.5*steady.cCap {
		t.Errorf("one tick's dip of grownRate took the capacity from %.0f to %.0f Mbit/s for capWindow", steady.cCap/mbit, dipped.cCap/mbit)
	}
}

// The LinkManager hands the autopilot the most grown of the ticks the
// sustained rate is taken over (finding 6).
func TestWcoreRedGrownSus(t *testing.T) {
	m := NewLinkManager(nil, 2, 2, 50, nil)
	f := newMeteredFake()
	f.m.wc = wcLogLink("link 1", wcEdge)
	ml := &managedLink{link: f, mtr: f.m}
	m.links = []*managedLink{ml}
	for i, g := range []float64{0, 3e6, 3e6, 3e6, 0} {
		f.m.wc.inStats.grown.Store(math.Float64bits(g))
		f.m.rdBytes.Add(20 << 20)
		m.sampleHealth()
		if i == 4 && (ml.grown != 0 || ml.grownSus != 3e6 || m.sample.links[0].grownSus != 3e6) {
			t.Fatalf("a tick with no grown after three with 3e6: grown %.0f, grownSus %.0f (sample %.0f), want 0 and 3e6",
				ml.grown, ml.grownSus, m.sample.links[0].grownSus)
		}
	}
}

// TestWcoreRedReorder: a path that reorders a share of the link's packets
// by a few ms (no loss at all): base against the core (HS2_WCORE_RED=1;
// HS2_WCORE_RED_RACK=1: the sender finds its losses as Linux's RACK does,
// cfg.rack, and does not resend what the path only held).
func TestWcoreRedReorder(t *testing.T) {
	if os.Getenv("HS2_WCORE_RED") != "1" {
		t.Skip("HS2_WCORE_RED=1")
	}
	rack := os.Getenv("HS2_WCORE_RED_RACK") == "1"
	for _, p := range []struct {
		rate   float64
		rtt, q int
	}{{300, 150, 50}, {1000, 100, 50}, {1000, 150, 50}} {
		for _, ro := range []struct {
			p  float64
			ms int
		}{{0, 0}, {0.0005, 2}, {0.002, 2}, {0.002, 5}, {0.01, 3}} {
			for seed := uint64(1); seed <= 2; seed++ {
				c := wmPath(p.rate, wmMS(p.rtt), wmMS(p.q))
				c.reorder, c.reorderMs, c.seed, c.dur, c.rack = ro.p, ro.ms, seed, 40*time.Second, rack
				b, k := wmRun(c.with(wmBase)), wmRun(c.with(wmCore))
				t.Logf("%s reorder %.2f%%/%dms s%d: base %.0f Mbit rs %.2f%% | core %.0f Mbit rs %.2f%% d%v p%v B%.1f",
					c.name, 100*ro.p, ro.ms, seed, b.mbit, 100*b.resend, k.mbit, 100*k.resend, k.drains, k.probes, float64(k.peakB)/(1<<20))
			}
		}
	}
}

// TestWcoreRedReorderTrace: HS2_WCORE_RED_RTRACE=rate/rtt/queue/reorder%/ms/seed[/base]
// (HS2_WCORE_RED_RACK=1 as TestWcoreRedReorder)
func TestWcoreRedReorderTrace(t *testing.T) {
	spec := os.Getenv("HS2_WCORE_RED_RTRACE")
	if spec == "" {
		t.Skip("HS2_WCORE_RED_RTRACE=rate/rtt/queue/reorder%/ms/seed[/base]")
	}
	var rate, rtt, q, ro float64
	var ms, seed int
	p := strings.Split(spec, "/")
	fmt.Sscan(p[0], &rate)
	fmt.Sscan(p[1], &rtt)
	fmt.Sscan(p[2], &q)
	fmt.Sscan(p[3], &ro)
	fmt.Sscan(p[4], &ms)
	fmt.Sscan(p[5], &seed)
	c := wmPath(rate, time.Duration(rtt)*time.Millisecond, time.Duration(q)*time.Millisecond)
	c.reorder, c.reorderMs, c.seed, c.trace, c.dur, c.mode = ro/100, ms, uint64(seed), true, 40*time.Second, wmCore
	c.rack = os.Getenv("HS2_WCORE_RED_RACK") == "1"
	if len(p) > 6 {
		c.mode = wmBase
	}
	r := wmRun(c)
	for _, l := range r.trace {
		t.Log(l)
	}
	t.Log(r)
}

// TestWcoreRedLogFlood (low): the rarer lines (random loss, a window at its
// ceiling, a route change) are capped per link only: when the path every
// link shares turns lossy, each of 300 links writes its line in the same
// minute. Report (HS2_WCORE_RED=1).
func TestWcoreRedLogFlood(t *testing.T) {
	if os.Getenv("HS2_WCORE_RED") != "1" {
		t.Skip("HS2_WCORE_RED=1")
	}
	ws := &wcSet{}
	var logs wcLogs
	logs.set(ws)
	now := time.Unix(1e9, 0)
	for i := 0; i < 300; i++ {
		l := wcLogLink(fmt.Sprintf("link %d", i), wcEdge)
		ws.logEvents(l, now.Add(time.Duration(i)*100*time.Millisecond), wcOut{events: []wcEvent{
			{kind: wcEvLossy, loss: 0.004, q: 18 * time.Millisecond, T: 100 * time.Millisecond},
			{kind: wcEvDrain, why: wcWhyLoss, from: 6 << 20, B: 3 << 20, n: 1, q: 30 * time.Millisecond},
			{kind: wcEvRoute, T: 400 * time.Millisecond},
		}})
	}
	ws.flush(now.Add(2 * time.Minute))
	t.Logf("300 links in 30 s: %d lines (loss %d, route %d, drain %d, summaries %d)", len(logs.all()),
		logs.count("loss with no queue"), logs.count("route change"), logs.count("path queue grew")+logs.count("packets were lost"), logs.count("more link(s)"))
}
