package udpcarrier

import (
	"testing"
	"time"
)

// pacerBurstCap gives a stage-limited, backlogged carrier a larger token-bucket
// burst when the host is saturated (pacerSatCredit) than when it is not
// (pacerLateCredit), and neither when the carrier is idle or not stage-limited.
// This is what lets a carrier whose send goroutine the scheduler put aside for
// tens of ms drain its backlog in the slice it gets, on an asymmetrically
// starved server — the field's 12-27% loss that plain pacing left on the table.
func TestPacerBurstCapHostSaturated(t *testing.T) {
	const rate = 2e6 // bytes/s
	const lenB = 1200
	quantum := rate * pacerQuantum.Seconds()
	late := rate * pacerLateCredit.Seconds()
	sat := rate * pacerSatCredit.Seconds()
	if !(pacerSatCredit > pacerLateCredit && pacerLateCredit > pacerQuantum) {
		t.Fatalf("constants out of order: quantum %v, late %v, sat %v", pacerQuantum, pacerLateCredit, pacerSatCredit)
	}
	cases := []struct {
		name                           string
		waiting, stageLimited, hostSat bool
		want                           float64
	}{
		{"idle, not stage-limited", false, false, false, quantum},
		{"idle even when saturated", false, true, true, quantum},     // no backlog: no flood
		{"backlog, not stage-limited", true, false, true, quantum},   // path-bound: unchanged
		{"backlog, stage-limited, room", true, true, false, late},    // the 10 ms late credit
		{"backlog, stage-limited, saturated", true, true, true, sat}, // the larger catch-up
	}
	for _, c := range cases {
		got := pacerBurstCap(rate, lenB, c.waiting, c.stageLimited, c.hostSat)
		if got != c.want {
			t.Errorf("%s: burst cap %.0f, want %.0f", c.name, got, c.want)
		}
	}
	// A stage-limited, saturated carrier may hold ~5x the room-host credit, so a
	// 50 ms scheduler gap is recovered where the 10 ms credit threw 40 ms away.
	gap := pacerBurstCap(rate, lenB, true, true, true) / pacerBurstCap(rate, lenB, true, true, false)
	if gap < 4 {
		t.Fatalf("saturated catch-up only %.1fx the room-host credit, want >= 4", gap)
	}
	_ = time.Millisecond
}

// SetHostSaturated flips the package flag the pacer reads.
func TestSetHostSaturated(t *testing.T) {
	t.Cleanup(func() { SetHostSaturated(false) })
	SetHostSaturated(true)
	if !hostSaturated.Load() {
		t.Fatal("SetHostSaturated(true) did not set the flag")
	}
	SetHostSaturated(false)
	if hostSaturated.Load() {
		t.Fatal("SetHostSaturated(false) did not clear the flag")
	}
}
