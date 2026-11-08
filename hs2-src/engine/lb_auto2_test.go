package engine

import (
	"bytes"
	"io"
	"math"
	"strings"
	"testing"
	"time"
)

func withAuto2(t *testing.T) {
	t.Helper()
	a, s := lbAuto2, lbSteer
	lbAuto2, lbSteer = true, true
	t.Cleanup(func() { lbAuto2, lbSteer = a, s })
}

// q2feed gives the judge readings every 5 s: loss per path.
func q2feed(j *q2Judge, now *time.Time, n int, loss map[string]float64) {
	for i := 0; i < n; i++ {
		*now = now.Add(5 * time.Second)
		rs := map[string]q2Reading{}
		for k, l := range loss {
			rs[k] = q2Reading{loss: l}
		}
		j.update(*now, 5*time.Second, rs)
	}
}

func TestQ2CutAfterTwoReadings(t *testing.T) {
	j := newQ2Judge(0.1, "kharej", "Iran", nil)
	now := time.Unix(1000, 0)
	good := map[string]float64{"k>a": 0.003, "k>b": 0.003, "k>c": 0.003}
	q2feed(j, &now, 3, good)
	bad := map[string]float64{"k>a": 0.07, "k>b": 0.003, "k>c": 0.003}
	q2feed(j, &now, 1, bad)
	if j.p["k>a"].cut {
		t.Fatal("cut after one reading")
	}
	if j.p["k>a"].target >= 0.5 {
		t.Fatalf("one 7%% reading should already lower the path: target %.2f", j.p["k>a"].target)
	}
	q2feed(j, &now, 1, bad)
	q := j.p["k>a"]
	if !q.cut || q.target != 0.1 || !strings.HasPrefix(q.reason, "cut: loss 7.0%") {
		t.Fatalf("not cut after two: cut=%v target=%.2f reason=%q", q.cut, q.target, q.reason)
	}
	if j.p["k>b"].target != 1 {
		t.Fatalf("good path target %.2f", j.p["k>b"].target)
	}
	// Recovery: back to 0.3%: the smoothing falls slowly, then the hold.
	start := now
	for i := 0; i < 100 && j.p["k>a"].cut; i++ {
		q2feed(j, &now, 1, good)
	}
	took := now.Sub(start)
	if j.p["k>a"].cut || took < lb2Hold+20*time.Second || took > 3*time.Minute {
		t.Fatalf("recovery after %s (cut=%v)", took, j.p["k>a"].cut)
	}
	// Flap: cut again soon after: the hold doubles.
	q2feed(j, &now, 2, bad)
	if !j.p["k>a"].cut || j.p["k>a"].hold != 2*lb2Hold {
		t.Fatalf("re-cut: cut=%v hold=%s", j.p["k>a"].cut, j.p["k>a"].hold)
	}
}

func TestQ2IPVerdictCutsAtOnce(t *testing.T) {
	// Iran IP a is lossy over both kharej IPs: one reading cuts both paths.
	j := newQ2Judge(0.1, "Iran", "kharej", nil)
	now := time.Unix(1000, 0)
	good := map[string]float64{"a>x": 0.003, "a>y": 0.003, "b>x": 0.003, "b>y": 0.003}
	q2feed(j, &now, 2, good)
	q2feed(j, &now, 1, map[string]float64{"a>x": 0.07, "a>y": 0.06, "b>x": 0.003, "b>y": 0.002})
	if !j.p["a>x"].cut || !j.p["a>y"].cut || j.p["b>x"].cut {
		t.Fatalf("cuts a>x=%v a>y=%v b>x=%v", j.p["a>x"].cut, j.p["a>y"].cut, j.p["b>x"].cut)
	}
	v := j.verdictList()
	if len(v) != 1 || v[0] != "Iran IP a lossy on every kharej IP" {
		t.Fatalf("verdicts %q", v)
	}
	// A kharej IP bad on every Iran IP: a column.
	j = newQ2Judge(0.1, "Iran", "kharej", nil)
	q2feed(j, &now, 2, good)
	q2feed(j, &now, 1, map[string]float64{"a>x": 0.07, "a>y": 0.003, "b>x": 0.05, "b>y": 0.002})
	if !j.p["a>x"].cut || !j.p["b>x"].cut || j.p["a>y"].cut {
		t.Fatal("column not cut")
	}
	if v := j.verdictList(); len(v) != 1 || !strings.HasPrefix(v[0], "kharej IP x") {
		t.Fatalf("verdicts %q", v)
	}
}

