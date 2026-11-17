package engine

import (
	"testing"
	"time"
)

// rel58: the frozen-session guard is off unless HS2_SMUX_STALL_S is set.
func TestSmuxStallDefaultOff(t *testing.T) {
	for _, c := range []struct {
		v    string
		want time.Duration
	}{
		{"", 0}, {"0", 0}, {"x", 0}, {"-3", 0},
		{"1", 2 * time.Second}, {"15", 15 * time.Second},
	} {
		if got := smuxStallFromEnv(c.v); got != c.want {
			t.Errorf("smuxStallFromEnv(%q) = %v, want %v", c.v, got, c.want)
		}
	}
}
