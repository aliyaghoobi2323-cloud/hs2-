package core

import "testing"

func TestPadDatagramTarget(t *testing.T) {
	const defMTU = 1280
	cases := []struct{ n, want int }{
		{0, 64}, {1, 64}, {40, 64}, {64, 64},
		{65, 128}, {100, 128}, {128, 128},
		{129, 256}, {256, 256},
		{257, 512}, {512, 512},
		{513, 1024}, {1024, 1024},
		{1025, 1025}, // bulk: above the largest bucket -> sent as-is
		{1200, 1200},
		{1280, 1280},
	}
	for _, c := range cases {
		if got := PadDatagramTarget(c.n, defMTU); got != c.want {
			t.Fatalf("PadDatagramTarget(%d, %d) = %d, want %d", c.n, defMTU, got, c.want)
		}
	}

	// Disabled (max <= 0) never pads.
	if got := PadDatagramTarget(40, 0); got != 40 {
		t.Fatalf("PadDatagramTarget(40, 0) = %d, want 40 (disabled)", got)
	}

	// Core invariants across every MTU: padding never shrinks a payload and a
	// PADDED result is always strictly below the MTU budget, so a padded frame
	// can never be pushed over the path MTU and fragment.
	for mtu := 1; mtu <= 1600; mtu++ {
		for _, n := range []int{0, 1, 63, 64, 65, 100, 500, 1000, 1023, 1024, 1025, 1300, 1500} {
			got := PadDatagramTarget(n, mtu)
			if got < n {
				t.Fatalf("PadDatagramTarget(%d, %d) = %d shrank the payload", n, mtu, got)
			}
			if got > n && got >= mtu {
				t.Fatalf("PadDatagramTarget(%d, %d) = %d padded to >= MTU budget (fragmentation risk)", n, mtu, got)
			}
		}
	}

	// A full-MTU (bulk) payload is never padded, so the bulk direction a tunnel
	// sizes for carries zero padding overhead.
	if got := PadDatagramTarget(defMTU, defMTU); got != defMTU {
		t.Fatalf("PadDatagramTarget(mtu, mtu) = %d, want %d (bulk must be untouched)", got, defMTU)
	}
}