func TestQ2AllBadKeepsShares(t *testing.T) {
	j := newQ2Judge(0.1, "kharej", "Iran", nil)
	now := time.Unix(1000, 0)
	q2feed(j, &now, 4, map[string]float64{"k>a": 0.07, "k>b": 0.06})
	for k, q := range j.p {
		if q.target != 1 || !strings.HasPrefix(q.reason, "all paths lossy") {
			t.Fatalf("%s: target %.2f reason %q", k, q.target, q.reason)
		}
	}
}

func TestQ2NoisyPathNotCut(t *testing.T) {
	// 2% steady (under the bar) with one 4% blip: lowered, never cut.
	j := newQ2Judge(0.1, "kharej", "Iran", nil)
	now := time.Unix(1000, 0)
	for i := 0; i < 20; i++ {
		l := 0.02
		if i == 7 {
			l = 0.04
		}
		q2feed(j, &now, 1, map[string]float64{"k>a": l, "k>b": 0.002})
		if j.p["k>a"].cut {
			t.Fatalf("cut at %d", i)
		}
	}
	if q := j.p["k>a"]; q.target >= 1 || q.target <= 0.1 {
		t.Fatalf("2%% path target %.2f", q.target)
	}
}

func TestLB2CutsAtOnceAndCaps(t *testing.T) {
	withAuto2(t)
	paths := []LBPath{
		{Local: "k", Remote: "a", Addr: "a:1", Weight: 34, Primary: true},
		{Local: "k", Remote: "b", Addr: "b:1", Weight: 33},
		{Local: "k", Remote: "c", Addr: "c:1", Weight: 33},
	}
	b, _ := NewLB(paths, nil)
	b.SetAuto(true)
	now := time.Unix(1000, 0)
	b.now = func() time.Time { return now }
	r := func(la, lb_, lc float64) map[int]q2Reading {
		return map[int]q2Reading{0: {loss: la}, 1: {loss: lb_}, 2: {loss: lc}}
	}
	for i := 0; i < 3; i++ {
		now = now.Add(5 * time.Second)
		b.adapt2(r(0.003, 0.003, 0.003))
	}
	for i := 0; i < 2; i++ {
		now = now.Add(5 * time.Second)
		b.adapt2(r(0.003, 0.07, 0.003))
	}
	if math.Abs(b.mult[1]-0.1) > 1e-9 {
		t.Fatalf("lossy path factor %.3f, want 0.1 at once", b.mult[1])
	}
	sh := lbShares(b.paths, b.mult)
	if sh[1] > 6 {
		t.Fatalf("lossy path share %.1f%%", sh[1])
	}
	st := b.Stats()
	if st[1].AutoFactor != 0.1 || !strings.HasPrefix(st[1].AutoReason, "cut: loss 7.0%") {
		t.Fatalf("status: %+v", st[1])
	}
	// The cap: two of three paths cut — the last may take ≤ 3× its share
	// (99%: no cap with 3 paths); with lb2CapX 1.5 it is held at 49.5%.
	old := lb2CapX
	lb2CapX = 1.5
	defer func() { lb2CapX = old }()
	for i := 0; i < 2; i++ {
		now = now.Add(5 * time.Second)
		b.adapt2(r(0.07, 0.07, 0.003))
	}
	sh = lbShares(b.paths, b.mult)
	if sh[2] > 49.5+0.5 {
		t.Fatalf("capped path share %.1f%%", sh[2])
	}
}

func TestLB2RecoveryIsGradual(t *testing.T) {
	withAuto2(t)
	b, _ := NewLB(twoPaths(50, 50), nil)
	b.SetAuto(true)
	now := time.Unix(1000, 0)
	b.now = func() time.Time { return now }
	step := func(l0, l1 float64) {
		now = now.Add(5 * time.Second)
		b.adapt2(map[int]q2Reading{0: {loss: l0}, 1: {loss: l1}})
	}
	for i := 0; i < 3; i++ {
		step(0.002, 0.08)
	}
	if b.mult[1] > 0.1+1e-9 {
		t.Fatalf("not cut: %.2f", b.mult[1])
	}
	prev := lbShares(b.paths, b.mult)[1]
	for i := 0; i < 80; i++ {
		step(0.002, 0.002)
		sh := lbShares(b.paths, b.mult)[1]
		if sh-prev > lbMaxStepPct+1e-6 {
			t.Fatalf("share rose %.1f points in one step", sh-prev)
		}
		prev = sh
	}
	if b.mult[1] < 0.99 {
		t.Fatalf("not recovered after 400 s: %.2f", b.mult[1])
	}
}

