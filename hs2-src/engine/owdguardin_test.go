package engine

import (
	"testing"
	"time"
)

// R1 round 5 (owd.go, OWDGuardIn): a QUIET link socket that received
// segments out of order since the beat before reads the other end's
// direction as none for a while (its messages came behind a hole); with
// no new hole the reading comes back; with the switch off it never goes.
func TestOWDGuardIn(t *testing.T) {
	for _, on := range []int{0, 1} {
		withTune(t, OWDGuardIn, on)
		o := &owdLink{quiet: true}
		ts := tcpStat{rtt: 75000, oooOK: true, rcvOOO: 10}
		fresh := func() {
			o.qIn.Store(int64(8 * time.Millisecond))
			o.qInAt.Store(ctrlNow())
		}
		o.noteSocket(ts, ctrlNow()) // the first beat: a baseline only
		fresh()
		if _, ok := o.queueIn(); !ok {
			t.Fatalf("on %d: no reading before any hole", on)
		}
		ts.rcvOOO += 3 // a hole since the beat before
		o.noteSocket(ts, ctrlNow())
		fresh()
		_, ok := o.queueIn()
		if on == 1 && ok {
			t.Fatal("a reading behind a hole")
		}
		if on == 0 && !ok {
			t.Fatal("the switch off took the reading")
		}
		if on == 1 && o.inDirties.Load() != 1 {
			t.Fatalf("in-dirty beats %d", o.inDirties.Load())
		}
		// past the hold, no new hole: back
		o.inDirtyUntil.Store(ctrlNow() - 1)
		o.noteSocket(ts, ctrlNow())
		fresh()
		if _, ok := o.queueIn(); !ok {
			t.Fatalf("on %d: no reading once the hold passed", on)
		}
	}
}
