package engine

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func withRescue(t *testing.T, on bool) {
	t.Helper()
	old := lbRescue
	lbRescue = on
	t.Cleanup(func() {
		lbRescue = old
		rescueLow.Range(func(k, _ any) bool { rescueLow.Delete(k); return true })
	})
}

func TestRescueMarkUnmark(t *testing.T) {
	withRescue(t, true)
	prev := rescueMark(nil, []any{1, 2})
	if !rescueLowKey(1) || !rescueLowKey(2) || rescueLowKey(3) || rescueLowKey(nil) {
		t.Fatal("marks")
	}
	prev = rescueMark(prev, []any{2})
	if rescueLowKey(1) || !rescueLowKey(2) {
		t.Fatal("a recovered link stays marked")
	}
	rescueMark(prev, nil)
	if rescueLowKey(2) {
		t.Fatal("not unmarked")
	}
}

func TestRescueRateBound(t *testing.T) {
	old := rescuePerSec
	rescuePerSec = 3
	t.Cleanup(func() { rescuePerSec = old })
	now := time.Unix(5000, 0)
	ok := 0
	for i := 0; i < 10; i++ {
		if rescueAllow(now) {
			ok++
		}
	}
	if ok != 3 {
		t.Fatalf("%d upgrades in one second, want 3", ok)
	}
	if !rescueAllow(now.Add(time.Second)) {
		t.Fatal("the next second refused")
	}
}

