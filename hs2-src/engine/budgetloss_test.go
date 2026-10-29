package engine

import (
	"testing"
	"time"
)

// R1 round 5 (budget.go, BudgetLossPpm): a BULK socket that resends a
// share of what it sends at least BudgetLossPpm over the window lifts its
// budget (B 0: the gate open, no pacing of its own) and takes it back
// budgetLossCalm after the resends stop; a socket that resends nothing (a
// link at its target on fq_codel) or a stray few never does; with the
// switch at 0 nothing changes.
func TestBudgetLossLift(t *testing.T) {
	prev := BudgetLossPpm
	t.Cleanup(func() { BudgetLossPpm = prev })
	run := func(ppm int) (lifted, back bool, bs []int64) {
		BudgetLossPpm = ppm
		lb := newLinkBudget(func() (int, error) { return 0, nil })
		var ts tcpStat
		ts.rtt, ts.minRTT, ts.deliveryRate, ts.sndMSS = 76000, 75000, 10e6, 1448
		now := time.Duration(0)
		step := func(n int, segs, resent uint32) {
			for i := 0; i < n; i++ {
				now += 100 * time.Millisecond
				ts.segsOut += segs / 10
				if i%10 == 0 {
					ts.retrans += uint64(resent)
				}
				ts.bytesAcked += uint64(segs/10) * 1448
				lb.step(now, true, ts, 10e6, nil)
				bs = append(bs, lb.b.Load())
			}
		}
		step(100, 7000, 0) // 10 s clean: the budget runs
		if lb.b.Load() <= 0 {
			t.Fatalf("ppm %d: no budget on a clean link", ppm)
		}
		step(150, 7000, 7) // 0.1 % resent for 15 s
		lifted = lb.loss.on && lb.b.Load() == 0
		step(400, 7000, 0) // 40 s clean again
		back = !lb.loss.on && lb.b.Load() > 0
		return
	}
	if lifted, back, _ := run(200); !lifted || !back {
		t.Fatalf("0.1 %% random loss: lifted %v, back after the calm %v", lifted, back)
	}
	if lifted, _, _ := run(0); lifted {
		t.Fatal("lifted with the switch off")
	}
	// a stray resend a window (0.0014 %) is not loss
	BudgetLossPpm = 200
	lb := newLinkBudget(func() (int, error) { return 0, nil })
	var ts tcpStat
	ts.rtt, ts.minRTT, ts.deliveryRate, ts.sndMSS = 76000, 75000, 10e6, 1448
	for i := 0; i < 300; i++ {
		ts.segsOut += 700
		if i%100 == 50 {
			ts.retrans++
		}
		ts.bytesAcked += 700 * 1448
		lb.step(time.Duration(i+1)*100*time.Millisecond, true, ts, 10e6, nil)
	}
	if lb.loss.on || lb.loss.ons != 0 {
		t.Fatal("a stray resend lifted the budget")
	}
}
