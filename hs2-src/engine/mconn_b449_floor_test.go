package engine

import (
	"testing"
	"time"
)

// b449c: Iran's lost-copy reset with nothing lost on the wire. The first
// sub's link swallows a request (it dies right after, the way a b449 move
// ends a stuck sub), the second sub joined < mHoleSafe ago (holeRecent), and
// the first sub has no kept copy of the request: (age) the write-path age
// trim dropped it without TCP proof (fx212 agegap), or (cap) the 32 MiB
// global keep cap refused it. Before the keep floor: no resend, the old rule
// resets the connection. With it: the request is kept, resent on the second
// sub, byte-exact.
func TestB449FloorAgeGapNoReset(t *testing.T) { b449FloorBoth(t, false) }

func TestB449FloorCapNoReset(t *testing.T) { b449FloorBoth(t, true) }

// b449FloorBoth: floor 0 (the code before b449c) must reproduce the reset,
// the default floor must not.
func b449FloorBoth(t *testing.T, capped bool) {
	for _, fl := range []int64{0, 64 << 10} {
		t.Run(map[bool]string{true: "floor", false: "nofloor"}[fl > 0], func(t *testing.T) {
			oldF := b449KeepFloor.Load()
			b449KeepFloor.Store(fl)
			t.Cleanup(func() { b449KeepFloor.Store(oldF) })
			b449FloorRun(t, capped)
		})
	}
}

func b449FloorRun(t *testing.T, capped bool) {
	setCloseHS(t, true)
	setB449Keep(t)
	n, iran, exit := newLitePair(t)
	t.Cleanup(func() { b449FloorDone(t, iran, exit) }) // before setB449Keep's restore (LIFO)
	hello := randBytes(300, 61)
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
	if capped {
		// every connection together is over the cap (an atomic shift of the
		// gauge: other tests' connections may still run)
		subResendStat.kept.Add(40 << 20)
		t.Cleanup(func() { subResendStat.kept.Add(-40 << 20) })
	}
	l := n.link(0)
	l.iran.drop.Store(true)
	l.exit.drop.Store(true)
	r0, c0 := subLostResets.Load(), subResendStat.capped.Load()
	req := randBytes(20000, 62)
	if _, e := iran.Write(req); e != nil {
		t.Fatal(e)
	}
	time.Sleep(100 * time.Millisecond)
	if !capped {
		// the write path's age trim of the first sub: its chunks are older
		// than the keep age and its link read lately (no TCP look)
		now := time.Now()
		iran.mu.Lock()
		for i := range s0.sent {
			s0.sent[i].t = now.Add(-2 * time.Second)
		}
		s0.lastRecv.Store(monoNano(now))
		iran.subAgeTrimLocked(now, false)
		s0.lastRecv.Store(monoNano(now.Add(-2 * time.Second))) // then dark: the 1 s look has no proof either
		iran.mu.Unlock()
	} else if subResendStat.capped.Load() == c0 && b449KeepFloor.Load() == 0 {
		t.Fatal("no write refused by the cap")
	}
	// the live subs read lately (fresh resend targets); Iran's joined lately
	for _, m := range []*mConn{iran, exit} {
		m.mu.Lock()
		for _, x := range m.subs {
			if x != s0 {
				x.lastRecv.Store(monoNano(time.Now()))
				if m == iran {
					x.lastHole = time.Now() // joined (or missed a copy) < mHoleSafe ago: a b449 move target
				}
			}
		}
		m.mu.Unlock()
	}
	iran.mu.Lock()
	gap := !s0.subGapT.IsZero()
	iran.mu.Unlock()
	l.iran.Close()
	l.exit.Close()
	err := readNTimeout(exit, req, 5*time.Second)
	mWaitFor(t, "the first sub gone", 3*time.Second, func() bool {
		iran.mu.Lock()
		defer iran.mu.Unlock()
		return s0.dead || iran.ended
	})
	time.Sleep(50 * time.Millisecond)
	resets := subLostResets.Load() - r0
	t.Logf("after: nofull=%d resends=%d subs=%d", subResendStat.noFull.Load(), subResendStat.resends.Load(), liveSubs(iran))
	t.Logf("floor %dK capped=%v: first sub gap=%v, lost-copy resets %d, iran ended=%v err=%v", b449KeepFloor.Load()>>10, capped, gap, resets, rstEnded(iran), err)
	if b449KeepFloor.Load() == 0 {
		// the old code: the reset (or a stall: either way the request is lost)
		if !gap || resets == 0 && err == nil {
			t.Fatalf("floor off: no repro (gap=%v resets=%d err=%v)", gap, resets, err)
		}
		iran.Close()
		exit.Close()
		return
	}
	if gap || resets != 0 || err != nil || rstEnded(iran) {
		t.Fatalf("lost-copy reset with nothing lost: gap=%v resets=%d err=%v ended=%v", gap, resets, err, rstEnded(iran))
	}
	more := randBytes(30000, 63)
	both(t, func() error { return xfer(iran, more, 2000, 0) }, func() error { return readN(exit, more) })
	iran.Close()
	exit.Close()
}

