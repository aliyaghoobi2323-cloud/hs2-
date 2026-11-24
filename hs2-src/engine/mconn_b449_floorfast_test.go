package engine

import (
	"testing"
	"time"
)

// rc14d HS2_B449_FLOORFAST: with the connection's kept bytes at or under the
// floor the write-path trim stops before the scan; on and off must leave the
// same chunks, gap and held count, under and over the floor, and over
// b449KeepHard (no hold: the old trim).
func TestB449FloorFastSame(t *testing.T) {
	oldF, oldFast := b449KeepFloor.Load(), b449FloorFast.Load()
	t.Cleanup(func() { b449KeepFloor.Store(oldF); b449FloorFast.Store(oldFast) })
	b449KeepFloor.Store(1000)
	type out struct {
		kept, n, held int
		gap           bool
	}
	run := func(fast bool, writes int, hard bool) out {
		b449FloorFast.Store(fast)
		m := &mConn{mode: planeLight}
		s := &msub{m: m}
		m.subs = []*msub{s}
		now := time.Now()
		s.lastRecv.Store(monoNano(now))
		m.mu.Lock()
		defer m.mu.Unlock()
		for range writes {
			fx212Keep(m, s, 300)
		}
		for i := range s.sent {
			s.sent[i].t = now.Add(-2 * time.Second)
		}
		if hard {
			subResendStat.kept.Add(b449KeepHard)
			defer subResendStat.kept.Add(-b449KeepHard)
		}
		h0 := b449FloorStat.ageHeld.Load()
		m.subAgeTrimLocked(now, false)
		o := out{m.keptBytes, len(s.sent), int(b449FloorStat.ageHeld.Load() - h0), !s.subGapT.IsZero()}
		m.dropSentLocked(s, len(s.sent))
		return o
	}
	for _, c := range []struct {
		writes int
		hard   bool
	}{{2, false}, {3, false}, {4, false}, {9, false}, {2, true}} {
		off, on := run(false, c.writes, c.hard), run(true, c.writes, c.hard)
		if off != on {
			t.Fatalf("writes %d hard %v: off %+v on %+v", c.writes, c.hard, off, on)
		}
		t.Logf("writes %d hard %v: %+v", c.writes, c.hard, on)
	}
}
