package engine

import (
	"fmt"
	"testing"
	"time"
)

// 6 Iran IPs × 2 kharej IPs, edge keys "iran>kharej".
func fl335Paths() map[string]bool {
	a := map[string]bool{}
	for i := 1; i <= 6; i++ {
		for k := 1; k <= 2; k++ {
			a[fmt.Sprintf("10.60.%d.1>10.70.%d.2", i, k)] = true
		}
	}
	return a
}

// fl335Win: every path at base loss, the paths of bad (an Iran IP) at loss.
func fl335Win(alive map[string]bool, base float64, bad string, loss float64, only string) map[string]flRead {
	c := map[string]flRead{}
	for k := range alive {
		l := base
		ir, _ := pathIPs(k)
		if ir == bad && (only == "" || k == only) {
			l = loss
		}
		c[k] = flRead{rt: uint64(l * 1000), segs: 1000}
	}
	return c
}

func fl335Run(t *testing.T, wins []map[string]flRead, alive map[string]bool) (*fastLowerDet, []bool) {
	t.Helper()
	d := newFastLowerDet(t.Logf)
	now := time.Unix(1e9, 0)
	var low []bool
	for _, w := range wins {
		now = now.Add(2 * time.Second)
		d.step(now, w, alive)
		low = append(low, d.lowered("10.60.3.1>10.70.1.2"))
	}
	return d, low
}

func TestFastLower335SharpBurst(t *testing.T) {
	a := fl335Paths()
	var w []map[string]flRead
	for i := 0; i < 3; i++ {
		w = append(w, fl335Win(a, 0.003, "", 0, ""))
	}
	for i := 0; i < 6; i++ { // 12 s burst
		w = append(w, fl335Win(a, 0.003, "10.60.3.1", 0.12, ""))
	}
	for i := 0; i < 10; i++ {
		w = append(w, fl335Win(a, 0.003, "", 0, ""))
	}
	d := newFastLowerDet(t.Logf)
	now := time.Unix(1e9, 0)
	for i := 0; i < 5; i++ {
		now = now.Add(2 * time.Second)
		d.step(now, w[i], a)
	}
	if d.lowered("10.60.2.1>10.70.1.2") || d.lowered("10.60.4.1>10.70.2.2") {
		t.Fatal("other Iran IPs lowered")
	}
	if !d.lowered("10.60.3.1>10.70.2.2") {
		t.Fatal("the IP's other path not lowered")
	}
	_, low := fl335Run(t, w, a)
	if low[3] {
		t.Fatal("lowered on the first burst window (needs two)")
	}
	if !low[4] {
		t.Fatalf("not lowered at the second burst window (≈4 s): %v", low)
	}
	// lifted: 3 calm windows and 10 s after the lower
	if !low[10] || low[11] {
		t.Fatalf("lift timing wrong: %v", low)
	}
}

func TestFastLower335SingleNoisy(t *testing.T) {
	a := fl335Paths()
	var w []map[string]flRead
	for i := 0; i < 20; i++ {
		if i%4 == 1 {
			w = append(w, fl335Win(a, 0.003, "10.60.3.1", 0.15, "")) // one noisy window at a time
		} else {
			w = append(w, fl335Win(a, 0.003, "", 0, ""))
		}
	}
	_, low := fl335Run(t, w, a)
	for i, l := range low {
		if l {
			t.Fatalf("lowered on single-window noise at %d", i)
		}
	}
}

func TestFastLower335SlowDrift(t *testing.T) {
	a := fl335Paths()
	var w []map[string]flRead
	// one IP drifts 0.3% → 2.5%: under the cut bar, never lowered here
	for i := 0; i < 30; i++ {
		w = append(w, fl335Win(a, 0.003, "10.60.3.1", 0.003+0.022*float64(i)/29, ""))
	}
	_, low := fl335Run(t, w, a)
	for i, l := range low {
		if l {
			t.Fatalf("lowered on a slow drift under the bar at %d", i)
		}
	}
}

func TestFastLower335OneBadLink(t *testing.T) {
	a := fl335Paths()
	var w []map[string]flRead
	for i := 0; i < 10; i++ {
		w = append(w, fl335Win(a, 0.003, "10.60.3.1", 0.2, "10.60.3.1>10.70.1.2"))
	}
	d, low := fl335Run(t, w, a)
	for i, l := range low {
		if l {
			t.Fatalf("one bad path lowered the whole IP at %d", i)
		}
	}
	// the kharej IP 10.70.1.2 has 6 paths, only one bad: not lowered either
	if d.lowered("10.60.1.1>10.70.1.2") {
		t.Fatal("kharej IP lowered on one bad path")
	}
}