func TestLB2RebalanceRetiresSeveral(t *testing.T) {
	withAuto2(t)
	b, _ := NewLB(twoPaths(50, 50), nil)
	b.SetAuto(true)
	retired := 0
	for i := 0; i < 10; i++ {
		j, _ := b.acquire()
		k := new(int)
		b.result(j, false, k, nil)
		b.attach(k, lbHooks{retire: func() bool { retired++; b.release(k); return true }})
	}
	b.mu.Lock()
	b.mult[1] = 0.1
	b.mu.Unlock()
	if n := b.rebalance2(); n != 1 {
		// one per path a round: only path 1 is over its share
		t.Fatalf("retired %d, want 1 (one per path a round)", n)
	}
	// The pool is not whole (the retired link not replaced): nothing more.
	if n := b.rebalance2(); n != 0 {
		t.Fatalf("retired %d while a replacement is missing", n)
	}
}

func TestReportRoundTripAndOldPeers(t *testing.T) {
	r := LBReport{Auto: true, Auto2: true, Verdicts: []string{"Iran IP 37.32.47.180 lossy on every kharej IP"},
		Paths: []LBPeerPath{
			{Local: "91.107.167.67", Remote: "37.32.47.180", EffPct: 4.2, Target: 4, Links: 5, Factor: 0.1, LossPct: 7.04, RawPct: 6.5, raw: true, Cut: true, Reason: "cut: loss 7.0%"},
			{Local: "2a01:4f8:1c17:f987::1", Remote: "2a05:9080:0:603:216:3eff:fefe:94d2", EffPct: 12.5, Target: 13, Links: 13, Factor: 1, LossPct: 0.2, Reason: "ok"},
			{Local: "", Remote: "5.57.38.162", EffPct: 12, Target: 12, Links: 12, Factor: 1},
		}}
	adv := encodeAdvert(nil)
	wire := append(append([]byte(nil), adv...), encodeReport(r)...)
	// An older edge reads the advert and stops: what follows is never read.
	rd := bytes.NewReader(wire)
	if l, err := readAdvert(rd); err != nil || len(l) != 0 {
		t.Fatalf("advert: %v %v", l, err)
	}
	got, ok := readReport(rd)
	if !ok || !got.Auto || !got.Auto2 || len(got.Paths) != 3 || len(got.Verdicts) != 1 {
		t.Fatalf("report: ok=%v %+v", ok, got)
	}
	p := got.Paths[0]
	if p.Local != "91.107.167.67" || p.Remote != "37.32.47.180" || p.EffPct != 4.2 || p.Target != 4 ||
		p.Factor != 0.1 || p.LossPct != 7.04 || p.RawPct != 6.5 || !p.Cut || p.Reason != "cut: loss 7.0%" {
		t.Fatalf("path 0: %+v", p)
	}
	if got.Paths[1].Remote != "2a05:9080:0:603:216:3eff:fefe:94d2" || got.Paths[1].RawPct != -1 {
		t.Fatalf("path 1: %+v", got.Paths[1])
	}
	if got.Paths[2].Local != "" {
		t.Fatalf("path 2 local %q", got.Paths[2].Local)
	}
	// An older exit: nothing after the advert.
	if _, ok := readReport(bytes.NewReader(nil)); ok {
		t.Fatal("report from nothing")
	}
	if _, ok := readReport(io.MultiReader(bytes.NewReader([]byte{1, 2, 3}))); ok {
		t.Fatal("report from garbage")
	}
}

