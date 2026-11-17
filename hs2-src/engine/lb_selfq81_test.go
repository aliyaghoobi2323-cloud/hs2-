package engine

import (
	"testing"
	"time"
)

// lbv81: our own heavy download starts on one clean path and its queue
// reading jumps to ~40 ms for two readings (the soak's ground truth). With
// HS2_LB_SELFQ off the judge lowers the clean path (production behaviour);
// with it on the path stays "ok". An outside queue (the path slower for
// everyone, our bytes not crowding it) is lowered either way.
func q2SelfQRun(t *testing.T, k float64, self bool) float64 {
	t.Helper()
	old := lbSelfQ
	lbSelfQ = k
	t.Cleanup(func() { lbSelfQ = old })
	j := newQ2Judge(0.1, "kharej", "Iran", nil)
	now := time.Unix(1000, 0)
	feed := func(qc float64, selfC bool) {
		now = now.Add(5 * time.Second)
		rs := map[string]q2Reading{}
		for _, p := range []string{"k>a", "k>b", "k>c", "k>d"} {
			rs[p] = q2Reading{loss: 0.002, queueMs: 11}
		}
		sl := selfLoaded([]uint64{11 << 20, 11 << 20, 11 << 20, 11 << 20})
		if selfC {
			sl = selfLoaded([]uint64{11 << 20, 11 << 20, 400 << 20, 11 << 20})
		}
		rs["k>c"] = q2Reading{loss: 0.002, queueMs: qc, selfQ: sl != nil && sl[2]}
		j.update(now, 5*time.Second, rs)
	}
	for i := 0; i < 6; i++ {
		feed(11, false)
	}
	minT := 1.0
	for i := 0; i < 3; i++ {
		feed(42, self)
		minT = min(minT, j.p["k>c"].target)
	}
	return minT
}

func TestSelfQueueNotPunished(t *testing.T) {
	off := q2SelfQRun(t, 0, true)
	on := q2SelfQRun(t, 4, true)
	t.Logf("clean path under our own heavy flow: min target off %.2f, on %.2f", off, on)
	if off >= 1 {
		t.Fatalf("test does not reproduce the lowering (off %.2f)", off)
	}
	if on < 1 {
		t.Fatalf("self-loaded path still lowered with the key on: %.2f", on)
	}
	ext := q2SelfQRun(t, 4, false)
	if ext >= 1 {
		t.Fatalf("outside queue no longer lowers the path with the key on: %.2f", ext)
	}
}

func TestSelfLoaded(t *testing.T) {
	old := lbSelfQ
	t.Cleanup(func() { lbSelfQ = old })
	lbSelfQ = 0
	if selfLoaded([]uint64{1, 2, 900 << 20}) != nil {
		t.Fatal("off must return nil")
	}
	lbSelfQ = 4
	s := selfLoaded([]uint64{11 << 20, 12 << 20, 338 << 20, 0, 10 << 20})
	if s == nil || !s[2] || s[0] || s[1] || s[3] || s[4] {
		t.Fatalf("got %v", s)
	}
	// small totals never count (a quiet tunnel, one link busy)
	if s := selfLoaded([]uint64{10 << 10, 10 << 10, 5 << 20}); s[2] {
		t.Fatal("under 8 MiB counted")
	}
	if selfLoaded([]uint64{0, 5 << 20, 900 << 20}) != nil {
		t.Fatal("two paths with bytes: no median")
	}
}
