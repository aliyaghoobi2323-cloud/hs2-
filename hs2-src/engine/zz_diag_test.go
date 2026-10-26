package engine

import (
	"testing"
	"time"
)

func TestDiagNewHeavy(t *testing.T) {
	r := newABRig(t, abRate, 10*time.Millisecond, true)
	defer r.close()
	ls := r.exit.Queue().(*linkSched)
	r.probe(7, 200*time.Millisecond)
	r.download(1, 1)
	time.Sleep(3 * time.Second)
	r.download(2, 16)
	cb, ch := r.counter(r.down, 1), r.counter(r.down, 2)
	for s := 0; s < 10; s++ {
		b0, h0 := cb.Load(), ch.Load()
		floorT, n := 0, 0
		var rate float64
		for range 100 {
			ls.mu.Lock()
			st := ls.s.Stats()
			ls.mu.Unlock()
			if st.FloorUsers > 0 {
				floorT++
			}
			rate = st.Rate
			n++
			time.Sleep(10 * time.Millisecond)
		}
		b, h := float64(cb.Load()-b0), float64(ch.Load()-h0)
		t.Logf("s%d: B %.3f H %.3f Mbit/s (B share %.3f), C est %.3f Mbit/s, floor mode %d%%", s, b*8/1e6, h*8/1e6, b/(b+h), rate*8/1e6, floorT*100/n)
	}
}
