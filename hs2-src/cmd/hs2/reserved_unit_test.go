package main

import "testing"

// rev53a: a tunnel named after one of hs2's helper units (hs2-stats.service is
// rewritten by the history sampler, hs2-alert-<u> by the alert timer, …) was
// accepted, and turning that helper on overwrote the tunnel's unit file.
func TestUnitNameRejectsHelperUnits(t *testing.T) {
	for _, n := range []string{"hs2-stats", "hs2-lb-park", "hs2-menu", "hs2-coreguard", "hs2-alert-hs2", "hs2-alert-de1"} {
		if err := unitNameErr(n); err == nil {
			t.Errorf("%s: accepted, but it is a helper unit name", n)
		}
	}
	for _, n := range []string{"hs2", "hs2-de1", "hs2-stats2", "hs2-alerts", "hs2-lb"} {
		if err := unitNameErr(n); err != nil {
			t.Errorf("%s: rejected: %v", n, err)
		}
	}
}
