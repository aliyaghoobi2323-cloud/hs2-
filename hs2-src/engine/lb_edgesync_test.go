package engine

import (
	"strings"
	"testing"
	"time"
)

// The production ping-pong (tunnel 1, 2026-11-11): the dialing side holds b
// cut for minutes with the same reason while its probe readings, relayed to
// the edge, read clean. Without HS2_LB_EDGE_SYNC the edge lifts its copy of
// the cut after its short hold and the next fresh report cuts it again
// within one reading; with it, the edge stays cut — and follows the report
// at once when the dialing side recovers the path.
func TestEdgeSyncNoPingPong(t *testing.T) {
	withAuto2(t)
	run := func(sync bool) (cuts, recs int, recAfterLift time.Duration) {
		old := lbEdgeSync
		lbEdgeSync = sync
		defer func() { lbEdgeSync = old }()
		m, _ := pathLinks("a>x", "a>x", "b>x", "b>x")
		m.steer = newSteerState()
		now := time.Unix(1000, 0)
		tick := func(cutB bool) {
			r := LBReport{Auto: true, Auto2: true, At: now}
			r.Paths = []LBPeerPath{
				{Local: "x", Remote: "a", EffPct: 95, Factor: 1, LossPct: 0.3, RawPct: 0.3, raw: true},
				{Local: "x", Remote: "b", EffPct: 5, Factor: 0.1, LossPct: 12.6, RawPct: 0.2, raw: true, Cut: cutB, Reason: "cut: loss 12.6%"},
			}
			m.mu.Lock()
			m.steer.rep = &r
			m.steerTickLocked(now, lb2QualEvery)
			for _, l := range m.steer.takeLogs() {
				if strings.Contains(l, "b>x cut") {
					cuts++
				}
				if strings.Contains(l, "b>x recovering") {
					recs++
				}
			}
			m.mu.Unlock()
			now = now.Add(lb2QualEvery)
		}
		for i := 0; i < 60; i++ { // 5 min: the dialing side holds the cut
			tick(true)
		}
		lift := now
		for i := 0; i < 30; i++ {
			tick(false)
			if recAfterLift == 0 && !m.steer.judge.p["b>x"].cut {
				recAfterLift = now.Sub(lift)
			}
		}
		return
	}
	c0, r0, _ := run(false)
	if c0 < 3 || r0 < 3 {
		t.Fatalf("without the key the ping-pong should show: %d cuts, %d recoveries", c0, r0)
	}
	c1, r1, after := run(true)
	if c1 != 1 || r1 != 1 {
		t.Fatalf("with the key: %d cuts, %d recoveries, want 1 and 1", c1, r1)
	}
	if after == 0 || after > 2*lb2QualEvery {
		t.Fatalf("edge followed the dialing side's recovery after %s", after)
	}
	t.Logf("5 min of one dialing-side cut: off %d cuts/%d recoveries, on %d/%d (recovered %s after the report)", c0, r0, c1, r1, after)
}

// A stale report (over 20 s) no longer holds the edge's cut: the edge judges
// alone again.
func TestEdgeSyncStaleReportReleases(t *testing.T) {
	withAuto2(t)
	old := lbEdgeSync
	lbEdgeSync = true
	defer func() { lbEdgeSync = old }()
	m, _ := pathLinks("a>x", "a>x", "b>x", "b>x")
	m.steer = newSteerState()
	now := time.Unix(1000, 0)
	r := LBReport{Auto: true, Auto2: true, At: now, Paths: []LBPeerPath{
		{Local: "x", Remote: "a", EffPct: 95, Factor: 1, LossPct: 0.3, RawPct: 0.3, raw: true},
		{Local: "x", Remote: "b", EffPct: 5, Factor: 0.1, LossPct: 9, RawPct: 9, raw: true, Cut: true, Reason: "cut: loss 9.0%"},
	}}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.steer.rep = &r
	m.steerTickLocked(now, lb2QualEvery)
	if q := m.steer.judge.p["b>x"]; q == nil || !q.cut || !q.remCut {
		t.Fatalf("forced cut not taken: %+v", q)
	}
	for i := 0; i < 40; i++ { // no new report: after 20 s the quiet rule may lift it
		now = now.Add(lb2QualEvery)
		m.steerTickLocked(now, lb2QualEvery)
	}
	if q := m.steer.judge.p["b>x"]; q.remCut || q.cut {
		t.Fatalf("stale report still holds the edge: remCut=%v cut=%v", q.remCut, q.cut)
	}
}
