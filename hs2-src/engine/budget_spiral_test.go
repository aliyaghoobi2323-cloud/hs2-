package engine

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

// spiralLink is a BULK link of the main server's spiral (R1 fix 2) for the
// budget's core: budget-limited at each reading (its users have more to
// send), its srtt min_rtt plus 8 to 12 ms it does not make (the peer's
// delayed acks of a small flight, the way back, other flows' backlog: the
// same whatever B), on a path that would carry pathRate for it. Its delivery
// rate is the budget's own pacing cap (BudgetPaceK × B / (base + T), as
// linkBudget.step sets it), at most the path's; it acks B a round trip.
type spiralLink struct {
	c       budgetCore
	b       float64
	now     time.Duration
	minRTT  time.Duration
	path    float64 // bytes/s
	rng     *rand.Rand
	dlv, tx float64 // the latest reading's
}

func (l *spiralLink) run(n int) {
	T := budgetTarget()
	for i := 0; i < n; i++ {
		l.now += 100 * time.Millisecond
		noise := 8*time.Millisecond + time.Duration(l.rng.Int64N(int64(4*time.Millisecond)))
		srtt := l.minRTT + noise
		l.dlv, l.tx = l.path, l.path
		if l.b > 0 && l.c.base > 0 {
			l.dlv = math.Min(l.path, float64(BudgetPaceK)/100*l.b/(l.c.base+T).Seconds())
			l.tx = math.Min(l.path, l.b/srtt.Seconds())
		}
		l.b = l.c.step(budgetIn{Now: l.now, On: true, Delivery: l.dlv, SRTT: srtt, MinRTT: l.minRTT,
			TxRate: l.tx, Blocked: true, MSS: 1448, Frame: 1 << 10, DlvMax: l.dlv})
	}
}

// TestBudgetSpiralRecovers: the main server's spiral and its way out. With
// the floors of the link's own held rate alone (HS2_TUNE_BUDGET_DFLOOR=0) a
// link whose srtt reads 8 to 12 ms it does not make is cut, reading after
// reading, to two segments (~3 KiB) and ~1 Mbit/s or less, on a path that
// would take 20 Mbit/s for it; the floor of delivery × min_rtt takes the
// same link, from there, back to the path's rate within 30 s, at every
// round trip of the field.
func TestBudgetSpiralRecovers(t *testing.T) {
	const path = 20e6 / 8
	for _, rtt := range []time.Duration{20, 40, 90, 150} {
		minRTT := rtt * time.Millisecond
		withTune(t, BudgetDFloor, 0)
		l := &spiralLink{minRTT: minRTT, path: path, rng: rand.New(rand.NewPCG(1, uint64(rtt)))}
		l.run(600) // 60 s
		t.Logf("min_rtt %v, DFLOOR 0: B %.1f KiB, delivery %.2f Mbit/s, acked %.2f Mbit/s, cuts %d",
			minRTT, l.b/1024, l.dlv*8/1e6, l.tx*8/1e6, l.c.cuts)
		if l.b > 4<<10 || l.tx > 1.6e6/8 {
			t.Errorf("min_rtt %v, DFLOOR 0: B %.1f KiB, acked %.2f Mbit/s: the spiral is gone, this test no longer shows it",
				minRTT, l.b/1024, l.tx*8/1e6)
		}
		withTune(t, BudgetDFloor, 100)
		cuts0 := l.c.cuts
		at := time.Duration(0)
		for i := 0; i < 600 && at == 0; i++ {
			l.run(1)
			if l.dlv >= 0.95*path {
				at = time.Duration(i+1) * 100 * time.Millisecond
			}
		}
		l.run(300)
		t.Logf("min_rtt %v, DFLOOR 100: B %.1f KiB, delivery %.2f Mbit/s, acked %.2f Mbit/s, the path's rate after %v, cuts %d more",
			minRTT, l.b/1024, l.dlv*8/1e6, l.tx*8/1e6, at, l.c.cuts-cuts0)
		if at == 0 || at > 30*time.Second {
			t.Errorf("min_rtt %v, DFLOOR 100: not back to the path's rate in 30 s (after %v)", minRTT, at)
		}
		if want := path * minRTT.Seconds(); l.b < 0.95*want {
			t.Errorf("min_rtt %v, DFLOOR 100: B %.0f under the path's rate × min_rtt %.0f", minRTT, l.b, want)
		}
		// past the floor its queue is still cut: B holds within the path's
		// rate × (min_rtt + the target) and BudgetPaceK
		if hi := float64(BudgetPaceK) / 100 * path * (minRTT + budgetTarget()).Seconds(); l.b > hi {
			t.Errorf("min_rtt %v, DFLOOR 100: B %.0f over %.0f: the noise grew it past the path", minRTT, l.b, hi)
		}
	}
}

// TestTopBudgetRows: the status shows the budget lines of the links with the
// most cuts, wherever they are in the pool, and the cuts of all.
func TestTopBudgetRows(t *testing.T) {
	var rows []budgetRow
	for i := 1; i <= 40; i++ {
		rows = append(rows, budgetRow{id: i, cuts: uint64(i % 7)})
	}
	rows[29].cuts = 12603 // link 30, the main server's
	rows[4].cuts = 900
	top, total := topBudgetRows(rows, coreLinesMax)
	sum := uint64(0)
	for _, r := range rows {
		sum += r.cuts
	}
	if total != sum {
		t.Fatalf("total %d, want %d", total, sum)
	}
	if len(top) != coreLinesMax || top[0].id != 30 || top[1].id != 5 {
		t.Fatalf("top %+v: want link 30, then 5, %d rows", top, coreLinesMax)
	}
	for i := 1; i < len(top); i++ {
		if top[i].cuts > top[i-1].cuts || top[i].cuts == top[i-1].cuts && top[i].id < top[i-1].id {
			t.Fatalf("top not by cuts, then the pool's order: %+v", top)
		}
	}
	if few, _ := topBudgetRows(rows[:3], coreLinesMax); len(few) != 3 {
		t.Fatalf("3 links: %d rows", len(few))
	}
}