func TestFastLower335AllLossy(t *testing.T) {
	a := fl335Paths()
	var w []map[string]flRead
	for i := 0; i < 6; i++ {
		w = append(w, fl335Win(a, 0.12, "", 0, ""))
	}
	_, low := fl335Run(t, w, a)
	for i, l := range low {
		if l {
			t.Fatalf("lowered while every path is lossy at %d", i)
		}
	}
}

func TestFastLower335FlapGuard(t *testing.T) {
	a := fl335Paths()
	var w []map[string]flRead
	cycle := func() {
		for i := 0; i < 2; i++ {
			w = append(w, fl335Win(a, 0.003, "10.60.3.1", 0.12, ""))
		}
		for i := 0; i < 8; i++ {
			w = append(w, fl335Win(a, 0.003, "", 0, ""))
		}
	}
	cycle()
	cycle()
	cycle() // third burst of two windows: needs three now
	_, low := fl335Run(t, w, a)
	if !low[1] || !low[11] {
		t.Fatalf("first two bursts not lowered: %v", low)
	}
	if low[21] {
		t.Fatalf("third two-window burst lowered despite the flap guard: %v", low)
	}
}

func TestFastLower335SmallSample(t *testing.T) {
	a := fl335Paths()
	d := newFastLowerDet(t.Logf)
	now := time.Unix(1e9, 0)
	for i := 0; i < 5; i++ {
		now = now.Add(2 * time.Second)
		c := fl335Win(a, 0.003, "", 0, "")
		c["10.60.3.1>10.70.1.2"] = flRead{rt: 5, segs: 20}
		c["10.60.3.1>10.70.2.2"] = flRead{rt: 5, segs: 20}
		d.step(now, c, a)
	}
	if d.lowered("10.60.3.1>10.70.1.2") {
		t.Fatal("lowered on a sample under flMinSegs")
	}
}

// The edge hook: a lowered IP's paths take the floor in steerFactor and the
// weights, and nothing applies when every path is lowered.
func TestFastLower335SteerHooks(t *testing.T) {
	old := b335FastLower
	b335FastLower = true
	defer func() { b335FastLower = old }()
	st := newSteerState()
	a := fl335Paths()
	for k := range a {
		st.judge.path(k)
	}
	st.fl = newFastLowerDet(t.Logf)
	now := time.Unix(1e9, 0)
	for i := 0; i < 2; i++ {
		now = now.Add(2 * time.Second)
		st.fl.step(now, fl335Win(a, 0.003, "10.60.3.1", 0.12, ""), a)
	}
	m := &LinkManager{steer: st}
	if f := m.steerFactor("10.60.3.1>10.70.1.2"); f > lb2Floor {
		t.Fatalf("lowered path factor %v", f)
	}
	if f := m.steerFactor("10.60.2.1>10.70.1.2"); f != 1 {
		t.Fatalf("good path factor %v", f)
	}
	st.active = true
	st.repShare = map[string]float64{"10.60.3.1>10.70.1.2": 33, "10.60.2.1>10.70.1.2": 33, "10.60.4.1>10.70.1.2": 33}
	keys := []string{"10.60.2.1>10.70.1.2", "10.60.3.1>10.70.1.2", "10.60.4.1>10.70.1.2"}
	ws, _ := m.steerWeightsLocked(false, keys)
	if ws[1] >= ws[0]/5 {
		t.Fatalf("lowered path weight %v vs %v", ws[1], ws[0])
	}
	lw, _ := m.steerWeightsLocked(true, keys)
	if lw[1] != 0 {
		t.Fatalf("light weight on the lowered path %v", lw[1])
	}
	b335FastLower = false
	if m.steerFactor("10.60.3.1>10.70.1.2") != 1 {
		t.Fatal("key off still lowers")
	}
}

// A lowered IP whose paths then carry too little for a reading is lifted
// all the same (no reading is no evidence): it must not stay lowered for good.
func TestFastLower335NoReadingLifts(t *testing.T) {
	a := fl335Paths()
	d := newFastLowerDet(t.Logf)
	now := time.Unix(1e9, 0)
	for i := 0; i < 2; i++ {
		now = now.Add(2 * time.Second)
		d.step(now, fl335Win(a, 0.003, "10.60.3.1", 0.12, ""), a)
	}
	if !d.lowered("10.60.3.1>10.70.1.2") {
		t.Fatal("not lowered")
	}
	for i := 0; i < 8; i++ {
		now = now.Add(2 * time.Second)
		c := fl335Win(a, 0.003, "", 0, "")
		delete(c, "10.60.3.1>10.70.1.2")
		delete(c, "10.60.3.1>10.70.2.2")
		d.step(now, c, a)
	}
	if d.lowered("10.60.3.1>10.70.1.2") {
		t.Fatal("still lowered after 16 s without a reading")
	}
}
