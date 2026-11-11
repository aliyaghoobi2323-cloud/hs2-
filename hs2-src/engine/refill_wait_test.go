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

// leaveHeldBounded on a waiter the hold already answered keeps that placement.
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
	ml, done := r.m.leaveHeldBounded(w)
	if !done || ml == nil || ml.link != l {
		t.Fatal("answered placement lost")
	}
	if u := r.m.users.Load(); u != 2 {
		t.Fatalf("%d users, want 2", u)
	}
}

// At the bound a waiter goes only to a link under twice the fair-share cap;
// when every link is that full it stays queued.
func TestRefillWaitBoundCap(t *testing.T) {
	r := newRefillRig(t, 100, 1)
	l := r.addLink()
	for i := 0; i < 2; i++ { // cap 1 (per_link 1, few connections): 2 = 2x cap
		r.m.mu.Lock()
		ml := r.m.mlOf2(l)
		r.m.placeLocked(ml)
		r.m.mu.Unlock()
	}
	_, _, ok, w := r.m.pickHeld()
	if ok || w == nil {
		t.Fatal("not queued")
	}
	if ml, done := r.m.leaveHeldBounded(w); done || ml != nil {
		t.Fatal("placed on a link at twice the cap")
	}
	if len(r.m.hold.queue) != 1 {
		t.Fatal("waiter left the queue")
	}
	l2 := r.addLink()
	r.m.mu.Lock()
	r.m.placeLocked(r.m.mlOf2(l2)) // under the cap x2 but not under the cap
	r.m.mu.Unlock()
	ml, done := r.m.leaveHeldBounded(w)
	if !done || ml == nil || ml.link != l2 {
		t.Fatal("not placed on the link with room under twice the cap")
	}
	if len(r.m.hold.queue) != 0 || r.usersOn(l2) != 2 {
		t.Fatal("queue or count wrong")
	}
}

func (m *LinkManager) mlOf2(l Link) *managedLink {
	for _, ml := range m.links {
		if ml.link == l {
			return ml
		}
	}
	return nil
}
