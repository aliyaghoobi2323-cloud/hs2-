package engine

// The R1 round-4 judge's switches (docs/architecture/wip/r1/round4-judge.fa.md):
// QuietFew, RTHold, RTShare, LatRR, BudgetDrainMs and HQGSegsMs.

import (
	"testing"
	"time"
)

// withTune sets a TuneInt for the test and puts it back after.
func withTune(t *testing.T, ti *TuneInt, v int) {
	t.Helper()
	old := ti.Get()
	ti.Set(v)
	t.Cleanup(func() { ti.Set(old) })
}

// withTuneInt is withTune for a switch that is a plain int.
func withTuneInt(t *testing.T, p *int, v int) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// TestQuietFewCount: QuietFew is the QUIET count up to quietOneMax links,
// QuietMany past it, and the pool's total holds them next to max_links.
func TestQuietFewCount(t *testing.T) {
	defer func(f, m, n, x int) { QuietFew.Set(f); QuietMany.Set(m); QuietN.Set(n); QuietExtra.Set(x) }(QuietFew.Get(), QuietMany.Get(), QuietN.Get(), QuietExtra.Get())
	QuietN.Set(0)
	QuietExtra.Set(1)
	QuietMany.Set(4)
	withTune(t, QuietLight, 0) // the light link's count: TestQuietCountLight
	for _, c := range []struct{ few, links, want, total int }{
		{1, 1, 1, 2}, {1, 5, 1, 6}, {1, 6, 4, 10}, {2, 1, 2, 3}, {2, 5, 2, 7}, {2, 6, 4, 10}, {2, 300, 4, 304}, {0, 1, 1, 2},
	} {
		QuietFew.Set(c.few)
		if got, tot := QuietCount(c.links), QuietTotal(c.links); got != c.want || tot != c.total {
			t.Fatalf("QuietFew %d, %d links: %d QUIET of %d in all, want %d of %d", c.few, c.links, got, tot, c.want, c.total)
		}
	}
}

// classRun ticks a classifier through the given per-tick bytes (tx, rx)
// and says the realtime allowance after each tick.
func classRun(c *flowClassifier, f *flowStats, at *time.Duration, ticks [][2]int) []int64 {
	var out []int64
	for _, v := range ticks {
		f.Add(true, v[0])
		f.Add(false, v[1])
		*at += classTick
		_, _, confirmed := c.Tick(f, *at)
		out = append(out, rtAllowance(c, confirmed))
	}
	return out
}

// TestRTHoldKeepsCall: a call (the exit's side: it sends the call's
// download, gets its user's upload) that turns to clumps when its link
// gets busy loses its realtime lane without the hold and keeps it with;
// a download and an upload never get it.
func TestRTHoldKeepsCall(t *testing.T) {
	steady := func(n int) [][2]int {
		var v [][2]int
		for range n {
			v = append(v, [2]int{18750, 18750})
		}
		return v
	}
	// held back: its download comes in clumps (a tick of 60 KiB in four,
	// none in the others) and its user's upload every other tick
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
	for _, hold := range []int{0, 20} {
		withTune(t, RTHold, hold)
		var c flowClassifier
		var f flowStats
		var at time.Duration
		a := classRun(&c, &f, &at, steady(30))
		if a[len(a)-1] <= rtLightAllow {
			t.Fatalf("hold %d: a steady call got no steady allowance (%d)", hold, a[len(a)-1])
		}
		b := classRun(&c, &f, &at, clumps(60))
		zero := 0
		for _, v := range b {
			if v == 0 {
				zero++
			}
		}
		t.Logf("hold %d: a call in clumps for 6 s: %d of %d ticks without a realtime allowance", hold, zero, len(b))
		switch {
		case hold == 0 && zero == 0:
			t.Fatalf("without the hold the clumped call kept its lane: the test does not test")
		case hold > 0 && zero != 0:
			t.Fatalf("with the hold the clumped call lost its lane %d ticks of %d", zero, len(b))
		}
	}
	// a download (the exit sends 60 KiB a tick after a request: steady to
	// the old rule while under RTMax) and an upload (the exit gets 100 KB a
	// tick, sends 50 B acks): the hold gives neither a tick more
	for _, k := range []struct {
		name string
		v    [2]int
	}{{"download", [2]int{60 << 10, 0}}, {"upload", [2]int{50, 100000}}} {
		var n [2]int
		for i, hold := range []int{0, 20} {
			withTune(t, RTHold, hold)
			var c flowClassifier
			var f flowStats
			var at time.Duration
			ticks := [][2]int{{0, 500}}
			for range 60 {
				ticks = append(ticks, k.v)
			}
			for _, v := range classRun(&c, &f, &at, ticks) {
				if v > rtLightAllow {
					n[i]++
				}
			}
		}
		t.Logf("the %s: %d ticks of 61 with a realtime allowance, %d with the hold", k.name, n[0], n[1])
		if n[1] != n[0] {
			t.Fatalf("the hold gave the %s %d ticks of realtime, %d without", k.name, n[1], n[0])
		}
	}
}

