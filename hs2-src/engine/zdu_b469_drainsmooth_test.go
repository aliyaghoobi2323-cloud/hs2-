package engine

import (
	"strings"
	"testing"
	"time"
)

// TestB469HoldOverStorm: a close storm raises the GC target, the previous
// one comes back 2 s after the storm; a slow trickle never holds.
func TestB469HoldOverStorm(t *testing.T) {
	cur := 100
	var sets []int
	h := &b469Hold{minEnds: 500, pct: 400, set: func(n int) int { p := cur; cur = n; sets = append(sets, n); return p }}
	t0 := time.Unix(1000, 0)
	var ends int64
	now := t0
	tick := func(add int64) string { ends += add; now = now.Add(b469Tick); return h.step(ends, now) }
	for i := 0; i < 50; i++ { // 40 ends a second: no storm
		if s := tick(4); s != "" || h.holding {
			t.Fatalf("trickle held: %q", s)
		}
	}
	if cur != 100 {
		t.Fatalf("target %d, want 100", cur)
	}
	for i := 0; i < 40; i++ { // 4 s of ~1500 ends a second
		tick(150)
	}
	if !h.holding || cur != 400 {
		t.Fatalf("storm: holding %v target %d", h.holding, cur)
	}
	var line string
	for i := 0; i < 30 && line == ""; i++ {
		line = tick(0)
	}
	if h.holding || cur != 100 || !strings.Contains(line, "back to 100%") {
		t.Fatalf("after storm: holding %v target %d line %q", h.holding, cur, line)
	}
}

// TestB469MaxAndOff: a hold ends after b469MaxDur even if the storm goes on;
// GC off (-1) or a target already looser is left alone.
func TestB469MaxAndOff(t *testing.T) {
	cur := 100
	h := &b469Hold{minEnds: 500, pct: 400, set: func(n int) int { p := cur; cur = n; return p }}
	now := time.Unix(1000, 0)
	var ends int64
	var line string
	for i := 0; i < 200 && line == ""; i++ {
		ends += 200
		now = now.Add(b469Tick)
		line = h.step(ends, now)
	}
	if line == "" || cur != 100 {
		t.Fatalf("no max-duration end: target %d", cur)
	}
	for _, start := range []int{-1, 500} {
		cur = start
		h2 := &b469Hold{minEnds: 500, pct: 400, set: func(n int) int { p := cur; cur = n; return p }}
		now, ends = time.Unix(1000, 0), 0
		for i := 0; i < 20; i++ {
			ends += 200
			now = now.Add(b469Tick)
			h2.step(ends, now)
		}
		if h2.holding || cur != start {
			t.Fatalf("start %d: holding %v target %d", start, h2.holding, cur)
		}
	}
}

// TestB469StepSmallBatches: HS2_B469_STEP_MS=5 (the default) closes 6034
// users at the same rate in batches of ~13 (a 25 ms step: ~66).
func TestB469StepSmallBatches(t *testing.T) {
	t.Setenv("HS2_B470_PAR", "1") // cur is not atomic
	for _, c := range []struct {
		env       string
		maxBatch  int
		minBatchs int
	}{{"25", 67, 90}, {"5", 14, 450}, {"", 14, 450}} {
		t.Setenv("HS2_B469_STEP_MS", c.env)
		ls := make([]zduPaceLink, 6034)
		cur, big := 0, 0
		for i := range ls {
			ls[i] = zduPaceLink{users: 1, close: func() { cur++ }}
		}
		var slept time.Duration
		sleep := func(d time.Duration) {
			big = max(big, cur)
			cur = 0
			slept += d
		}
		_, users, batches := zduPaceClose(ls, 2667, b469Step(), zduPaceMax, sleep)
		big = max(big, cur)
		if users != 6034 || big > c.maxBatch || batches < c.minBatchs {
			t.Fatalf("step %q: users %d biggest batch %d batches %d", c.env, users, big, batches)
		}
		if slept < 2*time.Second || slept > 3*time.Second { // 6034/2667 = 2.26 s either way
			t.Fatalf("step %q: slept %v", c.env, slept)
		}
	}
}
