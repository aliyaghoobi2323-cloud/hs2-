package udpcarrier

import (
	"math"
	"testing"
	"time"
)

// The W8 field case: the sender's CPU (and the socket its carriers share) is
// the bottleneck, not the path. TCP flows inside the tunnel, a ping on the
// first carrier, the CPU freed at 25 s. Rules on, every carrier leaves startup
// and keeps its allowance within ~2x what it gets out (field: all in startup,
// up to 18x); the CPU-bound throughput and the ping's fast lane are untouched;
// once the CPU frees, the path takes the backlog without a drop and the pool
// regrows to near what it reaches with the rules off. Rules off
// (HS2_FAIR_SHARE=0): startup as before.
func TestPoolSimCPUBoundSender(t *testing.T) {
	if testing.Short() {
		t.Skip("long simulation")
	}
	sec, ms := time.Second, time.Millisecond
	free := []cpuStep{{0, 80e6}, {25 * sec, 0}}
	three := []cpuPoolCarrier{{flows: 3, ping: true}, {start: sec, flows: 3, fbPhase: 37}, {start: 2 * sec, flows: 2, fbPhase: 71}}
	var eight []cpuPoolCarrier
	for i := 0; i < 8; i++ {
		eight = append(eight, cpuPoolCarrier{start: time.Duration(i) * 400 * ms, flows: 1, ping: i == 0, fbPhase: float64(i * 37 % 100)})
	}
	base := cpuPoolCfg{oneWay: 19 * ms, buffer: 100 * ms, parity: 8, dur: 28 * sec, warm: 12 * sec, winA: [2]float64{12000, 25000}, win: [2]float64{25000, 28000}}
	for _, sc := range []struct {
		name  string
		path  float64
		cs    []cpuPoolCarrier
		slice float64
	}{
		{"3 carriers, path 200", 200e6, three, 0},
		{"3 carriers, path 200, 2 ms slices", 200e6, three, 2},
		{"3 carriers, path 100", 100e6, three, 0},
		{"8 carriers, path 150", 150e6, eight, 0},
		{"8 carriers, path 150, 2 ms slices", 150e6, eight, 2},
	} {
		cfg := base
		cfg.capBps, cfg.cpu, cfg.slice = sc.path, free, sc.slice
		cfg.rules = false
		off := runCPUPool(cfg, sc.cs)
		cfg.rules, cfg.opt.lw = true, pacerLateCredit.Seconds()*1000 // the pacer keeps late credit with the rules on
		on := runCPUPool(cfg, sc.cs)
		worst := 0.0
		for _, x := range on.a.rOverAct {
			worst = math.Max(worst, x)
		}
		t.Logf("%s: CPU-bound %.1f Mbit/s (off %.1f), in startup %d/%d (off %d/%d), allowance up to %.1fx, ping p99 %.1f ms (off %.1f); CPU freed: %.1f Mbit/s (off %.1f), path drops %d (off %d), path queue max %.0f ms (off %.0f)",
			sc.name, on.a.tot, off.a.tot, on.a.inStartup, on.a.carriers, off.a.inStartup, off.a.carriers, worst, on.a.pingS99, off.a.pingS99,
			on.b.tot, off.b.tot, on.b.pathDrops, off.b.pathDrops, on.b.pathQMax, off.b.pathQMax)
		if off.a.inStartup != off.a.carriers {
			t.Errorf("%s: rules off, %d of %d carriers left startup: the off switch must keep the old behaviour", sc.name, off.a.carriers-off.a.inStartup, off.a.carriers)
		}
		if on.a.inStartup != 0 {
			t.Errorf("%s: %d of %d carriers still in startup", sc.name, on.a.inStartup, on.a.carriers)
		}
		if worst > 2.5 {
			t.Errorf("%s: allowance %.1fx what the carrier gets out, want <= 2.5", sc.name, worst)
		}
		if on.a.tot < 0.99*off.a.tot {
			t.Errorf("%s: CPU-bound throughput %.1f, rules off %.1f", sc.name, on.a.tot, off.a.tot)
		}
		if on.a.pingS99 > off.a.pingS99+1 {
			t.Errorf("%s: ping p99 %.1f ms in the sender, rules off %.1f", sc.name, on.a.pingS99, off.a.pingS99)
		}
		if on.b.pathDrops > 0 {
			t.Errorf("%s: %d path drops once the CPU freed", sc.name, on.b.pathDrops)
		}
		if on.b.tot < 0.9*off.b.tot {
			t.Errorf("%s: %.1f Mbit/s once the CPU freed, rules off %.1f: regrowth too slow", sc.name, on.b.tot, off.b.tot)
		}
	}
}

// The field's stale peak: carriers that first ran fast (CPU free), then
// share a CPU-bound host while the other tunnel's bursts put a queue on the
// path now and then. Before, the allowance stayed at ~6x what went out (field:
// up to 18x) and the freed CPU released it into the path's buffer.
func TestPoolSimCPUBoundStalePeak(t *testing.T) {
	if testing.Short() {
		t.Skip("long simulation")
	}
	sec, ms := time.Second, time.Millisecond
	for _, path := range []float64{100e6, 200e6} {
		cfg := cpuPoolCfg{capBps: path, oneWay: 19 * ms, buffer: 100 * ms, parity: 8, dur: 28 * sec, warm: 12 * sec,
			winA: [2]float64{12000, 25000}, win: [2]float64{25000, 28000},
			cpu:           []cpuStep{{0, 0}, {4 * sec, 45e6}, {25 * sec, 0}},
			crossBurstBps: path, crossBurstMs: 160, crossEveryMs: 1700}
		cs := []cpuPoolCarrier{{flows: 3, ping: true}, {flows: 3, fbPhase: 37}, {flows: 2, fbPhase: 71}}
		cfg.rules = false
		off := runCPUPool(cfg, cs)
		cfg.rules, cfg.opt.lw = true, pacerLateCredit.Seconds()*1000
		on := runCPUPool(cfg, cs)
		worst, worstOff := 0.0, 0.0
		for i := range on.a.rOverAct {
			worst = math.Max(worst, on.a.rOverAct[i])
			worstOff = math.Max(worstOff, off.a.rOverAct[i])
		}
		t.Logf("path %.0f: allowance up to %.1fx (off %.1fx), in startup %d (off %d); CPU freed: %.1f Mbit/s (off %.1f), path drops %d (off %d)",
			path/1e6, worst, worstOff, on.a.inStartup, off.a.inStartup, on.b.tot, off.b.tot, on.b.pathDrops, off.b.pathDrops)
		if worst > 2.5 || on.a.inStartup > 0 {
			t.Errorf("path %.0f: allowance %.1fx, %d in startup", path/1e6, worst, on.a.inStartup)
		}
		if on.b.pathDrops > off.b.pathDrops {
			t.Errorf("path %.0f: %d path drops once the CPU freed, rules off %d", path/1e6, on.b.pathDrops, off.b.pathDrops)
		}
		// The cost of the rule, pinned: a queue another tunnel's burst puts on
		// the path ends the fast regrowth, so it recovers more slowly than a
		// carrier that releases its stale allowance into the buffer (66-89
		// against 79-140 Mbit/s here) — not slower than this.
		if on.b.tot < 0.55*off.b.tot {
			t.Errorf("path %.0f: %.1f Mbit/s once the CPU freed, rules off %.1f", path/1e6, on.b.tot, off.b.tot)
		}
	}
}
