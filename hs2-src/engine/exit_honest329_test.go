package engine

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// From b329 lb_swap329_test.go (HS2_LB_SWAP itself is not in this build):
// the rig the SPAREPATH edge tests use, and the exit honesty test.

// swapRig: a reverse edge with five paths of equal reported share (6 of the
// 30 serving links each). a>x is the recovered path: 2 serving + 5 healthy
// retiring links (former cut-drain, flag cleared); b>x and c>x hold 9
// serving each (3 over); d>x and e>x 5 each (1 under, nothing retiring).
type swapRig struct {
	m     *LinkManager
	clk   *v2Clock
	lg    *v2Log
	links map[string][]*managedLink
	open  map[*managedLink]int
	// cleanup: run after a subtest (global keys a case changed)
	cleanup []func()
}

func newSwapRig(t *testing.T) *swapRig {
	t.Helper()
	m, clk, lg := newV2Manager(nil, 1, 128, true)
	r := &swapRig{m: m, clk: clk, lg: lg, links: map[string][]*managedLink{}, open: map[*managedLink]int{}}
	add := func(p string, n int, retiring bool) {
		for i := 0; i < n; i++ {
			ml := cutLink(m, p, 10+i)
			m.mu.Lock()
			ml.retiring = retiring
			if retiring {
				ml.retireSince = m.now()
			}
			m.mu.Unlock()
			r.links[p] = append(r.links[p], ml)
			r.open[ml] = ml.open
		}
	}
	add("a>x", 2, false)
	add("a>x", 5, true)
	add("b>x", 9, false)
	add("c>x", 9, false)
	add("d>x", 5, false)
	add("e>x", 5, false)
	st := newSteerState()
	st.active = true
	st.repShare = map[string]float64{"a>x": 20, "b>x": 20, "c>x": 20, "d>x": 20, "e>x": 20}
	st.repFactor = map[string]float64{"a>x": 1, "b>x": 1, "c>x": 1, "d>x": 1, "e>x": 1}
	st.repCut = map[string]bool{}
	st.repLoss = map[string]float64{}
	m.mu.Lock()
	m.steer = st
	m.mu.Unlock()
	return r
}

// Fix C: an ask the pool started no dial for is not booked.
func TestSpareForGapNotStarted(t *testing.T) {
	old := lbSpareEdge
	lbSpareEdge = true
	t.Cleanup(func() { lbSpareEdge = old })
	b := spare176LB(t)
	var mu sync.Mutex
	var lines []string
	b.log = func(f string, a ...any) { mu.Lock(); lines = append(lines, fmt.Sprintf(f, a...)); mu.Unlock() }
	b.mu.Lock()
	b.links = []int{10, 22, 22, 23}
	b.mu.Unlock()
	b.setSpare(func(n int) int { return 0 }) // at the slot cap
	b.setSpareWant(func() int { return 64 })
	if b.spareForGap(b.now()) {
		t.Fatal("spareForGap reported an ask the pool did not start")
	}
	b.mu.Lock()
	nd, ng := len(b.drained), len(b.gapBack)
	b.mu.Unlock()
	if nd != 0 || ng != 0 {
		t.Fatalf("not started but booked: drained %d, gapBack %d", nd, ng)
	}
	mu.Lock()
	all := strings.Join(lines, "\n")
	mu.Unlock()
	if !strings.Contains(all, "started no spare dial") || strings.Contains(all, "spare dial asked") {
		t.Fatalf("log:\n%s", all)
	}
	// started: booked as before
	b.setSpare(func(n int) int { return n })
	if !b.spareForGap(b.now()) {
		t.Fatal("no ask")
	}
	b.mu.Lock()
	nd, ng = len(b.drained), len(b.gapBack)
	b.mu.Unlock()
	if nd != 1 || ng != 1 {
		t.Fatalf("started: drained %d, gapBack %d; want 1, 1", nd, ng)
	}
}
