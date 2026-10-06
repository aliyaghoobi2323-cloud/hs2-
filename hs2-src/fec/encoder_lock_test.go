package fec

import (
	"testing"
	"time"
)

// A carrier's emit blocks while its pacer is full, inside Encode, holding the
// encoder's lock. The feedback that sets the loss estimate, and the status
// that reads the counters and the parity ratio, must not wait for it: the
// feedback runs on the carrier's receive loop, and the status on the pool's.
func TestEncoderReadersNotBlockedByEmit(t *testing.T) {
	e := NewEncoder(Config{K: 4})
	in, release := make(chan struct{}), make(chan struct{})
	go e.Encode(make([]byte, 100), time.Now(), func([]byte) {
		close(in)
		<-release // the pacer is full
	})
	<-in
	done := make(chan EncoderStats)
	go func() {
		e.SetLoss(0.2)
		_ = e.ParityRatio()
		done <- e.Stats()
	}()
	select {
	case st := <-done:
		if st.Data != 1 || st.DataBytes != 100 {
			t.Errorf("counters while an emit waits: %+v", st)
		}
		if e.Loss() != 0.2 {
			t.Errorf("loss %v", e.Loss())
		}
	case <-time.After(time.Second):
		t.Fatal("SetLoss/Stats/ParityRatio waited for an emit blocked in Encode")
	}
	close(release)
	// The estimate set meanwhile sizes the next group's parity.
	var parity int
	for i := 0; i < 4; i++ {
		e.Encode(make([]byte, 100), time.Now(), func(p []byte) {
			if h, _ := parseHeader(p); h.r > 0 {
				parity++
			}
		})
	}
	if want := e.parityFor(4); parity != want || want == 0 {
		t.Fatalf("%d parity shards for a group at 20%% loss, want %d", parity, want)
	}
}
