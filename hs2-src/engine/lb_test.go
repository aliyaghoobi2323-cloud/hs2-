package engine

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func twoPaths(wa, wb int) []LBPath {
	return []LBPath{
		{Local: "10.0.0.1", Remote: "10.1.0.1", Addr: "10.1.0.1:443", Weight: wa, Primary: true},
		{Local: "10.0.0.2", Remote: "10.1.0.2", Addr: "10.1.0.2:443", Weight: wb},
	}
}

// lbTestPool is an exit pool whose dials go through an LB, wired exactly as
// runKharejReverse wires it; down[i] makes path i's dials fail.
type lbTestPool struct {
	*testPool
	lb   *LB
	down [2]atomic.Bool
}

func newLBTestPool(t *testing.T, paths []LBPath, min, max int) *lbTestPool {
	t.Helper()
	lb, err := NewLB(paths, nil)
	if err != nil {
		t.Fatal(err)
	}
	ltp := &lbTestPool{testPool: newTestPool(t, min, max), lb: lb}
	inner := ltp.exitPool.dial
	ltp.exitPool.dial = func() (dialedLink, error) {
		v, err := lb.dialVia(func(p LBPath) (any, error) {
			for i, q := range paths {
				if q.Name() == p.Name() && ltp.down[i].Load() {
					return nil, errors.New("connection refused")
				}
			}
			return inner()
		})
		if err != nil {
			return nil, err
		}
		return v.(dialedLink), nil
	}
	serve := ltp.exitPool.serve
	ltp.exitPool.serve = func(ctx context.Context, c dialedLink) string {
		defer lb.release(c)
		return serve(ctx, c)
	}
	return ltp
}

func (l *lbTestPool) counts() (a, b int) {
	st := l.lb.Stats()
	return st[0].Links, st[1].Links
}

func near(got, want int) bool { return got >= want-1 && got <= want+1 }

// 70/30 on two paths: the link counts follow the weights after the fill and
// after the autopilot grows the pool.
func TestLBWeightedFillAndGrowth(t *testing.T) {
	l := newLBTestPool(t, twoPaths(70, 30), 1, 64)
	l.setTarget(10)
	if !waitFor(t, 15*time.Second, func() bool { return l.liveCount() == 10 }) {
		t.Fatalf("pool did not fill: %d", l.liveCount())
	}
	if a, b := l.counts(); !near(a, 7) || !near(b, 3) || a+b != 10 {
		t.Fatalf("after fill: %d/%d, want 7/3 ±1", a, b)
	}
	l.setTarget(30)
	if !waitFor(t, 20*time.Second, func() bool { return l.liveCount() == 30 }) {
		t.Fatalf("pool did not grow: %d", l.liveCount())
	}
	if a, b := l.counts(); !near(a, 21) || !near(b, 9) {
		t.Fatalf("after growth: %d/%d, want 21/9 ±1", a, b)
	}
	// replacements keep the balance: links that end are redialed on the
	// path furthest below its share
	for i := 0; i < 5; i++ {
		l.endOneWith("read: connection reset")
	}
	if !waitFor(t, 20*time.Second, func() bool { return l.liveCount() == 30 }) {
		t.Fatalf("pool did not refill: %d", l.liveCount())
	}
	if a, b := l.counts(); !near(a, 21) || !near(b, 9) {
		t.Fatalf("after replacement: %d/%d, want 21/9 ±1", a, b)
	}
}

// A path whose dials fail is cooled: its share goes to the other path, the
// tunnel never goes down, and after the cooldown a probe brings it back.
func TestLBPathDeathAndReturn(t *testing.T) {
	defer func(c time.Duration) { lbCooldown = c }(lbCooldown)
	lbCooldown = 4 * time.Second
	l := newLBTestPool(t, twoPaths(50, 50), 1, 64)
	l.down[1].Store(true) // the extra path is dead from the start
	l.setTarget(8)
	if !waitFor(t, 20*time.Second, func() bool { return l.liveCount() == 8 }) {
		t.Fatalf("pool did not fill on the live path: %d", l.liveCount())
	}
	if a, b := l.counts(); a != 8 || b != 0 {
		t.Fatalf("dead path: %d/%d, want 8/0", a, b)
	}
	if st := l.lb.Stats(); st[1].State != "cooling" && st[1].State != "probing" {
		t.Fatalf("dead path state %q", st[1].State)
	}
	l.down[1].Store(false) // healthy again
	time.Sleep(lbCooldown) // the next dial after the cooldown is its probe
	l.setTarget(16)        // growth after recovery goes to the path below its share
	if !waitFor(t, 20*time.Second, func() bool { a, b := l.counts(); return l.liveCount() == 16 && near(a, 8) && near(b, 8) }) {
		a, b := l.counts()
		t.Fatalf("after recovery: %d/%d (live %d), want 8/8 ±1", a, b, l.liveCount())
	}
}

