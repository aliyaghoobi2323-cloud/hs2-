package engine

import (
	"net"
	"sync"
	"testing"
	"time"
)

func withCs228(t *testing.T, place, visit bool) {
	t.Helper()
	op, ov := cs228PlaceKey.Load(), cs228VisitKey.Load()
	cs228PlaceKey.Store(place)
	cs228VisitKey.Store(visit)
	t.Cleanup(func() { cs228PlaceKey.Store(op); cs228VisitKey.Store(ov) })
}

func withCs228Pend(t *testing.T, f func(*msub, int64) bool) {
	t.Helper()
	old := cs228PendHook.Load()
	cs228PendHook.Store(&f)
	t.Cleanup(func() { cs228PendHook.Store(old) })
}

// Placement: another path only while its quality is not worse than the
// primary's; otherwise the old pick, never the live link itself.
func TestCs228PlaceQuality(t *testing.T) {
	withDupPaths(t, false)
	withCs226(t, true)
	withCs228(t, true, false)

	// equal RTT/loss: the other path, as cs226
	m, ls := pathLinks("a>x", "a>x", "a>x", "b>y", "b>y")
	if got := pickSecond(t, m, ls, 0, 40); got[3]+got[4] != 40 {
		t.Fatalf("equal quality: not on the other path: %v", got)
	}
	// within the margin (+4 ms on 50 ms: max(10 %, 5 ms) = 5 ms): still other
	m, ls = pathLinks("a>x", "a>x", "a>x", "b>y", "b>y")
	ls[3].mtr.rttMicros.Store(54_000)
	ls[4].mtr.rttMicros.Store(54_000)
	if got := pickSecond(t, m, ls, 0, 40); got[3]+got[4] != 40 {
		t.Fatalf("within margin: not on the other path: %v", got)
	}
	// slower other path (+30 ms): the old pick, which reaches the
	// primary's path (another link, never link 0)
	m, ls = pathLinks("a>x", "a>x", "a>x", "b>y", "b>y")
	ls[3].mtr.rttMicros.Store(80_000)
	ls[4].mtr.rttMicros.Store(80_000)
	d0 := cs228Stat.qualDrop.Load()
	if got := pickSecond(t, m, ls, 0, 200); got[1]+got[2] == 0 {
		t.Fatalf("slower other path still forced: %v", got)
	}
	if cs228Stat.qualDrop.Load() == d0 {
		t.Fatal("no qual drop counted")
	}
	// lossy other path (judged 5 % vs 0): old pick
	m, ls = pathLinks("a>x", "a>x", "a>x", "b>y", "b>y")
	m.links[3].lossFrac, m.links[4].lossFrac = 0.05, 0.05
	if got := pickSecond(t, m, ls, 0, 200); got[1]+got[2] == 0 {
		t.Fatalf("lossy other path still forced: %v", got)
	}
	// unknown RTT on the other path: old pick
	m, ls = pathLinks("a>x", "a>x", "a>x", "b>y", "b>y")
	ls[3].mtr.rttMicros.Store(0)
	ls[4].mtr.rttMicros.Store(0)
	if got := pickSecond(t, m, ls, 0, 200); got[1]+got[2] == 0 {
		t.Fatalf("unknown-quality other path still forced: %v", got)
	}
	// two other paths, one slow: only the good one
	m, ls = pathLinks("a>x", "a>x", "b>y", "c>z")
	ls[3].mtr.rttMicros.Store(120_000)
	if got := pickSecond(t, m, ls, 0, 40); got[2] != 40 {
		t.Fatalf("slow path not dropped: %v", got)
	}
	// the faster other path is fine (better than the primary's)
	m, ls = pathLinks("a>x", "a>x", "b>y")
	ls[2].mtr.rttMicros.Store(20_000)
	if got := pickSecond(t, m, ls, 0, 20); got[2] != 20 {
		t.Fatalf("faster other path not taken: %v", got)
	}
	// place key off (master on): cs225's pick (the primary's path reachable)
	withCs228(t, false, false)
	m, ls = pathLinks("a>x", "a>x", "a>x", "b>y", "b>y")
	if got := pickSecond(t, m, ls, 0, 200); got[1]+got[2] == 0 {
		t.Fatalf("place off: never the primary's path: %v", got)
	}
}

