package engine

import (
	"strings"
	"testing"
	"time"
)

func withFastVerdict(t *testing.T, on bool) {
	t.Helper()
	old := lbFastVerdict
	lbFastVerdict = on
	t.Cleanup(func() { lbFastVerdict = old })
}

// fvFeed: one reading every 5 s with loss, delay over the minimum and segs.
func fvFeed(j *q2Judge, now *time.Time, rs map[string]q2Reading) {
	*now = now.Add(5 * time.Second)
	j.update(*now, 5*time.Second, rs)
}

func fvGood() map[string]q2Reading {
	return map[string]q2Reading{
		"k>a": {loss: 0.011, queueMs: 3, segs: 20000},
		"k>b": {loss: 0.011, queueMs: 3, segs: 20000},
		"k>c": {loss: 0.011, queueMs: 4, segs: 20000},
	}
}

// B goes +80 ms and 3% corrupt (lab phase 4d): one reading lowers it to the
// floor; the cut still takes its two readings.
func TestFastVerdictClearDegradation(t *testing.T) {
	withFastVerdict(t, true)
	j := newQ2Judge(0.1, "kharej", "Iran", nil)
	now := time.Unix(1000, 0)
	for i := 0; i < 4; i++ {
		fvFeed(j, &now, fvGood())
	}
	rs := fvGood()
	rs["k>b"] = q2Reading{loss: 0.042, queueMs: 80, segs: 15000}
	fvFeed(j, &now, rs)
	q := j.p["k>b"]
	if !q.fast || q.cut || q.target != 0.1 || !strings.HasPrefix(q.reason, "lower at once") {
		t.Fatalf("one clear reading: fast=%v cut=%v target=%.2f reason=%q", q.fast, q.cut, q.target, q.reason)
	}
	if j.p["k>a"].target != 1 || j.p["k>c"].target != 1 {
		t.Fatalf("good paths moved: %.2f %.2f", j.p["k>a"].target, j.p["k>c"].target)
	}
	fvFeed(j, &now, rs)
	if !q.cut {
		t.Fatal("not cut after the second bad reading")
	}
	// Key off: the same reading lowers by the goodness ratio only.
	withFastVerdict(t, false)
	j2 := newQ2Judge(0.1, "kharej", "Iran", nil)
	now = time.Unix(1000, 0)
	for i := 0; i < 4; i++ {
		fvFeed(j2, &now, fvGood())
	}
	rs2 := fvGood()
	rs2["k>b"] = q2Reading{loss: 0.042, queueMs: 0, segs: 15000} // bad from the first packet: no delay shows
	fvFeed(j2, &now, rs2)
	if q := j2.p["k>b"]; q.fast || q.target <= 0.1 {
		t.Fatalf("key off: fast=%v target %.2f", q.fast, q.target)
	}
}

// The guards: a small sample, loss alone, delay alone, every path bad, the
// start grace — none of them lowers at once. A good reading clears it.
func TestFastVerdictGuards(t *testing.T) {
	withFastVerdict(t, true)
	cases := []struct {
		name  string
		warm  int
		b     q2Reading
		other float64 // the other paths' loss
	}{
		{"small sample", 4, q2Reading{loss: 0.05, queueMs: 90, segs: 600}, 0.011},
		{"loss alone", 4, q2Reading{loss: 0.05, queueMs: 10, segs: 20000}, 0.011},
		{"delay alone (a download's queue)", 4, q2Reading{loss: 0.012, queueMs: 120, segs: 20000}, 0.011},
		{"loss not over 2x best + 1pt", 4, q2Reading{loss: 0.031, queueMs: 90, segs: 20000}, 0.011},
		{"all paths bad", 4, q2Reading{loss: 0.09, queueMs: 90, segs: 20000}, 0.05},
		{"start grace", 0, q2Reading{loss: 0.05, queueMs: 90, segs: 20000}, 0.011},
	}
	for _, c := range cases {
		j := newQ2Judge(0.1, "kharej", "Iran", nil)
		now := time.Unix(1000, 0)
		for i := 0; i < c.warm; i++ {
			fvFeed(j, &now, fvGood())
		}
		if c.warm == 0 {
			now = now.Add(-5 * time.Second) // the very first reading
		}
		rs := map[string]q2Reading{
			"k>a": {loss: c.other, queueMs: 3, segs: 20000},
			"k>b": c.b,
			"k>c": {loss: c.other, queueMs: 4, segs: 20000},
		}
		fvFeed(j, &now, rs)
		if j.p["k>b"].fast {
			t.Errorf("%s: lowered at once", c.name)
		}
	}
	j := newQ2Judge(0.1, "kharej", "Iran", nil)
	now := time.Unix(1000, 0)
	for i := 0; i < 4; i++ {
		fvFeed(j, &now, fvGood())
	}
	rs := fvGood()
	rs["k>b"] = q2Reading{loss: 0.05, queueMs: 90, segs: 20000}
	fvFeed(j, &now, rs)
	if !j.p["k>b"].fast {
		t.Fatal("not lowered")
	}
	fvFeed(j, &now, fvGood())
	if j.p["k>b"].fast {
		t.Fatal("a good reading did not clear the fast verdict")
	}
}
