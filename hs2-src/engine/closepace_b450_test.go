package engine

import (
	"testing"
	"time"
)

func TestB450ClosePaceRate(t *testing.T) {
	if got := closePaceRate("", 10); got != 5000 {
		t.Fatalf("default rate %d", got)
	}
	if got := closePaceRate("0", 10); got != 0 {
		t.Fatalf("off rate %d", got)
	}
	if got := closePaceRate("1200", 10); got != 1200 {
		t.Fatalf("set rate %d", got)
	}
	if got := closePaceRate("x", 4); got != 2000 {
		t.Fatalf("bad env rate %d", got)
	}
}

// A burst within the allowance passes at once; past it, each close waits
// one interval more than the one before; after a quiet spell it is free again.
func TestB450ClosePaceDelay(t *testing.T) {
	closePaceTAT.Store(0)
	defer closePaceTAT.Store(0)
	iv, tau := int64(time.Millisecond), int64(10*time.Millisecond)
	now := int64(time.Hour)
	free := 0
	for i := 0; i < 11; i++ {
		if closePaceDelay(now, iv, tau, 0) <= 0 {
			free++
		}
	}
	if free != 11 {
		t.Fatalf("burst: %d of 11 free", free)
	}
	d1 := closePaceDelay(now, iv, tau, 0)
	d2 := closePaceDelay(now, iv, tau, 0)
	if d1 != iv || d2 != 2*iv {
		t.Fatalf("paced delays %d %d, want %d %d", d1, d2, iv, 2*iv)
	}
	if d := closePaceDelay(now+int64(time.Second), iv, tau, 0); d > 0 {
		t.Fatalf("after a quiet second: delay %d", d)
	}
}

func TestB452ClosePaceMaxWait(t *testing.T) {
	for env, want := range map[string]time.Duration{"": 2500 * time.Millisecond, "0": 0, "3s": 3 * time.Second,
		"2000": 2 * time.Second, "x": 2500 * time.Millisecond, "-1s": 2500 * time.Millisecond} {
		if got := closePaceMaxWait(env); got != want {
			t.Fatalf("maxwait %q = %v, want %v", env, got, want)
		}
	}
}

// A storm 3x the size the cap covers: no close waits past the cap, the
// overflow is spread over the cap window (not woken together), and right
// after the cap the pace is free again (no stale backlog).
func TestB452ClosePaceCap(t *testing.T) {
	closePaceTAT.Store(0)
	defer closePaceTAT.Store(0)
	iv, tau, capNs := int64(time.Millisecond), int64(10*time.Millisecond), int64(time.Second)
	now := int64(time.Hour)
	lanes0 := closeLanes.Load()
	n := 3 * int(capNs/iv)
	hist := make([]int, 10) // waits by 100 ms bucket
	for i := 0; i < n; i++ {
		d := closePaceDelay(now, iv, tau, capNs)
		if d > capNs {
			t.Fatalf("close %d waits %v, past the %v cap", i, time.Duration(d), time.Duration(capNs))
		}
		hist[min(max(d, 0)*10/capNs, 9)]++
	}
	if l := closeLanes.Load() - lanes0; l < 2 || l > 3 {
		t.Fatalf("%d new lanes, want 2-3", l)
	}
	for b, c := range hist {
		if c > n/10+2*int(tau/iv)+10 {
			t.Fatalf("bucket %d holds %d of %d closes: overflow bunched %v", b, c, n, hist)
		}
	}
	if d := closePaceDelay(now+capNs+tau+iv, iv, tau, capNs); d > 0 {
		t.Fatalf("after the cap: delay %v", time.Duration(d))
	}
	// Uncapped, the same storm would have queued 3x longer.
	closePaceTAT.Store(0)
	var last int64
	for i := 0; i < n; i++ {
		last = closePaceDelay(now, iv, tau, 0)
	}
	if last < 2*capNs {
		t.Fatalf("uncapped last wait %v", time.Duration(last))
	}
}