// Visits: an idle conn (nothing unconsumed) is not dark even on a silent
// link; a marked link needs a write unanswered; the visit key gates all.
func TestCs228VisitGates(t *testing.T) {
	setDarkcall(t)
	withCs226(t, true)
	withCs228(t, false, true)
	_, iran, exit := darkswPairDC(t)
	var inflight sync.Map // *msub -> bool
	darkcallMarksSet(func(s *msub) (uint32, uint32, bool) {
		if v, ok := inflight.Load(s); ok && v.(bool) {
			return 1000, 10, true
		}
		return 1000, 1000, true
	})
	visit := func(m *mConn) bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.callLikeAt, m.callOn = time.Time{}, false
		return m.cs226LiteVisitLocked(ctrlNow())
	}
	exit.mu.Lock()
	p := exit.primary.Load()
	exit.mu.Unlock()
	withCs226Silent(t, func(*msub, int64) bool { return true })
	if visit(exit) {
		t.Fatal("idle conn (all consumed) on a silent link: visit")
	}
	inflight.Store(p, true)
	if !visit(exit) {
		t.Fatal("unconsumed bytes on a silent link: no visit")
	}
	withCs228(t, false, false)
	if visit(exit) {
		t.Fatal("visit key off: visit")
	}
	withCs228(t, false, true)
	withCs226(t, false)
	if visit(exit) {
		t.Fatal("master off: visit")
	}
	withCs226(t, true)

	// marked (Iran): needs bytes unconsumed AND a write unanswered
	withCs226Silent(t, func(*msub, int64) bool { return false })
	iran.mu.Lock()
	ip := iran.primary.Load()
	iran.mu.Unlock()
	rescueLow.Store(ip.key, true)
	t.Cleanup(func() { rescueLow.Delete(ip.key) })
	inflight.Store(ip, true)
	withCs228Pend(t, func(*msub, int64) bool { return false })
	if visit(iran) {
		t.Fatal("marked link, nothing pending: visit")
	}
	withCs228Pend(t, func(*msub, int64) bool { return true })
	if !visit(iran) {
		t.Fatal("marked link with a write unanswered: no visit")
	}
	inflight.Store(ip, false)
	if visit(iran) {
		t.Fatal("marked link, conn idle: visit")
	}
}

// cs226Silent is "silent while pending": a quiet link with nothing unanswered
// is not silent (cs228SilentAt, darkcallAfter 50 ms here).
func TestCs228SilentNeedsPending(t *testing.T) {
	setDarkcall(t)
	now := ctrlNow()
	old := now - int64(time.Second)
	if cs228SilentAt(old, old-1, old-1, now) {
		t.Fatal("quiet link, nothing pending: silent")
	}
	if !cs228SilentAt(old, old+1, old+1, now) {
		t.Fatal("write unanswered for 1 s: not silent")
	}
	ms := now - int64(time.Millisecond)
	if cs228SilentAt(old, ms, ms, now) {
		t.Fatal("write 1 ms ago: silent already")
	}
	if cs228SilentAt(now, now, now, now) || cs228SilentAt(0, now, now, now+int64(time.Hour)) {
		t.Fatal("fresh / unknown read: silent")
	}
}

// A lite visit reads TCP_INFO with m.mu released: the read hook can take the
// conn lock within 1 s (it could not while the visit held it).
func TestCs228VisitReadsUnlocked(t *testing.T) {
	setDarkcall(t)
	withCs226(t, true)
	withCs228(t, false, true)
	_, _, exit := darkswPairDC(t)
	darkcallMarksSet(func(*msub) (uint32, uint32, bool) { return 1000, 10, true })
	withCs226Silent(t, func(*msub, int64) bool { return true })
	f := newDcFake()
	var mu sync.Mutex
	var locked, reads int
	var test sync.Mutex // only this test's own visit checks the lock
	test.Lock()
	darkcallReadSet(func(s *msub) (*net.TCPConn, string, int64, uint64, uint32, uint8, uint32, bool) {
		if s.m == exit && test.TryLock() {
			got := make(chan struct{})
			go func() { exit.mu.Lock(); exit.mu.Unlock(); close(got) }()
			held := false
			select {
			case <-got:
			case <-time.After(time.Second):
				held = true
			}
			mu.Lock()
			reads++
			if held {
				locked++
			}
			mu.Unlock()
			test.Unlock()
		}
		return f.read(s)
	})
	exit.mu.Lock()
	exit.callLikeAt, exit.callOn = time.Time{}, false
	exit.mu.Unlock()
	p0 := cs228Stat.pre.Load()
	test.Unlock() // the visit below may check now
	exit.darkcallVisit(time.Now(), ctrlNow())
	mu.Lock()
	r, l := reads, locked
	mu.Unlock()
	if r == 0 || cs228Stat.pre.Load() == p0 {
		t.Fatalf("no unlocked lite read (reads %d)", r)
	}
	if l != 0 {
		t.Fatalf("%d of %d reads ran under m.mu", l, r)
	}
	exit.mu.Lock()
	defer exit.mu.Unlock()
	for _, s := range exit.subs {
		if s.dcPre != nil {
			t.Fatal("prefetch left on a sub")
		}
	}
}
