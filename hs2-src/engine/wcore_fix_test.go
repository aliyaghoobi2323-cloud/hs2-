package engine

import (
	"testing"
	"time"
	"unsafe"
)

// Fixes of red team round 1's open findings (Phase Z5): one test per item,
// each failing before its change.

// Item 5a: the autopilot's freeze (apProbing) ends STARTUP where it is. No
// knee was found there: no "fill the path" line, and the path is not full
// (in stage 2 a full path takes c_min under need).
func TestWcoreFixApProbingStartup(t *testing.T) {
	for _, stage := range []int{1, 2} {
		c, in := wtStartup()
		c.cfg.stage = stage
		in.apProbing = true
		m := wtMeas(1, 36e6, 4*wtMiB)
		var ev []wcEvent
		c.startup(in, m, &ev)
		if c.state != wcHold {
			t.Errorf("stage %d: %v, want hold", stage, c.state)
		}
		for _, e := range ev {
			if e.kind == wcEvFilled {
				t.Errorf("stage %d: a %q event for a STARTUP the autopilot's probe stopped", stage, e)
			}
		}
		if c.pathFull || c.cmin() != wtNeed {
			t.Errorf("stage %d: pathFull %v, c_min %d: the freeze found no full path", stage, c.pathFull, c.cmin())
		}
	}
}

// wtWallJump is t with its wall clock moved by d and its monotonic reading
// kept (an NTP step or a VM's migration). ok is false if this Go's time.Time
// is not laid out as the test assumes (wall uint64 first, seconds since 1885
// in bits 30-62 when the monotonic bit is set).
func wtWallJump(t time.Time, d time.Duration) (time.Time, bool) {
	j := t
	w := (*uint64)(unsafe.Pointer(&j))
	if *w>>63 == 0 {
		return t, false
	}
	*w += uint64(d/time.Second) << 30
	return j, j.Sub(t) == 0 && j.Unix()-t.Unix() == int64(d/time.Second)
}

// Item 5b: the windowed minima (turnBase, the srtt fallback, q_ctrl's floor)
// count time as the rest of the core does, by the monotonic clock: a step of
// the wall clock keeps their history (on the wall clock an hour's step took
// all of turnBase's 60 s away, and its next sample, with the queue in it,
// was turnBase: a false "route change").
func TestWcoreFixMinWinMonotonic(t *testing.T) {
	base := time.Now()
	if _, ok := wtWallJump(base, time.Hour); !ok {
		t.Skip("time.Time's layout is not the one this test assumes")
	}
	var w wcMinWin
	for i := 0; i < 50; i++ {
		v := 130 * time.Millisecond
		if i == 10 {
			v = 100 * time.Millisecond
		}
		w.add(base.Add(time.Duration(i)*time.Second), v, wcTurnWin)
	}
	now := base.Add(50 * time.Second)
	if got := w.min(now, wcTurnWin); got != 100*time.Millisecond {
		t.Fatalf("min %v before the step, want 100ms", got)
	}
	for _, d := range []time.Duration{time.Hour, -time.Hour, 24 * time.Hour} {
		j, _ := wtWallJump(now, d)
		if got := w.min(j, wcTurnWin); got != 100*time.Millisecond {
			t.Errorf("wall clock stepped by %v: min %v, want 100ms (the 60 s of history kept)", d, got)
		}
	}
	// and a sample right after the step lands in the bucket of its own time
	j, _ := wtWallJump(now.Add(time.Second), time.Hour)
	w.add(j, 90*time.Millisecond, wcTurnWin)
	if got := w.min(now.Add(time.Second), wcTurnWin); got != 90*time.Millisecond {
		t.Errorf("a sample after the step: min %v, want 90ms", got)
	}
}

// Item 5c: a snapshot whose TCP_INFO could not be read has no counters of
// the socket; the next one's deltas are taken from the last one that had
// them, not from 0 (which made one wake's delta the link's whole lifetime).
func TestWcoreFixBuilderTCPInfoGap(t *testing.T) {
	var b wcBuilder
	b.build(wcLinkSnap{now: wtT0, segsIn: 100000, ooo: 900, oooOK: true, poolFree: -1})
	in := b.build(wcLinkSnap{now: wtT0.Add(100 * time.Millisecond), tcpNone: true, poolFree: -1})
	if in.segsIn != 0 || in.ooo != 0 || in.oooOK {
		t.Errorf("a wake without TCP_INFO: segsIn %d ooo %d oooOK %v, want nothing", in.segsIn, in.ooo, in.oooOK)
	}
	in = b.build(wcLinkSnap{now: wtT0.Add(200 * time.Millisecond), segsIn: 101000, ooo: 910, oooOK: true, poolFree: -1})
	if in.segsIn != 1000 || in.ooo != 10 {
		t.Errorf("after a wake without TCP_INFO: segsIn %d ooo %d, want 1000 and 10 (not the lifetime counters)", in.segsIn, in.ooo)
	}
}
