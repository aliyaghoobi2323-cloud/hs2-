package engine

import (
	"os"
	"testing"
)

// rc15: K4M2 (HS2_LITE_HEDGE8) is on by default in the binary. The older
// tests (fake openers, sub counts, call copies) were written for the
// hedge-off engine, so the test binary starts with the hedge keys off unless
// HS2_LITE_HEDGE8 is set in the env; the hedge tests (b455..b469n, rc15)
// switch the keys on themselves. rc15Init holds the package defaults for
// TestRC15HedgeDefaults.
var rc15Init struct{ h, h3, h4, h7, h8, key bool }

func TestMain(m *testing.M) {
	rc15Init.h, rc15Init.h3, rc15Init.h4, rc15Init.h7, rc15Init.h8, rc15Init.key = liteHedge, liteHedge3, liteHedge4, liteHedge7, liteHedge8, liteHedge8Key
	if _, ok := os.LookupEnv("HS2_LITE_HEDGE8"); !ok {
		liteHedge, liteHedge3, liteHedge4, liteHedge7, liteHedge8, liteHedge8Key = false, false, false, false, false, false
	}
	// rc15b: the old hedge tests assume the cumulative heavy rule
	if _, ok := os.LookupEnv("HS2_RC15_HEAVY_KBPS"); !ok {
		rc15HeavyBps = 0
	}
	os.Exit(m.Run())
}
