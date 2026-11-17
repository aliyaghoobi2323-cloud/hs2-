package engine

import (
	"os"
	"testing"
)

// audit246 L7: the half-open cap is sized for 4x scale (16384), unless
// HS2_HALF_CLOSE_MAX sets it.
func TestHalfCloseMaxDefault(t *testing.T) {
	if _, set := os.LookupEnv("HS2_HALF_CLOSE_MAX"); set {
		t.Skip("HS2_HALF_CLOSE_MAX set")
	}
	if got := halfCloseMax.Load(); got != halfCloseMaxDefault || halfCloseMaxDefault < 16384 {
		t.Fatalf("halfCloseMax %d (default %d), want 16384", got, halfCloseMaxDefault)
	}
}
