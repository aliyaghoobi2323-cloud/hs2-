package engine

import (
	"testing"
	"time"
)

// upLossRun drives the edge's steering tick every 2 s for d with the given
// upload loss per path (1000 segments a tick each) and a fresh report that
// calls every path clean (the exit sees only the download).
func upLossRun(m *LinkManager, now *time.Time, d time.Duration, up map[string]float64) {
	rep := map[string]repPath{}
	for k := range up {
		rep[k] = repPath{100 / float64(len(up)), 1, 0.003, false}
	}
	for t := time.Duration(0); t < d; t += 2 * time.Second {
		*now = now.Add(2 * time.Second)
		edgeReport(m, rep)
		m.mu.Lock()
		m.steer.rep.At = *now
		st := m.steer
		st.upRt, st.upSegs = map[string]uint64{}, map[string]uint64{}
		for k, l := range up {
			st.upSegs[k] = 1000
			st.upRt[k] = uint64(l * 1000)
		}
		m.steerTickLocked(*now, 2*time.Second)
		m.mu.Unlock()
	}
}

func withUpLoss(t *testing.T, on bool) {
	t.Helper()
	o := lbUpLoss
	lbUpLoss = on
	t.Cleanup(func() { lbUpLoss = o })
}

func TestUpLossCutsUploadOnlyPath(t *testing.T) {
	withAuto2(t)
	withUpLoss(t, true)
	m, ls := pathLinks("a>x", "a>x", "b>x", "b>x", "c>x", "c>x")
	m.steer = newSteerState()
	now := time.Unix(1000, 0)
	clean := map[string]float64{"a>x": 0.003, "b>x": 0.003, "c>x": 0.004}
	upLossRun(m, &now, 20*time.Second, clean)
	m.mu.RLock()
	if m.steer.repCut["a>x"] || m.steer.repFactor["a>x"] != 1 {
		t.Fatalf("clean: a cut=%v factor %.2f", m.steer.repCut["a>x"], m.steer.repFactor["a>x"])
	}
	m.mu.RUnlock()
	// a loses 3 % of its uploads only: cut within lb2CutRuns readings (5 s each)
	lossy := map[string]float64{"a>x": 0.03 + 0.005, "b>x": 0.003, "c>x": 0.004}
	upLossRun(m, &now, 12*time.Second, lossy)
	m.mu.RLock()
	st := m.steer
	if !st.repCut["a>x"] || st.repFactor["a>x"] > lb2SteerFloor+1e-9 || st.repShare["a>x"] > 2 {
		t.Fatalf("upload-lossy a not cut: cut=%v factor %.2f share %.1f (%s)", st.repCut["a>x"], st.repFactor["a>x"], st.repShare["a>x"], st.up)
	}
	if st.repCut["b>x"] || st.repFactor["b>x"] != 1 {
		t.Fatalf("clean b touched: cut=%v factor %.2f", st.repCut["b>x"], st.repFactor["b>x"])
	}
	if st.repLoss["a>x"] < 0.02 {
		t.Fatalf("a's loss for the draw %.3f, want the upload loss", st.repLoss["a>x"])
	}
	m.mu.RUnlock()
	// new connections avoid it
	onA := 0
	for i := 0; i < 1000; i++ {
		l, rel, ok := m.Pick()
		if !ok {
			t.Fatal("no pick")
		}
		if linkIndex(ls, l) < 2 {
			onA++
		}
		rel()
		m.mu.Lock()
		for _, ml := range m.links {
			ml.picks, ml.pickHist = 0, [pickWindow - 1]int{}
			ml.fpPicks.Store(0) // HS2_LM_FASTPICK: placements on the fast path count here (curPicks)
		}
		m.mu.Unlock()
	}
	if onA > 1000/15 {
		t.Fatalf("cut path got %d of 1000 new connections", onA)
	}
	// recovery: clean again — not at once (the hold), then step by step
	upLossRun(m, &now, 20*time.Second, clean)
	if !m.steer.up.judge.p["a>x"].cut {
		t.Fatal("recovered within 20 s: no hold")
	}
	start := now
	for i := 0; i < 120 && m.steer.up.judge.p["a>x"].cut; i++ {
		upLossRun(m, &now, 2*time.Second, clean)
	}
	if q := m.steer.up.judge.p["a>x"]; q.cut || q.factor >= 1 {
		t.Fatalf("after %s: cut=%v factor %.2f (want recovering, not full)", now.Sub(start), q.cut, q.factor)
	}
	upLossRun(m, &now, 60*time.Second, clean)
	if f := m.steer.repFactor["a>x"]; f != 1 {
		t.Fatalf("not back to full after a minute clean: %.2f", f)
	}
}

