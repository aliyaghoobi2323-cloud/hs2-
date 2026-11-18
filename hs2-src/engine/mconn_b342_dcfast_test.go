package engine

import (
	"net"
	"testing"
	"time"
)

func setB342(t *testing.T, on bool, srtt time.Duration) {
	t.Helper()
	old, of, os := b342DCFast.Load(), b342FloorNs.Load(), b342SRTTP.Load()
	b342DCFast.Store(on)
	f := func(*net.TCPConn) (time.Duration, bool) { return srtt, srtt > 0 }
	b342SRTTP.Store(&f)
	t.Cleanup(func() { b342DCFast.Store(old); b342FloorNs.Store(of); b342SRTTP.Store(os) })
}

func b342Primary(m *mConn) *msub {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.primary.Load()
}

func b342Setup(t *testing.T, on bool, srtt time.Duration) (*mConn, *mConn, *dcFake) {
	t.Helper()
	setDarkcall(t)
	setDc254(t, true)
	setB342(t, on, srtt)
	_, iran, exit := darkswPairDC(t)
	f := newDcFake()
	darkcallReadSet(f.read)
	darkcallMarksSet(func(*msub) (uint32, uint32, bool) { return 0, 0, true })
	return iran, exit, f
}

// The rb339 R1 shape: the primary has bytes in flight, no ack progress,
// backoff 0 and no retransmits (an inflated RTO has not fired yet); the
// other copy's link acks. Key off: darkcall waits (not dark, not txDark).
// Key on: switched once the stall passes max(3 srtt, 300 ms), not before.
func TestB342FastSwitchNoBackoff(t *testing.T) {
	iran, exit, f := b342Setup(t, false, 0)
	p := b342Primary(exit)
	f.set(p, "stall0")
	sw0 := darkcallStat.switched.Load()
	dcVisit(exit, 8) // ~600 ms
	if darkcallStat.switched.Load() != sw0 || b342Primary(exit) != p {
		t.Fatal("key off: switched without RTO evidence")
	}
	closeAndWait(t, iran, exit)

	iran, exit, f = b342Setup(t, true, 0)
	p = b342Primary(exit)
	f.set(p, "stall0")
	sw0, f0 := darkcallStat.switched.Load(), b342Stat.sw.Load()
	dcVisit(exit, 2) // ~150 ms < 300 ms floor
	if b342Primary(exit) != p || b342Stat.sw.Load() != f0 {
		t.Fatal("switched before the 300 ms floor")
	}
	dcVisit(exit, 6)
	np := b342Primary(exit)
	if np == p || b342Stat.sw.Load() != f0+1 || darkcallStat.switched.Load() != sw0+1 {
		t.Fatalf("key on: no single fast switch (sw %d)", b342Stat.sw.Load()-f0)
	}
	closeAndWait(t, iran, exit)
}

// The threshold follows the primary's srtt: srtt 200 ms -> 600 ms.
func TestB342ThreshSRTT(t *testing.T) {
	iran, exit, f := b342Setup(t, true, 200*time.Millisecond)
	p := b342Primary(exit)
	f.set(p, "stall0")
	f0 := b342Stat.sw.Load()
	dcVisit(exit, 5) // ~375 ms
	if b342Primary(exit) != p || b342Stat.sw.Load() != f0 {
		t.Fatal("switched before 3 x srtt")
	}
	dcVisit(exit, 8)
	if b342Primary(exit) == p || b342Stat.sw.Load() != f0+1 {
		t.Fatal("no switch after 3 x srtt")
	}
	closeAndWait(t, iran, exit)
}

// Never to a worse copy: every other sub stalled too (or silent, or on the
// same state) -> no fast switch, one no-candidate episode counted.
func TestB342NoWorseCopy(t *testing.T) {
	iran, exit, f := b342Setup(t, true, 0)
	p := b342Primary(exit)
	exit.mu.Lock()
	live := exit.rrLiveSubsLocked()
	exit.mu.Unlock()
	for _, s := range live {
		f.set(s, "stall0")
	}
	sw0, n0 := darkcallStat.switched.Load(), b342Stat.noCand.Load()
	dcVisit(exit, 10)
	if b342Primary(exit) != p || darkcallStat.switched.Load() != sw0 {
		t.Fatal("switched to a copy that is stalled too")
	}
	if got := b342Stat.noCand.Load() - n0; got != 1 {
		t.Fatalf("no-candidate episodes %d, want 1", got)
	}
	// a candidate whose link was not heard from is not taken either
	for _, s := range live {
		if s != p {
			f.set(s, "noback")
		}
	}
	dcVisit(exit, 4)
	if b342Primary(exit) != p || darkcallStat.switched.Load() != sw0 {
		t.Fatal("switched to a silent copy")
	}
	closeAndWait(t, iran, exit)
}

// Quiet call: every link acks (or has nothing in flight) -> no switch.
func TestB342QuietNoSwitch(t *testing.T) {
	iran, exit, _ := b342Setup(t, true, 0)
	p := b342Primary(exit)
	sw0 := darkcallStat.switched.Load()
	dcVisit(exit, 12)
	if b342Primary(exit) != p || darkcallStat.switched.Load() != sw0 {
		t.Fatal("switched a healthy call")
	}
	closeAndWait(t, iran, exit)
}

// Hysteresis: after a fast switch the darkcall gap holds; the new primary
// stalling inside the gap does not flip the call back; after the gap it may.
func TestB342Hysteresis(t *testing.T) {
	iran, exit, f := b342Setup(t, true, 0)
	setDarkcallTimes(50*time.Millisecond, 2*time.Second)
	p := b342Primary(exit)
	f.set(p, "stall0")
	f0 := b342Stat.sw.Load()
	dcVisit(exit, 7)
	np := b342Primary(exit)
	if np == p || b342Stat.sw.Load() != f0+1 {
		t.Fatal("no first switch")
	}
	f.set(p, "")
	f.set(np, "stall0")
	dcVisit(exit, 12) // ~900 ms < gap
	if b342Primary(exit) != np || b342Stat.sw.Load() != f0+1 {
		t.Fatal("flapped back inside the gap")
	}
	mWaitFor(t, "switch back after the gap", 4*time.Second, func() bool {
		dcVisit(exit, 1)
		return b342Primary(exit) != np
	})
	if b342Stat.sw.Load() != f0+2 {
		t.Fatalf("fast switches %d, want 2", b342Stat.sw.Load()-f0)
	}
	closeAndWait(t, iran, exit)
}
