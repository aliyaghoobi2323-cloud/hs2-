package engine

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/obfs"
)

// R1's event tallies (events.go): the runner takes what each link's own
// counters gained since its wake before, once (OWD guard's dirty beats of
// the link meter, the BULK budget's cuts, a QUIET link's writes and those
// on a record already started), and keeps them past the link's end; the
// process's own (UDP late drops, TUN moves, the exit's attached moves and
// sent hints, the edge pool's moves and heard hints) are read as they are.
func TestEventsTallies(t *testing.T) {
	var e r1Events
	l := &coreLink{mtr: &linkMeter{}, sc: &sendCapLink{budget: &linkBudget{}}, shp: &shapedConn{count: true}}
	l.mtr.owd.dirties.Add(3)
	l.sc.budget.core.cuts = 2
	l.shp.writes.Add(10)
	l.shp.joined.Add(4)
	base := e.now()
	l.noteEvents(&e)
	l.noteEvents(&e) // nothing new: nothing added
	d := e.now().since(base)
	if d.owdDirty != 3 || d.budgetCuts != 2 || d.writes != 10 || d.joined != 4 {
		t.Fatalf("after the first wake: %+v", d)
	}
	l.mtr.owd.dirties.Add(1)
	l.sc.budget.core.cuts++
	l.noteEvents(&e)
	if d := e.now().since(base); d.owdDirty != 4 || d.budgetCuts != 3 || d.writes != 10 {
		t.Fatalf("after the second wake: %+v", d)
	}
	// a link of no class, without a budget or a meter: nothing, no panic
	(&coreLink{sc: &sendCapLink{}}).noteEvents(&e)
	// the process's own
	p0 := e.now()
	udpLateDrops.Add(5)
	tunMoved.Add(2)
	exitGroups.attached.Add(1)
	exitHints.sent.Add(7)
	if d := e.now().since(p0); d.udpLate < 5 || d.tun < 2 || d.exitMoved < 1 || d.hintsTx < 7 {
		t.Fatalf("the process's tallies: %+v", d)
	}
	line := e.line()
	t.Log(line)
	for _, part := range []string{"events since start: udp late drops ", "; owd dirty 4; budget cuts 3; quiet writes 10, 4 of them on a record already started (records saved); moved to bulk ",
		"(back to quiet ", "tun flows ", "attached at the exit ", "move hints heard ", ", sent "} {
		if !strings.Contains(line, part) {
			t.Fatalf("line %q lacks %q", line, part)
		}
	}
	if q := placeSet.Load(); q == nil {
		// no edge pool in this process yet: the moves and heard hints read 0
		if s := e.now(); s.moved != 0 || s.movedBack != 0 || s.hintsHeard != 0 {
			t.Fatalf("no QUIET set, and moves %d back %d hints %d", s.moved, s.movedBack, s.hintsHeard)
		}
	}
}

// One log line a minute with what moved in it, none when nothing did (§7:
// never a line an event, however many).
func TestEventsMinuteLine(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	f := func(format string, a ...any) {
		if s := fmt.Sprintf(format, a...); strings.HasPrefix(s, "events in the last minute: ") {
			mu.Lock()
			lines = append(lines, s)
			mu.Unlock()
		}
	}
	got := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lines...)
	}
	var e r1Events
	e.logMinute(f) // (whatever the process tallied before: its first line)
	n := len(got())
	e.logMinute(f)
	if len(got()) != n {
		t.Fatalf("a line with nothing new: %q", got()[n:])
	}
	for i := 0; i < 1000; i++ {
		e.owdDirty.Add(1)
		e.budgetCuts.Add(2)
	}
	e.quietWrites.Add(30)
	e.quietJoined.Add(20)
	e.logMinute(f)
	ls := got()
	if len(ls) != n+1 {
		t.Fatalf("%d lines for a minute of 3000 events, want 1", len(ls)-n)
	}
	want := "owd dirty 1000, budget cuts 2000, quiet records saved 20 of 30 writes"
	if !strings.Contains(ls[n], want) {
		t.Fatalf("%q lacks %q", ls[n], want)
	}
	t.Log(ls[n])
}

// A QUIET link's records line counts its writes, and those that rode a
// record already started (records saved by the batching): five small
// messages, then a flush, are one record, four of them joined.
func TestQuietRecordsSaved(t *testing.T) {
	cc := &captureConn{}
	w := newShapedConn(cc, obfs.NewHTTPSLengthSampler())
	w.carry, w.count = true, true
	for i := 0; i < 5; i++ {
		w.Write(make([]byte, 2))
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if r := w.recs.Load(); r != 1 || w.writes.Load() != 5 || w.joined.Load() != 4 {
		t.Fatalf("%d records, %d writes, %d joined; want 1, 5, 4", r, w.writes.Load(), w.joined.Load())
	}
	line := w.recLine(1, 0)
	if !strings.Contains(line, "; 5 writes, 4 on a record already started)") {
		t.Fatalf("%q", line)
	}
	t.Log(line)
	// a BULK link's shaper (no count): nothing tallied
	b := newShapedConn(&captureConn{}, obfs.NewHTTPSLengthSampler())
	b.carry = true
	b.Write(make([]byte, 2))
	b.Write(make([]byte, 2))
	if b.writes.Load() != 0 || b.joined.Load() != 0 {
		t.Fatal("an uncounted shaper tallied its writes")
	}
}
