package engine

import (
	"testing"
	"time"
)

func withCs226(t *testing.T, on bool) {
	t.Helper()
	old := cs226LitePath()
	cs226LitePathKey.Store(on)
	t.Cleanup(func() { cs226LitePathKey.Store(old) })
}

// K2: a lite (non-call) conn's second sub took another link on the
// primary's path (HS2_DUP_PATHS off by default).
func TestCs226LiteCopyOffPath(t *testing.T) {
	withDupPaths(t, false) // the production default
	withCs226(t, true)
	withCs228(t, true, true) // pins cs226/cs228 placement (LITEPLACE off by default since rel68e)
	m, ls := pathLinks("a>x", "a>x", "a>x", "b>y", "b>y")
	before := Observed()
	if got := pickSecond(t, m, ls, 0, 40); got[3]+got[4] != 40 {
		t.Fatalf("lite copy not on the other path: %v", got)
	}
	if d := Observed().Sub(before); d.MConnPathOther != 40 || d.MConnPathSame != 0 {
		t.Fatalf("counters other %d same %d", d.MConnPathOther, d.MConnPathSame)
	}

	// The light-link rule first: every b link carries a download -> a
	// light link on a (a copy behind a download waits in its queue).
	m, ls = pathLinks("a>x", "a>x", "b>y", "b>y")
	m.links[2].heavy, m.links[3].heavy = 1, 1
	if got := pickSecond(t, m, ls, 0, 20); got[1] != 20 {
		t.Fatalf("loaded other path beat a light link: %v", got)
	}

	// b's links lowered/cut by the balancer: not a healthy other path.
	m, ls = pathLinks("a>x", "a>x", "b>y", "b>y")
	for _, i := range []int{2, 3} {
		k := Link(ls[i])
		rescueLow.Store(k, true)
		t.Cleanup(func() { rescueLow.Delete(k) })
	}
	if got := pickSecond(t, m, ls, 0, 40); got[1] == 0 {
		t.Fatalf("lowered path preferred over the primary's healthy path: %v", got)
	}

	// No avoid (a first sub): unchanged, any link.
	m, ls = pathLinks("a>x", "a>x", "b>y", "b>y")
	seen := map[int]bool{}
	for i := 0; i < 80; i++ {
		l, rel, ok := m.pickPlane(planeLight, nil)
		if !ok {
			t.Fatal("no first sub")
		}
		seen[linkIndex(ls, l)] = true
		rel()
	}
	if !seen[0] && !seen[1] {
		t.Fatalf("first sub never on a: %v", seen)
	}
}

func TestCs226KeyOff(t *testing.T) {
	withDupPaths(t, false)
	withCs226(t, false)
	m, ls := pathLinks("a>x", "a>x", "a>x", "b>y", "b>y")
	if got := pickSecond(t, m, ls, 0, 200); got[1]+got[2] == 0 {
		t.Fatalf("key off: never the primary's path: %v", got)
	}
}

// dcVisitLite: darkcall visits without making the conn call-like.
func dcVisitLite(m *mConn, times int) {
	for i := 0; i < times; i++ {
		m.mu.Lock()
		m.callLikeAt = time.Time{}
		m.callOn = false
		rr := m.darkcallLocked(time.Now(), ctrlNow())
		m.mu.Unlock()
		if rr {
			go m.rrStart()
		}
		time.Sleep(darkcallAfter() * 3 / 2)
	}
}

func withCs226Silent(t *testing.T, f func(*msub, int64) bool) {
	t.Helper()
	old := cs226SilentHook.Load()
	cs226SilentHook.Store(&f)
	t.Cleanup(func() { cs226SilentHook.Store(old) })
}

