package engine

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

// The post-merge review of "loss confirmed by the sender" (93d0faa): what
// undoCut takes back must be what holes alone did, and its lines, counters
// and trace fields must say so. TestRevUndoWipesSndLossTestCap and
// TestRevWeakWipesLaterCap are the reviewer's reproductions, as written.

// F1: an evaluation that resolves a resend-based loss test (lossTestDone,
// wcSrcSnd: the record is back at its level, so the losses were ours: noteCap)
// in an interval that also had holes past their level gets noted as a reaction
// to holes (provNote: bCapAt == now), and the next clean record takes the cap back.
func TestRevUndoWipesSndLossTestCap(t *testing.T) {
	c := wtS2Edge()
	c.lossTest, c.lossTestAt, c.lossTestB = wcSrcSnd, wtT0.Add(-3*time.Second), 8*wtMiB
	c.lastAt, c.evalAt = wtT0.Add(-400*time.Millisecond), wtT0
	c.holeSeen, c.holeMax = true, 0.3 // a loss-like hole came in this interval (lossHole)
	in := wtRedIn(wtT0, 400*time.Millisecond, wtS2Rec(0))
	in.ooo = 400 // 10% of the interval behind holes
	c.step(in)
	t.Logf("after the eval: lossTest %d bCap %.2f MiB bCapAt=now %v prov pending %v B %.2f drainLoss %d",
		c.lossTest, float64(c.bCap)/wtMiB, c.bCapAt.Equal(wtT0), !c.prov.at.IsZero(), float64(c.B)/wtMiB, c.cnt.drainLoss)
	capSnd := c.bCap
	wtS2Wakes(c, wtT0, [][2]uint64{{1000, 0}, {1000, 0}, {1000, 0}, {1000, 0}}, map[int]wcSnd{3: wtS2Rec(0)})
	t.Logf("after the clean record: undo %d bCap %.2f MiB B %.2f MiB state %v", c.cnt.undo, float64(c.bCap)/wtMiB, float64(c.B)/wtMiB, c.state)
	if capSnd > 0 && c.bCap != capSnd {
		t.Errorf("the loss point the resend-based loss test set (%.2f MiB) was taken back by undoCut (now %.2f MiB)", float64(capSnd)/wtMiB, float64(c.bCap)/wtMiB)
	}
}

// F2: weak had no expiry; a loss test resolved a minute after a weak cut
// was wiped by the weak restoration at a later reordering undo.
func TestRevWeakWipesLaterCap(t *testing.T) {
	c := wtS2Edge()
	hole := [][2]uint64{{1000, 0}, {1000, 500}}
	at := wtS2Wakes(c, wtT0, hole, nil)
	// a record 400 ms on with a small loss of the sender (60 of 100000: under the verdict, past sndNoLoss)
	at = wtS2Wakes(c, at, [][2]uint64{{1000, 0}, {1000, 0}, {1000, 0}, {1000, 0}}, map[int]wcSnd{3: wtS2Rec(60)})
	t.Logf("weak kept: %v (share %.2f) prov %v bCap %.2f B %.2f", !c.weak.at.IsZero(), c.weak.share, !c.prov.at.IsZero(), float64(c.bCap)/wtMiB, float64(c.B)/wtMiB)
	// a minute later: HOLD at 8 MiB again, the cap gone stale (hold: bCap, backoff = 0, 0),
	// and a resend-based loss test concluding at this eval that the losses were ours
	t1 := at.Add(time.Minute)
	c.state, c.B, c.bPub, c.lossDrain, c.refPending, c.settling = wcHold, 8*wtMiB, 8*wtMiB, false, false, false
	c.bCap, c.backoff = 0, 0
	c.holeSeen, c.holeMax, c.accOK, c.holeWakes = false, 0, false, nil // (what the minute between left: evaluations consumed it)
	c.lossTest, c.lossTestAt, c.lossTestB = wcSrcSnd, t1.Add(-3*time.Second), 8*wtMiB
	c.lastAt, c.evalAt, c.sndLastAt, c.chgAt = t1.Add(-400*time.Millisecond), t1, t1.Add(-time.Second), t1.Add(-10*time.Second)
	c.step(wtRedIn(t1, 400*time.Millisecond, wtS2Rec(0)))
	capSnd := c.bCap
	t.Logf("t1 eval: lossTest %d bCap %.2f MiB prov %v weak %v", c.lossTest, float64(capSnd)/wtMiB, !c.prov.at.IsZero(), !c.weak.at.IsZero())
	// a reordering cut 1 s later, taken back by the next clean record
	at = wtS2Wakes(c, t1.Add(time.Second), hole, nil)
	wtS2Wakes(c, at, [][2]uint64{{1000, 0}, {1000, 0}, {1000, 0}, {1000, 0}}, map[int]wcSnd{3: wtS2Rec(0)})
	t.Logf("after undo: undo %d bCap %.2f MiB weak %v", c.cnt.undo, float64(c.bCap)/wtMiB, !c.weak.at.IsZero())
	if capSnd > 0 && c.bCap != capSnd {
		t.Errorf("a loss point set a minute after the weak cut by the resend-based test (%.2f MiB) was wiped by the weak restoration (now %.2f MiB)", float64(capSnd)/wtMiB, float64(c.bCap)/wtMiB)
	}
}

