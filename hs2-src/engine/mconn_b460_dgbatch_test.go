package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func setDGBatch(t *testing.T, on bool) {
	t.Helper()
	old := b460DGBatch.Load()
	b460DGBatch.Store(on)
	t.Cleanup(func() { b460DGBatch.Store(old) })
}

// TestB460NoQueueAliveFalse: the old generation's links are still in the pool
// but none is Alive() (the process exited, DropLink has not run: no pool
// change, so b459's per-epoch scan cannot see it). A join pinned to it fails
// at once and never waits for a cap slot; a join pinned to the new one opens.
func TestB460NoQueueAliveFalse(t *testing.T) {
	setLite(t, false)
	setZDU(t, true)
	setOpenNoLock(t, true)
	for _, on := range []bool{true, false} {
		setDGBatch(t, on)
		b457SetPickCap(1)
		m, ls := zduPool(2, 0)
		e := zduNewEdge(m, 7)
		if _, _, err := e.open(planeLight, nil); err != nil {
			t.Fatal(err)
		}
		zduAddLinks(m, zduLink(zduInstNew))
		e2 := zduNewEdge(m, 8)
		if _, _, err := e2.open(planeLight, nil); err != nil {
			t.Fatal(err)
		}
		m.b460Live()           // a fresh live set with both generations
		for _, l := range ls { // the old process is gone; its links stay listed
			l.dead.Store(true)
		}
		hold := b457PickAcquire(context.Background()) // the cap is full
		f0, w0 := b460Stat.fast.Load(), b458Stat.wait.n.Load()
		time.Sleep(3 * b460Fresh)
		t0 := time.Now()
		res := make(chan error, 1)
		go func() { _, _, err := e.open(planeLight, nil); res <- err }()
		var err error
		select {
		case err = <-res:
		case <-time.After(400 * time.Millisecond):
		}
		d := time.Since(t0)
		hold()
		if !on {
			if d < 400*time.Millisecond {
				t.Fatalf("key off: the join returned in %v (%v), want it queued", d, err)
			}
			<-res
			e.end()
			e2.end()
			continue
		}
		if !errors.Is(err, errMConnNoLink) || d > 300*time.Millisecond {
			t.Fatalf("dead pin: err %v after %v, want errMConnNoLink at once", err, d)
		}
		if b460Stat.fast.Load() != f0+1 || b458Stat.wait.n.Load() != w0 {
			t.Fatalf("fast %d->%d capwaits %d->%d (want +1, +0)", f0, b460Stat.fast.Load(), w0, b458Stat.wait.n.Load())
		}
		if _, _, err := e2.open(planeLight, nil); err != nil {
			t.Fatalf("join pinned to the live generation: %v", err)
		}
		// the whole pool not Alive: no newer live generation, not dead
		m3, l3 := zduPool(1, 0)
		l3[0].dead.Store(true)
		if m3.b460Scan().dead(zduInstOld) {
			t.Fatal("empty pool: not dead without a newer live generation")
		}
		e.end()
		e2.end()
	}
	b457SetPickCap(0)
}

// TestB460ReleasesInBatches: waiters queued at the cap when their generation
// dies are released, all of them, but at most b460Batch per tick.
func TestB460ReleasesInBatches(t *testing.T) {
	setLite(t, false)
	setZDU(t, true)
	setOpenNoLock(t, true)
	oldB := b460Batch
	b460Batch = 20
	defer func() { b460Batch = oldB }()
	const n = 200
	for _, on := range []bool{true, false} {
		setDGBatch(t, on)
		b457SetPickCap(1)
		m, _ := zduPool(2, 0)
		var es []*mconnEdge
		for i := 0; i < n; i++ {
			e := zduNewEdge(m, uint64(100+i))
			if _, _, err := e.open(planeLight, nil); err != nil {
				t.Fatal(err)
			}
			es = append(es, e)
		}
		zduAddLinks(m, zduLink(zduInstNew))
		hold := b457PickAcquire(context.Background())
		w0 := b458Stat.waiting.Load()
		r0, k0 := b460Stat.released.Load(), b460Stat.ticks.Load()
		var wg sync.WaitGroup
		errs := make(chan error, n)
		for _, e := range es {
			wg.Add(1)
			go func(e *mconnEdge) { defer wg.Done(); _, _, err := e.open(planeLight, nil); errs <- err }(e)
		}
		for dl := time.Now().Add(3 * time.Second); b458Stat.waiting.Load() < w0+n; {
			if time.Now().After(dl) {
				t.Fatalf("only %d of %d joins queued", b458Stat.waiting.Load()-w0, n)
			}
			time.Sleep(time.Millisecond)
		}
		zduDropInst(m, zduInstOld) // the old process exited
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(1500 * time.Millisecond):
		}
		hold()
		if !on {
			select {
			case <-done:
				t.Fatal("key off: the dead-gen waiters left the queue without a slot")
			default:
			}
			<-done
		} else {
			select {
			case <-done:
			default:
				t.Fatalf("released %d of %d", b460Stat.released.Load()-r0, n)
			}
			if r := b460Stat.released.Load() - r0; r != n {
				t.Fatalf("released %d, want %d", r, n)
			}
			if k := b460Stat.ticks.Load() - k0; k < n/int64(b460Batch) {
				t.Fatalf("released in %d ticks, want >= %d (batches of %d)", k, n/b460Batch, b460Batch)
			}
			close(errs)
			for err := range errs {
				if !errors.Is(err, errMConnNoLink) {
					t.Fatalf("released waiter: %v", err)
				}
			}
		}
		for _, e := range es {
			e.end()
		}
	}
	b457SetPickCap(0)
}
