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

// wtBk is a wake of n segments, ooo of them behind a hole, of which wait
// still wait behind one at its end (full segments of wmMSS bytes): the
// in-order bytes are those of the rest, plus what waited at the wake
// before and passed (rel).
func wtBk(n, ooo, wait, rel uint64) wcIn {
	return wcIn{segsIn: n, ooo: ooo, oooOK: true, inOrder: (n - wait + rel) * wmMSS, inOrderOK: true}
}

// TestWcoreReoBacklog: the receiver's own view of the oldest hole
// (wcBacklog). A path's reordering opens holes that close within a few ms:
// whatever share of a wake's packets came behind them, what waits at its end
// is a few ms' worth, never past T_base/4. A loss's hole stays open until
// its resend comes: open past T_base/4 at a wake's end, or open all of a
// wake (nothing passed), it is a loss's. An age measured through wakes in
// which something passed while a hole stayed open (dense reordering, segB's
// error adding up) is not trusted.
func TestWcoreReoBacklog(t *testing.T) {
	dt := 100 * time.Millisecond
	fresh := func() *wcBacklog {
		var b wcBacklog
		for i := 0; i < 3; i++ {
			b.wake(wtBk(1000, 0, 0, 0), dt)
		}
		if b.segB != wmMSS || !b.ok {
			t.Fatalf("segB %.0f after wakes with no hole, want %d", b.segB, wmMSS)
		}
		return &b
	}
	b := fresh()
	b.wake(wtBk(1000, 400, 5, 0), dt) // 40% behind holes, 5 waiting at the end: 0.5 ms
	if b.bk != 0 || b.open(100*time.Millisecond) {
		t.Errorf("reordering, 40%% of a wake behind short holes: %.0f waiting, %v old, open %v — want none", b.bk, b.age, b.open(100*time.Millisecond))
	}
	b.wake(wtBk(1000, 300, 300, 0), dt) // a loss's hole opened 30 ms before the wake's end
	if !b.open(100*time.Millisecond) || b.open(200*time.Millisecond) || b.age != 30*time.Millisecond {
		t.Errorf("a hole 30 ms old: age %v, open at T_base 100 ms %v (want true), at 200 ms %v (want false)",
			b.age, b.open(100*time.Millisecond), b.open(200*time.Millisecond))
	}
	b.wake(wcIn{segsIn: 1000, ooo: 1000, oooOK: true, inOrderOK: true}, dt) // nothing passed
	if b.age != 130*time.Millisecond || !b.open(200*time.Millisecond) {
		t.Errorf("the same hole a wake later: age %v open %v, want 130ms and open at T_base 200 ms", b.age, b.open(200*time.Millisecond))
	}
	b.wake(wtBk(1000, 0, 0, 1300), dt) // its resend came: all of it passed
	if b.bk != 0 || b.open(100*time.Millisecond) {
		t.Errorf("after the resend: %.0f waiting, open %v", b.bk, b.open(100*time.Millisecond))
	}
	// dense reordering: a hole open at each wake's end, the one before passed
	b = fresh()
	b.wake(wtBk(1000, 500, 30, 0), dt)
	for i := 0; i < 5; i++ {
		b.wake(wtBk(1000, 500, 300, 30), dt) // segB's error, or a burst, reads 300 waiting
	}
	if b.trusted || b.open(100*time.Millisecond) || !b.openAny(100*time.Millisecond) {
		t.Errorf("an age through wakes that passed while holes stayed open: trusted %v, open %v (want false), openAny %v (want true)",
			b.trusted, b.open(100*time.Millisecond), b.openAny(100*time.Millisecond))
	}
	// no in-order counter (an old kernel, a test's input): nothing judged
	b = fresh()
	b.wake(wcIn{segsIn: 1000, ooo: 1000, oooOK: true}, dt)
	if b.ok || b.open(100*time.Millisecond) {
		t.Errorf("a wake with no in-order counter: ok %v open %v", b.ok, b.open(100*time.Millisecond))
	}
}

