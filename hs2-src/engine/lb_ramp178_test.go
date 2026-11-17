package engine

import (
	"fmt"
	"testing"
	"time"
)

// wj178: a recovering path among many climbs to 1 in steps; the old rule
// jumped from ~0.55 to 1 when its verdict turned "ok".
func rampRun(t *testing.T, ramp bool, cuts int) (maxJump float64, toFull int, log []string) {
	t.Helper()
	withAuto2(t)
	old := lbRecRamp
	lbRecRamp = ramp
	t.Cleanup(func() { lbRecRamp = old })
	var paths []LBPath
	for i := 0; i < 16; i++ {
		paths = append(paths, LBPath{Local: "k", Remote: fmt.Sprintf("r%d", i), Addr: fmt.Sprintf("r%d:1", i), Weight: 10, Primary: i == 0})
	}
	b, _ := NewLB(paths, nil)
	b.SetAuto(true)
	now := time.Unix(1000, 0)
	b.now = func() time.Time { return now }
	step := func(bad float64) {
		now = now.Add(5 * time.Second)
		rs := map[int]q2Reading{}
		for i := range paths {
			rs[i] = q2Reading{loss: 0.002}
		}
		rs[3] = q2Reading{loss: bad}
		b.adapt2(rs)
	}
	for i := 0; i < 3; i++ {
		step(0.002)
	}
	for c := 0; c < cuts; c++ {
		for i := 0; i < 3; i++ {
			step(0.08)
		}
		if b.mult[3] > 0.1+1e-9 {
			t.Fatalf("not cut: %.2f", b.mult[3])
		}
		if c == cuts-1 {
			break
		}
		for i := 0; i < 400 && b.judge.p[paths[3].Name()].cut; i++ {
			step(0.002)
		}
		step(0.002)
	}
	for i := 0; i < 400 && b.judge.p[paths[3].Name()].cut; i++ {
		step(0.002)
	}
	prev := b.mult[3]
	for i := 0; i < 120; i++ {
		step(0.002)
		maxJump = max(maxJump, b.mult[3]-prev)
		if b.mult[3]-prev > 0.02 {
			log = append(log, fmt.Sprintf("%.2f→%.2f", prev, b.mult[3]))
		}
		prev = b.mult[3]
		if b.mult[3] >= 0.999 && toFull == 0 {
			toFull = i + 1
		}
	}
	return
}

func TestLB2RecRampNoJump(t *testing.T) {
	j0, f0, l0 := rampRun(t, false, 1)
	j1, f1, l1 := rampRun(t, true, 1)
	t.Logf("old: max jump %.2f, full after %d readings %v", j0, f0, l0)
	t.Logf("ramp: max jump %.2f, full after %d readings %v", j1, f1, l1)
	if j0 < 0.35 {
		t.Logf("note: old rule did not reproduce the jump here (%.2f)", j0)
	}
	if j1 > 0.31 || f1 == 0 {
		t.Fatalf("ramp: max jump %.2f, full after %d", j1, f1)
	}
}

func TestLB2RecRampFlapperSlower(t *testing.T) {
	j1, f1, _ := rampRun(t, true, 1)
	j3, f3, l3 := rampRun(t, true, 3)
	t.Logf("1 cut: jump %.2f full %d; 3 cuts: jump %.2f full %d %v", j1, f1, j3, f3, l3)
	if f3 == 0 || j3 > 0.16 || f3 <= f1 {
		t.Fatalf("flapper: jump %.2f full %d (one cut: %d)", j3, f3, f1)
	}
}

func TestLB2RecRampHoldsOnBadReading(t *testing.T) {
	q := &q2Path{ramp: 0.3, lastRead: time.Unix(5, 0), raw: 0.05}
	rampAdvance(q, time.Unix(5, 0), 0.015)
	if q.ramp != 0.3 {
		t.Fatalf("ramp moved on a bad reading: %.2f", q.ramp)
	}
	q.raw = 0.02
	rampAdvance(q, time.Unix(5, 0), 0.015)
	if q.ramp <= 0.3 {
		t.Fatal("ramp did not move")
	}
}
