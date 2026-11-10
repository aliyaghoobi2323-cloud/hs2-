package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// flapSim runs a judge over paths for dur; lossy(k, cutNow, sinceUncut) gives
// path k's loss this reading. It returns the cuts per path and the logs.
func flapSim(t *testing.T, pen bool, paths []string, dur time.Duration, lossy func(k string, q *q2Path, now time.Time) float64) (*q2Judge, map[string]int, []string) {
	t.Helper()
	o1, o2, o3 := lbFlapPen, lbRecConfirm, lbFastRecover
	lbFlapPen, lbRecConfirm, lbFastRecover = pen, true, true
	t.Cleanup(func() { lbFlapPen, lbRecConfirm, lbFastRecover = o1, o2, o3 })
	var logs []string
	j := newQ2Judge(0.1, "kharej", "Iran", func(f string, a ...any) { logs = append(logs, strings.TrimSpace(fmt.Sprintf(f, a...))) })
	now := time.Unix(1000, 0)
	cuts := map[string]int{}
	was := map[string]bool{}
	for end := now.Add(dur); now.Before(end); {
		now = now.Add(5 * time.Second)
		rs := map[string]q2Reading{}
		for _, k := range paths {
			rs[k] = q2Reading{loss: lossy(k, j.path(k), now)}
		}
		j.update(now, 5*time.Second, rs)
		for _, k := range paths {
			c := j.p[k].cut
			if c && !was[k] {
				cuts[k]++
			}
			was[k] = c
		}
	}
	return j, cuts, logs
}

var six = []string{"k1>a", "k1>b", "k1>c", "k2>a", "k2>b", "k2>c"}

// loadBad: path k reads clean while cut (no load) and bad 20 s after it
// recovers (its users are back): the prod flapper.
func loadBad(bad map[string]bool) func(string, *q2Path, time.Time) float64 {
	return func(k string, q *q2Path, now time.Time) float64 {
		if bad[k] && !q.cut && !q.uncutAt.IsZero() && now.Sub(q.uncutAt) >= 20*time.Second {
			return 0.15
		}
		if bad[k] && q.uncutAt.IsZero() && !q.cut {
			return 0.15
		}
		return 0.003
	}
}

