package udpcarrier

import (
	"testing"
	"time"
)

// camoProbeOffset is deterministic and bounded: a given period index always
// maps to the same shift, inside [-camoProbeJit, +camoProbeJit]. Determinism is
// what keeps the pool synchronised (every carrier shifts the k-th probe alike).
func TestCamoProbeOffsetDeterministicBounded(t *testing.T) {
	seen := map[int64]time.Duration{}
	distinct := map[time.Duration]bool{}
	for k := int64(0); k < 2000; k++ {
		o := camoProbeOffset(k)
		if o < -camoProbeJit || o > camoProbeJit {
			t.Fatalf("k=%d: offset %v out of [-%v,+%v]", k, o, camoProbeJit, camoProbeJit)
		}
		if prev, ok := seen[k]; ok && prev != o {
			t.Fatalf("k=%d: not deterministic (%v vs %v)", k, prev, o)
		}
		seen[k] = o
		if o2 := camoProbeOffset(k); o2 != o {
			t.Fatalf("k=%d: second call differs (%v vs %v)", k, o, o2)
		}
		distinct[o] = true
	}
	// The offsets must actually spread, not collapse to a few values.
	if len(distinct) < 1500 {
		t.Fatalf("offsets not well spread: only %d distinct of 2000", len(distinct))
	}
}

// Under camouflage the whole pool still probes together: two carriers sharing
// the epoch compute the SAME next-probe time, so their collective back-off
// still measures a clean min-RTT. Only the grid stops being fixed-period.
func TestCamoProbeSynchronisedButNotFixedGrid(t *testing.T) {
	old := icmpCamo
	icmpCamo = true
	defer func() { icmpCamo = old }()

	a, b := newRateControl(), newRateControl()
	a.fair, b.fair = true, true
	base := a.epoch // both default to probeEpoch
	if !a.epoch.Equal(b.epoch) {
		t.Fatal("carriers do not share the probe epoch")
	}
	var intervals []time.Duration
	var last time.Time
	for i := 0; i < 20; i++ {
		now := base.Add(time.Duration(i) * baseProbeEvery)
		pa, pb := a.nextProbe(now), b.nextProbe(now)
		if !pa.Equal(pb) {
			t.Fatalf("i=%d: carriers probe at different times (%v vs %v) — pool desynced", i, pa, pb)
		}
		if i > 0 {
			intervals = append(intervals, pa.Sub(last))
		}
		last = pa
	}
	// Not a fixed grid: the intervals between consecutive shared probes vary.
	allEqual := true
	for _, d := range intervals {
		if d != intervals[0] {
			allEqual = false
			break
		}
	}
	if allEqual {
		t.Fatal("probe schedule is still a fixed grid under camouflage (0.25 Hz line intact)")
	}
	// But they stay roughly a period apart — never back-to-back, never runaway.
	for _, d := range intervals {
		if d < baseProbeEvery-2*camoProbeJit || d > baseProbeEvery+2*camoProbeJit {
			t.Fatalf("probe interval %v outside the jittered band around %v", d, baseProbeEvery)
		}
	}
}

func TestCamoJitterBounds(t *testing.T) {
	const d = 100 * time.Millisecond
	var sum time.Duration
	const n = 5000
	for i := 0; i < n; i++ {
		j := camoJitter(d, 0.4)
		if j < 60*time.Millisecond || j > 140*time.Millisecond {
			t.Fatalf("jitter %v outside [60ms,140ms]", j)
		}
		sum += j
	}
	avg := sum / n
	if avg < 90*time.Millisecond || avg > 110*time.Millisecond {
		t.Fatalf("mean jitter %v far from 100ms", avg)
	}
}
