package engine

import (
	"testing"
	"time"
)

// recoverAfter feeds good readings until path k is no longer cut.
func recoverAfter(j *q2Judge, now *time.Time, k string, good map[string]float64) time.Duration {
	start := *now
	for i := 0; i < 200 && j.p[k].cut; i++ {
		q2feed(j, now, 1, good)
	}
	return now.Sub(start)
}

// HS2_LB_FAST_RECOVER: the first recovery comes ~25 s after the loss ends
// (3 readings under the bar, then the 10 s hold), not ~80 s; a path cut again
// within lb2FlapWin still waits its full doubled hold.
func TestQ2FastRecover(t *testing.T) {
	good := map[string]float64{"k>a": 0.003, "k>b": 0.003, "k>c": 0.003}
	bad := map[string]float64{"k>a": 0.07, "k>b": 0.003, "k>c": 0.003}
	run := func(fast bool) (first, second time.Duration, j *q2Judge) {
		old := lbFastRecover
		lbFastRecover = fast
		defer func() { lbFastRecover = old }()
		j = newQ2Judge(0.1, "kharej", "Iran", nil)
		now := time.Unix(1000, 0)
		q2feed(j, &now, 3, good)
		q2feed(j, &now, 8, bad) // 40 s at 7%
		if !j.p["k>a"].cut {
			t.Fatal("not cut")
		}
		first = recoverAfter(j, &now, "k>a", good)
		q2feed(j, &now, 4, good)
		q2feed(j, &now, 8, bad) // flap: cut again within lb2FlapWin
		if !j.p["k>a"].cut || j.p["k>a"].hold != 2*lb2Hold {
			t.Fatalf("fast=%v re-cut: cut=%v hold=%s", fast, j.p["k>a"].cut, j.p["k>a"].hold)
		}
		second = recoverAfter(j, &now, "k>a", good)
		return
	}
	f1, f2, jf := run(true)
	s1, s2, _ := run(false)
	t.Logf("first recovery: fast %s, slow %s; after a flap: fast %s, slow %s", f1, s1, f2, s2)
	if f1 > 35*time.Second || s1 < 60*time.Second {
		t.Fatalf("first recovery fast %s slow %s", f1, s1)
	}
	if f2 < 2*lb2Hold+lbFastOKRuns*5*time.Second-5*time.Second {
		t.Fatalf("after a flap the fast recovery took only %s (hold %s)", f2, 2*lb2Hold)
	}
	if jf.p["k>a"].fastResets == 0 {
		t.Fatal("no smoothed-loss reset counted")
	}
	// A path that is not cut and was not just recovered keeps the slow smoothing.
	old := lbFastRecover
	lbFastRecover = true
	defer func() { lbFastRecover = old }()
	j := newQ2Judge(0.1, "kharej", "Iran", nil)
	now := time.Unix(1000, 0)
	q2feed(j, &now, 3, good)
	q2feed(j, &now, 1, map[string]float64{"k>a": 0.025, "k>b": 0.003, "k>c": 0.003})
	e := j.p["k>a"].ewma
	q2feed(j, &now, 4, good)
	if j.p["k>a"].cut || j.p["k>a"].fastResets != 0 || j.p["k>a"].ewma < 0.003+0.3*(e-0.003) {
		t.Fatalf("uncut path reset: ewma %.4f (was %.4f) resets %d", j.p["k>a"].ewma, e, j.p["k>a"].fastResets)
	}
}
