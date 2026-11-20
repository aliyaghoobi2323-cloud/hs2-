package engine

import (
	"testing"
	"time"
)

// b406 (sk405): a path STALLPICK has marked is no callmove target while the
// mark holds; the judge's smoothed loss still reads it as the best path.
func TestB406CallMoveSkipsStallMarked(t *testing.T) {
	setB399(t, true)
	m, _ := pathLinks("a>x", "a>x", "b>x", "b>x", "c>x", "c>x")
	m.steer = newSteerState()
	m.steer.judge = newQ2Judge(0, "Iran", "kharej", func(string, ...any) {})
	m.steer.judge.p["a>x"] = &q2Path{have: true, ewma: 0.004}
	m.steer.judge.p["b>x"] = &q2Path{have: true, ewma: 0.0005} // lowest loss, freshly cut
	m.steer.judge.p["c>x"] = &q2Path{have: true, ewma: 0.001}
	old := cmQ.Load()
	t.Cleanup(func() { cmQ.Store(old) })
	snap := func() *cmSnap {
		m.mu.Lock()
		defer m.mu.Unlock()
		byPath := map[string][]*managedLink{}
		for _, ml := range m.links {
			byPath[mlPath(ml)] = append(byPath[mlPath(ml)], ml)
		}
		m.cmSnapLocked(byPath, time.Now())
		return cmQ.Load()
	}
	mark := func(on bool) { // STALLPICK marks every link of b
		for _, ml := range m.links {
			if mlPath(ml) == "b>x" {
				ml.b399Stall.Store(on)
			}
		}
	}
	if s := snap(); !s.p["b>x"].ok || !s.p["c>x"].ok {
		t.Fatalf("no mark: b and c eligible, got %+v", s.p)
	}
	mark(true)
	k0 := cmStallSkip.Load()
	s := snap()
	if s.p["b>x"].ok {
		t.Fatalf("stall-marked path b is a callmove target: %+v", s.p)
	}
	if !s.p["c>x"].ok || !s.p["a>x"].ok {
		t.Fatalf("unmarked paths lost eligibility: %+v", s.p)
	}
	if cmStallSkip.Load() == k0 {
		t.Fatal("stall-skip counter did not move")
	}
	// mark clears: b is a target again
	mark(false)
	if s := snap(); !s.p["b>x"].ok {
		t.Fatalf("cleared mark: b still ineligible: %+v", s.p)
	}
	// key off: a stale mark is ignored
	mark(true)
	b399On.Store(false)
	if s := snap(); !s.p["b>x"].ok {
		t.Fatalf("key off: stale mark made b ineligible: %+v", s.p)
	}
}
