package engine

import (
	"testing"
	"time"
)

func withCallLate(t *testing.T, paths map[any]string, srtt map[any]time.Duration) {
	t.Helper()
	withRescue(t, true)
	oldOn, oldF := lbCallLate, callLateSRTT
	lbCallLate = true
	callLateSRTT = func(s *msub) (time.Duration, bool) {
		d, ok := srtt[s.key]
		return d, ok
	}
	for k, p := range paths {
		callLinkPath.Store(k, p)
	}
	t.Cleanup(func() {
		lbCallLate, callLateSRTT = oldOn, oldF
		callLinkPath.Range(func(k, _ any) bool { callLinkPath.Delete(k); return true })
	})
}

// Unit (8.12): a call sub on another path whose srtt is 100 ms and twice
// above the best one's for a second opens a fresh sub off that path; equal
// srtt, one path, a short spike, the key off: nothing; the late sub is given
// up only once a sub opened after the trigger has read.
func TestCallLateLocked(t *testing.T) {
	srtt := map[any]time.Duration{10: 80 * time.Millisecond, 11: 240 * time.Millisecond, 12: 80 * time.Millisecond}
	withCallLate(t, map[any]string{10: "C", 11: "B", 12: "C", 13: "A"}, srtt)
	newM := func(keys ...any) (*mConn, []*msub) {
		m := &mConn{iran: true, lite: true}
		m.callOn = true
		m.open = func(mPlane, []any) (mSub, any, error) { return nil, nil, errMConnClosed }
		var live []*msub
		for i, k := range keys {
			live = append(live, &msub{key: k, idx: i})
		}
		return m, live
	}
	now := time.Now()
	o0 := callLateOpened.Load()

	// late copy on B: no trigger before callLateFor, a break restarts it
	m, live := newM(10, 11)
	m.callLateLocked(live, now)
	srtt[11] = 120 * time.Millisecond // a spike's end: not 100 ms above
	m.callLateLocked(live, now.Add(500*time.Millisecond))
	srtt[11] = 240 * time.Millisecond
	m.callLateLocked(live, now.Add(600*time.Millisecond))
	m.callLateLocked(live, now.Add(1500*time.Millisecond))
	if callLateOpened.Load() != o0 {
		t.Fatal("trigger before callLateFor of unbroken evidence")
	}
	m.callLateLocked(live, now.Add(1700*time.Millisecond))
	if callLateOpened.Load() != o0+1 || m.lateKey != 11 {
		t.Fatalf("no trigger on the late copy: %d %v", callLateOpened.Load()-o0, m.lateKey)
	}
	// pending: the old good sub reading is no fresh sub; nothing given up
	live[0].lastRecv.Store(now.Add(2 * time.Second).UnixNano())
	if m.callLateLocked(live, now.Add(2*time.Second)); m.lateKey != 11 {
		t.Fatal("late sub released by a sub that was live at the trigger")
	}
	// a fresh sub read: the late one is due (its rw has no handshake here,
	// so it is not closed, but the pending state ends)
	fresh := &msub{key: 13, idx: 2}
	fresh.lastRecv.Store(now.Add(2100 * time.Millisecond).UnixNano())
	m.callLateLocked(append(live, fresh), now.Add(2200*time.Millisecond))
	if m.lateKey != nil {
		t.Fatal("pending state kept after a fresh sub read")
	}
	// one trigger per callStuckEvery
	time.Sleep(50 * time.Millisecond) // the failed open clears lightOpening
	m.mu.Lock()
	m.lightOpening, m.lightRetryAt = false, time.Time{}
	m.mu.Unlock()
	for d := 2300 * time.Millisecond; d < 6*time.Second; d += 100 * time.Millisecond {
		m.callLateLocked(live, now.Add(d))
	}
	if callLateOpened.Load() != o0+1 {
		t.Fatal("second trigger within callStuckEvery")
	}

	// both subs on one path (the stuck trigger's case): nothing
	m2, live2 := newM(10, 12)
	srtt[12] = 300 * time.Millisecond
	for d := time.Duration(0); d < 3*time.Second; d += 100 * time.Millisecond {
		m2.callLateLocked(live2, now.Add(d))
	}
	srtt[12] = 80 * time.Millisecond
	// equal paths: nothing
	srtt[11] = 90 * time.Millisecond
	m3, live3 := newM(10, 11)
	for d := time.Duration(0); d < 3*time.Second; d += 100 * time.Millisecond {
		m3.callLateLocked(live3, now.Add(d))
	}
	// key off: nothing
	srtt[11] = 240 * time.Millisecond
	lbCallLate = false
	m4, live4 := newM(10, 11)
	for d := time.Duration(0); d < 3*time.Second; d += 100 * time.Millisecond {
		m4.callLateLocked(live4, now.Add(d))
	}
	lbCallLate = true
	// not a call: nothing
	m5, live5 := newM(10, 11)
	m5.callOn = false
	for d := time.Duration(0); d < 3*time.Second; d += 100 * time.Millisecond {
		m5.callLateLocked(live5, now.Add(d))
	}
	if callLateOpened.Load() != o0+1 {
		t.Fatalf("false triggers: %d", callLateOpened.Load()-o0-1)
	}
}

// Unit (8.12): with a late sub and two timely ones, every call chunk goes on
// the two timely subs (both copies), none on the late one; without a third
// sub, the late one keeps its copy (the usual dual mode).
func TestCallPairSkipsLate(t *testing.T) {
	setCall(t, "1")
	m := newMConn(1, true, nil)
	m.lite = true
	defer func() { // the test subs have no writers: end without them
		m.mu.Lock()
		m.subs = nil
		m.mu.Unlock()
		m.end(errMConnClosed, false)
	}()
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := 0; i < 3; i++ {
		m.subs = append(m.subs, &msub{m: m, idx: i})
	}
	m.subs[1].callLate = true
	for i := 0; i < 6; i++ {
		c := getChunk(mRecData)
		c.n = 1000
		if ok, _ := m.callPairLocked(c); !ok {
			t.Fatal("chunk not paired")
		}
	}
	if len(m.subs[1].q) != 0 || len(m.subs[0].q) != 6 || len(m.subs[2].q) != 6 {
		t.Fatalf("queues %d/%d/%d, want 6/0/6", len(m.subs[0].q), len(m.subs[1].q), len(m.subs[2].q))
	}
	m.subs = m.subs[:2] // the late sub and one timely: usual dual mode
	if ok, _ := m.callPairLocked(getChunk(mRecData)); ok {
		t.Fatal("paired with one timely sub")
	}
}