// readNTimeout: readN with a deadline.
func readNTimeout(m *mConn, want []byte, d time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- readN(m, want) }()
	select {
	case e := <-done:
		return e
	case <-time.After(d):
		return errTimeoutTest
	}
}

// b449c: the floor's unit rules: the write-path trim drops only down to the
// floor; a write over the global cap is kept under the floor, not over it,
// and never over b449KeepHard.
func TestB449FloorRules(t *testing.T) {
	oldF := b449KeepFloor.Load()
	t.Cleanup(func() { b449KeepFloor.Store(oldF) })
	b449KeepFloor.Store(1000)
	m := &mConn{mode: planeLight}
	s := &msub{m: m}
	m.subs = []*msub{s}
	now := time.Now()
	s.lastRecv.Store(monoNano(now))
	m.mu.Lock()
	defer m.mu.Unlock()
	for range 3 {
		fx212Keep(m, s, 600)
	}
	for i := range s.sent {
		s.sent[i].t = now.Add(-2 * time.Second)
	}
	m.subAgeTrimLocked(now, false)
	if m.keptBytes != 600 || len(s.sent) != 1 || s.subGapT.IsZero() {
		t.Fatalf("kept %d in %d chunks gap %v, want 600 in 1 (down to the 1000 floor), a gap for the 1200 dropped", m.keptBytes, len(s.sent), s.subGapT)
	}
	s.subGapT = time.Time{}
	// over the global cap (an atomic shift: other tests' connections may run)
	over := subKeepGlobalMax + 1
	subResendStat.kept.Add(over)
	defer subResendStat.kept.Add(-over)
	if !m.subKeepRoomLocked(s) {
		t.Fatal("a write under the floor refused over the global cap")
	}
	fx212Keep(m, s, 600)
	if m.subKeepRoomLocked(s) {
		t.Fatal("a write over the floor kept over the global cap")
	}
	m.dropSentLocked(s, 1)
	subResendStat.kept.Add(b449KeepHard)
	defer subResendStat.kept.Add(-b449KeepHard)
	if m.subKeepRoomLocked(s) {
		t.Fatal("a write kept over b449KeepHard")
	}
	fx212Keep(m, s, 600)
	for i := range s.sent {
		s.sent[i].t = now.Add(-2 * time.Second)
	}
	m.subAgeTrimLocked(now, false)
	if len(s.sent) != 0 {
		t.Fatal("the age hold ran over b449KeepHard")
	}
	m.dropSentLocked(s, len(s.sent))
	m.subSweepDropLocked() // the over-cap refusal armed the sweeper
}

// b449FloorDone: both ends closed and out of the keep sweeper, so restoring
// the keys (setB449Keep, fx212SubKeys) does not race the sweeper's reads.
func b449FloorDone(t *testing.T, ms ...*mConn) {
	for _, m := range ms {
		m.Close()
	}
	mWaitFor(t, "out of the keep sweeper", 5*time.Second, func() bool {
		for _, m := range ms {
			if subSweepHas(m) {
				return false
			}
		}
		return true
	})
}
