package engine

import (
	"testing"
	"time"
)

// TestCs224CopyIdle: a copy sub moving bytes is not taken as idle by the
// drain reclaim (C2: the healthy primary, a former copy sub, was closed).
func TestCs224CopyIdle(t *testing.T) {
	old := cs224CopyIdle.Load()
	t.Cleanup(func() { cs224CopyIdle.Store(old) })
	run := func(on bool) (moving, quiet bool) {
		cs224CopyIdle.Store(on)
		now := time.Now()
		l := &mtcpLink{flows: map[*countedStream]struct{}{}}
		busy := &countedStream{link: l, lastActive: now.Add(-time.Minute)}
		idle := &countedStream{link: l, lastActive: now.Add(-time.Minute)}
		busy.copyOnly.Store(true)
		idle.copyOnly.Store(true)
		l.flows[busy], l.flows[idle] = struct{}{}, struct{}{}
		idle.bytes.Store(5000)
		l.flowStats(now.Add(-30*time.Second), 2*time.Second, 10*time.Second) // idle's last move, 30 s ago
		busy.bytes.Store(5000)
		l.flowStats(now.Add(-2*time.Second), 2*time.Second, 10*time.Second)
		busy.bytes.Add(580_000) // ~290 KB/s, the call
		l.flowStats(now, 2*time.Second, 10*time.Second)
		for _, c := range l.idleStreams(now, drainStall, 100) {
			switch c.cs {
			case busy:
				moving = true
			case idle:
				quiet = true
			}
		}
		return
	}
	if moving, quiet := run(true); moving || !quiet {
		t.Fatalf("key on: moving copy taken idle %v, quiet copy taken %v (want false, true)", moving, quiet)
	}
	if moving, _ := run(false); !moving {
		t.Fatal("key off: the old rule took the moving copy as idle (want true)")
	}
}

// TestCs224StaleSwitch: a second switch off a sub an earlier switch left
// (still dark, primary again by fallback) resends nothing from the ring;
// once that sub lit again, a switch off it resends as before.
func TestCs224StaleSwitch(t *testing.T) {
	setDarkcall(t)
	oldK, oldS := cs221Darkcall.Load(), cs224StaleSwitchOn.Load()
	t.Cleanup(func() { cs221Darkcall.Store(oldK); cs224StaleSwitchOn.Store(oldS) })
	cs221Darkcall.Store(true)
	cs224StaleSwitchOn.Store(true)
	_, iran, exit := darkswPairDC(t)
	f := newDcFake()
	darkcallReadSet(f.read)
	const unc = 60_000
	darkcallMarksSet(func(*msub) (uint32, uint32, bool) { return unc, 0, true })
	data := randBytes(150_000, 37)
	both(t,
		func() error { return xfer(exit, data, 2000, 0) },
		func() error { return readN(iran, data) },
	)
	exit.mu.Lock()
	defer exit.mu.Unlock()
	p := exit.primary.Load()
	var to *msub
	for _, s := range exit.rrLiveSubsLocked() {
		if s != p {
			to = s
		}
	}
	if to == nil || exit.missLost {
		t.Fatalf("setup: to=%v missLost=%v", to, exit.missLost)
	}
	to.missEnd = exit.sendOff // a fresh join: lacks all below
	r0, s0 := darkcallStat.resent.Load(), cs224Stat.stale.Load()
	exit.dcSwitchLocked(p, to, time.Now(), 0)
	first := darkcallStat.resent.Load() - r0
	if first == 0 || !p.dcLeft || cs224Stat.stale.Load() != s0 {
		t.Fatalf("first switch: resent %d dcLeft %v stale +%d (want >0, true, 0)", first, p.dcLeft, cs224Stat.stale.Load()-s0)
	}
	// to is gone, p (still dark) is primary again; a fresh sub joins
	r0 = darkcallStat.resent.Load()
	exit.dcSwitchLocked(p, to, time.Now(), 0)
	if got := darkcallStat.resent.Load() - r0; got != 0 || cs224Stat.stale.Load() != s0+1 {
		t.Fatalf("second switch off a left dark sub: resent %d stale +%d (want 0, 1)", got, cs224Stat.stale.Load()-s0)
	}
	// p lit again (dcLookLocked clears the mark), then dark: fresh marks
	f.set(p, "live")
	p.dc.isDark = true
	exit.dcLookLocked(p, time.Now(), ctrlNow())
	if p.dcLeft {
		t.Fatal("lit sub kept dcLeft")
	}
	r0 = darkcallStat.resent.Load()
	exit.dcSwitchLocked(p, to, time.Now(), 0)
	if got := darkcallStat.resent.Load() - r0; got != first {
		t.Fatalf("switch after lit: resent %d, want %d", got, first)
	}
	// key off: the old rule resends again off a left sub
	cs224StaleSwitchOn.Store(false)
	r0 = darkcallStat.resent.Load()
	exit.dcSwitchLocked(p, to, time.Now(), 0)
	if got := darkcallStat.resent.Load() - r0; got != first {
		t.Fatalf("key off: resent %d, want %d", got, first)
	}
}
