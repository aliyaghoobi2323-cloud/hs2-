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
	// it judges nothing, the next one does (holeLag: 2·101 ms + a wake on
	// the record's srtt)
	c = wtS2Edge()
	at = wtS2Wakes(c, wtT0, hole, nil)
	at = wtS2Wakes(c, at, [][2]uint64{{1000, 0}}, map[int]wcSnd{0: wtS2Rec(0)})
	if c.B != floor {
		t.Errorf("a record 100 ms after the holes (T_base 100 ms): B %.1f MiB, want the cut kept until a later one", float64(c.B)/wtMiB)
	}
	wtS2Wakes(c, at, [][2]uint64{{1000, 0}, {1000, 0}, {1000, 0}}, map[int]wcSnd{2: wtS2Rec(0)})
	if c.B != 8*wtMiB {
		t.Errorf("the next record, 400 ms after the holes, nothing resent: B %.1f MiB, want 8", float64(c.B)/wtMiB)
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

// A cut taken back restores B through setB, as any budget: within what the
// window pool covers now (and n_F·MaxStreamBuffer, the burst cap), not the
// budget from before at any cost; in STARTUP with the comparison its next
// step is judged by as the cut found it (fullR, prevR, prevB, flat, gear),
// not what an evaluation in between left.
func TestWcoreS2UndoThroughSetB(t *testing.T) {
	hole := [][2]uint64{{1000, 0}, {1000, 500}}
	// the pool: one stream at 2 MiB (the cut's), 1 MiB left in the pool
	c := wtS2Edge()
	at := wtS2Wakes(c, wtT0, hole, nil)
	at = wtS2Wakes(c, at, [][2]uint64{{1000, 0}, {1000, 0}, {1000, 0}}, nil)
	at = at.Add(100 * time.Millisecond)
	in := wtWake(c, at, 1000, 0)
	in.snd, in.poolFree, in.streams[0].win = wtS2Rec(0), 1<<20, wtNeed
	c.step(in)
	if want := uint64(wtNeed + 1<<20); c.cnt.undo != 1 || c.B != want || !c.capped {
		t.Errorf("taken back with 1 MiB left in the pool: undo %d B %.2f MiB capped %v, want 1, %.2f MiB (need + the pool) and capped",
			c.cnt.undo, float64(c.B)/wtMiB, c.capped, float64(want)/wtMiB)
	}

	// STARTUP: its comparison comes back as the cut found it
	c = wtS2Edge()
	c.state, c.B, c.bPub = wcStartup, 6*wtMiB, 6*wtMiB
	c.fullR, c.prevR, c.prevB, c.flat, c.gear = 50e6, 48e6, 4*wtMiB, 1, 2
	at = wtS2Wakes(c, wtT0, hole, nil)
	if c.state != wcDrain || c.B != wtNeed {
		t.Fatalf("STARTUP's holes: %v B %.1f MiB, want a cut to the floor", c.state, float64(c.B)/wtMiB)
	}
	c.fullR, c.prevR, c.prevB, c.flat, c.gear = 1, 1, 1, 3, 0 // (what an evaluation in between could leave)
	wtS2Wakes(c, at, [][2]uint64{{1000, 0}, {1000, 0}, {1000, 0}, {1000, 0}}, map[int]wcSnd{3: wtS2Rec(0)})
	if c.state != wcStartup || c.B != 6*wtMiB || c.fullR != 50e6 || c.prevR != 48e6 || c.prevB != 4*wtMiB || c.flat != 1 || c.gear != 2 {
		t.Errorf("STARTUP's cut taken back: %v B %.1f MiB fullR %.0f prevR %.0f prevB %d flat %d gear %d, want STARTUP at 6 MiB with 5e7, 4.8e7, 4 MiB, 1, 2",
			c.state, float64(c.B)/wtMiB, c.fullR, c.prevR, c.prevB, c.flat, c.gear)
	}
}

// A queue at the exit holds its record back behind the data, and RACK's
// wait grows with its srtt: a record judges a cut only 2·max(T_base, the
// exit's srtt) and a wake after the latest hole, by its own srtt or the
// latest record's (sndRTT). On T_base alone (300 ms here) a clean record
// 400 ms on took the cut back before the resend could show in it.
func TestWcoreS2HoleLagExitQueue(t *testing.T) {
	queued := wcSnd{fresh: true, srtt: 500 * time.Millisecond, minRTT: 100 * time.Millisecond, segsOut: 100000}
	for _, tc := range []struct {
		name string
		rec  wcSnd
		rtt  time.Duration // sndRTT: the exit's srtt by its latest record before
	}{{"the record's own srtt 500 ms", queued, 0}, {"the latest record's srtt 500 ms", wtS2Rec(0), 500 * time.Millisecond}} {
		c := wtS2Edge()
		c.sndRTT, c.sndRTTAt = tc.rtt, wtT0
		at := wtS2Wakes(c, wtT0, [][2]uint64{{1000, 0}, {1000, 500}}, nil)
		at = wtS2Wakes(c, at, [][2]uint64{{1000, 0}, {1000, 0}, {1000, 0}, {1000, 0}}, map[int]wcSnd{3: tc.rec})
		if c.B != wtNeed || c.cnt.undo != 0 {
			t.Errorf("%s: a clean record 400 ms after the holes: B %.1f MiB undo %d, want the cut kept (holeLag 1.1 s)", tc.name, float64(c.B)/wtMiB, c.cnt.undo)
		}
		wtS2Wakes(c, at, [][2]uint64{{1000, 0}, {1000, 0}, {1000, 0}, {1000, 0}, {1000, 0}, {1000, 0}, {1000, 0}}, map[int]wcSnd{6: tc.rec})
		if c.B != 8*wtMiB || c.cnt.undo != 1 {
			t.Errorf("%s: a clean record 1.1 s after the holes: B %.1f MiB undo %d, want it taken back", tc.name, float64(c.B)/wtMiB, c.cnt.undo)
		}
	}
}

// The path's reordering level (oooRo) holds wcQcapTTL after the latest cut
// taken back, and goes with a route change and with OFF: holes as dense cut
// again after it.
func TestWcoreS2ReorderLevelExpires(t *testing.T) {
	hole := [][2]uint64{{1000, 0}, {1000, 500}}
	learn := func() (*wcore, time.Time) {
		c := wtS2Edge()
		at := wtS2Wakes(c, wtT0, hole, nil)
		at = wtS2Wakes(c, at, [][2]uint64{{1000, 0}, {1000, 0}, {1000, 0}, {1000, 0}}, map[int]wcSnd{3: wtS2Rec(0)})
		if c.roLevel(at) != 0.625 || c.cnt.undo != 1 {
			t.Fatalf("a cut taken back: level %.3f undo %d, want 0.625 (×1.25 of half a wake) and 1", c.roLevel(at), c.cnt.undo)
		}
		return c, at
	}
	c, undoAt := learn()
	// (the exit's records keep coming: sndLastAt)
	t1 := undoAt.Add(wcQcapTTL - 2*time.Second)
	c.sndLastAt = t1
	wtS2Wakes(c, t1, hole, nil)
	if c.cnt.drainLoss != 1 {
		t.Errorf("holes as dense within wcQcapTTL: %d cut(s), want none past the first", c.cnt.drainLoss-1)
	}
	t2 := undoAt.Add(wcQcapTTL + time.Second)
	c.sndLastAt = t2
	wtS2Wakes(c, t2, hole, nil)
	if c.roLevel(t2) != 0 || c.cnt.drainLoss != 2 {
		t.Errorf("holes as dense wcQcapTTL after the cut taken back: level %.3f, %d cut(s) past the first, want 0 and one", c.roLevel(t2), c.cnt.drainLoss-1)
	}

	c, at := learn()
	c.toOff()
	if c.roLevel(at) != 0 {
		t.Errorf("OFF: level %.3f, want 0", c.roLevel(at))
	}

	// a real route change, 72 → 300 ms (as TestWcoreS2SlowStreamsAreNoRouteChange)
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	fast := func(turn time.Duration) wcStream {
		return wcStream{id: 1, read: 1 << 20, upd: 4, hit: 4, turnMin: turn}
	}
	r := wsCore(wcEdge)
	now := wtT0
	wsPaths(r, &now, time.Minute, ms(72), ms(73), ms(74), ms(72), fast(ms(75)))
	r.oooRo, r.oooRoAt = 0.5, now
	r.inF = map[uint32]bool{1: true}
	if n := wsPaths(r, &now, 80*time.Second, ms(72), ms(302), ms(302), ms(72), fast(ms(303))); n == 0 || r.roLevel(now) != 0 {
		t.Errorf("a route change 72 → 300 ms: %d route change(s), level %.3f, want one and 0", n, r.roLevel(now))
	}
}
