package engine

import (
	"testing"
	"time"
)

// Red team of S2's "loss confirmed by the sender" (wcore.go: judgeCut,
// undoCut): the findings, each a test that failed on the angle as the judge
// took it (fe15687). The reports that found them are in
// wcore_s2_red_test.go; the probe below lets them read the angle's own
// state.

func init() {
	wmS2RedProbe = func(c *wcore, now time.Time) wmS2RedSee {
		return wmS2RedSee{ro: c.roLevel(now), undo: c.cnt.undo, provAt: c.prov.at}
	}
}

// Finding 1: a provisional cut is judged by a record holeLag after the
// latest hole (so says its rule), but only the holes that cut or gave a
// verdict were counted: those that came while the drain held B (lossDrain,
// no cut) were not. A loss right after a burst of the path's reordering
// (its holes past the level go on for the next wakes) was taken back by a
// record that came before the loss's resend could show in it.
func TestWcoreS2RedHolesAfterCut(t *testing.T) {
	c := wtS2Edge()
	at := wtS2Wakes(c, wtT0, [][2]uint64{{1000, 0}, {1000, 500}}, nil) // the cut (t0+200 ms)
	at = wtS2Wakes(c, at, [][2]uint64{{1000, 900}, {1000, 900}}, nil)  // a loss's holes, B held (t0+300, +400)
	// a clean record 400 ms after the cut, 200 ms after the latest hole
	// (holeLag 302 ms)
	at = wtS2Wakes(c, at, [][2]uint64{{1000, 0}, {1000, 0}}, map[int]wcSnd{1: wtS2Rec(0)})
	if c.cnt.undo != 0 || c.B != wtNeed {
		t.Errorf("a clean record 200 ms after the latest hole: undo %d B %.1f MiB, want the cut kept until a record holeLag after it",
			c.cnt.undo, float64(c.B)/wtMiB)
	}
	// the next record, 400 ms after the latest hole, judges: nothing resent
	wtS2Wakes(c, at, [][2]uint64{{1000, 0}, {1000, 0}}, map[int]wcSnd{1: wtS2Rec(0)})
	if c.cnt.undo != 1 || c.B != 8*wtMiB {
		t.Errorf("a clean record 400 ms after the latest hole: undo %d B %.1f MiB, want it taken back", c.cnt.undo, float64(c.B)/wtMiB)
	}
}

// Finding 3: a cut taken back brings B back unless a drain for a queue or a
// slower path moved it since (moved: those drains' counters), and R̂'s
// samples from before it when R̂ was reset since (as an evaluation's verdict
// for holes does). A drain for a slower path that finds B at the floor
// already (the cut put it there) drains nothing and counts nothing, but it
// resets R̂ to the slower rate: the record that took the cut back then
// restored the budget from before the cut and the faster R̂ with it, and
// HOLD sized B from that into the slower path's queue, the failure hold's
// slowed() exists for (red team, round 2: 1000 → 100 Mbit/s behind 200 ms,
// 7 cuts in 10 s, the loss rule's streak 4). Above the floor (the drain
// counted) R̂ still came back.
func TestWcoreS2RedUndoKeepsSlowerRate(t *testing.T) {
	for _, floor := range []bool{true, false} {
		c := wtS2Edge()
		for i := 1; i <= 8; i++ {
			c.noteRate(wtT0.Add(-time.Duration(i)*100*time.Millisecond), 60e6)
		}
		at := wtS2Wakes(c, wtT0, [][2]uint64{{1000, 0}, {1000, 500}}, nil) // the cut (t0+200 ms)
		if !floor {
			c.B, c.bPub = 6*wtMiB, 6*wtMiB // (as if halved: a queue behind the holes)
		}
		// the path slows down to 12 MB/s with a standing queue: the drain's
		// evaluation sees it (hold: slowed) and resets R̂ to the slower rate
		at = at.Add(100 * time.Millisecond)
		in := wtRedIn(at, 500*time.Millisecond, wcSnd{})
		in.streams[0].win = wtNeed
		m := wtMeas(1, 12e6, wtNeed)
		m.at = at
		m.qOwn, m.qOwnOK = 140*time.Millisecond, true
		var ev []wcEvent
		c.hold(in, m, &ev)
		b := c.B
		if len(c.rhat) != 1 || c.rhat[0].r != 12e6 || b > 6*wtMiB {
			t.Fatalf("floor %v: the path slowed after the cut: R̂ %d samples, B %.1f MiB; want R̂ reset to the slower rate", floor, len(c.rhat), float64(b)/wtMiB)
		}
		// a clean record takes the cut back (the holes were reordering)
		wtS2Wakes(c, at, [][2]uint64{{1000, 0}, {1000, 0}, {1000, 0}, {1000, 0}}, map[int]wcSnd{3: wtS2Rec(0)})
		if c.cnt.undo != 1 {
			t.Fatalf("floor %v: a clean record after the holes: undo %d, want 1", floor, c.cnt.undo)
		}
		if r := c.rhatF(at.Add(500*time.Millisecond), m); r != 12e6 || c.B > b {
			t.Errorf("floor %v: after the cut was taken back: R̂ %.0f MB/s, B %.1f MiB; want the slower path's 12 MB/s and B at most the drain's %.1f MiB (its verdict stands)",
				floor, r/1e6, float64(c.B)/wtMiB, float64(b)/wtMiB)
		}
	}
}

