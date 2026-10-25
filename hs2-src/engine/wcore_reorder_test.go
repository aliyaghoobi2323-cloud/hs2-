package engine

import (
	"testing"
	"time"
)

// wtReo is the HOLD core of wtHold on the edge with the exit's records
// coming (sndSeen), its holes at a level of 0.05%, stepped by wtReoWake.
func wtReo() *wcore {
	c, _ := wtHold()
	c.oooRef = wcLossRef{v: 0.0005, b: 8 * wtMiB, ok: true}
	c.sndSeen, c.sndLastAt, c.lastAt, c.wakeAt = true, wtT0, wtT0, wtT0
	c.evalAt = wtT0.Add(time.Second)
	return c
}

// wtReoWake steps c by one runner wake at now (100 ms after the one before):
// segs segments in, ooo of them behind a hole, and with rec >= 0 a record of
// the exit that resent rec of 20000 segments.
func wtReoWake(c *wcore, now time.Time, segs, ooo uint64, rec int) {
	in := wtRedIn(now, 100*time.Millisecond, wcSnd{})
	in.segsIn, in.ooo = segs, ooo
	if rec >= 0 {
		in.snd = wcSnd{fresh: true, srtt: 101 * time.Millisecond, minRTT: 100 * time.Millisecond, retrans: uint64(rec), segsOut: 20000}
	}
	c.step(in)
}

// wtReoRun steps c through wakes from *now: each is {segs, ooo, rec} (rec -1:
// no record in that wake).
func wtReoRun(c *wcore, now *time.Time, wakes ...[3]int) {
	for _, w := range wakes {
		*now = now.Add(100 * time.Millisecond)
		wtReoWake(c, *now, uint64(w[0]), uint64(w[1]), w[2])
	}
}

// wtReoQuiet is n wakes of 1000 segments, none behind a hole, a clean
// record of the exit every 20th from the first.
func wtReoQuiet(n int) [][3]int {
	var w [][3]int
	for i := 0; i < n; i++ {
		rec := -1
		if i%20 == 0 {
			rec = 0
		}
		w = append(w, [3]int{1000, 0, rec})
	}
	return w
}

// TestWcoreReoUndo (field round of step 1): on the edge's 72 ms path to
// Iran the core cut B to the 2 MiB floor 4 to 11 times per download for
// "packets arrived behind holes on a clean path", each with a cap left for
// 10 minutes, while the exit resent 0.000–0.002%: the path reorders packets
// in bursts (to 38% of a half-second's at 300 Mbit/s) and Linux's RACK
// resends none of them. A burst of holes alone cuts as before, but once the
// exit's records that cover its hole and its resend come with nothing
// resent, the cut is undone: the budget, no loss point (bCap) and no Q_cap
// from it. A record that did resend keeps it, and so does a core with no
// records (the exit's side).
func TestWcoreReoUndo(t *testing.T) {
	burst := [][3]int{{1000, 0, -1}, {1000, 300, -1}, {1000, 300, -1}}
	t.Run("the records resent nothing: undone", func(t *testing.T) {
		c := wtReo()
		now := wtT0
		wtReoRun(c, &now, burst...)
		if c.cnt.drainLoss != 1 || c.bCap == 0 {
			t.Fatalf("a burst of holes (30%% of two wakes) on a clean path: drains for loss %d, cap %d — want the cut as before", c.cnt.drainLoss, c.bCap)
		}
		cutB := c.B
		// (a clean record 100 ms after the cut, the next 2 s after it)
		wtReoRun(c, &now, wtReoQuiet(21)...)
		if c.cnt.undo != 1 {
			t.Fatalf("clean records after the cut: undone %d, want 1 (B %.1f MiB, cap %.1f MiB)", c.cnt.undo, float64(c.B)/wtMiB, float64(c.bCap)/wtMiB)
		}
		if c.B != 8*wtMiB || c.bCap != 0 || c.qcapKnown(now) || c.lossDrain {
			t.Errorf("undone: B %.1f MiB (cut to %.1f, want 8), cap %.1f MiB (want none), Q_cap known %v, loss drain %v",
				float64(c.B)/wtMiB, float64(cutB)/wtMiB, float64(c.bCap)/wtMiB, c.qcapKnown(now), c.lossDrain)
		}
	})
	t.Run("the records resent: the cut stands", func(t *testing.T) {
		c := wtReo()
		now := wtT0
		wtReoRun(c, &now, burst...)
		w := wtReoQuiet(30)
		w[4][2] = 12 // a record half a second after the cut resent 12 segments
		wtReoRun(c, &now, w...)
		if c.cnt.undo != 0 || c.bCap == 0 {
			t.Errorf("a record with resends after the cut: undone %d, cap %.1f MiB — want the cut and its cap kept", c.cnt.undo, float64(c.bCap)/wtMiB)
		}
	})
	t.Run("no record of the exit: the cut stands", func(t *testing.T) {
		c := wtReo()
		c.cfg.side = wcExit
		c.sndSeen = false
		now := wtT0
		wtReoRun(c, &now, burst...)
		var w [][3]int
		for i := 0; i < 30; i++ {
			w = append(w, [3]int{1000, 0, -1})
		}
		wtReoRun(c, &now, w...)
		if c.cnt.drainLoss != 1 || c.cnt.undo != 0 || c.bCap == 0 {
			t.Errorf("the exit's side (no records): drains %d, undone %d, cap %.1f MiB — want the cut kept", c.cnt.drainLoss, c.cnt.undo, float64(c.bCap)/wtMiB)
		}
	})
	t.Run("a record before the hole's resend could be in one says nothing", func(t *testing.T) {
		c := wtReo()
		now := wtT0
		wtReoRun(c, &now, burst...)
		wtReoRun(c, &now, [3]int{1000, 0, 0}) // 100 ms after the cut: its resend is not sent yet
		if c.cnt.undo != 0 || c.undoDue {
			t.Errorf("a clean record 100 ms after the cut undid it (undone %d)", c.cnt.undo)
		}
	})
}