// TestWcoreReoSelf: with no record of the sender (the exit's side), the
// receiver learns the profile from its own proof where it is one: on a
// 200 ms path a loss's hole is always seen open past T_base/4 at some
// wake's end, so a window whose holes summed past T_base/4 while none stayed
// open is reordering's, and the same burst no longer cuts; a loss's hole
// still does, and teaches nothing. On a 72 ms path a loss's hole can open
// and close between two wakes: nothing is learned there.
func TestWcoreReoSelf(t *testing.T) {
	run := func(tBase time.Duration) (*wcore, *time.Time) {
		c := wtReo()
		c.cfg.side, c.sndSeen, c.tBase = wcExit, false, tBase
		now := wtT0
		return c, &now
	}
	wake := func(c *wcore, now *time.Time, n, ooo, wait, rel uint64) {
		*now = now.Add(100 * time.Millisecond)
		in := wtRedIn(*now, 100*time.Millisecond, wcSnd{})
		tb := c.tBase
		in.minRTT, in.srtt = tb, tb+time.Millisecond
		in.streams[0].turnMin, in.streams[0].turnLast = tb+5*time.Millisecond, tb+5*time.Millisecond
		in.segsIn, in.ooo, in.inOrderOK = n, ooo, true
		if wait <= n+rel {
			in.inOrder = wtBk(n, ooo, wait, rel).inOrder
		}
		c.step(in)
	}
	quiet := func(c *wcore, now *time.Time, k int) {
		for i := 0; i < k; i++ {
			wake(c, now, 1000, 0, 0, 0)
		}
	}
	burst := func(c *wcore, now *time.Time) {
		for i := 0; i < 3; i++ {
			wake(c, now, 1000, 450, 3, 3*uint64(min(i, 1))) // 45% behind holes of a few ms
		}
	}
	c, now := run(200 * time.Millisecond)
	quiet(c, now, 5)
	burst(c, now)
	quiet(c, now, 10)
	if c.cnt.drainLoss != 1 || c.reo.share(*now, 10000) <= 0 {
		t.Fatalf("T_base 200 ms, a burst of short holes: drains %d (want the cut, as before), profile %.2f (want it learned)",
			c.cnt.drainLoss, c.reo.share(*now, 10000))
	}
	d := c.cnt.drainLoss
	quiet(c, now, 30)
	if c.lossDrain || c.oooRef.v >= wcOooClean {
		t.Fatalf("T_base 200 ms: still in the cut's loss drain (%v) or holes past wcOooClean (%.2f): the next burst could not cut anyway", c.lossDrain, c.oooRef.v)
	}
	burst(c, now)
	quiet(c, now, 10)
	if c.cnt.drainLoss != d {
		t.Errorf("T_base 200 ms: the same burst after the profile cut again")
	}
	// a loss: a hole 40 ms old at a wake's end, then open all of the next
	quiet(c, now, 30)
	pts := len(c.reo.pts)
	wake(c, now, 1000, 400, 400, 0)
	wake(c, now, 1000, 1000, 1400, 0)
	if c.cnt.drainLoss == d {
		t.Errorf("T_base 200 ms: a loss's hole open 140 ms did not cut")
	}
	wake(c, now, 1000, 0, 0, 1400)
	quiet(c, now, 10)
	if len(c.reo.pts) != pts {
		t.Errorf("T_base 200 ms: the loss's window was learned as reordering (%d points, %d before)", len(c.reo.pts), pts)
	}
	// a 72 ms path: no proof
	c, now = run(72 * time.Millisecond)
	quiet(c, now, 5)
	burst(c, now)
	quiet(c, now, 10)
	if c.reo.share(*now, 10000) != 0 {
		t.Errorf("T_base 72 ms: a profile learned with no record (%.2f): a loss's hole fits between two wakes there", c.reo.share(*now, 10000))
	}
}
