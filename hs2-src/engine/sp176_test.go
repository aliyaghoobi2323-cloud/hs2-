package engine

import (
	"math/rand/v2"
	"testing"
	"time"
)

// apFlap drives the autopilot with active connections wandering around the
// 21 × 8 line (as the main tunnel at night: 146–171 active) and counts
// pattern changes over an hour.
func apFlap(t *testing.T, hyst bool) (changes int, minT int) {
	t.Helper()
	old := apShrinkHyst
	apShrinkHyst = hyst
	t.Cleanup(func() { apShrinkHyst = old })
	a := newAutopilot(2, 200, 8)
	a.lane = 0
	a.T = 22
	rnd := rand.New(rand.NewPCG(1, 76))
	now := time.Unix(1_700_000_000, 0)
	fl := 160.0
	prev := a.T
	minT = a.T
	for i := 0; i < int(time.Hour/healthTick); i++ {
		now = now.Add(healthTick)
		fl += (160-fl)*0.05 + rnd.NormFloat64()*3
		f := int(fl)
		var ls []apLink
		for k := 0; k < a.T; k++ {
			ls = append(ls, apLink{id: k, serving: true, flowing: f / a.T, open: 2641 / a.T, rate: 1e5, rate10: 1e5})
		}
		d := a.decide(apSample{now: now, links: ls, G: 25e6, flowing: f, open: 2641, growable: true})
		if d.target != prev {
			changes++
			prev = d.target
		}
		minT = min(minT, d.target)
	}
	return changes, minT
}

func TestAPShrinkHystStopsBoundaryFlap(t *testing.T) {
	off, _ := apFlap(t, false)
	on, minT := apFlap(t, true)
	t.Logf("pattern changes in an hour: key off %d, key on %d (lowest T %d)", off, on, minT)
	if off < 10 {
		t.Fatalf("the replay does not flap without the key (%d changes): not the production case", off)
	}
	if on > off/4 {
		t.Fatalf("with the key %d changes (off %d), want at most a quarter", on, off)
	}
	if minT < 20 {
		t.Fatalf("pattern fell to %d with ~160 active (per_link 8)", minT)
	}
}

// A grow is unchanged: 200 active connections get 25 links at once with the
// key on.
func TestAPShrinkHystGrowsAtOnce(t *testing.T) {
	old := apShrinkHyst
	apShrinkHyst = true
	t.Cleanup(func() { apShrinkHyst = old })
	a := newAutopilot(2, 200, 8)
	a.lane = 0
	a.T = 21
	now := time.Unix(1_700_000_000, 0)
	for i := 0; i < 5; i++ {
		now = now.Add(healthTick)
		ls := make([]apLink, a.T)
		for k := range ls {
			ls[k] = apLink{id: k, serving: true, flowing: 9, rate: 1e5}
		}
		a.decide(apSample{now: now, links: ls, G: 25e6, flowing: 200, open: 2000, growable: true})
	}
	if a.T != 25 {
		t.Fatalf("T=%d after 200 active for 5 ticks, want 25", a.T)
	}
}

// The main tunnel (sp176): 77 links up, the edge serving 64 (13 retiring, held
// by their idle user connections). A path at its share of the 64 the edge
// serves is "2.1 links under" its share of all 77, and every minute a spare
// was asked for it that the edge then refused. With HS2_LB_SPARE_EDGE no spare
// is asked; a path really under its share of the serving links still gets one.
func TestLBSpareEdgeFrame(t *testing.T) {
	for _, tc := range []struct {
		name   string
		key    bool
		target int
		want   bool
	}{
		{"key off: asks (old behaviour)", false, 64, true},
		{"edge serves 64: no spare", true, 64, false},
		{"target unknown: shares of all links", true, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := lbSpareEdge
			lbSpareEdge = tc.key
			t.Cleanup(func() { lbSpareEdge = old })
			b := spare176LB(t)
			var asked int
			b.setSpare(func(n int) { asked += n })
			tgt := tc.target
			b.setSpareWant(func() int { return tgt })
			got := b.spareForGap(b.now())
			if got != tc.want || (asked > 0) != tc.want {
				t.Fatalf("spareForGap=%v asked=%d, want %v", got, asked, tc.want)
			}
		})
	}
}

// spare176LB: 4 equal paths to 4 edge addresses, 77 links. A has 17: one
// over its share of the 64 the edge serves (16), 2.25 under its share of all
// 77 (19.25); B/C/D hold the 13 retiring links on top of their serving ones.
func spare176LB(t *testing.T) *LB {
	t.Helper()
	withAuto2(t)
	var ps []LBPath
	for i, r := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4"} {
		ps = append(ps, LBPath{Local: "10.1.0.1", Remote: r, Addr: r + ":2082", Weight: 25, Primary: i == 0})
	}
	b, err := NewLB(ps, nil)
	if err != nil {
		t.Fatal(err)
	}
	b.SetAuto(true)
	b.mu.Lock()
	b.links = []int{17, 19, 20, 21}
	b.upAt = b.now()
	b.mu.Unlock()
	return b
}

// The backoff: a path whose spare did not stay waits 1, 2, 4 … minutes.
func TestLBSpareGapBackoff(t *testing.T) {
	old := lbSpareEdge
	lbSpareEdge = true
	t.Cleanup(func() { lbSpareEdge = old })
	b := spare176LB(t)
	b.mu.Lock()
	b.links = []int{10, 22, 22, 23} // A really under: 10 of 77, share 19
	b.mu.Unlock()
	var asked int
	b.setSpare(func(n int) { asked++ })
	b.setSpareWant(func() int { return 64 })
	now := b.now()
	var at []int
	for m := 0; m < 20; m++ {
		b.mu.Lock()
		b.drained = nil
		b.mu.Unlock()
		if b.spareForGap(now.Add(time.Duration(m) * time.Minute)) {
			at = append(at, m)
		}
	}
	// minute 0, 1 (first miss), then waits 1, 2, 4, 8: 0 1 2 4 8 16
	want := []int{0, 1, 2, 4, 8, 16}
	if len(at) != len(want) {
		t.Fatalf("asks at minutes %v, want %v", at, want)
	}
	for i := range want {
		if at[i] != want[i] {
			t.Fatalf("asks at minutes %v, want %v", at, want)
		}
	}
	// The share moves (A gains a link): asks resume at once.
	b.mu.Lock()
	b.links[0] = 11
	b.drained = nil
	b.mu.Unlock()
	if !b.spareForGap(now.Add(17 * time.Minute)) {
		t.Fatal("no ask after the path gained a link")
	}
}
