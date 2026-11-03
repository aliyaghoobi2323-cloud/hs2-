package engine

import (
	"strings"
	"testing"
	"time"
)

func TestParseLane(t *testing.T) {
	for v, want := range map[string]int{"": 0, "0": 0, "off": 0, " 3 ": 3, "8": 8, "on": laneDefault, "yes": laneDefault, "99": laneMax, "-1": laneDefault} {
		if got := parseLane(v); got != want {
			t.Errorf("parseLane(%q) = %d, want %d", v, got, want)
		}
	}
}

// With a lane, while downloads run the autopilot keeps `lane` serving links
// free of them on top of the links that carry them; when the downloads end
// the extra links are held for the minute and then shrink away; without a
// lane the size follows the active flows exactly as before.
func TestAutopilotLane(t *testing.T) {
	run := func(lane int) (during, after int, notes []string) {
		a := newAutopilot(4, 32, 8)
		a.lane = lane
		a.T = 4 // a cold start, so the lane's growth shows
		now := time.Unix(1e9, 0)
		tick := func(downloads bool) {
			now = now.Add(healthTick)
			S := a.T
			smp := apSample{now: now, G: 1e6, flowing: 8, open: 40, growable: true}
			for id := 0; id < S; id++ {
				l := apLink{id: id, serving: true, rate: 1e6 / float64(S), rate10: 1e6 / float64(S), flowing: 2}
				if downloads && id < 4 {
					l.heavy = 1 // four links carry the 8 active flows' downloads
				}
				smp.links = append(smp.links, l)
			}
			if d := a.decide(smp); d.note != "" {
				notes = append(notes, d.note)
			}
		}
		for i := 0; i < 30; i++ { // a minute of downloads
			tick(true)
		}
		during = a.T
		for i := 0; i < 300; i++ { // ten quiet minutes
			tick(false)
		}
		return during, a.T, notes
	}
	d0, a0, _ := run(0)
	d3, a3, notes := run(3)
	if d3 != 4+3 {
		t.Fatalf("lane 3 with 4 download links: %d links, want 7 (notes %q)", d3, notes)
	}
	if d0 >= d3 {
		t.Fatalf("without a lane the pool should stay smaller: %d vs %d", d0, d3)
	}
	if a3 != a0 {
		t.Fatalf("after the downloads the lane's links should go: %d links, %d without a lane", a3, a0)
	}
	found := false
	for _, n := range notes {
		if strings.Contains(n, "kept free of them") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no log line says why the pool grew: %q", notes)
	}
}

// withLane sets the lane (and the placement it brings) for a test whatever
// HS2_PICK_LANE says, and returns the restore.
func withLane(n int) func() {
	oldLane, oldClass := pickLane, pickClass
	pickLane, pickClass = n, n > 0
	return func() { pickLane, pickClass = oldLane, oldClass }
}