// With every extra path dead all links go to the primary.
func TestLBAllExtrasDeadFallBackToPrimary(t *testing.T) {
	paths := append(twoPaths(40, 30), LBPath{Local: "10.0.0.3", Remote: "10.1.0.1", Addr: "10.1.0.1:443", Weight: 30})
	lb, _ := NewLB(paths, nil)
	var held []any
	for i := 0; i < 30; i++ {
		v, err := lb.dialVia(func(p LBPath) (any, error) {
			if !p.Primary {
				return nil, errors.New("timeout")
			}
			return new(int), nil
		})
		if err == nil {
			held = append(held, v)
		}
	}
	st := lb.Stats()
	if st[0].Links != len(held) || st[1].Links != 0 || st[2].Links != 0 {
		t.Fatalf("links %d/%d/%d, held %d", st[0].Links, st[1].Links, st[2].Links, len(held))
	}
	if len(held) < 30-2*lbFailMax {
		t.Fatalf("only %d of 30 dials reached the primary", len(held))
	}
	for _, h := range held {
		lb.release(h)
		lb.release(h) // twice is harmless
	}
	if st := lb.Stats(); st[0].Links != 0 {
		t.Fatalf("release: %d", st[0].Links)
	}
}

// Without the peer's capLB only the primary remote IP is dialed (local
// source IPs are this side's own choice); once it is seen, the rest too.
func TestLBPeerCapabilityGate(t *testing.T) {
	paths := []LBPath{
		{Local: "", Remote: "10.1.0.1", Weight: 50, Primary: true},
		{Local: "", Remote: "10.1.0.2", Weight: 50},
	}
	lb, _ := NewLB(paths, nil)
	var known, has atomic.Bool
	lb.setPeerState(func() (bool, bool) { return known.Load(), has.Load() })
	for i := 0; i < 6; i++ {
		lb.dialVia(func(p LBPath) (any, error) { return new(int), nil })
	}
	if st := lb.Stats(); st[1].Links != 0 || st[1].State != "no-peer" {
		t.Fatalf("old peer got %d links on an extra IP (%s)", st[1].Links, st[1].State)
	}
	known.Store(true) // a peer without the bit: still nothing new
	lb.dialVia(func(p LBPath) (any, error) { return new(int), nil })
	if st := lb.Stats(); st[1].Links != 0 {
		t.Fatal("peer without capLB was dialed on an extra IP")
	}
	has.Store(true)
	for i := 0; i < 7; i++ {
		lb.dialVia(func(p LBPath) (any, error) { return new(int), nil })
	}
	if st := lb.Stats(); st[1].Links != 7 { // 7 primary + 7 new: the extra catches up
		t.Fatalf("capable peer: extra path has %d links, want 7", st[1].Links)
	}
}

// Placement is exact largest remainder: every prefix stays within one link of
// the weights.
func TestLBLargestRemainderPrefix(t *testing.T) {
	paths := []LBPath{{Weight: 50, Primary: true, Remote: "a"}, {Weight: 30, Remote: "b"}, {Weight: 20, Remote: "c"}}
	links := make([]int, 3)
	for n := 1; n <= 100; n++ {
		i := pickLargestRemainder([]int{50, 30, 20}, links, func(int) bool { return true }, 0)
		links[i]++
		for j, p := range paths {
			share := float64(p.Weight*n) / 100
			if d := float64(links[j]) - share; d > 1 || d < -1 {
				t.Fatalf("n=%d path %d: %d links, share %.1f", n, j, links[j], share)
			}
		}
	}
}

// Without lb the kindInfo bytes are unchanged: capLB is only set when asked.
func TestLBCapOnlyWhenEnabled(t *testing.T) {
	off := KharejConfig{}.info(8)
	on := KharejConfig{LB: true}.info(8)
	if off.Caps&capLB != 0 || on.Caps&capLB == 0 || on.Caps&^capLB != off.Caps {
		t.Fatalf("caps off %08b on %08b", off.Caps, on.Caps)
	}
}

func TestMergeListeners(t *testing.T) {
	a, _ := net.Listen("tcp", "127.0.0.1:0")
	b, _ := net.Listen("tcp", "127.0.0.1:0")
	m := MergeListeners(a, b)
	var wg sync.WaitGroup
	got := make(chan string, 2)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 2; i++ {
			c, err := m.Accept()
			if err != nil {
				return
			}
			got <- c.LocalAddr().String()
			c.Close()
		}
	}()
	for _, ln := range []net.Listener{a, b} {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
	}
	wg.Wait()
	seen := map[string]bool{<-got: true, <-got: true}
	if !seen[a.Addr().String()] || !seen[b.Addr().String()] {
		t.Fatalf("accepted %v", seen)
	}
	m.Close()
	if _, err := m.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("after close: %v", err)
	}
}