func TestApplyReportRows(t *testing.T) {
	rows := []LBPathView{{Local: "a", key: "a"}, {Local: "b", key: "b"}}
	rx := 14.0
	rows[0].LossDown = &rx
	rep := LBReport{Auto: true, Auto2: true, Paths: []LBPeerPath{
		{Local: "x", Remote: "a", EffPct: 2, Target: 2, Links: 2, Factor: 0.1, LossPct: 7, Cut: true, Reason: "cut: loss 7.0%"},
		{Local: "y", Remote: "a", EffPct: 2, Target: 2, Links: 2, Factor: 0.1, LossPct: 7, Cut: true, Reason: "cut: loss 7.0%"},
		{Local: "x", Remote: "b", EffPct: 48, Target: 20, Links: 20, Factor: 1, LossPct: 0.2, Reason: "ok"},
		{Local: "y", Remote: "b", EffPct: 48, Target: 20, Links: 20, Factor: 1, LossPct: 0.2, Reason: "ok"},
	}}
	applyReport(rows, rep, func(p LBPeerPath) string { return p.Remote })
	if rows[0].EffPct != 4 || rows[0].Target != 4 || rows[0].AutoFactor != 0.1 || rows[0].AutoReason != "cut: loss 7.0%" {
		t.Fatalf("row a: %+v", rows[0])
	}
	if *rows[0].LossDown != 7 || *rows[0].LossDownRx != 14 || rows[0].LossSrc != "sender" {
		t.Fatalf("row a loss: %v %v %s", *rows[0].LossDown, rows[0].LossDownRx, rows[0].LossSrc)
	}
	if rows[1].EffPct != 96 || rows[1].Target != 40 || rows[1].AutoReason != "ok" {
		t.Fatalf("row b: %+v", rows[1])
	}
}

// The edge's steering: with path b cut, new connections go to path a, except
// a small share that keeps b measured; b's links beyond the keepers take none.
func TestSteerPicksAvoidCutPath(t *testing.T) {
	withAuto2(t)
	m, ls := pathLinks("a>x", "a>x", "a>x", "a>x", "b>x", "b>x", "b>x", "b>x")
	m.steer = newSteerState()
	now := time.Unix(1000, 0)
	m.mu.Lock()
	q2feed(m.steer.judge, &now, 3, map[string]float64{"a>x": 0.003, "b>x": 0.07})
	m.steer.judge.stepEdge()
	m.mu.Unlock()
	if !m.steer.judge.p["b>x"].cut {
		t.Fatal("b not cut")
	}
	m.mu.Lock()
	// a tick with no new readings: marks the keepers
	m.steerTickLocked(now, 2*time.Second)
	m.mu.Unlock()
	off := 0
	for _, l := range ls[4:] {
		ml := m.links[linkIndex(ls, l)]
		if ml.steer.off {
			off++
		}
	}
	if off != 2 {
		t.Fatalf("%d of b's 4 links off, want 2 (2 keepers)", off)
	}
	got := make([]int, len(ls))
	for i := 0; i < 2000; i++ {
		l, rel, ok := m.Pick()
		if !ok {
			t.Fatal("no pick")
		}
		got[linkIndex(ls, l)]++
		rel()
		m.mu.Lock()
		for _, ml := range m.links { // keep the burst cap out of the way
			ml.picks, ml.pickHist = 0, [pickWindow - 1]int{}
		}
		m.mu.Unlock()
	}
	b := got[4] + got[5] + got[6] + got[7]
	if b == 0 || b > 2000/15 {
		t.Fatalf("cut path got %d of 2000 new connections: %v", b, got)
	}
	// Steering off: the old spread.
	m.steer = nil
	got = make([]int, len(ls))
	for i := 0; i < 800; i++ {
		l, rel, _ := m.Pick()
		got[linkIndex(ls, l)]++
		rel()
	}
	if b := got[4] + got[5] + got[6] + got[7]; b < 300 {
		t.Fatalf("off: path b got %d of 800", b)
	}
}

// Light subs go to the best path only.
func TestSteerLightBestPath(t *testing.T) {
	withAuto2(t)
	m, ls := pathLinks("a>x", "a>x", "b>x", "b>x")
	m.steer = newSteerState()
	now := time.Unix(1000, 0)
	m.mu.Lock()
	q2feed(m.steer.judge, &now, 3, map[string]float64{"a>x": 0.003, "b>x": 0.02})
	m.steer.judge.stepEdge()
	m.steerTickLocked(now, 2*time.Second)
	m.mu.Unlock()
	if f := m.steer.judge.p["b>x"].factor; f >= 0.9 || f <= 0.05 {
		t.Fatalf("b factor %.2f", f)
	}
	for i := 0; i < 200; i++ {
		l, rel, ok := m.pickPlane(planeLight, nil)
		if !ok {
			t.Fatal("no pick")
		}
		m.mu.Lock()
		for _, ml := range m.links {
			ml.picks, ml.pickHist = 0, [pickWindow - 1]int{}
		}
		m.mu.Unlock()
		if j := linkIndex(ls, l); j >= 2 {
			t.Fatalf("light sub on the worse path (link %d)", j)
		}
		rel()
	}
}
