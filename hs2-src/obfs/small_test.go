package obfs

import (
	"math/rand/v2"
	"testing"
)

// A small write's padded size is never below what it needs nor above
// SmallMax, covers the whole range, and follows smallRanges' weights.
func TestSmallTarget(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for _, need := range []int{4, 12, 13, 40, 41, 119, 120, 200, SmallMax} {
		seen := map[int]bool{}
		for i := 0; i < 20000; i++ {
			s := SmallTarget(need, r.Float64(), r.Float64())
			if s < need || s > SmallMax {
				t.Fatalf("need %d: size %d outside [%d, %d]", need, s, need, SmallMax)
			}
			seen[s] = true
		}
		lo := max(need, 13)
		if want := SmallMax - lo + 1; len(seen) < want*9/10 {
			t.Errorf("need %d: %d distinct sizes of %d possible", need, len(seen), want)
		}
	}
	if s := SmallTarget(13, 0, 1); s != 40 {
		t.Errorf("edge draw v=1 in the first range: %d, want its top 40", s)
	}
	n, inFirst := 100000, 0
	for i := 0; i < n; i++ {
		if SmallTarget(12, r.Float64(), r.Float64()) <= 40 {
			inFirst++
		}
	}
	if f := float64(inFirst) / float64(n); f < 0.48 || f > 0.53 {
		t.Errorf("share of control-frame sizes %.3f, want ~0.5", f)
	}
}
