package engine

import (
	"context"
	"testing"
	"time"
)

// HS2_REFILL_WAIT_MS: a connection waits in the start hold at most the bound,
// then takes the least-loaded link; with the key off it keeps waiting.
func TestRefillWaitBound(t *testing.T) {
	for _, on := range []bool{false, true} {
		old := refillWait
		refillWait = 0
		if on {
			refillWait = 50 * time.Millisecond
		}
		cut0 := refillWaitCut.Load()
		m := NewLinkManager(nil, 1, 4, 1, nil)
		m.target.Store(4)
		m.hold.tick = 10 * time.Millisecond
		ctx, cancel := context.WithCancel(context.Background())
		m.scaleCtx = ctx
		m.AddLink(&fakeLink{alive: true}, "test")
		if _, _, ok := pickWait(ctx, m, true); !ok {
			t.Fatal("first connection not placed")
		}
		res := make(chan time.Duration, 1)
		done := make(chan struct{})
		start := time.Now()
		go func() {
			defer close(done)
			if _, _, ok := pickWait(ctx, m, true); ok {
				res <- time.Since(start)
			}
		}()
		select {
		case d := <-res:
			if !on {
				t.Fatalf("key off: placed after %v while the hold should keep it", d)
			}
			if d < 40*time.Millisecond || d > time.Second {
				t.Fatalf("placed after %v, want ~50ms", d)
			}
			if refillWaitCut.Load() != cut0+1 {
				t.Fatal("cut not counted")
			}
			if u := m.users.Load(); u != 2 {
				t.Fatalf("%d users counted, want 2", u)
			}
			m.mu.Lock()
			q := len(m.hold.queue)
			m.mu.Unlock()
			if q != 0 {
				t.Fatal("waiter left in the queue")
			}
		case <-time.After(300 * time.Millisecond):
			if on {
				t.Fatal("key on: still waiting past the bound")
			}
		}
		cancel()
		<-done
		m.hold.wg.Wait()
		refillWait = old
	}
}

// leaveHeld on a waiter the hold already answered keeps that placement.
func TestRefillLeaveHeldAnswered(t *testing.T) {
	r := newRefillRig(t, 4, 1)
	r.addLink()
	if _, _, ok, _ := r.m.pickHeld(); !ok {
		t.Fatal("first not placed")
	}
	_, _, _, w := r.m.pickHeld()
	if w == nil {
		t.Fatal("not queued")
	}
	l := r.addLink()
	r.step()
	ml := r.m.leaveHeld(w)
	if ml == nil || ml.link != l {
		t.Fatal("answered placement lost")
	}
	if u := r.m.users.Load(); u != 2 {
		t.Fatalf("%d users, want 2", u)
	}
}
