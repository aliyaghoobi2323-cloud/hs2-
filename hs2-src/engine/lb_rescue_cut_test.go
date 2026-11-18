package engine

import (
	"runtime"
	"testing"
	"time"
)

func withRescueCut(t *testing.T, on bool) {
	t.Helper()
	old := lbRescueCut
	lbRescueCut = on
	t.Cleanup(func() {
		waitConnLoops(t.Logf, mCloseLinger+mRSTWait+2*time.Second)
		lbRescueCut = old
		rescueCutLinks.Range(func(k, _ any) bool { rescueCutLinks.Delete(k); return true })
		rescueCutCount.Store(0) // b340
	})
}

// The steering tick marks the links of CUT paths (not merely lowered ones)
// for HS2_LB_RESCUE_CUT, and unmarks them when the path is no longer cut.
func TestSteerRescueCutMarks(t *testing.T) {
	withRescue(t, true)
	withRescueCut(t, true)
	m := &LinkManager{steer: newSteerState()}
	st := m.steer
	la, lb, lc := &fakeLink{alive: true}, &fakeLink{alive: true}, &fakeLink{alive: true}
	byPath := map[string][]*managedLink{"a": {{link: la}}, "b": {{link: lb}}, "c": {{link: lc}}}
	st.active = true
	st.repFactor = map[string]float64{"a": 1, "b": 0.1, "c": 0.05}
	st.repCut = map[string]bool{"c": true}
	m.steerRescueLocked(byPath)
	if !rescueLowKey(Link(lb)) || !rescueLowKey(Link(lc)) {
		t.Fatal("lowered/cut links not marked low")
	}
	if !rescueCutKey(Link(lc)) || rescueCutKey(Link(lb)) || rescueCutKey(Link(la)) {
		t.Fatal("cut mark: only the cut path's link")
	}
	st.repCut = map[string]bool{}
	m.steerRescueLocked(byPath)
	if rescueCutKey(Link(lc)) {
		t.Fatal("no longer cut: still marked")
	}
	st.repCut = map[string]bool{"a": true, "b": true, "c": true}
	m.steerRescueLocked(byPath)
	if rescueCutKey(Link(la)) {
		t.Fatal("every path cut: nowhere better, nothing should be marked")
	}
	lbRescueCut = false
	if rescueCutKey(Link(lc)) {
		t.Fatal("key off: reported cut")
	}
}

// The cut bound is its own, wider than rescuePerSec.
func TestRescueCutBound(t *testing.T) {
	withRescueCut(t, true)
	old, oldC := rescuePerSec, rescueCutPerSec
	rescuePerSec, rescueCutPerSec = 2, 5
	t.Cleanup(func() { rescuePerSec, rescueCutPerSec = old, oldC })
	rescueCutMark(nil, []any{9})
	now := time.Unix(7000, 0)
	cut, low := 0, 0
	for i := 0; i < 10; i++ {
		if rescueTake(9, now) {
			cut++
		}
		if rescueTake(8, now) {
			low++
		}
	}
	if cut != 5 || low != 2 {
		t.Fatalf("cut %d (want 5), lowered %d (want 2)", cut, low)
	}
}

