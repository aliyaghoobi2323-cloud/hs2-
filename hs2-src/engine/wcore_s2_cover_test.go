package engine

import (
	"testing"
	"time"
)

// With smux's covers on, a cut for holes alone finds the path full
// (pathFull): the core's floor is the need again and it takes the session's
// covers (wcOut.noCover: Session.SetCover(false)). The record that takes the
// cut back (undoCut) gives back what the cut found: pathFull, the covers'
// floor and the covers (smux gives each stream the cover the core took at
// its next update: CoverHold, TestCoverBackAfterSetCover). A record that
// shows the sender's loss keeps them taken, and a cut that found the path
// full already gives back none.
func TestWcoreS2UndoGivesTheCoversBack(t *testing.T) {
	hole := [][2]uint64{{1000, 0}, {1000, 500}}
	clean := [][2]uint64{{1000, 0}, {1000, 0}, {1000, 0}, {1000, 0}}
	cover := uint32(wtNeed + wtNeed/2)
	steps := func(c *wcore, at time.Time, wakes [][2]uint64, rec map[int]wcSnd) (time.Time, wcOut) {
		var o wcOut
		for i, w := range wakes {
			at = at.Add(100 * time.Millisecond)
			in := wtWake(c, at, w[0], w[1])
			if s, ok := rec[i]; ok {
				in.snd = s
			}
			o = c.step(in)
		}
		return at, o
	}
	for _, tc := range []struct {
		name    string
		full    bool   // the path found full before the holes
		retrans uint64 // the record's resent segments (of 100000)
		back    bool
	}{
		{"no loss: the cut taken back", false, 0, true},
		{"the sender's loss: the cut stands", false, 5000, false},
		{"no loss, the path full before the holes", true, 0, false},
	} {
		c := wtS2Edge()
		c.cfg.noCover, c.pathFull = false, tc.full
		at, o := steps(c, wtT0, hole, nil)
		if c.cnt.drainLoss != 1 || !c.pathFull || !o.noCover || c.cmin() != wtNeed || c.B != wtNeed {
			t.Fatalf("%s: the cut: drains %d pathFull %v covers taken %v floor %d B %.2f MiB, want a cut to the need, the path full and the covers taken",
				tc.name, c.cnt.drainLoss, c.pathFull, o.noCover, c.cmin(), float64(c.B)/wtMiB)
		}
		_, o = steps(c, at, clean, map[int]wcSnd{3: wtS2Rec(tc.retrans)})
		undone := tc.retrans == 0
		if undone != (c.cnt.undo == 1) {
			t.Fatalf("%s: undo %d", tc.name, c.cnt.undo)
		}
		if tc.back {
			if c.pathFull || o.noCover || c.cmin() != cover || c.B != 8*wtMiB {
				t.Errorf("%s: pathFull %v covers taken %v floor %d B %.2f MiB, want the path not full, the covers back, their floor %d and B at 8 MiB",
					tc.name, c.pathFull, o.noCover, c.cmin(), float64(c.B)/wtMiB, cover)
			}
		} else if !c.pathFull || !o.noCover || c.cmin() != wtNeed {
			t.Errorf("%s: pathFull %v covers taken %v floor %d, want the path full and the covers taken, the need as floor",
				tc.name, c.pathFull, o.noCover, c.cmin())
		}
	}
}