func countLogs(logs []string, sub string) int {
	n := 0
	for _, l := range logs {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

func TestFlapPenFlapperKeptOut(t *testing.T) {
	bad := map[string]bool{"k1>a": true}
	_, c0, _ := flapSim(t, false, six, 2*time.Hour, loadBad(bad))
	_, c1, l1 := flapSim(t, true, six, 2*time.Hour, loadBad(bad))
	t.Logf("cuts of the flapper in 2h: off %d, on %d", c0["k1>a"], c1["k1>a"])
	for _, l := range l1 {
		if strings.Contains(l, "flap") {
			t.Log(l)
		}
	}
	if c1["k1>a"] >= c0["k1>a"]/2 {
		t.Fatalf("penalty did not cut the cycles: off %d on %d", c0["k1>a"], c1["k1>a"])
	}
	if countLogs(l1, "kept out 30m") == 0 || countLogs(l1, "on probation") == 0 || countLogs(l1, "kept out 1h00m") == 0 {
		t.Fatal("no penalty / probation / doubled penalty logged")
	}
	for _, k := range six[1:] {
		if c1[k] != 0 {
			t.Fatalf("clean path %s cut %d times", k, c1[k])
		}
	}
}

func TestFlapPenOnlyRelativeCuts(t *testing.T) {
	// Every path lossy at once (the border): no relative cut, no penalty.
	_, c, l := flapSim(t, true, six, time.Hour, func(k string, q *q2Path, now time.Time) float64 {
		if (now.Unix()/60)%2 == 0 {
			return 0.12
		}
		return 0.003
	})
	if countLogs(l, "flapping") != 0 || c["k1>a"] != 0 {
		t.Fatalf("border-wide loss penalized: cuts %v %v", c, l)
	}
	// One remote IP (a) lossy on all its paths, on and off: IP verdicts,
	// which must not count.
	j, _, l2 := flapSim(t, true, six, time.Hour, func(k string, q *q2Path, now time.Time) float64 {
		if strings.HasSuffix(k, ">a") && (now.Unix()/150)%2 == 0 {
			return 0.15
		}
		return 0.003
	})
	ipCuts := countLogs(l2, "cut together")
	t.Logf("ip verdicts %d, flapping logs %d", ipCuts, countLogs(l2, "flapping"))
	if ipCuts > 0 && countLogs(l2, "kept out") != 0 {
		t.Fatalf("IP-verdict cuts counted: %v", l2)
	}
	_ = j
}

func TestFlapPenCap(t *testing.T) {
	bad := map[string]bool{"k1>a": true, "k1>b": true, "k2>c": true}
	j, _, l := flapSim(t, true, six, 2*time.Hour, func(k string, q *q2Path, now time.Time) float64 {
		v := loadBad(bad)(k, q, now)
		if j0 := q; j0 != nil && v > 0.1 {
			return v
		}
		return v
	})
	maxPen := 0
	for _, s := range l {
		if strings.Contains(s, "kept out") {
			// "penalty N of max M"
			i := strings.Index(s, "penalty ")
			n := int(s[i+len("penalty ")] - '0')
			maxPen = max(maxPen, n)
		}
	}
	t.Logf("max penalties at once %d, no-penalty logs %d", maxPen, countLogs(l, "no penalty"))
	if maxPen > 2 || maxPen == 0 {
		t.Fatalf("cap: %d at once", maxPen)
	}
	_ = j
	// 2 paths: never.
	_, _, l2 := flapSim(t, true, []string{"k1>a", "k1>b"}, time.Hour, loadBad(map[string]bool{"k1>a": true}))
	if countLogs(l2, "kept out") != 0 {
		t.Fatal("penalty with 2 paths")
	}
}

func TestFlapPenRelease(t *testing.T) {
	bad := map[string]bool{"k1>a": true}
	var penAt time.Time
	j, _, l := flapSim(t, true, six, time.Hour, func(k string, q *q2Path, now time.Time) float64 {
		if k == "k1>a" && q.flap.pen && penAt.IsZero() {
			penAt = now
		}
		if !penAt.IsZero() && now.Sub(penAt) > 2*time.Minute && (k == "k1>b" || k == "k1>c" || k == "k2>a") {
			return 0.08 // three of the five others go bad
		}
		return loadBad(bad)(k, q, now)
	})
	if penAt.IsZero() {
		t.Fatal("never penalized")
	}
	if countLogs(l, "flap penalties lifted") == 0 {
		t.Fatalf("not lifted: %v", l)
	}
	if j.p["k1>a"].flap.pen {
		t.Fatal("still penalized")
	}
	for _, s := range l {
		if strings.Contains(s, "lifted") {
			t.Log(s)
		}
	}
}

func TestFlapPenProbation(t *testing.T) {
	// Clean after its first penalty's early return, then one cut within the
	// probation: the next penalty at once, doubled.
	bad := map[string]bool{"k1>a": true}
	returned := time.Time{}
	_, _, l := flapSim(t, true, six, 90*time.Minute, func(k string, q *q2Path, now time.Time) float64 {
		if k == "k1>a" && q.flap.level == 1 && !q.flap.pen && returned.IsZero() {
			returned = now
		}
		if k == "k1>a" && !returned.IsZero() {
			if now.Sub(returned) > 5*time.Minute && now.Sub(returned) < 6*time.Minute && !q.cut {
				return 0.15
			}
			return 0.003
		}
		return loadBad(bad)(k, q, now)
	})
	if countLogs(l, "cut again on probation — kept out 1h00m") != 1 {
		t.Fatalf("probation: %v", l)
	}
}
