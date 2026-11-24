package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestB457PickCap: with HS2_B457_PICKCAP=n at most n holders at once, a
// waiter gets a slot when one is released, a done ctx frees a waiter without a
// slot, and the key off is a no-op.
func TestB457PickCap(t *testing.T) {
	defer b457SetPickCap(0)
	b457SetPickCap(0)
	for i := 0; i < 100; i++ {
		b457PickAcquire(context.Background()) // off: never blocks
	}
	b457SetPickCap(2)
	var cur, peak atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel := b457PickAcquire(context.Background())
			n := cur.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			cur.Add(-1)
			rel()
		}()
	}
	wg.Wait()
	if p := peak.Load(); p > 2 || p < 1 {
		t.Fatalf("peak holders %d, want 1..2", p)
	}
	r1 := b457PickAcquire(context.Background())
	r2 := b457PickAcquire(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan struct{})
	go func() { b457PickAcquire(ctx)(); close(got) }()
	select {
	case <-got:
		t.Fatal("third acquire did not wait")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("done ctx did not free the waiter")
	}
	r1()
	r3 := b457PickAcquire(nil)
	r3()
	r2()
}

// rc14: HS2_B457_PICKCAP defaults to 2; "0" turns it off.
func TestB457PickCapDefault(t *testing.T) {
	for v, want := range map[string]int{"": 2, " ": 2, "x": 2, "0": 0, "-1": -1, "1": 1, "4": 4} {
		if got := b457PickCapEnv(v); got != want {
			t.Fatalf("b457PickCapEnv(%q) = %d, want %d", v, got, want)
		}
	}
}
