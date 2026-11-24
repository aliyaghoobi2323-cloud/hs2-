package engine

import (
	"testing"
	"time"
)

// b449d: the no-fresh-target lost-copy reset. The first sub's link swallows
// a request and dies; the request is kept (the b449c floor, no gap), but the
// other sub has not read for 2 s (no fresh resend target, r68fix3) and joined
// < mHoleSafe ago (holeRecent): the old rule resets. With the target wait the
// chunks wait, the other sub reads 200 ms later (or Iran's open joins) and
// takes them: no reset, the connection goes on byte-exact.
func TestB449TgtWaitNoReset(t *testing.T) {
	for _, w := range []time.Duration{0, 1500 * time.Millisecond} {
		t.Run(map[bool]string{true: "wait", false: "nowait"}[w > 0], func(t *testing.T) {
			old := b449TgtWait.Load()
			b449TgtWait.Store(int64(w))
			t.Cleanup(func() { b449TgtWait.Store(old) })
			b449TgtWaitRun(t, w > 0)
		})
	}
}

func b449TgtWaitRun(t *testing.T, on bool) {
	setCloseHS(t, true)
	setB449Keep(t)
	n, iran, exit := newLitePair(t)
	t.Cleanup(func() { b449FloorDone(t, iran, exit) })
	hello := randBytes(300, 71)
	both(t, func() error { return xfer(iran, hello, 300, 0) }, func() error { return readN(exit, hello) })
	mWaitFor(t, "two subs", mLiteAfter+4*time.Second, func() bool { return liveSubs(iran) == 2 && liveSubs(exit) == 2 })
	iran.mu.Lock()
	s0 := iran.subs[0]
	for _, x := range iran.subs {
		if x.idx < s0.idx {
			s0 = x
		}
	}
	iran.mu.Unlock()
	l := n.link(0)
	l.iran.drop.Store(true)
	l.exit.drop.Store(true)
	r0, w0, sv0 := subLostResets.Load(), b449TwStat.waits.Load(), b449TwStat.saved.Load()
	req := randBytes(20000, 72)
	if _, e := iran.Write(req); e != nil {
		t.Fatal(e)
	}
	time.Sleep(100 * time.Millisecond)
	// Iran's other sub: no read for 2 s (not a fresh target), joined lately (holeRecent)
	old := time.Now().Add(-2 * time.Second)
	iran.mu.Lock()
	var s1 *msub
	for _, x := range iran.subs {
		if x != s0 {
			s1 = x
			x.lastRecv.Store(monoNano(old))
			x.lastHole = time.Now()
		}
	}
	gap := !s0.subGapT.IsZero()
	kept := len(s0.sent)
	iran.mu.Unlock()
	if gap || kept == 0 {
		t.Fatalf("the first sub must hold the request with no gap: gap=%v kept=%d", gap, kept)
	}
	l.iran.Close()
	l.exit.Close()
	mWaitFor(t, "the first sub gone", 3*time.Second, func() bool {
		iran.mu.Lock()
		defer iran.mu.Unlock()
		return s0.dead || iran.ended
	})
	if on {
		time.Sleep(200 * time.Millisecond)
		iran.mu.Lock()
		s1.lastRecv.Store(monoNano(time.Now())) // it reads: a fresh target
		iran.mu.Unlock()
	}
	err := readNTimeout(exit, req, 5*time.Second)
	time.Sleep(100 * time.Millisecond)
	resets := subLostResets.Load() - r0
	t.Logf("wait=%v: lost-copy resets %d, waits %d saved %d, iran ended=%v err=%v", on, resets, b449TwStat.waits.Load()-w0, b449TwStat.saved.Load()-sv0, rstEnded(iran), err)
	if !on {
		if resets == 0 {
			t.Fatalf("wait off: no repro of the no-fresh-target reset (err=%v)", err)
		}
		iran.Close()
		exit.Close()
		return
	}
	if resets != 0 || err != nil || rstEnded(iran) || b449TwStat.saved.Load() == sv0 {
		t.Fatalf("reset with a target 200 ms away: resets=%d err=%v ended=%v saved=%d", resets, err, rstEnded(iran), b449TwStat.saved.Load()-sv0)
	}
	more := randBytes(30000, 73)
	both(t, func() error { return xfer(iran, more, 2000, 0) }, func() error { return readN(exit, more) })
	iran.Close()
	exit.Close()
}

// b449d: the wait is bounded: no target by the deadline is the old reset
// (over = true, counted), never a hang; a target takes the orphans at the
// front of its queue, in order, with their references and copies.
func TestB449TgtWaitTick(t *testing.T) {
	m := &mConn{mode: planeLight}
	dead := &msub{m: m, idx: 0}
	live := &msub{m: m, idx: 1, kick: make(chan struct{}, 1)}
	live.lastRecv.Store(monoNano(time.Now().Add(-5 * time.Second))) // not fresh
	m.subs = []*msub{live}
	m.nsubs = 2
	c1, c2 := getChunk(mRecData), getChunk(mRecData)
	c1.off, c1.n, c2.off, c2.n = 100, 10, 0, 100
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.b449TwStartLocked(dead, []*mchunk{c1, c2}, now)
	if len(m.orphan) != 2 || m.orphan[0] != c2 || c1.copies != 1 {
		t.Fatalf("orphans %d (first off %d), copies %d: want 2 sorted, one copy each", len(m.orphan), m.orphan[0].off, c1.copies)
	}
	if over, d := m.b449TwTickLocked(now.Add(time.Second)); over || d <= 0 {
		t.Fatalf("1 s in: over=%v d=%v, want a later look", over, d)
	}
	e0 := b449TwStat.expired.Load()
	if over, _ := m.b449TwTickLocked(now.Add(time.Duration(b449TgtWait.Load()) + time.Millisecond)); !over {
		t.Fatal("no reset at the deadline: the wait would hang")
	}
	if b449TwStat.expired.Load() != e0+1 || m.b449TwDead != nil {
		t.Fatal("expiry not counted or the wait still on")
	}
	// again, and a sub joins after the death: it takes them, in order
	m.b449TwStartLocked(dead, nil, now)
	m.b449TwDead, m.b449TwN = dead, 2
	nw := &msub{m: m, idx: 2, kick: make(chan struct{}, 1)}
	m.subs = append(m.subs, nw)
	if over, _ := m.b449TwTickLocked(now.Add(100 * time.Millisecond)); over {
		t.Fatal("reset with a joined sub")
	}
	if len(nw.q) != 2 || nw.q[0] != c2 || nw.q[1] != c1 || len(m.orphan) != 0 || m.b449TwDead != nil || nw.txEnd != 110 {
		t.Fatalf("joined sub q=%d txEnd=%d orphans=%d", len(nw.q), nw.txEnd, len(m.orphan))
	}
	for _, c := range nw.q {
		c.copies--
		c.unref()
	}
	nw.q = nil
}
