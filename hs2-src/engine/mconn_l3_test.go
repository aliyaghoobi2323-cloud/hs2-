package engine

import (
	"sync"
	"testing"
	"time"
)

// audit246 L3: a timer visit of a lite conn that needs no prefetch walks its
// subs (cs226LiteVisitLocked) once, not twice; one that prefetches (m.mu
// released meanwhile) looks again after the reads.
func TestDarkcallVisitOneLiteWalk(t *testing.T) {
	setDarkcall(t)
	withCs226(t, true)
	withCs228(t, false, true)
	_, _, exit := darkswPairDC(t)
	var inflight sync.Map // *msub -> bool
	darkcallMarksSet(func(s *msub) (uint32, uint32, bool) {
		if v, ok := inflight.Load(s); ok && v.(bool) {
			return 1000, 10, true
		}
		return 1000, 1000, true
	})
	withCs226Silent(t, func(*msub, int64) bool { return true })
	walks := func(n int) uint32 {
		exit.mu.Lock()
		exit.callLikeAt, exit.callOn = time.Time{}, false
		w0 := exit.dc.liteWalks
		exit.mu.Unlock()
		for i := 0; i < n; i++ {
			exit.darkcallVisit(time.Now(), ctrlNow())
		}
		exit.mu.Lock()
		defer exit.mu.Unlock()
		return exit.dc.liteWalks - w0
	}
	const n = 20
	// idle: no visit, no prefetch -> one walk per visit (was two). The
	// global darkcall timer may visit this conn too: a little slack.
	if w := walks(n); w < n || w > n+4 {
		t.Fatalf("idle lite conn: %d walks in %d visits, want %d", w, n, n)
	}
	// in flight on a silent link: a lite visit with prefetch -> two walks
	exit.mu.Lock()
	p := exit.primary.Load()
	exit.mu.Unlock()
	inflight.Store(p, true)
	if w := walks(n); w < 2*n || w > 2*n+8 {
		t.Fatalf("prefetching lite conn: %d walks in %d visits, want %d", w, n, 2*n)
	}
}