// TestWcoreReoProfile: the windows of holes the exit's records showed held
// no loss are the link's reordering profile, and lossHole asks for more
// than it allows at the window's rate (up to wcReoCap of T_base): the same
// burst no longer cuts, nor one at a faster rate (reordering grows with
// it: the field's 2% at 300, 6% at 600 Mbit/s), while a loss's hole (a round
// trip of waiting) still does. With no profile learned (no records) the
// burst cuts.
func TestWcoreReoProfile(t *testing.T) {
	burst := [][3]int{{1000, 300, -1}, {1000, 300, -1}}
	c := wtReo()
	now := wtT0
	// the burst (a cut, undone by the next clean record): learned
	wtReoRun(c, &now, burst...)
	wtReoRun(c, &now, wtReoQuiet(60)...)
	if c.cnt.drainLoss != 1 || c.cnt.undo != 1 {
		t.Fatalf("the first burst: drains %d undone %d, want 1 and 1", c.cnt.drainLoss, c.cnt.undo)
	}
	if s := c.reo.share(now, 10000); s <= 0 {
		t.Fatalf("no reordering learned from a burst the records showed clean")
	}
	for _, tc := range []struct {
		name  string
		wakes [][3]int
		cut   bool
	}{
		{"the same burst", burst, false},
		{"the same burst at a 1.5 times faster rate", [][3]int{{1500, 450, -1}, {1500, 450, -1}}, false},
		{"a loss: a round trip behind its hole", [][3]int{{1000, 1000, -1}, {1000, 600, -1}}, true},
	} {
		d := c.cnt.drainLoss
		wtReoRun(c, &now, tc.wakes...)
		if cut := c.cnt.drainLoss > d; cut != tc.cut {
			t.Errorf("%s after the profile: cut %v, want %v (threshold %v)", tc.name, cut, tc.cut, c.holeThr(now, 2000, 200*time.Millisecond))
		}
		wtReoRun(c, &now, wtReoQuiet(60)...)
	}
	// no records: nothing learned, the burst cuts
	c = wtReo()
	c.cfg.side, c.sndSeen = wcExit, false
	now = wtT0
	wtReoRun(c, &now, burst...)
	if c.cnt.drainLoss != 1 || c.reo.share(now, 10000) != 0 {
		t.Errorf("the exit's side: drains %d, profile %.2f — want the cut and no profile", c.cnt.drainLoss, c.reo.share(now, 10000))
	}
}
