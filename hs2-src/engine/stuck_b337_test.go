package engine

import (
	"testing"
	"time"
)

// b337: links of a path stalled whole (a cut path coming back) are not
// judged stuck before stuckPathWait; a link throttled alone on a path whose
// other links answer still is, and with the key off the old rule holds.
func TestB337PathStallHeld(t *testing.T) {
	for _, on := range []bool{true, false} {
		old := b337PathStall.Load()
		b337PathStall.Store(on)
		r := newStuckRig(t, 30)
		for _, g := range r.good {
			g.m.path = "10.0.0.2>10.0.1.1"
		}
		var fs []*meteredFakeLink
		var mls []*managedLink
		for i := 0; i < 3; i++ { // the whole of the cut path
			f, ml := r.add()
			f.m.path = "10.0.0.2>10.0.9.1"
			fs, mls = append(fs, f), append(mls, ml)
		}
		lone, loneML := r.add() // throttled alone on a working path
		lone.m.path = "10.0.0.2>10.0.1.1"
		r.step(nil, 0)
		for i := 0; i < stuckStreak; i++ {
			for _, f := range fs {
				waiting(f, 9*time.Second)
			}
			waiting(lone, 9*time.Second)
			r.step(nil, 0)
		}
		if !loneML.degraded {
			t.Fatalf("on=%v: a link stuck alone on a working path was not judged", on)
		}
		held := 0
		for _, ml := range mls {
			if !ml.degraded {
				held++
			}
		}
		if on && held != len(mls) {
			t.Fatalf("on: %d of %d links of a whole-path stall judged stuck before %s", len(mls)-held, len(mls), stuckPathWait)
		}
		if !on && held != 0 {
			t.Fatalf("off: %d links of a whole-path stall held (want the old rule)", held)
		}
		if on {
			if r.lg.count("held: their path is stalled whole") != 1 {
				t.Fatalf("no held line:\n%s", r.lg)
			}
			// Still waiting at stuckPathWait: a path throttled for all is judged.
			for _, f := range fs {
				waiting(f, stuckPathWait+time.Second)
			}
			r.step(nil, 0)
			for k, ml := range mls {
				if !ml.degraded {
					t.Fatalf("link %d of a path stalled %s+ not judged", k, stuckPathWait)
				}
			}
		}
		b337PathStall.Store(old)
	}
}

func TestB337StalledPaths(t *testing.T) {
	per := map[string]*pathStallCount{
		"a": {wait: 3, prompt: 1}, // stalled
		"b": {wait: 2, prompt: 2}, // not: as many answer
		"c": {wait: 1, prompt: 0}, // not: one link
		"":  {wait: 9, prompt: 0}, // never: no path
	}
	s := b337StalledPaths(per)
	if len(s) != 1 || !s["a"] {
		t.Fatalf("stalled = %v, want only a", s)
	}
}
