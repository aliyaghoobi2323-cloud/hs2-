package engine

import (
	"testing"
	"time"
)

// TestUL257Stuck: a raw upload blocked in Write for most of the sample and
// moving under stripeFastRate is stuck; app-limited, fast, small, downloads
// and the key off are not.
func TestUL257Stuck(t *testing.T) {
	m := newMConn(1, true, nil)
	m.rawW.Store(true)
	m.rawSent.Store(4 << 20)
	w := time.Second
	slow := float64(stripeFastRate.Load()) / 4
	if !m.ul257StuckLocked(true, 1, slow, w, 700*time.Millisecond) {
		t.Fatal("blocked slow raw upload not stuck")
	}
	if m.ul257StuckLocked(true, 1, slow, w, 300*time.Millisecond) {
		t.Fatal("app-limited upload (writer mostly free) taken as stuck")
	}
	if m.ul257StuckLocked(true, 1, float64(stripeFastRate.Load())*2, w, w) {
		t.Fatal("fast upload taken as stuck")
	}
	if m.ul257StuckLocked(false, 1, slow, w, w) {
		t.Fatal("download taken as stuck")
	}
	if m.ul257StuckLocked(true, 2, slow, w, w) {
		t.Fatal("already striped taken as stuck")
	}
	m.rawSent.Store(100 << 10)
	if m.ul257StuckLocked(true, 1, slow, w, w) {
		t.Fatal("small upload taken as stuck")
	}
	m.rawSent.Store(4 << 20)
	m.rawW.Store(false)
	if m.ul257StuckLocked(true, 1, slow, w, w) {
		t.Fatal("non-raw taken as stuck")
	}
	m.rawW.Store(true)
	m.stEarlyW = 200 * time.Millisecond
	if m.ul257StuckLocked(true, 1, slow, w, w) {
		t.Fatal("early window taken as stuck")
	}
	m.stEarlyW = 0
	ul257Stuck.Store(false)
	defer ul257Stuck.Store(true)
	if m.ul257StuckLocked(true, 1, slow, w, w) {
		t.Fatal("key off but stuck")
	}
}

// TestUL257Blocked: finished writes add their time; a write still blocked
// counts up to now; each call returns only the time since the last one.
func TestUL257Blocked(t *testing.T) {
	m := newMConn(1, true, nil)
	t0 := m.ul257WriteStart()
	time.Sleep(30 * time.Millisecond)
	m.ul257WriteEnd(t0)
	if d := m.rawBlockedLocked(time.Now()); d < 25*time.Millisecond || d > 500*time.Millisecond {
		t.Fatalf("finished write blocked %v", d)
	}
	if d := m.rawBlockedLocked(time.Now()); d != 0 {
		t.Fatalf("second read %v, want 0", d)
	}
	m.ul257WriteStart() // stuck write, never returns in this test
	time.Sleep(40 * time.Millisecond)
	d1 := m.rawBlockedLocked(time.Now())
	if d1 < 35*time.Millisecond {
		t.Fatalf("in-progress write counted %v", d1)
	}
	time.Sleep(20 * time.Millisecond)
	if d2 := m.rawBlockedLocked(time.Now()); d2 < 15*time.Millisecond || d2 > 400*time.Millisecond {
		t.Fatalf("in-progress delta %v", d2)
	}
}

// TestUL257Up: a sample that moved nothing either way (a blacked-out first
// sub) still counts as an upload for the stuck rule; a download does not.
func TestUL257Up(t *testing.T) {
	if !ul257Up(0, 0, false) {
		t.Fatal("0/0 sample (blackout) must count as an upload")
	}
	if !ul257Up(1<<20, 100, false) || ul257Up(100, 1<<20, false) || ul257Up(1<<20, 0, true) {
		t.Fatal("direction")
	}
}