// F2, the rule itself: a cut that stood on a small loss of the sender's
// (weak) is taken back with a later cut for holes as dense only within
// wcWeakTTL of its latest hole, and a loss point learned from the exit's
// records since (keptAt) stands even within it.
func TestWcoreS2WeakLifetime(t *testing.T) {
	hole := [][2]uint64{{1000, 0}, {1000, 500}}
	weakCut := func() (*wcore, time.Time) {
		c := wtS2Edge()
		at := wtS2Wakes(c, wtT0, hole, nil)
		at = wtS2Wakes(c, at, [][2]uint64{{1000, 0}, {1000, 0}, {1000, 0}, {1000, 0}}, map[int]wcSnd{3: wtS2Rec(60)})
		if c.weak.at.IsZero() || c.bCap == 0 {
			t.Fatalf("a small loss of the sender's: weak %v cap %.2f MiB, want the cut kept aside with its cap", !c.weak.at.IsZero(), float64(c.bCap)/wtMiB)
		}
		// HOLD at 8 MiB again, as the drain's end leaves it
		c.state, c.B, c.bPub, c.lossDrain = wcHold, 8*wtMiB, 8*wtMiB, false
		return c, at
	}
	later := func(c *wcore, at time.Time) {
		c.sndLastAt = at
		at = wtS2Wakes(c, at, hole, nil)
		wtS2Wakes(c, at, [][2]uint64{{1000, 0}, {1000, 0}, {1000, 0}, {1000, 0}}, map[int]wcSnd{3: wtS2Rec(0)})
	}
	c, at := weakCut()
	later(c, at.Add(2*time.Second))
	if c.cnt.undo != 1 || c.bCap != 0 {
		t.Errorf("a cut as dense 2 s on, taken back: undo %d cap %.2f MiB, want the weak cut's cap taken back too", c.cnt.undo, float64(c.bCap)/wtMiB)
	}
	c, at = weakCut()
	cp := c.bCap
	later(c, at.Add(wcWeakTTL+time.Second))
	if c.cnt.undo != 1 || c.bCap != cp {
		t.Errorf("a cut as dense past wcWeakTTL, taken back: undo %d cap %.2f MiB, want the weak cut's cap %.2f kept", c.cnt.undo, float64(c.bCap)/wtMiB, float64(cp)/wtMiB)
	}
	c, at = weakCut()
	c.bCap, c.bCapAt, c.keptAt = 6*wtMiB, at.Add(time.Second), at.Add(time.Second) // (a resend-based loss test's)
	later(c, at.Add(2*time.Second))
	if c.cnt.undo != 1 || c.bCap != 6*wtMiB {
		t.Errorf("a resend-based loss test's cap since the weak cut: cap %.2f MiB after the undo, want its 6 MiB kept", float64(c.bCap)/wtMiB)
	}
}

// What a probe that paid or a cap gone stale eased since a cut for holes
// (relaxAt) goes back to the easier of the two when the cut is taken back:
// the probe's cap is the cut's, raised as far as the probe found the path
// clean, and no loss point of its own.
func TestWcoreS2UndoAfterProbePaid(t *testing.T) {
	hole := [][2]uint64{{1000, 0}, {1000, 500}}
	for _, tc := range []struct {
		name          string
		before, probe uint64 // the cap before the cut, the cap the probe left (0: none)
		want          uint64
	}{
		{"no cap before, the probe raised the cut's", 0, 6 * wtMiB, 0},
		{"a cap of 5 MiB before, the probe raised the cut's to 6", 5 * wtMiB, 6 * wtMiB, 6 * wtMiB},
		{"a cap of 7 MiB before, the probe raised the cut's to 6", 7 * wtMiB, 6 * wtMiB, 7 * wtMiB},
		{"a cap of 5 MiB before, the probe cleared it (at the floor)", 5 * wtMiB, 0, 0},
	} {
		c := wtS2Edge()
		c.bCap, c.bCapAt, c.backoff = tc.before, wtT0, 2
		at := wtS2Wakes(c, wtT0, hole, nil)
		c.bCap, c.backoff, c.relaxAt = tc.probe, 0, at.Add(50*time.Millisecond) // (a probe that paid)
		wtS2Wakes(c, at, [][2]uint64{{1000, 0}, {1000, 0}, {1000, 0}, {1000, 0}}, map[int]wcSnd{3: wtS2Rec(0)})
		if c.cnt.undo != 1 || c.bCap != tc.want || c.backoff != 0 {
			t.Errorf("%s: undo %d cap %.1f MiB backoff %d, want 1, %.1f MiB and 0", tc.name, c.cnt.undo, float64(c.bCap)/wtMiB, c.backoff, float64(tc.want)/wtMiB)
		}
	}
}

