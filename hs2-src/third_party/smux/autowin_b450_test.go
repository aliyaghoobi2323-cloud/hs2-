package smux

import (
	"testing"
	"time"
)

// b450: a stream that never grew skips the budget's process-wide lock on
// release, and a tracked one leaves the budget's set.
func TestB450UntrackSkipsUntracked(t *testing.T) {
	b := NewWindowBudget(1 << 20)
	s := &Stream{aw: &streamAW{}}
	b.mu.Lock()
	done := make(chan struct{})
	go func() { b.untrack(s); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		b.mu.Unlock()
		t.Fatal("untrack of a never-tracked stream waited on the budget lock")
	}
	b.mu.Unlock()

	b.mu.Lock()
	b.running = true // no sweeper in this test
	b.streams[s] = struct{}{}
	s.aw.tracked.Store(true)
	b.mu.Unlock()
	b.untrack(s)
	if b.Stats().Streams != 0 || s.aw.tracked.Load() {
		t.Fatalf("tracked stream not removed: streams %d tracked %v", b.Stats().Streams, s.aw.tracked.Load())
	}
}

// b450: closing a stream releases its window after the session's
// streamLock: while the budget lock is busy (a mass close elsewhere), the
// session's streamLock (its recvLoop) stays free.
func TestB450CloseReleasesOutsideStreamLock(t *testing.T) {
	aw := newAW(64<<20, 1<<20)
	cli, _ := pair(t, time.Millisecond, awConfig(aw), awConfig(nil))
	st, err := cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if st.aw == nil {
		t.Skip("no auto window on the opened stream")
	}
	b := aw.Budget
	b.mu.Lock()
	b.running = true
	b.streams[st] = struct{}{}
	st.aw.tracked.Store(true) // as if it had grown
	closed := make(chan struct{})
	go func() { st.Close(); close(closed) }()
	free := false
	for i := 0; i < 200 && !free; i++ {
		time.Sleep(5 * time.Millisecond)
		if !cli.streamLock.TryLock() {
			continue // the old order: held while waiting on the budget
		}
		_, in := cli.streams[st.id]
		cli.streamLock.Unlock()
		free = !in // the close got past the map under streamLock
	}
	b.mu.Unlock()
	<-closed
	if !free {
		t.Fatal("stream close held streamLock while the budget lock was busy")
	}
	if b.Stats().Streams != 0 {
		t.Fatalf("budget still tracks the closed stream")
	}
}
