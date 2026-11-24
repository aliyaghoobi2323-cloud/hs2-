package engine

import (
	"sync"
	"testing"
	"time"
)

func withRescFast(t *testing.T, fast, resc bool) {
	t.Helper()
	of, or := lmFastPick.Load(), rescFastPick.Load()
	lmFastPick.Store(fast)
	rescFastPick.Store(resc)
	t.Cleanup(func() { lmFastPick.Store(of); rescFastPick.Store(or) })
}

func TestRescFastOn(t *testing.T) {
	for v, want := range map[string]bool{"": true, "1": true, " 0 ": false, "0": false, "x": true} {
		if rescFastOn(v) != want {
			t.Fatalf("HS2_RESC_FASTPICK=%q: want %v", v, want)
		}
	}
}

// b451: the rescue copy is placed under the read lock, on the same link the
// write-locked pick chooses (off call links, off lowered ones), counted once;
// with no rescue link the ordinary pick follows (never an avoided link).
func TestRescuePickFastPath(t *testing.T) {
	for _, on := range []bool{true, false} {
		withRescueCallFree(t)
		withRescFast(t, true, on)
		m, ls := pickLinks([]int{0, 0, 0, 0}, []bool{false, false, false, false})
		rescueMark(nil, []any{Link(ls[0])})
		call := &mConn{iran: true, subs: []*msub{{key: Link(ls[1])}, {key: Link(ls[2])}}}
		call.rescueCallMarkLocked(time.Now(), true)
		f0 := rescFastStat.fast.Load()
		for i := 0; i < 3; i++ {
			l, rel, ok := m.pickPlaneCall(planeLight, []any{Link(ls[0]), rescueAvoidMark{}})
			if !ok || linkIndex(ls, l) != 3 {
				t.Fatalf("on=%v: rescue copy on link %d, want 3", on, linkIndex(ls, l))
			}
			defer rel()
		}
		if got := rescFastStat.fast.Load() - f0; on && got != 3 || !on && got != 0 {
			t.Fatalf("on=%v: %d fast rescue picks", on, got)
		}
		if u := m.links[3].users.Load(); u != 3 {
			t.Fatalf("on=%v: link 3 users %d, want 3", on, u)
		}
		l, rel, ok := m.pickPlaneCall(planeLight, []any{Link(ls[0]), Link(ls[3]), rescueAvoidMark{}})
		if !ok || (linkIndex(ls, l) != 1 && linkIndex(ls, l) != 2) {
			t.Fatalf("on=%v: fallback on link %d", on, linkIndex(ls, l))
		}
		rel()
	}
}

// Many rescue picks at once (the storm): every placement counted exactly
// once, spread like serial picks (CAS on the ranked count), all released.
func TestRescuePickFastConcurrent(t *testing.T) {
	withRescueCallFree(t)
	withRescFast(t, true, true)
	m, ls := pickLinks([]int{0, 0, 0, 0, 0, 0}, []bool{false, false, false, false, false, false})
	rescueMark(nil, []any{Link(ls[0])})
	const n = 600
	rels := make([]func(), n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l, rel, ok := m.pickPlaneCall(planeLight, []any{Link(ls[0]), rescueAvoidMark{}})
			if !ok || linkIndex(ls, l) == 0 {
				t.Errorf("pick %d on link %d", i, linkIndex(ls, l))
				return
			}
			rels[i] = rel
		}(i)
	}
	wg.Wait()
	tot := int64(0)
	for i := 1; i < len(ls); i++ {
		tot += int64(m.links[i].users.Load())
	}
	if tot != n || m.users.Load() != n {
		t.Fatalf("placed %d (pool %d), want %d", tot, m.users.Load(), n)
	}
	for _, r := range rels {
		if r != nil {
			r()
		}
	}
	if m.users.Load() != 0 {
		t.Fatalf("users %d after release", m.users.Load())
	}
}
