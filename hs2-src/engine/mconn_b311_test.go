package engine

import (
	"sync"
	"testing"
	"time"
)

// b311: a heavy connection's (striped or bulk) sub closes synchronously even
// with the async close on; a light one stays async; HEAVYSYNC=0 is b307.
func TestB311HeavyCloseSync(t *testing.T) {
	for _, tc := range []struct {
		name      string
		heavy     bool
		heavySync bool
		wantSync  bool
	}{
		{"light", false, true, false},
		{"heavy", true, true, true},
		{"heavy-key-off", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b311HeavySync.Store(tc.heavySync)
			defer b311HeavySync.Store(true)
			rw := &blockRW{release: make(chan struct{})}
			s := b307Sub(rw)
			if tc.heavy {
				s.m.mu.Lock()
				s.m.stripeJoinLocked(s) // marks the connection heavy
				s.m.mu.Unlock()
			}
			if s.m.b311Heavy.Load() != tc.heavy {
				t.Fatalf("heavy mark %v", s.m.b311Heavy.Load())
			}
			ret := make(chan struct{})
			go func() { s.close(); close(ret) }()
			select {
			case <-ret:
				if tc.wantSync {
					t.Fatal("heavy close returned before its stream closed")
				}
			case <-time.After(200 * time.Millisecond):
				if !tc.wantSync {
					t.Fatal("light close blocked")
				}
			}
			close(rw.release)
			<-ret
			for i := 0; i < 200 && !s.rwClosed.Load(); i++ {
				time.Sleep(5 * time.Millisecond)
			}
			if !s.rwClosed.Load() || rw.calls.Load() != 1 {
				t.Fatalf("rwClosed=%v calls=%d", s.rwClosed.Load(), rw.calls.Load())
			}
		})
	}
}

// Marking a connection heavy is race-free against its subs' closes.
func TestB311HeavyMarkRace(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		rw := &blockRW{release: make(chan struct{})}
		close(rw.release)
		s := b307Sub(rw)
		wg.Add(2)
		go func() { defer wg.Done(); s.m.b311MarkHeavy() }()
		go func() { defer wg.Done(); s.close() }()
	}
	wg.Wait()
	if b311CloseLine() == "" {
		// at least one close may have seen the mark; the line is informative only
		t.Log("no heavy-sync close counted (ordering)")
	}
}