// F3 (stage 2: pathFull holds c_min under need): a drain for a queue or a
// slower path since the cut (movedAt) keeps what it found, the path full
// too; undoCut restored pathFull before it looked.
func TestWcoreS2UndoKeepsPathFullAfterMove(t *testing.T) {
	c := wtS2Edge()
	c.pathFull = false
	at := wtS2Wakes(c, wtT0, [][2]uint64{{1000, 0}, {1000, 500}}, nil)
	if !c.pathFull {
		t.Fatal("a cut for holes left the path not full")
	}
	c.movedAt = at.Add(100 * time.Millisecond) // a drain for the queue since
	wtS2Wakes(c, at, [][2]uint64{{1000, 0}, {1000, 0}, {1000, 0}, {1000, 0}}, map[int]wcSnd{3: wtS2Rec(0)})
	if c.cnt.undo != 1 || !c.pathFull {
		t.Errorf("taken back after a drain for the queue: undo %d pathFull %v, want 1 and the path still full", c.cnt.undo, c.pathFull)
	}
}

// F6: the undo's line (per link, its 30 s cap) and the minute's summary
// past wcAggLinks links, the link-end note's "for loss" with the cuts taken
// back in it, and the trace's ooo_ro and undo.
func TestWcoreS2UndoLines(t *testing.T) {
	ws := &wcSet{}
	var logs wcLogs
	logs.set(ws)
	now := time.Unix(1e9, 0)
	undo := wcEvent{kind: wcEvUndo, from: 2 << 20, B: 8 << 20, n: 1, T: 72 * time.Millisecond, loss: 0.00002}
	l := wcLogLink("link 3", wcEdge)
	ws.logEvents(l, now, wcOut{events: []wcEvent{undo}})
	want := "mtcp: link 3: the other server resent 0.002% — the holes were reordering, not loss; download windows back 2.0 MiB → 8.0 MiB"
	if got := logs.all(); len(got) != 1 || got[0] != want {
		t.Fatalf("the undo's line: %q, want %q", got, want)
	}
	ws.logEvents(l, now.Add(20*time.Second), wcOut{events: []wcEvent{undo}})
	if n := logs.count("reordering, not loss"); n != 1 {
		t.Errorf("a second undo 20 s on: %d lines, want 1 (its cap: 30 s)", n)
	}
	// past wcAggLinks links in a minute: one summary
	ws = &wcSet{}
	logs = wcLogs{}
	logs.set(ws)
	for i := 0; i < wcAggLinks+3; i++ {
		ws.logEvents(wcLogLink("link "+string(rune('a'+i)), wcEdge), now.Add(time.Duration(i)*time.Second), wcOut{events: []wcEvent{undo}})
	}
	ws.flushAgg(now.Add(2 * time.Minute))
	if n := logs.count("reordering, not loss"); n != wcAggLinks {
		t.Errorf("%d links' undos in a minute: %d lines, want %d", wcAggLinks+3, n, wcAggLinks)
	}
	sum := "mtcp: window core: 3 more link(s) took back 3 drain(s) for holes in the last minute (the other server resent no more than its level: reordering)"
	if logs.count(sum) != 1 {
		t.Errorf("the minute's summary: %q, want %q", logs.all(), sum)
	}
	if logs.count("resent nothing") != 0 {
		t.Errorf("a line says the other server resent nothing: %q", logs.all())
	}

	// the link-end note
	n := wcLogLink("link 4", wcEdge)
	st := &n.inStats
	st.active.Store(true)
	st.drainQ.Store(3)
	st.drainLoss.Store(5)
	st.undo.Store(2)
	st.oooRo.Store(math.Float64bits(0.6))
	note := wcoreNote(&linkMeter{wc: n})
	if w := "drained 3 time(s) for queue, 5 for loss (2 of them for holes taken back: the other server resent no more than its level; reordering level 60%), held by"; !strings.Contains(note, w) {
		t.Errorf("the link-end note %q, want %q in it", note, w)
	}
	st.undo.Store(0)
	if note := wcoreNote(&linkMeter{wc: n}); strings.Contains(note, "taken back") || !strings.Contains(note, "5 for loss, held by") {
		t.Errorf("no undo: the link-end note %q", note)
	}

	// the trace: the level as it holds now, and the undos so far
	c := n.core
	c.oooRo, c.oooRoAt, c.cnt.undo = 0.6, now, 2
	tl := n.traceOf(now.Add(time.Second), wcOut{})
	b, _ := json.Marshal(tl)
	if tl.OooRo != 0.6 || tl.Undo != 2 || !strings.Contains(string(b), `"ooo_ro":0.6`) || !strings.Contains(string(b), `"undo":2`) {
		t.Errorf("trace: ooo_ro %v undo %d (%s), want 0.6 and 2", tl.OooRo, tl.Undo, b)
	}
	if tl := n.traceOf(now.Add(wcQcapTTL), wcOut{}); tl.OooRo != 0 {
		t.Errorf("trace wcQcapTTL on: ooo_ro %v, want 0 (expired)", tl.OooRo)
	}
}
