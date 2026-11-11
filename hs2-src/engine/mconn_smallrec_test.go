package engine

import (
	"testing"
	"time"
)

// A user app that stops reading while the other end keeps writing small
// messages (a backgrounded phone, a push channel): what the receive side holds
// must stay near what it counts (mRecvBuf of payload), not one 16 KB chunk per
// small record.
func TestMConnSmallRecordsHeldMemory(t *testing.T) {
	_, iran, exit := newTPair(t, true)
	defer closeAndWait(t, iran, exit)
	msg := make([]byte, 100)
	const N = 1500
	for i := 0; i < N; i++ {
		if _, err := iran.Write(msg); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Microsecond) // spaced: no two writes share a record
	}
	mWaitFor(t, "all bytes at the exit", 5*time.Second, func() bool {
		r := &exit.r
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.readyBytes == N*len(msg)
	})
	r := &exit.r
	r.mu.Lock()
	held := 0
	for _, e := range r.ready[r.head:] {
		if e.c != nil {
			held += cap(e.c.buf)
		} else {
			held += cap(e.b)
		}
	}
	payload, recs := r.readyBytes, len(r.ready)-r.head
	r.mu.Unlock()
	t.Logf("payload %d B in %d records holds %d B of chunks (x%.0f)", payload, recs, held, float64(held)/float64(payload))
	if held > 4*payload+64<<10 {
		t.Fatalf("payload %d B holds %d B of chunks (x%.0f)", payload, held, float64(held)/float64(payload))
	}
}
