package engine

import (
	"testing"
	"time"
)

// lbv81: a copy sub (copyOnly) carrying a 2 Mbit call through flowStats
// keeps ewma 0 and steady 0, so the force close on a retiring (cut-drained)
// link takes it for trickling and closes it (production behaviour, key off);
// with HS2_LB_SLOW_SKIP_COPY=1 it stays. A trickling primary is still closed.
func copySlowRun(t *testing.T, skip bool) (copyClosed, trickleClosed bool) {
	t.Helper()
	old := slowSkipCopy
	slowSkipCopy = skip
	t.Cleanup(func() { slowSkipCopy = old })
	m, _, _ := newV2Manager(nil, 1, 8, false)
	pl := newV2PipeLink(t)
	trickle, cp := pl.open(t), pl.open(t)
	cp.copyOnly.Store(true)
	now := time.Now()
	pl.flowMu.Lock()
	trickle.ewma, trickle.lastActive = 300, now
	pl.flowMu.Unlock()
	// one second of call traffic per sample on the copy sub
	for i := 0; i < 5; i++ {
		now = now.Add(time.Second)
		cp.bytes.Add(250 << 10)
		pl.flowStats(now, time.Second, 10*time.Second)
	}
	ml := addManaged(m, pl)
	ml.retiring = true
	ml.reclaiming.Store(true)
	m.reclaimIdle(ml, now, drainIdleDefault, true)
	return cp.done.Load(), trickle.done.Load()
}

func TestSlowSkipCopy(t *testing.T) {
	c, tr := copySlowRun(t, false)
	if !c {
		t.Fatal("key off: expected the flowing copy sub to be force-closed (the bug this key fixes)")
	}
	if !tr {
		t.Fatal("key off: trickling stream not closed")
	}
	c, tr = copySlowRun(t, true)
	if c {
		t.Fatal("key on: a copy sub carrying a call was force-closed as trickling")
	}
	if !tr {
		t.Fatal("key on: the trickling stream must still be closed")
	}
}