// A game-like (non-call) download whose exit primary goes dark while its
// link is silent: the lite visit switches it to the other path's sub and
// resends what Iran had not consumed; every byte arrives once, in order.
func TestCs226LiteDarkSwitch(t *testing.T) {
	setDarkcall(t)
	withCs226(t, true)
	n, iran, exit := darkswPairDC(t)
	exit.mu.Lock()
	p := exit.primary.Load()
	before := exit.sendOff
	exit.mu.Unlock()
	l := exitLinkOf(n, p)
	if l == nil {
		t.Fatal("no link for the exit primary")
	}
	f := newDcFake()
	darkcallReadSet(f.read)
	darkcallMarksSet(func(s *msub) (uint32, uint32, bool) {
		if s != p {
			return 0, 0, true
		}
		u := uint32(exit.sendOff-before) + uint32(exit.sendOff-before)/100 + 64
		return u, 0, true
	})
	withCs226Silent(t, func(s *msub, _ int64) bool { return s == p })
	l.exit.drop.Store(true)
	f.set(p, "dark")
	sw0, v0 := cs226Stat.sw.Load(), cs226Stat.visits.Load()
	data := randBytes(150_000, 31)
	got := make(chan error, 1)
	go func() { got <- readN(iran, data) }()
	if err := xfer(exit, data, 2000, 0); err != nil {
		t.Fatal(err)
	}
	dcVisitLite(exit, 3)
	exit.mu.Lock()
	newP := exit.primary.Load()
	exit.mu.Unlock()
	if cs226Stat.visits.Load() == v0 || cs226Stat.sw.Load() == sw0 {
		t.Fatalf("no lite visit/switch (visits +%d sw +%d)", cs226Stat.visits.Load()-v0, cs226Stat.sw.Load()-sw0)
	}
	if newP == p {
		t.Fatal("primary still on the dark sub")
	}
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("download stuck after the switch")
	}
	noExtra(t, iran, exit)
	closeAndWait(t, iran, exit)
}

// Gates (per connection; the global counters also see the timer's visits
// of other tests' connections): no silent/marked link -> no lite visit; key
// off -> none even with a silent link; a lite visit never caps a sub.
func TestCs226LiteGates(t *testing.T) {
	setDarkcall(t)
	_, iran, exit := darkswPairDC(t)
	f := newDcFake()
	darkcallReadSet(f.read)
	darkcallMarksSet(func(s *msub) (uint32, uint32, bool) { return 1 << 30, 0, true }) // way past any cap
	exit.mu.Lock()
	p := exit.primary.Load()
	exit.mu.Unlock()
	visit := func() bool {
		exit.mu.Lock()
		defer exit.mu.Unlock()
		exit.callLikeAt, exit.callOn = time.Time{}, false
		return exit.cs226LiteVisitLocked(ctrlNow())
	}
	withCs226(t, true)
	withCs226Silent(t, func(*msub, int64) bool { return false })
	if visit() {
		t.Fatal("visit with no silent/marked link")
	}
	withCs226(t, false)
	withCs226Silent(t, func(*msub, int64) bool { return true })
	if visit() {
		t.Fatal("key off: visit")
	}
	withCs226(t, true)
	if !visit() {
		t.Fatal("silent link: no visit")
	}
	// Iran: a link the balancer marked lowered/cut alone also visits
	withCs226Silent(t, func(*msub, int64) bool { return false })
	iran.mu.Lock()
	ip := iran.primary.Load()
	iran.callLikeAt, iran.callOn = time.Time{}, false
	iran.mu.Unlock()
	if ip == nil || ip.key == nil {
		t.Fatal("no Iran primary key")
	}
	rescueLow.Store(ip.key, true)
	t.Cleanup(func() { rescueLow.Delete(ip.key) })
	withCs228Pend(t, func(*msub, int64) bool { return true }) // cs228: marked AND a write unanswered
	iran.mu.Lock()
	marked := iran.cs226LiteVisitLocked(ctrlNow())
	iran.mu.Unlock()
	if !marked {
		t.Fatal("marked link: no visit")
	}
	f.set(p, "dark")
	dcVisitLite(exit, 2)
	exit.mu.Lock()
	defer exit.mu.Unlock()
	for _, s := range exit.subs {
		if s.dc.full {
			t.Fatal("lite sub capped")
		}
	}
}
