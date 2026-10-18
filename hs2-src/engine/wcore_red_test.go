package engine

import (
	"fmt"
	"os"
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
func TestWcoreRedLossRuleSweep(t *testing.T) {
	if os.Getenv("HS2_WCORE_RED") != "1" {
		t.Skip("HS2_WCORE_RED=1")
	}
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
	c := wmPath(rate, time.Duration(rtt)*time.Millisecond, time.Duration(q)*time.Millisecond)
	c.loss, c.seed, c.exit, c.policer, c.trace, c.dur = loss/100, uint64(seed), exit == 1, pol*wmMbit, true, 40*time.Second
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
// up|dn|both).
func TestWcoreRedGrid(t *testing.T) {
	if os.Getenv("HS2_WCORE_RED") != "1" {
		t.Skip("HS2_WCORE_RED=1")
	}
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
// the core resent 0.05-2%.
func TestWcoreRedBlindHoley(t *testing.T) {
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
		b, k := wmRun(c.with(wmBase)), wmRun(c.with(wmCore))
		side := map[bool]string{true: "up", false: "dn-old"}[x.exit]
		msg := fmt.Sprintf("%s %s l%g s%d: base %.0f Mbit resent %.2f%% [%s]; core %.0f Mbit resent %.2f%% [%s] drains %v probes %v",
			side, c.name, 100*x.loss, x.seed, b.mbit, 100*b.resend, wmLossNote(b), k.mbit, 100*k.resend, wmLossNote(k), k.drains, k.probes)
		if wmDrainedByCore(b, k) {
			t.Errorf("the loss rule would drain the link under the core: %s", msg)
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
