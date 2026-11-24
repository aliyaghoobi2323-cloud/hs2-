package engine

import (
	"context"
	"errors"
	"testing"
	"time"
)

func setDeadGen(t *testing.T, on bool) {
	t.Helper()
	old := b459DeadGen.Load()
	b459DeadGen.Store(on)
	t.Cleanup(func() { b459DeadGen.Store(old) })
}

// b459Refresh: what DropLink / sampleHealth run after the pool changed.
func b459Refresh(m *LinkManager) {
	m.mu.Lock()
	m.zduRefreshLocked()
	m.mu.Unlock()
}

// TestB459DeadGenFailFast: a join of a connection pinned to the old
// generation, after its last link went and a newer one serves, fails at once
// (no 2 s pick retry, no cap slot); a join pinned to the live generation still
// opens.
func TestB459DeadGenFailFast(t *testing.T) {
	setLite(t, false)
	setZDU(t, true)
	setOpenNoLock(t, true)
	setDeadGen(t, true)
	defer b457SetPickCap(0)
	b457SetPickCap(2)
	m, _ := zduPool(2, 0)
	e := zduNewEdge(m, 7)
	if _, _, err := e.open(planeLight, nil); err != nil {
		t.Fatal(err)
	}
	zduAddLinks(m, zduLink(zduInstNew), zduLink(zduInstNew))
	e2 := zduNewEdge(m, 8)
	if _, l, err := e2.open(planeLight, nil); err != nil || linkZDUInst(l.(Link)) != zduInstNew {
		t.Fatalf("new conn: %v", err)
	}
	if e.b459PinDead() || e2.b459PinDead() {
		t.Fatal("both generations live: none dead")
	}
	zduDropInst(m, zduInstOld)
	f0, w0 := b459Stat.fast.Load(), b458Stat.wait.n.Load()
	t0 := time.Now()
	if _, _, err := e.open(planeLight, nil); !errors.Is(err, errMConnNoLink) {
		t.Fatalf("join pinned to the dead generation: %v", err)
	}
	if d := time.Since(t0); d > 300*time.Millisecond {
		t.Fatalf("join pinned to the dead generation took %v (want fail-fast)", d)
	}
	if b459Stat.fast.Load() != f0+1 || b458Stat.wait.n.Load() != w0 {
		t.Fatalf("fast %d->%d capwaits %d->%d", f0, b459Stat.fast.Load(), w0, b458Stat.wait.n.Load())
	}
	if _, _, err := e2.open(planeLight, nil); err != nil {
		t.Fatalf("join pinned to the live generation: %v", err)
	}
	e.end()
	e2.end()

	// the whole pool gone (no newer generation live): not dead, links come back
	m3, _ := zduPool(1, 0)
	zduDropInst(m3, zduInstOld)
	if m3.b459Dead(zduInstOld) {
		t.Fatal("empty pool: a generation is not dead without a newer one")
	}
	// key off: no rule
	setDeadGen(t, false)
	if m.b459Dead(zduInstOld) != true || e.b459PinDead() {
		t.Fatal("key off: the edge check must be off")
	}
}

// TestB459DeadGenDropsWaiters: a join waiting for a cap slot is dropped when
// its generation's last link goes, while the slot is still held.
func TestB459DeadGenDropsWaiters(t *testing.T) {
	setLite(t, false)
	setZDU(t, true)
	setOpenNoLock(t, true)
	setDeadGen(t, true)
	defer b457SetPickCap(0)
	b457SetPickCap(1)
	m, _ := zduPool(2, 0)
	e := zduNewEdge(m, 9)
	if _, _, err := e.open(planeLight, nil); err != nil {
		t.Fatal(err)
	}
	zduAddLinks(m, zduLink(zduInstNew))
	hold := b457PickAcquire(context.Background()) // the cap is full
	defer hold()
	d0, wk0 := b459Stat.dropped.Load(), b459Stat.wakes.Load()
	waiting0 := b458Stat.waiting.Load()
	res := make(chan error, 1)
	go func() { _, _, err := e.open(planeLight, nil); res <- err }()
	deadline := time.Now().Add(2 * time.Second)
	for b458Stat.waiting.Load() == waiting0 {
		if time.Now().After(deadline) {
			t.Fatal("the join never waited for the cap")
		}
		time.Sleep(time.Millisecond)
	}
	zduDropInst(m, zduInstOld)
	b459Refresh(m) // DropLink's zduRefreshLocked
	select {
	case err := <-res:
		if !errors.Is(err, errMConnNoLink) {
			t.Fatalf("dropped waiter: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("waiter of the dead generation still queued")
	}
	if b459Stat.dropped.Load() != d0+1 || b459Stat.wakes.Load() <= wk0 {
		t.Fatalf("dropped %d->%d wakes %d->%d", d0, b459Stat.dropped.Load(), wk0, b459Stat.wakes.Load())
	}
	e.end()
}
