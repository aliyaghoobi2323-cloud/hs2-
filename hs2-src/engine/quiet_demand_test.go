package engine

import (
	"testing"
	"time"
)

// A connection flowing on a QUIET link counts in the pool's demand and in what
// it shows, while G, what the pool's rate rules read, stays its own links'.
func TestQuietFlowsCountInPoolDemand(t *testing.T) {
	r := newQuietRig(t, 12.5e6, 5*time.Millisecond, false)
	st := r.open(1, qpEcho)
	defer st.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		b := make([]byte, 2<<10) // under the move rule's 4 KiB a tick
		go func() {
			rb := make([]byte, 4<<10)
			for {
				if _, err := st.Read(rb); err != nil {
					return
				}
			}
		}()
		for {
			select {
			case <-done:
				return
			case <-time.After(100 * time.Millisecond):
				if _, err := st.Write(b); err != nil {
					return
				}
			}
		}
	}()
	var flowing int
	var quietG float64
	for i := 0; i < 40; i++ {
		time.Sleep(250 * time.Millisecond)
		r.lm.sampleHealth()
		flowing, quietG = r.lm.sample.flowing, r.lm.sample.quietG
		if flowing >= 1 && quietG > 0 {
			break
		}
	}
	if flowing < 1 || quietG <= 0 {
		t.Fatalf("a connection flowing on QUIET: pool sees %d flowing, %.0f B/s", flowing, quietG)
	}
	if got := r.q.moved.Load(); got != 0 {
		t.Fatalf("the connection moved to BULK (%d): this test needs it on QUIET", got)
	}
	if g := r.lm.sample.G; g >= quietG {
		t.Errorf("G %.0f B/s took in the QUIET link's %.0f B/s", g, quietG)
	}
	r.lm.publishStats()
	if st := r.lm.stats.Load(); st.Flowing < 1 || st.MbitPerS <= 0 {
		t.Errorf("shown: %d flowing, %.3f Mbit/s", st.Flowing, st.MbitPerS)
	}
}
