package engine

import (
	"fmt"
	"testing"
	"time"
)

func withCommonGuard(t *testing.T, on bool) {
	t.Helper()
	o := lbCommonGuard
	lbCommonGuard = on
	t.Cleanup(func() { lbCommonGuard = o })
}

func tenPaths(spike map[int]float64) map[string]float64 {
	base := []float64{0.002, 0.005, 0.01, 0.02, 0.003, 0.008, 0.015, 0.002, 0.006, 0.012}
	m := map[string]float64{}
	for i, b := range base {
		if s, ok := spike[i]; ok {
			b = s
		}
		m[fmt.Sprintf("10.0.%d.1>k", i)] = b
	}
	return m
}

// hv83: 5 of 10 paths jump at once (own load) — the guard holds, no cut;
// off, they are cut as before.
func TestCommonGuardHoldsSimultaneousJump(t *testing.T) {
	spike := map[int]float64{0: 0.039, 1: 0.05, 3: 0.097, 5: 0.106, 6: 0.10}
	for _, on := range []bool{false, true} {
		withCommonGuard(t, on)
		j := newQ2Judge(0.1, "Iran", "kharej", nil)
		now := time.Unix(1000, 0)
		q2feed(j, &now, 4, tenPaths(nil))
		q2feed(j, &now, 2, tenPaths(spike))
		cuts := 0
		for _, q := range j.p {
			if q.cut {
				cuts++
			}
		}
		if on && cuts != 0 {
			t.Fatalf("guard on: %d paths cut", cuts)
		}
		if !on && cuts < 4 {
			t.Fatalf("guard off: only %d cut (test does not reproduce)", cuts)
		}
		if on {
			// a jump that stays is real: judged after lbCommonMaxRuns readings
			q2feed(j, &now, lbCommonMaxRuns+2, tenPaths(spike))
			if !j.p["10.0.5.1>k"].cut {
				t.Fatal("persistent jump never judged")
			}
		}
	}
}

// One path going bad is still cut with the guard on.
func TestCommonGuardSinglePathStillCut(t *testing.T) {
	withCommonGuard(t, true)
	j := newQ2Judge(0.1, "Iran", "kharej", nil)
	now := time.Unix(1000, 0)
	q2feed(j, &now, 4, tenPaths(nil))
	q2feed(j, &now, 2, tenPaths(map[int]float64{4: 0.08}))
	if !j.p["10.0.4.1>k"].cut {
		t.Fatal("single bad path not cut")
	}
}

// An IP verdict (every path of Iran IP a) still cuts at once.
func TestCommonGuardIPVerdictStillCuts(t *testing.T) {
	withCommonGuard(t, true)
	j := newQ2Judge(0.1, "Iran", "kharej", nil)
	now := time.Unix(1000, 0)
	good := map[string]float64{"a>x": 0.003, "a>y": 0.003, "b>x": 0.003, "b>y": 0.003}
	q2feed(j, &now, 2, good)
	q2feed(j, &now, 1, map[string]float64{"a>x": 0.07, "a>y": 0.06, "b>x": 0.003, "b>y": 0.002})
	if !j.p["a>x"].cut || !j.p["a>y"].cut {
		t.Fatal("IP verdict held by the guard")
	}
}
