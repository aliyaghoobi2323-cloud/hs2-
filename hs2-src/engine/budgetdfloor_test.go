package engine

import (
	"testing"
	"time"
)

// TestBudgetDFloor: the spiral of round 5's LX on the budget's core. A
// one-way link delivers 5 Mbit/s and its srtt reads 12 ms over min_rtt (the
// peer's delayed acks on a small flight; no queue of its own): budget-limited,
// it was cut to a few KiB. With BudgetDFloor at 100 it stays at its rate ×
// min_rtt.
func TestBudgetDFloor(t *testing.T) {
	const dlv = 5e6 / 8 // bytes/s
	minRTT := 75 * time.Millisecond
	run := func(k int) float64 {
		withTune(t, BudgetDFloor, k)
		var c budgetCore
		now := 100 * time.Millisecond
		b := 0.0
		for i := 0; i < 400; i++ {
			now += 100 * time.Millisecond
			tx := 0.0
			if b > 0 {
				tx = b / minRTT.Seconds()
			}
			b = c.step(budgetIn{Now: now, On: true, Delivery: dlv, SRTT: minRTT + 12*time.Millisecond, MinRTT: minRTT,
				TxRate: tx, Blocked: true, MSS: 1448, Frame: 1 << 10, DlvMax: dlv})
		}
		return b
	}
	want := dlv * minRTT.Seconds()
	if off := run(0); off > want/2 {
		t.Errorf("off: B %.0f, the spiral this floor is for is gone (rate × min_rtt %.0f)", off, want)
	}
	if on := run(100); on < want || on > 1.1*want {
		t.Errorf("on: B %.0f, want rate × min_rtt %.0f", on, want)
	}
	// a queue of its own over the floor is still cut to it: nothing else changes
	withTune(t, BudgetDFloor, 100)
	var c budgetCore
	now := 100 * time.Millisecond
	b := 0.0
	for i := 0; i < 400; i++ {
		now += 100 * time.Millisecond
		b = c.step(budgetIn{Now: now, On: true, Delivery: 100e6 / 8, SRTT: minRTT + 12*time.Millisecond, MinRTT: minRTT,
			TxRate: 100e6 / 8, Blocked: true, MSS: 1448, Frame: 1 << 10, DlvMax: 100e6 / 8})
		if i == 399 && b > 1.1*100e6/8*minRTT.Seconds() {
			t.Errorf("a real queue at 100 Mbit/s: B %.0f stays over rate × min_rtt", b)
		}
	}
}
