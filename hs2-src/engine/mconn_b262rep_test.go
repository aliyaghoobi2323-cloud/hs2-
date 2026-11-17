package engine

import (
	"slices"
	"testing"
	"time"
)

func TestB262RepWant(t *testing.T) {
	p, s := &msub{}, &msub{}
	s.dcLeft, s.dc.ok, s.dc.unacked = true, true, 100
	if !b262RepWant(s, p, 3*time.Second, true) {
		t.Fatal("dark left sub with a live primary: want replace")
	}
	for name, ok := range map[string]bool{
		"short dark":   b262RepWant(s, p, time.Second, true),
		"primary":      b262RepWant(s, s, 3*time.Second, true),
		"primary dark": b262RepWant(s, p, 3*time.Second, false),
		"no primary":   b262RepWant(s, nil, 3*time.Second, true),
	} {
		if ok {
			t.Fatalf("%s: replace, want none", name)
		}
	}
	s.dcLeft = false
	if b262RepWant(s, p, 3*time.Second, true) {
		t.Fatal("a sub no switch left: replace, want none")
	}
	s.dcLeft, s.dc.unacked = true, 0
	if b262RepWant(s, p, 3*time.Second, true) {
		t.Fatal("nothing unacked: replace, want none")
	}
}

// TestB262ReplaceStale: a call's sub left dark >= 2 s with a big backlog is
// closed (CLOSE-REQ, its link avoided) once; healthy subs never are.
func TestB262ReplaceStale(t *testing.T) {
	setDarkcall(t)
	oldOn, oldQ, oldHS := b262RepOn.Load(), b262OutqOf, b262RepCloseHS
	t.Cleanup(func() { b262OutqOf, b262RepCloseHS = oldQ, oldHS; b262RepOn.Store(oldOn) })
	b262RepOn.Store(true)
	outq := uint64(2 << 20)
	b262OutqOf = func(*msub) (uint64, bool) { return outq, true }
	b262RepCloseHS = func(*msub) bool { return true }
	_, iran, _ := darkswPairDC(t)
	iran.mu.Lock()
	defer iran.mu.Unlock()
	iran.callOn = true
	live := iran.rrLiveSubsLocked()
	if len(live) < 2 {
		t.Fatalf("setup: %d live subs", len(live))
	}
	p := iran.primary.Load()
	var s *msub
	for _, x := range live {
		if x != p {
			s = x
		}
	}
	now := time.Now()
	nowc := ctrlNow()
	healthy := func(x *msub) {
		x.dcLeft = false
		x.dc.ok, x.dc.unacked, x.dc.ackAt, x.dc.lastRd = true, 0, now, nowc
		x.dc.isDark, x.dc.txDark = false, false
	}
	healthy(p)
	healthy(s)
	r0 := b262RepStat.rep.Load()
	// cut-free control: healthy subs with big sockets (bulk) are never replaced
	for i := 0; i < 50; i++ {
		iran.b262ReplaceLocked(p, live, now.Add(time.Duration(i)*time.Second), nowc)
	}
	if got := b262RepStat.rep.Load() - r0; got != 0 || s.closeHS {
		t.Fatalf("healthy: %d replacements, want 0", got)
	}
	// left by a switch, dark 1.5 s: not yet
	s.dcLeft, s.dc.unacked, s.dc.ackAt = true, 1500, now.Add(-1500*time.Millisecond)
	iran.b262RepAt = time.Time{}
	iran.b262ReplaceLocked(p, live, now, nowc)
	if s.closeHS {
		t.Fatal("dark 1.5 s: replaced, want not yet")
	}
	// small backlog: kept
	s.dc.ackAt = now.Add(-3 * time.Second)
	outq = 8 << 10
	iran.b262ReplaceLocked(p, live, now, nowc)
	if s.closeHS {
		t.Fatal("8 KB backlog: replaced, want kept")
	}
	// the primary itself not live (dark): kept (nothing to carry the call)
	outq = 2 << 20
	p.dc.txDark = true
	iran.b262ReplaceLocked(p, live, now, nowc)
	if s.closeHS {
		t.Fatal("dark primary: replaced, want kept")
	}
	p.dc.txDark = false
	// key off: kept
	b262RepOn.Store(false)
	iran.b262ReplaceLocked(p, live, now, nowc)
	if s.closeHS {
		t.Fatal("key off: replaced")
	}
	b262RepOn.Store(true)
	iran.b262ReplaceLocked(p, live, now, nowc)
	if !s.closeHS || !s.retired || b262RepStat.rep.Load()-r0 != 1 {
		t.Fatalf("stale sub: closeHS %v retired %v rep +%d, want closed once", s.closeHS, s.retired, b262RepStat.rep.Load()-r0)
	}
	if iran.primary.Load() != p {
		t.Fatal("the primary moved")
	}
	dead := false
	for _, k := range iran.deadAvoidLocked() {
		if k == s.key || k == avoidKey(s.key) {
			dead = true
		}
	}
	if !dead && !slices.Contains(iran.deadKeys, s.key) {
		t.Fatalf("its link is not avoided: %v", iran.deadKeys)
	}
	if n := len(iran.rrLiveSubsLocked()); n != len(live)-1 {
		t.Fatalf("live subs %d, want %d (callMoreSubsLocked opens the replacement)", n, len(live)-1)
	}
	// per-connection bound: a second stale sub within b262RepEvery waits
	r1 := b262RepStat.rep.Load()
	healthy(s)
	iran.b262ReplaceLocked(p, live, now.Add(time.Second), nowc)
	if b262RepStat.rep.Load() != r1 {
		t.Fatal("second replacement inside b262RepEvery")
	}
}

func TestB262RepRate(t *testing.T) {
	now := time.Unix(1_900_000_000, 0)
	n := 0
	for i := 0; i < 3*b262RepRate; i++ {
		if b262RepAllow(now) {
			n++
		}
	}
	if n != b262RepRate {
		t.Fatalf("allowed %d in one second, want %d", n, b262RepRate)
	}
	if !b262RepAllow(now.Add(time.Second)) {
		t.Fatal("next second refused")
	}
}
