package engine

import (
	"testing"
	"time"
)

// lbv81: a path at 0.85-0.95 % loss beside 0.1 % paths, with reading noise
// around lb2Dead, must not flip lower/ok every few readings with the band on;
// without the band it does (the production behaviour, kept by default).
func q2Flips(t *testing.T, hyst float64) int {
	t.Helper()
	old := lbDeadHyst
	lbDeadHyst = hyst
	t.Cleanup(func() { lbDeadHyst = old })
	j := newQ2Judge(0.1, "kharej", "Iran", nil)
	now := time.Unix(1000, 0)
	flips, prev := 0, 1.0
	for i := 0; i < 120; i++ {
		// a 0.8 % path as the soak read it: mostly 0.55 %, a 1.1 % reading
		// every 40 s (the smoothed loss jumps up at once and decays slowly)
		l := 0.0055
		if i%8 == 0 {
			l = 0.0110
		}
		q2feed(j, &now, 1, map[string]float64{"k>a": 0.001, "k>b": 0.001, "k>h": l})
		q := j.p["k>h"]
		if q.cut {
			t.Fatalf("0.8%% path cut at %d", i)
		}
		if i > 10 && (q.target < 1) != (prev < 1) {
			flips++
		}
		prev = q.target
	}
	return flips
}

func TestDeadHystStopsFlipping(t *testing.T) {
	off := q2Flips(t, 0)
	on := q2Flips(t, 0.08)
	t.Logf("lower/ok flips in 10 min: band off %d, band 8 points %d", off, on)
	if on > 2 {
		t.Fatalf("band on still flips %d times", on)
	}
	if off <= on {
		t.Fatalf("test does not reproduce the flip (off %d on %d)", off, on)
	}
}

func TestDeadHystOffIsProduction(t *testing.T) {
	if lbDeadHyst != 0 {
		t.Skip("HS2_LB_DEAD_HYST set in the environment")
	}
	q := &q2Path{target: 0.5}
	if deadBar(q) != lb2Dead {
		t.Fatal("band must be off by default")
	}
}
