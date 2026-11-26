package engine

import (
	"sync/atomic"
	"testing"
	"time"
)

// l485 (lab3 t1Q): while the first light open hangs on a dark link, the b449e
// slow open must start one second open, not one per manager tick once each
// second open joins. Off: a storm (> 3 opens in 2.5 s of kicked ticks); on:
// exactly 2 opens, and the second carries LIVEPICK's mark.
func TestL485SlowOpenOnce(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(map[bool]string{true: "on", false: "off"}[on], func(t *testing.T) {
			oldSO, oldK := b449SlowOpen.Load(), l485CallOpen.Load()
			b449SlowOpen.Store(int64(time.Second))
			l485CallOpen.Store(on)
			t.Cleanup(func() { b449SlowOpen.Store(oldSO); l485CallOpen.Store(oldK) })
			l485SlowOpenRun(t, on)
		})
	}
}

func l485SlowOpenRun(t *testing.T, on bool) {
	_, iran, exit := newLitePair(t)
	t.Cleanup(func() { iran.Close(); exit.Close() })
	hello := randBytes(300, 85)
	both(t, func() error { return xfer(iran, hello, 300, 0) }, func() error { return readN(exit, hello) })
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var calls, marked atomic.Int32
	iran.mu.Lock()
	orig := iran.open
	iran.open = func(p mPlane, avoid []any) (mSub, any, error) {
		n := calls.Add(1)
		if n == 1 {
			<-release // the first open hangs on a dark link
		} else {
			for _, a := range avoid {
				if _, ok := a.(l480LiveMark); ok {
					marked.Add(1)
				}
			}
		}
		return orig(p, avoid)
	}
	iran.lightOpening = true
	iran.mu.Unlock()
	t0 := time.Now()
	go iran.openLight(nil)
	for time.Since(t0) < 2500*time.Millisecond {
		iran.kickMgr() // a call's writes wake the manager every ~20 ms
		time.Sleep(10 * time.Millisecond)
	}
	n := calls.Load()
	t.Logf("on=%v: opens %d (second opens marked live %d) sodup %d", on, n, marked.Load(), l485Stat.soDup.Load())
	if on {
		if n != 2 {
			t.Fatalf("on: %d opens, want 2 (one second open per pending open)", n)
		}
		if marked.Load() != 1 {
			t.Fatalf("on: second open without LIVEPICK's mark")
		}
	} else if n <= 3 {
		t.Fatalf("off: %d opens; the storm this key fixes did not show (test no longer models it?)", n)
	}
}

// l485FreshCapLocked: 3 subs joined just now that received nothing cap
// further opens; a read on one, or the key off, lifts the cap.
func TestL485FreshCap(t *testing.T) {
	old := l485CallOpen.Load()
	t.Cleanup(func() { l485CallOpen.Store(old) })
	now := time.Now()
	m := &mConn{}
	for i := 0; i < 3; i++ {
		s := &msub{}
		s.lastRecv.Store(monoNano(now))
		s.l485Join = s.lastRecv.Load()
		m.subs = append(m.subs, s)
	}
	l485CallOpen.Store(false)
	if m.l485FreshCapLocked(now) {
		t.Fatal("off: capped")
	}
	l485CallOpen.Store(true)
	if !m.l485FreshCapLocked(now) {
		t.Fatal("on: 3 fresh silent subs not capped")
	}
	if m.l485FreshCapLocked(now.Add(l485FreshFor + time.Millisecond)) {
		t.Fatal("on: aged-out subs still capped")
	}
	m.subs[0].lastRecv.Store(monoNano(now) + 1) // one read
	if m.l485FreshCapLocked(now) {
		t.Fatal("on: a sub that read still counted fresh")
	}
	if got := l480WithLiveAlways(l480WithLiveAlways(nil)); len(got) != 1 {
		t.Fatalf("mark twice: %d", len(got))
	}
}
