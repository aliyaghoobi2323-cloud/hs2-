package engine

import (
	"testing"
	"time"
)

// R1 round 5 (rtlane.go, RTLoose): a call held back from its first tick —
// its two ways in clumps, never steady — gets the steady realtime allowance
// with RTLoose, and keeps it (RTHold); without it, it never does. A
// download that acknowledges in its own data (2 % and 3.5 % back), an
// upload, and a plain download never get it.
func TestRTLooseCall(t *testing.T) {
	withTune(t, RTHold, 20)
	// a 1.5 Mbit/s call held back both ways from its start: a tick of 60 KiB
	// in four this way, 37.5 KB every other tick the other
	clumps := func(n int) [][2]int {
		var v [][2]int
		for i := range n {
			tx, rx := 0, 0
			if i%4 == 3 {
				tx = 60 << 10
			}
			if i%2 == 1 {
				rx = 37500
			}
			v = append(v, [2]int{tx, rx})
		}
		return v
	}
	for _, loose := range []int{0, 1} {
		withTune(t, RTLoose, loose)
		var c flowClassifier
		var f flowStats
		var at time.Duration
		a := classRun(&c, &f, &at, clumps(60))
		zero := 0
		for _, v := range a[classSlots:] {
			if v == 0 {
				zero++
			}
		}
		t.Logf("RTLoose %d: a call in clumps from its start: %d of %d ticks past its first window without a realtime allowance", loose, zero, len(a)-classSlots)
		switch {
		case loose == 0 && zero == 0:
			t.Fatal("without RTLoose the clumped call got its lane: the test does not test")
		case loose == 1 && zero != 0:
			t.Fatalf("with RTLoose the clumped call went %d ticks without its lane", zero)
		}
	}
	withTune(t, RTLoose, 1)
	for _, k := range []struct {
		name string
		v    [2]int
	}{
		{"download acknowledged in its data (2 %)", [2]int{60 << 10, 1229}},
		{"slow download acknowledged in its data (3.5 %)", [2]int{12500, 438}},
		{"upload", [2]int{50, 100000}},
		{"download", [2]int{60 << 10, 0}},
	} {
		var c flowClassifier
		var f flowStats
		var at time.Duration
		ticks := make([][2]int, 60)
		for i := range ticks {
			ticks[i] = k.v
		}
		got := 0
		for _, v := range classRun(&c, &f, &at, ticks) {
			if v == rtAllowMax {
				got++
			}
		}
		if got != 0 {
			t.Errorf("%s: %d ticks of the steady allowance", k.name, got)
		}
	}
}
