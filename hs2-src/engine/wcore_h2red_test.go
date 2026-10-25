package engine

import (
	"fmt"
	"testing"
	"time"
)

// The H2 red team (smux's covers, the window cover): what it found and
// fixed.

// Finding 1: links to one peer whose downloads' windows without the core
// already fill the path. Each link's download, held by its own window,
// pauses at its limit (the other links' data fills the gap on the
// bottleneck), so smux covers it; the covers together put the need of every
// link in flight, far past the path's knee: 300/72 behind 50 ms with two
// links resent 1.0–1.4% against 0.1–0.17% (the queue 24–26 ms against
// 12–18), 1000/100 behind 50 ms with eight links 0.8–0.9% against 0.0%
// (806 against 860 Mbit/s, p99 next to them 431–468 ms against 159–233).
// The cores could not take the covers back: the queue and the resends the
// covers made kept every core OFF (do no harm), and in OFF a core took the
// covers only while another core of the peer was on (grp), which none was.
//
// The core now takes its session's covers in OFF once two of the latest
// three records of the exit show its queue overflowing (resends with a
// queue past Δq: coverNote), and holds them back for wcCoverHarmHold after
// the latest evaluation that found so unless it leaves OFF first
// (coverHarmed): the windows are the need's again and the queue goes. (The
// cores may stay OFF after all: the covered rate raised each sender's
// STARTUP pacing for good, and the need's windows then burst past Δq on the
// exit's records, harm; the path is full either way.)
func TestWcoreH2RedSharedPathFull(t *testing.T) {
	if !smuxCoverOn(false) {
		t.Skip("the covers are off (HS2_TUNE_SMUX_COVER=0)")
	}
	for _, cl := range []struct {
		rate   float64
		rtt, q int
		n      int
	}{{300, 72, 50, 2}, {1000, 100, 50, 8}, {300, 72, 20, 2}} {
		for s := uint64(1); s <= 2; s++ {
			c := wmPath(cl.rate, wmMS(cl.rtt), wmMS(cl.q))
			c.dur, c.warm, c.seed = 60*time.Second, 20*time.Second, s
			var k0 wmShared
			func() {
				defer func(v int) { SmuxCover = v }(SmuxCover)
				SmuxCover = 0
				k0 = wmRunShared(c.with(wmCore), cl.n, nil)
			}()
			k := wmRunShared(c.with(wmCore), cl.n, nil)
			rs := func(r wmShared) (v float64) {
				for _, l := range r.links {
					v += l.resend / float64(len(r.links))
				}
				return v
			}
			msg := fmt.Sprintf("%s x%d s%d: covers %.1f Mbit resent %.2f%% q %.1f ms p99 %.0f ms; without them %.1f Mbit resent %.2f%% q %.1f ms p99 %.0f ms",
				c.name, cl.n, s, k.mbit, 100*rs(k), k.qMean, k.iP99, k0.mbit, 100*rs(k0), k0.qMean, k0.iP99)
			// (the cores may stay OFF: the covered rate raised each sender's
			// STARTUP pacing for good, and the need's windows then burst past
			// Δq on the exit's records, harm; the path is full either way.
			// A queue the covers stand in with no loss is not taken: 300/72
			// with two links 23 ms against 12 in one seed, with 6% more rate,
			// the cost of a cover on a path whose knee lies between the need
			// less a step and the need)
			if rs(k) > rs(k0)+0.002 || k.qMean > k0.qMean+15 || k.mbit < 0.95*k0.mbit {
				t.Errorf("%s", msg)
				continue
			}
			t.Log(msg)
		}
	}
}

