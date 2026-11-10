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
	if f2 < 2*lb2Hold+time.Duration(lbFastOKRuns)*5*time.Second-5*time.Second {
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

// HS2_LB_START_GRACE_S: no whole-IP cut in the first seconds.
func TestQ2StartGraceNoIPCut(t *testing.T) {
	old := lbIPCutGrace
	lbIPCutGrace = 20 * time.Second
	defer func() { lbIPCutGrace = old }()
	j := newQ2Judge(0.1, "Iran", "kharej", nil)
	now := time.Unix(1000, 0)
	good := map[string]float64{"a>x": 0.003, "a>y": 0.003, "b>x": 0.003, "b>y": 0.003}
	bad := map[string]float64{"a>x": 0.07, "a>y": 0.06, "b>x": 0.003, "b>y": 0.002}
	q2feed(j, &now, 2, good)
	q2feed(j, &now, 1, bad)
	if j.p["a>x"].cut || len(j.verdictList()) != 0 {
		t.Fatal("IP cut inside the start grace")
	}
	q2feed(j, &now, 20, good)
	q2feed(j, &now, 1, bad)
	if !j.p["a>x"].cut || !j.p["a>y"].cut {
		t.Fatal("no IP cut after the grace")
	}
}

// HS2_LB_REC_CONFIRM: a cut path with a long (flap-doubled) hold whose tiny
// samples read a lone spike over the bar now and then still recovers after
// its hold; a path whose readings stay over the bar in a row stays cut.
func TestQ2RecConfirm(t *testing.T) {
	good := map[string]float64{"k>a": 0.003, "k>b": 0.003, "k>c": 0.003}
	bad := map[string]float64{"k>a": 0.07, "k>b": 0.003, "k>c": 0.003}
	spike := map[string]float64{"k>a": 0.05, "k>b": 0.003, "k>c": 0.003}
	run := func(confirm bool, still bool) (cut bool, took time.Duration) {
		oc, of := lbRecConfirm, lbFastRecover
		lbRecConfirm, lbFastRecover = confirm, true
		defer func() { lbRecConfirm, lbFastRecover = oc, of }()
		j := newQ2Judge(0.1, "kharej", "Iran", nil)
		now := time.Unix(1000, 0)
		q2feed(j, &now, 3, good)
		q2feed(j, &now, 8, bad)
		if !j.p["k>a"].cut {
			t.Fatal("not cut")
		}
		j.p["k>a"].hold = lb2HoldMax // a flapped path: 5 min hold
		start := now
		for i := 0; i < 240 && j.p["k>a"].cut; i++ { // 20 min
			switch {
			case still && i%2 == 0:
				q2feed(j, &now, 1, bad)
			case still:
				q2feed(j, &now, 1, spike)
			case i%9 == 8: // a lone spike every 45 s
				q2feed(j, &now, 1, spike)
			default:
				q2feed(j, &now, 1, good)
			}
		}
		return j.p["k>a"].cut, now.Sub(start)
	}
	c0, t0 := run(false, false)
	c1, t1 := run(true, false)
	t.Logf("healthy path with lone spikes, 5 min hold: off cut=%v %s, on cut=%v %s", c0, t0, c1, t1)
	if c1 || t1 > lb2HoldMax+30*time.Second {
		t.Fatalf("confirm: still cut=%v after %s", c1, t1)
	}
	if !c0 {
		t.Fatalf("baseline recovered in %s: the test does not show the spike reset", t0)
	}
	if c, d := run(true, true); !c {
		t.Fatalf("confirm: a still-bad path recovered after %s", d)
	}
	// Short hold: same as without the key on clean readings.
	oc, of := lbRecConfirm, lbFastRecover
	lbRecConfirm, lbFastRecover = true, true
	defer func() { lbRecConfirm, lbFastRecover = oc, of }()
	j := newQ2Judge(0.1, "kharej", "Iran", nil)
	now := time.Unix(1000, 0)
	q2feed(j, &now, 3, good)
	q2feed(j, &now, 8, bad)
	if d := recoverAfter(j, &now, "k>a", good); d > 35*time.Second {
		t.Fatalf("confirm: first recovery took %s", d)
	}
}