// Finding 2: a cut that stood on a small loss of the sender's (weak: RACK's
// resends before its first DSACK) is taken back with the next cut for holes
// as dense, its loss point, Q_cap and backoff with it: back to what they
// were before it, and everything learned since went with them. A loss worth
// a verdict in between (a policer's, from the exit's record: its loss
// point, Q_cap, the backoff) was wiped by a later burst of reordering.
func TestWcoreS2RedWeakAfterVerdict(t *testing.T) {
	hole := [][2]uint64{{1000, 0}, {1000, 500}}
	c := wtS2Edge()
	// the weak cut: 50 of 100000 segments resent (past sndNoLoss, under the
	// verdict's 0.1%)
	at := wtS2Wakes(c, wtT0, hole, nil)
	at = wtS2Wakes(c, at, [][2]uint64{{1000, 0}, {1000, 0}, {1000, 0}, {1000, 0}}, map[int]wcSnd{3: wtS2Rec(50)})
	if c.weak.at.IsZero() || c.B != wtNeed {
		t.Fatalf("a cut whose record showed 50 segments resent: weak %v B %.1f MiB, want it kept aside, B at the floor", !c.weak.at.IsZero(), float64(c.B)/wtMiB)
	}
	// 3 s on a policer's record comes (5% resent, no queue): the evaluation
	// learns its loss point, Q_cap and backoff (hold's verdict for loss)
	at = at.Add(3 * time.Second)
	c.lossDrain, c.state, c.B, c.bPub = false, wcHold, 6*wtMiB, 6*wtMiB
	at = wtS2Wakes(c, at, [][2]uint64{{1000, 0}}, map[int]wcSnd{0: wtS2Rec(5000)})
	m := wcMeas{at: at, snd: 0.05, sndOK: true, sndSegs: 100000, sndAt: at, qSnd: time.Millisecond, qSndOK: true}
	c.learnQcap(at, m)
	c.noteCap(at, 6*wtMiB)
	c.backoff++
	cp, back, qc := c.bCap, c.backoff, c.qcap
	if !c.policed(at) || back < wcPoliceBackoff {
		t.Fatalf("the policer's verdict: policed %v backoff %d, want a policer's", c.policed(at), back)
	}
	// later a burst of the path's reordering cuts and a clean record takes
	// that cut back (and teaches the level)
	at = at.Add(time.Second)
	c.sndLastAt = at
	at = wtS2Wakes(c, at, hole, nil)
	wtS2Wakes(c, at, [][2]uint64{{1000, 0}, {1000, 0}, {1000, 0}, {1000, 0}}, map[int]wcSnd{3: wtS2Rec(0)})
	if c.cnt.undo != 1 {
		t.Fatalf("the reordering's cut: undo %d, want it taken back", c.cnt.undo)
	}
	if c.bCap != cp || c.backoff != back || c.qcap != qc {
		t.Errorf("after the reordering's cut was taken back: loss point %.2f MiB backoff %d Q_cap %v, want the policer's %.2f MiB, %d, %v kept",
			float64(c.bCap)/wtMiB, c.backoff, c.qcap, float64(cp)/wtMiB, back, qc)
	}
}