// The core's side of finding 1: in OFF the covers go once two of the latest
// three records of the exit show its queue overflowing (resends with a
// queue past Δq), and come back wcCoverHarmHold after the latest evaluation
// that found so; leaving OFF ends the hold (the core's own floor rules from
// then on). Resends with no queue (a path's random loss), a standing queue
// with no resends (a knee between the need less a step and the need), or
// one record of three, take nothing. With the covers off nothing is noted.
func TestWcoreH2RedCoverOffPastKnee(t *testing.T) {
	rec := func(q time.Duration, retrans uint64) wcSnd {
		return wcSnd{fresh: true, srtt: 100*time.Millisecond + q, minRTT: 100 * time.Millisecond, retrans: retrans, segsOut: 100000}
	}
	// steps evaluates c in OFF each second from at on for n seconds, with
	// recs[k] the record of the k-th even second (a span of the exit's
	// records); it returns the time of the last step and its out.
	steps := func(c *wcore, at time.Time, n int, recs func(k int) wcSnd) (time.Time, wcOut) {
		var o wcOut
		for i := 0; i < n; i++ {
			at = at.Add(time.Second)
			in := wtRedIn(at, time.Second, wcSnd{})
			if at.Sub(wtT0)%(2*time.Second) == 0 {
				in.snd = recs(int(at.Sub(wtT0) / (2 * time.Second)))
			}
			o = c.step(in)
		}
		return at, o
	}
	clean := func(int) wcSnd { return rec(time.Millisecond, 0) }
	for _, tc := range []struct {
		name  string
		recs  func(k int) wcSnd
		taken bool
	}{
		{"the queue overflowing: resends with a queue past Δq", func(int) wcSnd { return rec(30*time.Millisecond, 200) }, true},
		{"resends with no queue (random loss)", func(int) wcSnd { return rec(time.Millisecond, 2000) }, false},
		{"a standing queue with no resends", func(int) wcSnd { return rec(40*time.Millisecond, 0) }, false},
		{"one record of three overflowing", func(k int) wcSnd {
			if k%3 == 0 {
				return rec(30*time.Millisecond, 200)
			}
			return rec(time.Millisecond, 0)
		}, false},
		{"clean records", clean, false},
	} {
		c := wtCore(1)
		c.cfg.noCover = false
		at, o := steps(c, wtT0, 12, tc.recs)
		if c.coverHarmed(at) != tc.taken {
			t.Errorf("%s: covers held back %v, want %v (records %+v)", tc.name, c.coverHarmed(at), tc.taken, c.coverRecs)
			continue
		}
		if c.state == wcOff && o.noCover != tc.taken {
			t.Errorf("%s: OFF, covers taken %v, want %v", tc.name, o.noCover, tc.taken)
		}
		if !tc.taken {
			continue
		}
		// clean records from now on: the covers come back (or the core left
		// OFF, its floor ruling them) within the records' span and the hold
		last := at
		for at.Sub(last) < wcSndHist*wcSndSpan+wcCoverHarmHold+2*time.Second && c.state == wcOff {
			at, o = steps(c, at, 1, clean)
		}
		if o.noCover || c.coverHarmed(at) && c.state == wcOff {
			t.Errorf("%s: the covers still taken %v after the last record past the knee (%v)", tc.name, at.Sub(last), c.state)
		}
	}
	// leaving OFF ends the hold
	c := wtCore(1)
	c.cfg.noCover = false
	at, _ := steps(c, wtT0, 12, func(int) wcSnd { return rec(30*time.Millisecond, 200) })
	if !c.coverHarmed(at) {
		t.Fatal("no hold after records of an overflowing queue")
	}
	c.pathFull = false
	in := wtRedIn(at.Add(time.Second), time.Second, wcSnd{})
	c.coverRecs = [wcSndHist]wcCoverRec{} // (as if the queue had gone)
	c.qSnds = [wcSndHist]wcRate{}
	c.offN = 5
	c.off(in, wtMeas(1, 80e6, 8*wtMiB), new([]wcEvent)) // (a rate the 8 MiB window is not past the knee of: harm)
	if c.state != wcStartup || c.coverHarmed(in.now) || c.out(in, in.now, nil).noCover {
		t.Errorf("leaving OFF: %v, covers held back %v", c.state, c.coverHarmed(in.now))
	}
	// the covers off: nothing noted, whatever the records say
	c = wtCore(1)
	at, _ = steps(c, wtT0, 12, func(int) wcSnd { return rec(40*time.Millisecond, 2000) })
	if !c.coverHarmAt.IsZero() || c.coverRecs[0].bad {
		t.Error("the covers off: a hold of covers there are none of")
	}
}

// Finding 2: covers no one supervises behind a policer. Only the exit's
// record shows a policer's loss to the edge (the core grows nothing without
// it, off), and a cover puts the need in flight where the windows had the
// need less a step: past a policer that the need less a step stayed under,
// at BBR's STARTUP pace. An edge with an old exit (no TCP_INFO in its
// records) kept its covers with its core OFF for good: on 300/72 behind a
// policer at 60% of the path it resent 10.4% against base's 0.3%, on
// 1000/72 behind one at 20% 7.4% against 0.2%, on 300/100 at 60% 5.5%
// against 0.2%. The edge's core now holds the covers back while the exit's
// records come without the sender's fields, or stopped (coverBlind): such
// an edge is base's resends once its first record came, and most of base's
// rate (the covered start took the sender out of BBR's STARTUP for good:
// TestWcoreStep1ModelBlindSidesAreBase).
func TestWcoreH2RedBlindPolicer(t *testing.T) {
	if !smuxCoverOn(false) {
		t.Skip("the covers are off (HS2_TUNE_SMUX_COVER=0)")
	}
	for _, x := range []struct {
		rate float64
		rtt  int
		pol  float64
	}{{300, 72, 0.6}, {1000, 72, 0.2}, {300, 100, 0.6}} {
		for s := uint64(1); s <= 2; s++ {
			c := wmPath(x.rate, wmMS(x.rtt), wmMS(50))
			c.policer, c.oldExit, c.seed, c.dur = x.pol*x.rate*wmMbit, true, s, 40*time.Second
			b, k := wmRun(c.with(wmBase)), wmRun(c.with(wmCore))
			msg := fmt.Sprintf("%s pol%.1f old exit s%d: base %.1f Mbit resent %.2f%%; the default config %.1f Mbit resent %.2f%%",
				c.name, x.pol, s, b.mbit, 100*b.resend, k.mbit, 100*k.resend)
			if k.resend > b.resend+0.005 || k.mbit < 0.85*b.mbit {
				t.Errorf("%s", msg)
			} else {
				t.Log(msg)
			}
		}
	}
	// the core's side: OFF on the edge whose exit's records came without the
	// sender's fields, or stopped for wcSndQuiet, holds the covers back; with
	// records, or before the first one, it does not
	c := wtCore(1)
	c.cfg.noCover = false
	in := wtIn(wtT0, 1, wtNeed)
	if o := c.out(in, wtT0, nil); o.noCover {
		t.Error("before the first record: the covers taken")
	}
	c.sndAny, c.sndAnyAt, c.sndLastAt = true, wtT0, wtT0
	if o := c.out(in, wtT0, nil); !o.noCover || !c.coverBlind(wtT0) {
		t.Error("records with no sender's fields (an old exit): the covers kept")
	}
	c.sndSeen = true
	if o := c.out(in, wtT0, nil); o.noCover {
		t.Error("records of the exit: the covers taken")
	}
	if at := wtT0.Add(wcSndQuiet + time.Second); !c.out(in, at, nil).noCover {
		t.Error("the records stopped: the covers kept")
	}
	c.cfg.noCover = true
	if c.coverBlind(wtT0.Add(time.Hour)) {
		t.Error("the covers off: blind for covers there are none of")
	}
}