// Off: the same upload loss changes nothing (the report alone steers).
func TestUpLossOffIgnoresUpload(t *testing.T) {
	withAuto2(t)
	withUpLoss(t, false)
	m, _ := pathLinks("a>x", "a>x", "b>x", "b>x", "c>x", "c>x")
	m.steer = newSteerState()
	now := time.Unix(1000, 0)
	upLossRun(m, &now, 40*time.Second, map[string]float64{"a>x": 0.05, "b>x": 0.003, "c>x": 0.003})
	if m.steer.up != nil || m.steer.repCut["a>x"] || m.steer.repFactor["a>x"] != 1 {
		t.Fatalf("off: a cut=%v factor %.2f", m.steer.repCut["a>x"], m.steer.repFactor["a>x"])
	}
}

// Loss every path shares (Iran's own uplink, a full queue at the edge) is no
// path's fault: nothing is lowered. Neither is a path whose loss is under the
// relative bar (2x the best + 1 point).
func TestUpLossSharedOrMildNotCut(t *testing.T) {
	withAuto2(t)
	withUpLoss(t, true)
	for name, up := range map[string]map[string]float64{
		"shared": {"a>x": 0.05, "b>x": 0.045, "c>x": 0.055},
		"mild":   {"a>x": 0.025, "b>x": 0.003, "c>x": 0.004},
		"rel":    {"a>x": 0.035, "b>x": 0.02, "c>x": 0.018},
	} {
		m, _ := pathLinks("a>x", "a>x", "b>x", "b>x", "c>x", "c>x")
		m.steer = newSteerState()
		now := time.Unix(1000, 0)
		upLossRun(m, &now, 60*time.Second, up)
		for k := range up {
			if m.steer.repCut[k] {
				t.Fatalf("%s: %s cut (%s)", name, k, m.steer.up)
			}
		}
		if name == "shared" {
			for k := range up {
				if f := m.steer.repFactor[k]; f != 1 {
					t.Fatalf("shared: %s factor %.2f, want shares as set", k, f)
				}
			}
		}
	}
}

// Too few upload segments: no reading, no verdict (a quiet path is not judged
// on a handful of retransmits).
func TestUpLossNeedsSegments(t *testing.T) {
	withAuto2(t)
	withUpLoss(t, true)
	m, _ := pathLinks("a>x", "b>x", "c>x")
	m.steer = newSteerState()
	now := time.Unix(1000, 0)
	for i := 0; i < 30; i++ {
		now = now.Add(2 * time.Second)
		edgeReport(m, map[string]repPath{"a>x": {33, 1, 0.003, false}, "b>x": {33, 1, 0.003, false}, "c>x": {33, 1, 0.003, false}})
		m.mu.Lock()
		m.steer.rep.At = now
		m.steer.upRt = map[string]uint64{"a>x": 10, "b>x": 0, "c>x": 0}
		m.steer.upSegs = map[string]uint64{"a>x": 40, "b>x": 40, "c>x": 40} // 80-120 a 5 s reading
		m.steerTickLocked(now, 2*time.Second)
		m.mu.Unlock()
	}
	if m.steer.repCut["a>x"] || m.steer.repFactor["a>x"] != 1 {
		t.Fatalf("judged on %d segments: cut=%v factor %.2f", 80, m.steer.repCut["a>x"], m.steer.repFactor["a>x"])
	}
}