// latOrder pushes a call user's steady frames, then a web start and a
// game frame, and says where the web start comes out (1-based), with the
// steady bytes before it.
func latOrder(t *testing.T) (pos, before int) {
	s := NewSched[int]()
	for i := range 15 {
		s.Push(i, 2, 1, LaneRTSteady, 4000) // 60 KB of a call's frames (its credit 64 KiB)
	}
	s.Push(100, 3, 2, LaneLatency, 1500) // a web page's first bytes
	s.Push(200, 4, 3, LaneRT, 300)       // a game frame
	for k := 1; ; k++ {
		it, ok := s.NextUrgent()
		if !ok {
			t.Fatalf("the web start never came out")
		}
		switch {
		case k == 1 && it != 200:
			t.Fatalf("the game frame is not first (%d)", it)
		case it == 100:
			return k, before
		case it < 100:
			before += 4000
		}
	}
}

// TestSchedRTShare: with RTShare a latency-lane frame waits behind at most
// the share (and one frame) of steady realtime bytes; without it, behind
// every one queued. The light realtime lane stays first either way.
func TestSchedRTShare(t *testing.T) {
	withTune(t, RTShare, 0)
	_, b0 := latOrder(t)
	withTune(t, RTShare, 8<<10)
	_, b1 := latOrder(t)
	t.Logf("a web start behind %d B of call frames, %d B with RTShare 8 KiB", b0, b1)
	if b0 != 60000 || b1 > 8<<10+4000 {
		t.Fatalf("steady bytes ahead of the web start: %d without the share (want 60000), %d with it", b0, b1)
	}
}

// TestSchedLatRR: a user's burst of new connections' first bytes (its
// whole latency credit) no longer sits ahead of another user's light
// message with LatRR; each flow keeps its order; Stats counts the items.
func TestSchedLatRR(t *testing.T) {
	for _, rr := range []int{0, 1} {
		withTune(t, LatRR, rr)
		s := NewSched[[2]int]()
		for i := range 16 { // 8 flows of two frames: 32000 B, the credit
			s.Push([2]int{1, i}, 1, uint64(10+i%8), LaneLatency, 2000)
		}
		s.Push([2]int{2, 0}, 2, 99, LaneLatency, 200)
		if p := s.Stats().Pending[LaneLatency]; p != 17 {
			t.Fatalf("rr %d: %d latency items pending, want 17", rr, p)
		}
		pos, last := 0, map[uint64]int{}
		for k := 1; ; k++ {
			it, ok := s.NextUrgent()
			if !ok {
				break
			}
			if it[0] == 2 {
				pos = k
				continue
			}
			fl := uint64(10 + it[1]%8)
			if prev, seen := last[fl]; seen && prev > it[1] {
				t.Fatalf("rr %d: flow %d out of order", rr, fl)
			}
			last[fl] = it[1]
		}
		t.Logf("LatRR %d: the other user's light message came out %d of 17", rr, pos)
		if rr == 1 && pos != 2 || rr == 0 && pos != 17 {
			t.Fatalf("LatRR %d: the light message came out %d", rr, pos)
		}
		if p := s.Stats().Pending[LaneLatency]; p != 0 {
			t.Fatalf("rr %d: %d latency items left", rr, p)
		}
	}
}

