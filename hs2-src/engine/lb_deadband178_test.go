package engine

import (
	"testing"
	"time"
)

// wj178: noise (0.2 % loss, 30 ms queue beside a 0.05 % / 5 ms path) does not
// lower a path; 2 % loss still lowers it at once; 1 % lowers it on the second
// reading.
func TestLB2LowerEvidNoiseNotLowered(t *testing.T) {
	run := func(on bool, loss, queue float64, n int) (float64, string) {
		old := lbLowerEvid
		lbLowerEvid = on
		defer func() { lbLowerEvid = old }()
		j := newQ2Judge(lb2Floor, "l", "r", func(string, ...any) {})
		now := time.Unix(1000, 0)
		feed := func(l, q float64) {
			now = now.Add(5 * time.Second)
			j.update(now, 5*time.Second, map[string]q2Reading{
				"a>x": {loss: 0.0005, queueMs: 5}, "b>x": {loss: 0.0005, queueMs: 5}, "c>x": {loss: l, queueMs: q},
			})
		}
		for i := 0; i < 4; i++ {
			feed(0.0005, 5)
		}
		for i := 0; i < n; i++ {
			feed(loss, queue)
		}
		q := j.p["c>x"]
		return q.target, q.reason
	}
	if tg, why := run(false, 0.002, 30, 3); tg >= 1 {
		t.Logf("old rule did not lower the noisy path (%.2f %s)", tg, why)
	} else {
		t.Logf("old rule: noisy path lowered to %.2f (%s)", tg, why)
	}
	if tg, why := run(true, 0.002, 30, 6); tg < 1 {
		t.Fatalf("noise lowered the path: %.2f %s", tg, why)
	}
	if tg, why := run(true, 0.025, 5, 1); tg >= 1 {
		t.Fatalf("2.5%% loss not lowered at once: %.2f %s", tg, why)
	}
	tg1, _ := run(true, 0.012, 5, 1)
	tg2, why2 := run(true, 0.012, 5, 2)
	if tg1 < 1 || tg2 >= 1 {
		t.Fatalf("1.2%% loss: first reading %.2f, second %.2f (%s)", tg1, tg2, why2)
	}
}
