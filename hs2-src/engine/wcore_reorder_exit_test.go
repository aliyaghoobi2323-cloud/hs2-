package engine

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// TestWcoreReoExitReport (report, HS2_WCORE_RED=1): reordering on the
// exit's side (its core for uploads, the lab knob WcoreUpload), which has no
// record of the sender: the field's reordering and a steady 0.2% held 2 ms,
// with the RACK sender, at 72, 150 and 200 ms, base against the core.
func TestWcoreReoExitReport(t *testing.T) {
	if os.Getenv("HS2_WCORE_RED") != "1" {
		t.Skip("HS2_WCORE_RED=1")
	}
	up := WcoreUpload
	WcoreUpload = 1
	t.Cleanup(func() { WcoreUpload = up })
	for _, rtt := range []int{72, 150, 200} {
		for _, ro := range []string{"clean", "field", "ro0.2%/2ms"} {
			var b, k float64
			for seed := uint64(1); seed <= 3; seed++ {
				c := wmPath(1000, wmMS(rtt), wmMS(50))
				c.exit, c.rack, c.seed, c.dur = true, true, seed, 40*time.Second
				switch ro {
				case "field":
					c.reoField = true
				case "ro0.2%/2ms":
					c.reorder, c.reorderMs = 0.002, 2
				}
				rb, rk := wmRun(c.with(wmBase)), wmRun(c.with(wmCore))
				b += rb.mbit / 3
				k += rk.mbit / 3
				t.Logf("up 1000/%d q50 %-10s s%d: base %6.1f Mbit rs %.3f%% | core %6.1f Mbit rs %.3f%% d%v cut%d p%v B%.1f",
					rtt, ro, seed, rb.mbit, 100*rb.resend, rk.mbit, 100*rk.resend, rk.drains, rk.cuts, rk.probes, float64(rk.peakB)/(1<<20))
			}
			t.Logf("%s", fmt.Sprintf("up 1000/%d q50 %-10s mean base %6.1f core %6.1f Mbit", rtt, ro, b, k))
		}
	}
}
