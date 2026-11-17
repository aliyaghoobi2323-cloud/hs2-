package engine

import (
	"log"
	"sync"
	"time"
)

// Test-only race fix (go test -race): the process-wide rescue sweeper (one
// goroutine, started by the first rescue test through rescueSweepOnce) reads
// rrRingCap (rrMemCap in its 10 s log line, rrCapFor in rrSweep) while test
// helpers change rrRingCap for their own run. In the test binary the sweeper
// is this copy of rescueSweeper (the same calls on the same period), started
// at init through the same Once so the production one never starts here; it
// holds rescueKnobMu for reading while it works, and setRRRingCap takes it for
// writing. Production code is unchanged. Keep this loop in step with
// rescueSweeper (mconn_rescue.go).
var rescueKnobMu sync.RWMutex

func init() { rescueSweepOnce.Do(func() { go rescueSweeperTest() }) }

func rescueSweeperTest() {
	var last string
	for i := 1; ; i++ {
		time.Sleep(time.Second)
		rescueKnobMu.RLock()
		rescueSweep(time.Now())
		rrSweep(time.Now())
		if i%10 == 0 {
			if l := rescueLogLine() + " | " + rescueRecLogLine() + " | " + rescue145LogLine() + " | " + rescue169LogLine() + " | " + rescueRstLogLine() + " | " + darkcallLogLine(); l != last {
				last = l
				log.Print(l)
			}
		}
		rescueKnobMu.RUnlock()
	}
}

// setRRRingCap: rrRingCap = v for this test (restored on cleanup), in step
// with the sweeper.
func setRRRingCap(t interface{ Cleanup(func()) }, v int) {
	rescueKnobMu.Lock()
	old := rrRingCap
	rrRingCap = v
	rrRingCapDC.Store(int64(v))
	rescueKnobMu.Unlock()
	t.Cleanup(func() {
		rescueKnobMu.Lock()
		rrRingCap = old
		rrRingCapDC.Store(int64(rrRingCap))
		rescueKnobMu.Unlock()
	})
}

// setRRBigMax (rel68d, also hl229 race228): rrBigMax = v for this test
// (restored on cleanup), in step with the sweeper (its 10 s log line reads rrBigMax).
func setRRBigMax(t interface{ Cleanup(func()) }, v int64) {
	rescueKnobMu.Lock()
	old := rrBigMax
	rrBigMax = v
	rescueKnobMu.Unlock()
	t.Cleanup(func() {
		rescueKnobMu.Lock()
		rrBigMax = old
		rescueKnobMu.Unlock()
	})
}
