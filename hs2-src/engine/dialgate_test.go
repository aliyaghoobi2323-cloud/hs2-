package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// instantGate is a dial gate with no spacing, for unit tests of pool logic.
func instantGate() *dialGate { return newDialGate(gateInflight, func() time.Duration { return 0 }) }

// settle waits until every dial the pool queued has finished.
func settle(m *LinkManager) { m.dialWG.Wait() }

// The gate never lets more than its in-flight limit dial at once, and spaces
// successive starts by at least the gap.
func TestDialGatePacesAndBounds(t *testing.T) {
	const gap = 20 * time.Millisecond
	g := newDialGate(3, func() time.Duration { return gap })
	var mu sync.Mutex
	var starts []time.Time
	var cur, peak atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, ok := g.acquire(context.Background())
			if !ok {
				t.Error("acquire failed")
				return
			}
			mu.Lock()
			starts = append(starts, time.Now())
			mu.Unlock()
			n := cur.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(50 * time.Millisecond) // the handshake
			cur.Add(-1)
			release()
		}()
	}
	wg.Wait()
	if p := peak.Load(); p > 3 {
		t.Fatalf("%d dials at once, limit 3", p)
	}
	mu.Lock()
	defer mu.Unlock()
	first, last := starts[0], starts[0]
	for _, s := range starts {
		if s.Before(first) {
			first = s
		}
		if s.After(last) {
			last = s
		}
	}
	if span := last.Sub(first); span < 11*gap-5*time.Millisecond {
		t.Fatalf("12 starts within %s, want them spaced by >= %s", span, gap)
	}
}

// A dial waiting for its turn gives up when its context ends, and frees its
// place.
func TestDialGateCancel(t *testing.T) {
	g := newDialGate(1, func() time.Duration { return time.Hour })
	r1, ok := g.acquire(context.Background())
	if !ok {
		t.Fatal("first acquire failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, ok := g.acquire(ctx); ok {
		t.Fatal("acquire succeeded while the only place was taken")
	}
	r1()
	if g.inflight() != 0 {
		t.Fatalf("inflight %d after release", g.inflight())
	}
}
