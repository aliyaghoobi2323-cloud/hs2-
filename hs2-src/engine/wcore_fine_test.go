package engine

import (
	"fmt"
	"testing"
)

// TestWcoreModelFineStep: the window-core model's base (no core: the field's
// downloads) and core, with the coarse step and with the fine one
// (smux.FineStepOf, "The fine step of a held stream" in smux's core.go),
// three seeds each: goodput, resends, the bottleneck queue, interactive
// replies, and the receiver's window updates a second and per MiB read.
//
//   - the field's round trip (75 ms) at 100, 300 and 1000 Mbit/s, and the
//     rig's half-window lock (300/100: 83.7 Mbit/s, 1 MiB a round trip);
//   - the base gains where its 2 MiB window holds it (its sender then
//     paces at what the window lets through: BBR's STARTUP, wmStartPace),
//     loses nowhere, and resends no more;
//   - a held stream sends at most 4 updates per MiB, the base's 1 under
//     its 2 MiB window (WINDOW_CORE.fa.md §8.2).
func TestWcoreModelFineStep(t *testing.T) {
	type cell struct {
		cfg  wmCfg
		lock bool // the base is held by its window here: it must gain
	}
	var cells []cell
	add := func(rate float64, rtt, q int, lock bool, mod func(*wmCfg)) {
		c := wmPath(rate, wmMS(rtt), wmMS(q))
		if mod != nil {
			mod(&c)
		}
		cells = append(cells, cell{c, lock})
	}
	add(100, 75, 50, false, nil)
	add(300, 75, 50, true, nil)
	add(1000, 75, 50, true, nil)
	add(300, 100, 50, true, nil)
	add(1000, 100, 50, true, nil)
	add(300, 150, 50, false, nil) // (0.8 of w/RTT already: 2 MiB a round trip is 112)
	add(100, 100, 50, false, nil)
	add(1000, 75, 50, false, func(c *wmCfg) {
		c.streams = make([]wmStreamCfg, 4)
		c.name += " x4"
	})
	add(1000, 100, 50, false, func(c *wmCfg) {
		c.policer = 300 * wmMbit
		c.name += " policer 300"
	})
	add(300, 150, 50, false, func(c *wmCfg) {
		c.streams = []wmStreamCfg{{}, {read: 2 * wmMbit}}
		c.name += " +slow reader"
	})
	add(300, 75, 50, false, func(c *wmCfg) {
		c.streams = []wmStreamCfg{{src: 5 * wmMbit}}
		c.name += " slow source 5"
	})
	line := func(r wmResult, step string) string {
		return fmt.Sprintf("%-26s %4s %6s %6.1f Mbit resend %5.2f%% q %5.1f/%5.1f ms i %4.0f/%4.0f upd %5.1f/s %4.2f/MiB",
			r.cfg.name, r.cfg.mode, step, r.mbit, 100*r.resend, r.qMean, r.q99, r.iP50, r.iP99, r.updS, r.updMiB)
	}
	for _, c := range cells {
		for _, mode := range []wmMode{wmBase, wmCore} {
			coarse := c.cfg.with(mode)
			coarse.coarse = true
			rc, rf := wmRunN(coarse, 3), wmRunN(c.cfg.with(mode), 3)
			t.Log(line(rc, "coarse"))
			t.Log(line(rf, "fine"))
			if mode != wmBase {
				continue
			}
			if rf.mbit < 0.97*rc.mbit {
				t.Errorf("%s: base %.1f Mbit/s with the fine step, %.1f with the coarse one", c.cfg.name, rf.mbit, rc.mbit)
			}
			if c.lock && rf.mbit < 1.3*rc.mbit {
				t.Errorf("%s: base %.1f Mbit/s with the fine step, %.1f with the coarse one: the half-window lock stays", c.cfg.name, rf.mbit, rc.mbit)
			}
			if rf.resend > rc.resend+0.003 {
				t.Errorf("%s: base resent %.2f%% with the fine step, %.2f%% with the coarse one", c.cfg.name, 100*rf.resend, 100*rc.resend)
			}
			if rf.updMiB > 4.2 {
				t.Errorf("%s: base %.2f updates per MiB with the fine step, want at most 4", c.cfg.name, rf.updMiB)
			}
		}
	}
}
