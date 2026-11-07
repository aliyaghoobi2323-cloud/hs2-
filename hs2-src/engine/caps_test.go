package engine

import "testing"

// TestCapsDistinct: every kindInfo capability bit is its own bit of the one
// caps byte. Two features written on separate branches once both took 1<<4,
// so a peer advertising one was read as supporting the other.
func TestCapsDistinct(t *testing.T) {
	caps := map[string]int{
		"capPortTags": capPortTags,
		"capL3Quiet":  capL3Quiet,
		"capL3Dup":    capL3Dup,
		"capMConn":    capMConn,
		"capMConnRaw": capMConnRaw,
		"capLBAdvert": capLBAdvert,
		"capLB":       capLB,
	}
	seen := map[int]string{}
	for name, v := range caps {
		if v <= 0 || v > 0xff || v&(v-1) != 0 {
			t.Fatalf("%s = %#x: not a single bit of the caps byte", name, v)
		}
		if other, ok := seen[v]; ok {
			t.Fatalf("%s and %s share bit %#x", name, other, v)
		}
		seen[v] = name
	}
}