// A light connection whose one sub is on a lowered path gets its copy on
// another link even when the duplication budget refuses lite's upgrade
// (TestMConnLiteCapStopsUpgrade: the same budget, key off: single). Nothing
// is closed: the first sub stays, so no reset.
func TestMConnRescueCappedSingle(t *testing.T) {
	withRescue(t, true)
	before := runtime.NumGoroutine()
	n, iran, exit := newLitePair(t)
	liteBud.add(time.Now().Unix(), 1_000_000, 100_000) // budget exhausted
	rescueMark(nil, []any{0})                          // link 0 (the first sub's) on a lowered path
	mWaitFor(t, "rescue upgrade", mLiteAfter+3*time.Second, func() bool {
		return liveSubs(iran) == 2 && liveSubs(exit) == 2
	})
	if l := n.link(1); l.plane != planeLight || len(l.avoid) == 0 || l.avoid[0] != 0 {
		t.Fatalf("copy: plane %v avoid %v, want light avoiding link 0", l.plane, l.avoid)
	}
	if n.count(planeBulk) != 0 {
		t.Fatal("bulk opened")
	}
	resetLiteBudget()
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// The same on a raw first sub (capMConnRaw: what nearly every single-sub
// lite connection of a real crowd is): the rescue reaches its rate timer
// too. Before the fix a raw connection on a lowered link stayed single under
// a binding budget (lab k runs: 0-4 copies past the budget for a crowd of
// 3000). Not on a lowered link, it stays raw and single. HS2_LB_RESCUE_RAW
// (default off) gates it.
func TestMConnRescueCappedRaw(t *testing.T) {
	withRescue(t, true)
	lbRescueRaw = true
	t.Cleanup(func() { lbRescueRaw = false })
	before := runtime.NumGoroutine()
	n, iran, exit := newRawPair(t)
	liteBud.add(time.Now().Unix(), 1_000_000, 100_000) // budget exhausted
	rescueMark(nil, []any{7})                          // some other link
	time.Sleep(mLiteAfter + 1500*time.Millisecond)
	if liveSubs(iran) != 1 || n.link(1) != nil {
		t.Fatalf("upgraded off a lowered path: %d subs", liveSubs(iran))
	}
	liteBud.add(time.Now().Unix(), 1_000_000, 100_000)
	rescueMark(map[any]bool{7: true}, []any{0}) // now link 0 (the raw sub's)
	mWaitFor(t, "raw rescue upgrade", mLiteAfter+3*time.Second, func() bool {
		return liveSubs(iran) == 2 && liveSubs(exit) == 2 && !iran.rawW.Load()
	})
	if l := n.link(1); l.plane != planeLight || len(l.avoid) == 0 || l.avoid[0] != 0 {
		t.Fatalf("copy: plane %v avoid %v, want light avoiding link 0", l.plane, l.avoid)
	}
	up, down := randBytes(64<<10, 31), randBytes(64<<10, 32)
	both(t,
		func() error { return xfer(iran, up, 4<<10, 0) },
		func() error { return xfer(exit, down, 4<<10, 0) },
		func() error { return readN(exit, up) },
		func() error { return readN(iran, down) },
	)
	if !rawDone(iran) {
		t.Fatal("raw handshake not over after a two-way transfer")
	}
	rescueMark(map[any]bool{0: true}, nil)
	resetLiteBudget()
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// Not on a lowered path, or a download: the budget still refuses.
func TestMConnRescueSparesOthers(t *testing.T) {
	withRescue(t, true)
	before := runtime.NumGoroutine()
	n, iran, exit := newLitePair(t)
	liteBud.add(time.Now().Unix(), 1_000_000, 100_000)
	rescueMark(nil, []any{7}) // some other link
	time.Sleep(mLiteAfter + 1500*time.Millisecond)
	if liveSubs(iran) != 1 || n.count(planeLight) != 1 {
		t.Fatalf("upgraded off a lowered path: %d subs", liveSubs(iran))
	}
	resetLiteBudget()
	closeAndWait(t, iran, exit)
	noLeak(t, before)

	// A download on a lowered link stays single (lite's own light test).
	before = runtime.NumGoroutine()
	n, iran, exit = newLitePair(t)
	rescueMark(nil, []any{0})
	liteBud.add(time.Now().Unix(), 1_000_000, 100_000)
	n.link(0).exit.rate.Store(4 << 20)
	data := randBytes(6<<20, 11)
	errs := make(chan error, 2)
	go func() { errs <- writeRandomly(exit, data, 12, 64<<10) }()
	go func() { errs <- readAllCheck(iran, data) }()
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if liveSubs(iran) != 1 || n.count(planeLight) != 1 || n.count(planeBulk) != 0 {
		t.Fatalf("download rescued: %d subs, %d light", liveSubs(iran), n.count(planeLight))
	}
	resetLiteBudget()
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// A dual light connection with both subs on lowered links gives up its copy
// through the close handshake and gets a new copy elsewhere, while pings run
// both ways: every ping arrives (pingPong fails the test on a lost or
// corrupt one), nothing resets.
func TestMConnRescueDualAllLowered(t *testing.T) {
	withRescue(t, true)
	setCloseHS(t, true)
	before := runtime.NumGoroutine()
	n, iran, exit := newLitePair(t)
	var w, l atomic.Int64
	var wg sync.WaitGroup
	pingPong(t, iran, exit, 250, &w, &l, &wg) // 5 s of pings
	mWaitFor(t, "two subs", mLiteAfter+2*time.Second, func() bool { return liveSubs(iran) == 2 && liveSubs(exit) == 2 })
	d0 := rescueDual.Load()
	rescueMark(nil, []any{0, 1}) // both on a lowered path
	mWaitFor(t, "copy moved off", 4*time.Second, func() bool {
		return rescueDual.Load() > d0 && n.count(planeLight) >= 3 && liveSubs(iran) == 2
	})
	rescueMark(map[any]bool{0: true, 1: true}, []any{0, 1, 2})
	time.Sleep(1500 * time.Millisecond)
	if got := rescueDual.Load() - d0; got > 3 {
		t.Fatalf("%d copy subs given up in 1.5 s with every link lowered: want the rate of lite's own polls at most", got)
	}
	wg.Wait()
	if modeOf(iran) != planeLight || n.count(planeBulk) != 0 {
		t.Fatal("left light mode")
	}
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// Key off: a dual connection on lowered links keeps both subs.
func TestMConnRescueDualKeyOff(t *testing.T) {
	withRescue(t, false)
	setCloseHS(t, true)
	before := runtime.NumGoroutine()
	n, iran, exit := newLitePair(t)
	var w, l atomic.Int64
	var wg sync.WaitGroup
	pingPong(t, iran, exit, 150, &w, &l, &wg)
	mWaitFor(t, "two subs", mLiteAfter+2*time.Second, func() bool { return liveSubs(iran) == 2 && liveSubs(exit) == 2 })
	rescueMark(nil, []any{0, 1})
	wg.Wait()
	if n.count(planeLight) != 2 {
		t.Fatalf("key off: %d light opens", n.count(planeLight))
	}
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// The steering tick marks the links of lowered (<= rescueFactor) and cut
// paths from the dialing side's fresh report, unmarks them once the path
// recovers, and marks nothing when every path is lowered.
func TestSteerRescueMarks(t *testing.T) {
	withRescue(t, true)
	m := &LinkManager{steer: newSteerState()}
	st := m.steer
	la, lb1, lb2, lc := &fakeLink{alive: true}, &fakeLink{alive: true}, &fakeLink{alive: true}, &fakeLink{alive: true}
	byPath := map[string][]*managedLink{
		"a": {{link: la}}, "b": {{link: lb1}, {link: lb2}}, "c": {{link: lc}},
	}
	st.active = true
	st.repFactor = map[string]float64{"a": 1, "b": 0.1, "c": 0.6}
	st.repCut = map[string]bool{}
	m.steerRescueLocked(byPath)
	if !rescueLowKey(Link(lb1)) || !rescueLowKey(Link(lb2)) || rescueLowKey(Link(la)) || rescueLowKey(Link(lc)) {
		t.Fatal("lowered path's links not marked alone")
	}
	st.repFactor["c"], st.repCut["c"] = 0.9, true // cut: marked whatever the factor
	m.steerRescueLocked(byPath)
	if !rescueLowKey(Link(lc)) {
		t.Fatal("cut path not marked")
	}
	st.repFactor = map[string]float64{"a": 1, "b": 0.5, "c": 1}
	st.repCut = map[string]bool{}
	m.steerRescueLocked(byPath)
	if rescueLowKey(Link(lb1)) || rescueLowKey(Link(lc)) {
		t.Fatal("recovered paths still marked")
	}
	st.repFactor = map[string]float64{"a": 0.2, "b": 0.1, "c": 0.15}
	m.steerRescueLocked(byPath)
	if rescueLowKey(Link(la)) || rescueLowKey(Link(lb1)) {
		t.Fatal("every path lowered: nowhere better, nothing should be marked")
	}
	st.active = false
	st.repFactor = map[string]float64{"a": 1, "b": 0.1}
	m.steerRescueLocked(byPath)
	if rescueLowKey(Link(lb1)) {
		t.Fatal("no fresh report: marked")
	}
}

// HS2_LB_RESCUE_RAW off (the default): a raw connection on a lowered link
// stays single under a binding budget.
func TestMConnRescueRawOffByDefault(t *testing.T) {
	withRescue(t, true)
	before := runtime.NumGoroutine()
	n, iran, exit := newRawPair(t)
	liteBud.add(time.Now().Unix(), 1_000_000, 100_000)
	rescueMark(nil, []any{0})
	time.Sleep(mLiteAfter + 1500*time.Millisecond)
	if liveSubs(iran) != 1 || n.link(1) != nil || !iran.rawW.Load() {
		t.Fatalf("raw connection rescued with HS2_LB_RESCUE_RAW off: %d subs", liveSubs(iran))
	}
	resetLiteBudget()
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}