// HS2_LB_RESCUE_CUT: a light raw connection on a CUT link gets its copy past
// a binding budget, with HS2_LB_RESCUE_RAW off; on a merely lowered link it
// stays single (TestMConnRescueRawOffByDefault).
func TestMConnRescueCutRaw(t *testing.T) {
	withRescue(t, true)
	withRescueCut(t, true)
	before := runtime.NumGoroutine()
	n, iran, exit := newRawPair(t)
	liteBud.add(time.Now().Unix(), 1_000_000, 100_000)
	rescueMark(nil, []any{0})
	time.Sleep(mLiteAfter + 1500*time.Millisecond)
	if liveSubs(iran) != 1 || n.link(1) != nil {
		t.Fatalf("lowered (not cut): rescued raw: %d subs", liveSubs(iran))
	}
	liteBud.add(time.Now().Unix(), 1_000_000, 100_000)
	rescueCutMark(nil, []any{0})
	mWaitFor(t, "cut raw rescue", mLiteAfter+3*time.Second, func() bool {
		return liveSubs(iran) == 2 && liveSubs(exit) == 2 && !iran.rawW.Load()
	})
	if l := n.link(1); l.plane != planeLight || len(l.avoid) == 0 || l.avoid[0] != 0 {
		t.Fatalf("copy: plane %v avoid %v, want light avoiding link 0", l.plane, l.avoid)
	}
	up, down := randBytes(32<<10, 41), randBytes(32<<10, 42)
	both(t,
		func() error { return xfer(iran, up, 4<<10, 0) },
		func() error { return xfer(exit, down, 4<<10, 0) },
		func() error { return readN(exit, up) },
		func() error { return readN(iran, down) },
	)
	rescueMark(map[any]bool{0: true}, nil)
	rescueCutMark(map[any]bool{0: true}, nil)
	resetLiteBudget()
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// HS2_LB_RESCUE_CUT: a dual light connection with its first sub on a CUT
// link and an established copy elsewhere gives up the bad sub through the
// close handshake; the copy becomes its primary, data flows both ways, no
// byte lost. Key off: both subs stay.
func TestMConnRescueCutDrop(t *testing.T) {
	for _, on := range []bool{false, true} {
		withRescue(t, true)
		withRescueCut(t, on)
		setCloseHS(t, true)
		before := runtime.NumGoroutine()
		_, iran, exit := newLitePair(t)
		iran.setStripe(8, nil) // as in production (HS2_MCONN_STRIPE=8): may stripe, never did
		mWaitFor(t, "upgrade to two subs", mLiteAfter+2*time.Second, func() bool {
			return liveSubs(iran) == 2 && liveSubs(exit) == 2
		})
		up, down := randBytes(16<<10, 51), randBytes(16<<10, 52)
		both(t,
			func() error { return xfer(iran, up, 4<<10, 0) },
			func() error { return xfer(exit, down, 4<<10, 0) },
			func() error { return readN(exit, up) },
			func() error { return readN(iran, down) },
		)
		prev := rescueMark(nil, []any{0})
		prevC := rescueCutMark(nil, []any{0})
		if on {
			mWaitFor(t, "bad sub given up", 3*time.Second, func() bool {
				return liveSubs(iran) == 1 && liveSubs(exit) == 1
			})
			iran.mu.Lock()
			p := iran.primary.Load()
			iran.mu.Unlock()
			if p == nil || p.key == any(0) {
				t.Fatalf("primary %v still on the cut link", p)
			}
		} else {
			time.Sleep(2 * mLitePoll)
			if liveSubs(iran) != 2 {
				t.Fatalf("key off: %d subs", liveSubs(iran))
			}
		}
		up, down = randBytes(64<<10, 53), randBytes(64<<10, 54)
		both(t,
			func() error { return xfer(iran, up, 4<<10, 0) },
			func() error { return xfer(exit, down, 4<<10, 0) },
			func() error { return readN(exit, up) },
			func() error { return readN(iran, down) },
		)
		rescueMark(prev, nil)
		rescueCutMark(prevC, nil)
		resetLiteBudget()
		closeAndWait(t, iran, exit)
		noLeak(t, before)
	}
}

// prod73: a light raw connection capped by the dup budget on a CUT path
// takes its copy past the budget (bounded only by rescueCutPerSec) and is
// counted in rescueDiagRawCappedCut; on a merely lowered path (no
// HS2_LB_RESCUE_RAW) it is counted as capped but not cut, and takes none.
func TestRescueRawCutBypassesBudget(t *testing.T) {
	withRescue(t, true)
	withRescueCut(t, true)
	oldRaw := lbRescueRaw
	lbRescueRaw = false
	t.Cleanup(func() { lbRescueRaw = oldRaw })
	cutK, lowK := new(int), new(int)
	prevLow := rescueMark(nil, []any{cutK, lowK})
	t.Cleanup(func() { rescueMark(prevLow, nil) })
	rescueCutMark(nil, []any{cutK})
	now := time.Unix(time.Now().Unix()+1000, 0)
	m := &mConn{iran: true, lite: true, mode: planeLight}
	c0, cc0, r0 := rescueDiagRawCapped.Load(), rescueDiagRawCappedCut.Load(), rescueCutRaw.Load()
	m.raw1 = &msub{key: cutK}
	if !m.rescueRawLocked(now) {
		t.Fatal("cut path: no copy past the budget")
	}
	m.raw1 = &msub{key: lowK}
	if m.rescueRawLocked(now) {
		t.Fatal("lowered path took the raw rescue without HS2_LB_RESCUE_RAW")
	}
	if rescueDiagRawCapped.Load() != c0+2 || rescueDiagRawCappedCut.Load() != cc0+1 || rescueCutRaw.Load() != r0+1 {
		t.Fatalf("counters: capped +%d cut +%d copies +%d", rescueDiagRawCapped.Load()-c0, rescueDiagRawCappedCut.Load()-cc0, rescueCutRaw.Load()-r0)
	}
}