// TestBudgetDrain: with BudgetDrainMs a budget-limited link in its run
// holds its in-flight at BudgetDrainK of d × min_rtt for two min_rtt about
// each period, and B as without it otherwise (the same readings into a
// core without the drain).
func TestBudgetDrain(t *testing.T) {
	defer func(ms, k int) { BudgetDrainMs, BudgetDrainK = ms, k }(BudgetDrainMs, BudgetDrainK)
	BudgetDrainK = 92
	var on, off budgetCore
	in := budgetIn{On: true, Delivery: 12.5e6, MinRTT: 72 * time.Millisecond,
		TxRate: 12.5e6, Blocked: true, MSS: 1448, Frame: 16 << 10, DlvMax: 12.5e6}
	low := 0
	for i := range 200 { // 20 s
		in.Now = time.Duration(i) * coreTick
		in.SRTT = 74 * time.Millisecond // a queue of 2 ms: B holds and grows slowly
		if i < 5 {
			in.SRTT = 76 * time.Millisecond // past the target: the run begins
		}
		BudgetDrainMs = 1000
		a := on.step(in)
		BudgetDrainMs = 0
		b := off.step(in)
		if a == b {
			continue
		}
		low++
		want := 0.92 * on.d * in.MinRTT.Seconds()
		if a > b || a < want-1 || a > want+1 {
			t.Fatalf("reading %d: B %.0f with the drain, %.0f without, want %.0f while it drains", i, a, b, want)
		}
	}
	t.Logf("drain 1 s: %d readings of 200 drained, drains %d; none without (%d)", low, on.drains, off.drains)
	if on.drains < 12 || on.drains > 28 || low < int(on.drains) || low > 2*int(on.drains) || off.drains != 0 {
		t.Fatalf("%d drains, %d readings drained (without the switch %d drains)", on.drains, low, off.drains)
	}
}

// TestHostGateSegsMs: with HQGSegsMs the segment floor is at most that
// many ms of the rate (at least hqgSegsLow segments): at 1 Mbit/s a light
// frame waits on the host within §3.3's bound and the frame in progress;
// at 10 Mbit/s and up the floor is HQGSegs's as before.
func TestHostGateSegsMs(t *testing.T) {
	defer func(s, g, ms int) { HQGSegs, HQGGate, HQGSegsMs = s, g, ms }(HQGSegs, HQGGate, HQGSegsMs)
	HQGSegs, HQGGate, HQGSegsMs = 8, 1, 30
	for _, c := range []struct {
		mbit float64
		segs int
	}{{1, 3}, {2, 5}, {5, 8}, {10, 8}} {
		g := newHostGate()
		_, cp, _ := g.Update(c.mbit*mbit, 1448)
		if cp != c.segs*1448 {
			t.Fatalf("%.0f Mbit/s: cap %d, want %d segments", c.mbit, cp, c.segs)
		}
	}
	r := 1 * mbit
	worst, _ := hostWait(r, 0)
	bound := time.Duration(float64(3*1448+BulkFrameSize(r)) / r * float64(time.Second))
	t.Logf("1 Mbit/s, SEGS=8 bounded to 30 ms: a light frame waits up to %v on the host (3 segments and a frame: %v)", worst, bound)
	if worst > bound {
		t.Fatalf("the host wait at 1 Mbit/s is %v, past %v", worst, bound)
	}
}
