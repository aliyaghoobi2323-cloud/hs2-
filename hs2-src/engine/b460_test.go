package engine

import (
	"net"
	"testing"
	"time"
)

func setHedge4(t *testing.T, on bool) {
	o4 := liteHedge4
	setHedge3(t, on)
	liteHedge4 = on
	t.Cleanup(func() { liteHedge4 = o4 })
}

// Exit, hedge-created conn: Iran's first sub joining later becomes the
// primary (and is counted); with the key off the hedge sub stays primary.
func TestB460FirstSubPrimary(t *testing.T) {
	for _, on := range []bool{true, false} {
		setHedge4(t, on)
		liteHedge3, liteHedge = true, true
		mc := newMConnExit(51)
		closeAtEnd(t, mc)
		a1, b1 := net.Pipe()
		a2, b2 := net.Pipe()
		t.Cleanup(func() { a1.Close(); b1.Close(); a2.Close(); b2.Close() })
		mc.mu.Lock()
		mc.h4Made = true
		mc.mu.Unlock()
		mc.attach(newTSub(b1), mconnRoleFirst) // the hedge sub came first
		mc.mu.Lock()
		hedge := mc.primary.Load()
		mc.hedge4WantFirstLocked()
		mc.mu.Unlock()
		p0, s0 := hedge4Stat.firstPref.Load(), hedge4Stat.switched.Load()
		mc.attach(newTSub(b2), mconnRoleJoin) // Iran's first sub joins
		mc.mu.Lock()
		p, first := mc.primary.Load(), mc.h4First
		mc.mu.Unlock()
		if on {
			if first == nil || p != first || p == hedge {
				t.Fatalf("on: primary %v first %v hedge %v", p, first, hedge)
			}
			if hedge4Stat.firstPref.Load() != p0+1 || hedge4Stat.switched.Load() != s0+1 {
				t.Fatal("on: not counted")
			}
			mc.mu.Lock()
			mc.h3Heavy = true
			f0, h0 := hedge4Stat.firstB.Load(), hedge4Stat.hedgeB.Load()
			mc.hedge4OneCopyLocked(first, 100)
			mc.hedge4OneCopyLocked(hedge, 7)
			mc.mu.Unlock()
			if hedge4Stat.firstB.Load() != f0+100 || hedge4Stat.hedgeB.Load() != h0+7 {
				t.Fatal("on: byte split wrong")
			}
		} else if p != hedge || first != nil {
			t.Fatalf("off: primary moved (%v %v)", p, first)
		}
	}
}

// A heavy download over a hedged pair with HEDGE4 is delivered whole, goes
// one copy, and counts its one-copy bytes on the first sub.
func TestB460HeavyDownload(t *testing.T) {
	setHedge4(t, true)
	_, iran, exit := newHedge2Pair(t, nil)
	mWaitFor(t, "two subs", time.Second, func() bool { return liveSubs(iran) == 2 })
	exit.exitHedge3Mark(false)
	f0 := hedge4Stat.firstB.Load()
	resp := randBytes(8*int(hedge3Bytes), 21)
	both(t, func() error { return xfer(exit, resp, 4000, 0) }, func() error { return readN(iran, resp) })
	if hedge4Stat.firstB.Load() == f0 {
		t.Fatal("no one-copy bytes on the first sub")
	}
	iran.kickMgr()
	mWaitFor(t, "hedge dropped", 3*time.Second, func() bool { return liveSubs(iran) == 1 })
	more := randBytes(5000, 22)
	both(t, func() error { return xfer(exit, more, 1000, 0) }, func() error { return readN(iran, more) })
}
