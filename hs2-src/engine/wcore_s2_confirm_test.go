package engine

import (
	"testing"
	"time"
)

// S2, the angle "loss confirmed by the sender": on the edge, where the
// exit's records come, what holes alone did to B (fastStop's cut, the
// evaluation's verdict on Rcv_ooopack) waits for the exit's next record:
// one that shows a loss of the sender's own keeps it (a policer's floor
// too), one that shows none takes it back and teaches the path's reordering
// level. The exit's side and an edge whose records stopped keep the rules
// they had. (wcore.go: provSnap, judgeCut, undoCut; the field: cuts to the
// floor "packets arrived behind holes on a clean path" 4 to 11 times a run
// while the exit resent 0.000–0.002%.)

// wtS2Edge is wtHold's core on the edge with the exit's records coming (an
// extended record came just now), packets behind holes at 0.05% and no
// queue at the link socket (wtRedIn: srtt 1 ms past T_base): a cut for
// holes goes to the floor.
func wtS2Edge() *wcore {
	c, _ := wtHold()
	c.sndSeen, c.sndLastAt = true, wtT0
	c.oooRef = wcLossRef{v: 0.0005, b: 8 * wtMiB, ok: true}
	return c
}

// wtS2Rec is a record of the exit of 100000 segments, retrans of them
// resent, with no queue at its socket.
func wtS2Rec(retrans uint64) wcSnd {
	return wcSnd{fresh: true, srtt: 101 * time.Millisecond, minRTT: 100 * time.Millisecond, retrans: retrans, segsOut: 100000}
}

// wtS2Wakes steps c through runner wakes 100 ms apart from at on, between
// evaluations: n segments each, ooo of them behind a hole; a record of the
// exit comes in the wake rec[i] names (0: none).
func wtS2Wakes(c *wcore, at time.Time, wakes [][2]uint64, rec map[int]wcSnd) time.Time {
	for i, w := range wakes {
		at = at.Add(100 * time.Millisecond)
		in := wtWake(c, at, w[0], w[1])
		if s, ok := rec[i]; ok {
			in.snd = s
		}
		c.step(in)
	}
	return at
}

func TestWcoreS2HolesTakenBack(t *testing.T) {
	floor := uint64(wtNeed)
	hole := [][2]uint64{{1000, 0}, {1000, 500}} // half a wake behind a hole: fastStop's test of a loss

	// reordering: the record that comes a round trip and more after the
	// holes shows nothing resent; B is back where it was, so is its loss
	// point, and holes as dense cut no more
	c := wtS2Edge()
	at := wtS2Wakes(c, wtT0, hole, nil)
	if c.cnt.drainLoss != 1 || c.B != floor || c.bCap == 0 {
		t.Fatalf("holes with no queue behind them: drains %d B %.1f MiB cap %.1f MiB, want a cut to the floor at once (as before)",
			c.cnt.drainLoss, float64(c.B)/wtMiB, float64(c.bCap)/wtMiB)
	}
	at = wtS2Wakes(c, at, [][2]uint64{{1000, 0}, {1000, 0}, {1000, 0}, {1000, 0}}, map[int]wcSnd{3: wtS2Rec(0)})
	if c.B != 8*wtMiB || c.bCap != 0 || c.state != wcHold || c.lossDrain {
		t.Fatalf("a record 400 ms after the holes with nothing resent: B %.1f MiB cap %.1f MiB %v lossDrain %v, want B back at 8 MiB, no cap, HOLD",
			float64(c.B)/wtMiB, float64(c.bCap)/wtMiB, c.state, c.lossDrain)
	}
	at = wtS2Wakes(c, at, hole, nil)
	if c.cnt.drainLoss != 1 || c.B != 8*wtMiB {
		t.Errorf("holes as dense once more: drains %d B %.1f MiB, want no cut (the path's reordering level)", c.cnt.drainLoss, float64(c.B)/wtMiB)
	}
	// a loss's hole (90% of a wake behind it) still cuts, and a record
	// that shows the sender's resends keeps the cut
	wtS2Wakes(c, at, [][2]uint64{{1000, 0}, {1000, 900}, {1000, 0}, {1000, 0}, {1000, 0}, {1000, 0}}, map[int]wcSnd{5: wtS2Rec(5000)})
	if c.cnt.drainLoss != 2 || c.B != floor || c.bCap == 0 {
		t.Errorf("a loss's hole and a record of 5%% resent: drains %d B %.1f MiB cap %.1f MiB, want the cut to the floor kept",
			c.cnt.drainLoss, float64(c.B)/wtMiB, float64(c.bCap)/wtMiB)
	}

	// a policer: the record shows resends and no queue at the exit's
	// socket; the cut to the floor and its loss point stand
	c = wtS2Edge()
	at = wtS2Wakes(c, wtT0, hole, nil)
	cp := c.bCap
	wtS2Wakes(c, at, [][2]uint64{{1000, 0}, {1000, 0}, {1000, 0}, {1000, 0}}, map[int]wcSnd{3: wtS2Rec(2000)})
	if c.B != floor || c.bCap != cp {
		t.Errorf("a policer's holes and a record of 2%% resent: B %.1f MiB cap %.1f MiB, want the floor and the cap %.1f kept",
			float64(c.B)/wtMiB, float64(c.bCap)/wtMiB, float64(cp)/wtMiB)
	}

	// too soon: a record that came before the holes' resends could show in
	// it judges nothing, the next one does
	c = wtS2Edge()
	at = wtS2Wakes(c, wtT0, hole, nil)
	at = wtS2Wakes(c, at, [][2]uint64{{1000, 0}}, map[int]wcSnd{0: wtS2Rec(0)})
	if c.B != floor {
		t.Errorf("a record 100 ms after the holes (T_base 100 ms): B %.1f MiB, want the cut kept until a later one", float64(c.B)/wtMiB)
	}
	wtS2Wakes(c, at, [][2]uint64{{1000, 0}, {1000, 0}}, map[int]wcSnd{1: wtS2Rec(0)})
	if c.B != 8*wtMiB {
		t.Errorf("the next record, nothing resent: B %.1f MiB, want 8", float64(c.B)/wtMiB)
	}

	// no record judges where the records stopped (blind: sndQuiet) or on
	// the exit's side: holes cut as before, and a clean record takes nothing
	// back
	for _, tc := range []struct {
		name string
		mod  func(c *wcore)
	}{
		{"records stopped 10 s ago", func(c *wcore) { c.sndLastAt = wtT0.Add(-10 * time.Second) }},
		{"the exit's side", func(c *wcore) { c.cfg.side = wcExit }},
		{"an edge with no extended record", func(c *wcore) { c.sndSeen = false }},
	} {
		c := wtS2Edge()
		tc.mod(c)
		at := wtS2Wakes(c, wtT0, hole, nil)
		wtS2Wakes(c, at, [][2]uint64{{1000, 0}, {1000, 0}, {1000, 0}, {1000, 0}}, map[int]wcSnd{3: wtS2Rec(0)})
		if c.cnt.drainLoss != 1 || c.B != floor {
			t.Errorf("%s: drains %d B %.1f MiB, want the cut to the floor, kept", tc.name, c.cnt.drainLoss, float64(c.B)/wtMiB)
		}
	}
}
