package engine

import (
	"strings"
	"testing"
	"time"
)

// fakeFlowStream records Close (the flow's stream).
type fakeFlowStream struct {
	stream
	closed bool
}

func (s *fakeFlowStream) Close() error { s.closed = true; return nil }

func udpMoveFlow(mtr *linkMeter, idle time.Duration, now time.Time) (*udpFlow, *fakeFlowStream) {
	st := &fakeFlowStream{}
	return &udpFlow{q: make(chan []byte, 1), stop: make(chan struct{}), st: st, mtr: mtr, last: now.Add(-idle)}, st
}

// TestZDUUDPMoveQuietOldFlows (g400): with the key on, a UDP flow that is
// quiet on a peerOld link is ended (its next datagram opens a flow on a
// current link); an active flow on a peerOld link, a quiet flow on a current
// link and a flow with no stream yet stay. Key off: only the udpIdle rule.
func TestZDUUDPMoveQuietOldFlows(t *testing.T) {
	m00 := zduUDPMoved.Load()
	t.Cleanup(func() { zduUDPMoved.Store(m00) }) // the exact mem-line tests (zdu_counts_test.go) expect no zdu_udp_moved
	now := time.Now()
	oldM, curM := &linkMeter{}, &linkMeter{}
	oldM.zduOld.Store(true)
	build := func() (map[string]*udpFlow, map[string]*fakeFlowStream) {
		fl, sts := map[string]*udpFlow{}, map[string]*fakeFlowStream{}
		add := func(k string, m *linkMeter, idle time.Duration) {
			f, st := udpMoveFlow(m, idle, now)
			fl[k], sts[k] = f, st
		}
		add("old-quiet", oldM, zduUDPMoveIdle+time.Second)
		add("old-active", oldM, time.Second)
		add("cur-quiet", curM, zduUDPMoveIdle+time.Second)
		add("cur-idle", curM, udpIdle+time.Second)
		fl["no-stream"] = &udpFlow{q: make(chan []byte, 1), stop: make(chan struct{}), last: now.Add(-zduUDPMoveIdle - time.Second)}
		return fl, sts
	}
	for _, on := range []bool{false, true} {
		setZDU(t, on)
		m0 := zduUDPMoved.Load()
		fl, sts := build()
		sweepUDPFlows(fl, now)
		want := map[string]bool{"old-active": true, "cur-quiet": true, "no-stream": true, "old-quiet": !on}
		for _, k := range []string{"old-quiet", "old-active", "cur-quiet", "cur-idle", "no-stream"} {
			_, kept := fl[k]
			if kept != want[k] {
				t.Errorf("key %v: flow %s kept=%v, want %v", on, k, kept, want[k])
			}
			if st := sts[k]; st != nil && st.closed == want[k] {
				t.Errorf("key %v: flow %s stream closed=%v, want %v", on, k, st.closed, !want[k])
			}
		}
		moved := zduUDPMoved.Load() - m0
		if wantMoved := map[bool]int64{false: 0, true: 1}[on]; moved != wantMoved {
			t.Errorf("key %v: zdu_udp_moved +%d, want +%d", on, moved, wantMoved)
		}
		if on && !strings.Contains(zduCountsLine(), " zdu_udp_moved=") {
			t.Errorf("key on: mem line %q lacks zdu_udp_moved", zduCountsLine())
		}
	}
	setZDU(t, false)
	if udpSweepEvery() != 30*time.Second {
		t.Errorf("key off sweep period %v, want 30s (unchanged)", udpSweepEvery())
	}
}

// TestZDUPickRetiringBeforeOld (g400): with no serving link, the picker's
// fallback takes a healthy retiring link of the current generation before a
// healthy link of an older one; only with no such link does it use the old.
func TestZDUPickRetiringBeforeOld(t *testing.T) {
	setZDU(t, true)
	m, _ := zduPool(4, 0)
	zduAddLinks(m, zduLink(zduInstNew), zduLink(zduInstNew))
	m.mu.Lock()
	var cur []*managedLink
	for _, ml := range m.links {
		if !ml.zduOld {
			ml.retiring = true
			cur = append(cur, ml)
		}
	}
	m.mu.Unlock()
	if len(cur) != 2 {
		t.Fatalf("current links %d, want 2", len(cur))
	}
	for i := 0; i < 20; i++ {
		m.mu.Lock()
		ml := m.pickLocked(0)
		m.mu.Unlock()
		if ml == nil || ml.zduOld {
			t.Fatalf("pick %d: got an old-generation link (or none) while a current retiring link was healthy", i)
		}
	}
	m.mu.Lock()
	for _, ml := range cur {
		ml.degraded = true
	}
	ml := m.pickLocked(0)
	m.mu.Unlock()
	if ml == nil || !ml.zduOld {
		t.Fatal("current links degraded: the healthy old link must be the fallback")
	}
}
