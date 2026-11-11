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
	defer func(c, g time.Duration) { lbCooldown, lbStartGrace = c, g }(lbCooldown, lbStartGrace)
	lbCooldown, lbStartGrace = 4*time.Second, 0 // a death, not a startup race
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
	defer func(g time.Duration) { lbStartGrace = g }(lbStartGrace)
	lbStartGrace = 0 // dead, not a startup race
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

// The initial links spread by weight from the first dial, before the peer's
// capLB is known (dialing it is not gated on capLB): waiting put every initial
// link on the primary and the extra path could stay empty for good.
func TestLBInitialSpreadBeforePeerCap(t *testing.T) {
	paths := []LBPath{
		{Local: "", Remote: "10.1.0.1", Weight: 50, Primary: true},
		{Local: "", Remote: "10.1.0.2", Weight: 50},
	}
	lb, _ := NewLB(paths, nil)
	lb.setPeerState(func() (bool, bool) { return false, false }) // no kindInfo answer yet
	for i := 0; i < 8; i++ {
		if _, err := lb.dialVia(func(p LBPath) (any, error) { return new(int), nil }); err != nil {
			t.Fatal(err)
		}
	}
	if st := lb.Stats(); st[0].Links != 4 || st[1].Links != 4 {
		t.Fatalf("initial links %d/%d, want 4/4", st[0].Links, st[1].Links)
	}
}

// An older peer does not listen on the extra IP: the dials there fail, the
// path is cooled after lbFailMax failures past the startup grace, and every
// link then stays on the primary; the status says why.
func TestLBOldPeerExtraPathCooled(t *testing.T) {
	paths := []LBPath{
		{Local: "", Remote: "10.1.0.1", Weight: 50, Primary: true},
		{Local: "", Remote: "10.1.0.2", Weight: 50},
	}
	lb, _ := NewLB(paths, nil)
	clock := time.Unix(1e9, 0)
	lb.now = func() time.Time { return clock }
	lb.setPeerState(func() (bool, bool) { return true, false }) // an old peer: no capLB
	refused := errors.New("connection refused")
	extra := 0
	dial := func(n int) {
		for i := 0; i < n; i++ {
			lb.dialVia(func(p LBPath) (any, error) {
				if p.Remote != "10.1.0.1" {
					extra++
					return nil, refused
				}
				return new(int), nil
			})
		}
	}
	dial(10) // within the grace: retried, never cooled
	if st := lb.Stats(); st[1].State != "ok" || st[1].Fails < lbFailMax || st[0].Links != 10-extra {
		t.Fatalf("within the grace: extra path %q after %d fails, links %d/%d", st[1].State, st[1].Fails, st[0].Links, st[1].Links)
	}
	clock = clock.Add(lbStartGrace)
	extra = 0
	dial(20)
	st := lb.Stats()
	if extra != 1 { // its failures so far count now: the next one cools it
		t.Fatalf("%d dials to the extra IP past the grace, want 1 then cooled", extra)
	}
	if st[1].Links != 0 {
		t.Fatalf("links %d/%d", st[0].Links, st[1].Links)
	}
	if st[1].State != "no-peer" || st[1].Target != 0 {
		t.Fatalf("extra path state %q target %d", st[1].State, st[1].Target)
	}
}

// Reverse startup race: the extra address refuses its first dials (the peer
// is not listening on it yet) while the primary already works. It must not be
// cooled for that — a 50/50 pair fills 5/5, not 10/0 (the lab's 7/2) — and
// before any dial has succeeded (the peer down) no path is cooled either.
func TestLBStartupRaceNoCooling(t *testing.T) {
	lb, _ := NewLB(twoPaths(50, 50), nil)
	clock := time.Unix(1e9, 0)
	lb.now = func() time.Time { return clock }
	refused := errors.New("connection refused")
	peerDown, extraDown := 4, 6 // failed dials: everything, then the extra address only
	held := 0
	for tries := 0; held < 10 && tries < 100; tries++ {
		clock = clock.Add(500 * time.Millisecond) // the slots' backoff; well inside the grace
		_, err := lb.dialVia(func(p LBPath) (any, error) {
			switch {
			case peerDown > 0:
				peerDown--
				return nil, refused
			case !p.Primary && extraDown > 0:
				extraDown--
				return nil, refused
			}
			return new(int), nil
		})
		if err == nil {
			held++
		}
	}
	st := lb.Stats()
	if st[0].Links != 5 || st[1].Links != 5 {
		t.Fatalf("startup race: %d/%d links (extra path %s, %d fails in all), want 5/5", st[0].Links, st[1].Links, st[1].State, st[1].FailsAll)
	}
	if st[1].State != "ok" || st[1].FailsAll < uint64(lbFailMax) {
		t.Fatalf("extra path %q, %d fails in all", st[1].State, st[1].FailsAll)
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

// Phase 4: a path that fails fast during the start grace (a reality-only IP
// whose target is down: EOF right after the hello) backs off exponentially
// instead of taking ~600 dials in 30 s, is still not cooled, and the pool
// keeps growing on the other paths.
func TestLBStartGraceFastFailBackoff(t *testing.T) {
	paths := append(twoPaths(40, 30), LBPath{Local: "10.0.0.3", Remote: "10.1.0.1", Addr: "10.1.0.1:443", Weight: 30})
	lb, _ := NewLB(paths, nil)
	clock := time.Unix(1e9, 0)
	lb.now = func() time.Time { return clock }
	bad := 0
	var held []any
	for step := 0; step < 3000; step++ { // 30 s in 10 ms steps, one dial each
		clock = clock.Add(10 * time.Millisecond)
		v, err := lb.dialVia(func(p LBPath) (any, error) {
			if p.Local == "10.0.0.3" {
				bad++
				return nil, errors.New("EOF")
			}
			return new(int), nil
		})
		if err == nil {
			held = append(held, v)
			if len(held) > 200 { // keep the pool below its share pressure
				lb.release(held[0])
				held = held[1:]
			}
		}
	}
	if bad > 25 {
		t.Fatalf("%d dials to the failing path in the 30 s grace, want <= 25", bad)
	}
	if bad < lbGraceFreeFails {
		t.Fatalf("only %d dials to the failing path", bad)
	}
	st := lb.Stats()
	if st[2].State != "ok" {
		t.Fatalf("failing path %q within the grace, want ok (not cooled)", st[2].State)
	}
	if st[0].Links+st[1].Links < 190 {
		t.Fatalf("pool %d/%d did not grow on the good paths", st[0].Links, st[1].Links)
	}
}

// rel47: both sides starting, the dialer ~1 s ahead. Every address refuses
// until the peer listens; those failures must not put the non-primary paths
// into the start back-off, or the redial burst lands every start link on the
// primary (lab: [100,0,2] at 6 s, the primary overloaded and cut).
func TestLBPeerNotUpYetNoBackoff(t *testing.T) {
	paths := append(twoPaths(34, 33), LBPath{Local: "10.0.0.3", Remote: "10.1.0.3", Addr: "10.1.0.3:443", Weight: 33})
	lb, _ := NewLB(paths, nil)
	clock := time.Unix(1e9, 0)
	lb.now = func() time.Time { return clock }
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}
	// the peer is not listening yet: the start burst (45 dials in flight at
	// once, as the pool dials them, so they spread over the paths) is refused
	// and then the one retrying slot's dials, ~1 s in all
	for k := 0; k < 20; k++ {
		clock = clock.Add(50 * time.Millisecond)
		var in []int
		burst := 1
		if k == 0 {
			burst = 45
		}
		lb.mu.Lock()
		for c := 0; c < burst; c++ {
			i, _ := lb.acquireLocked()
			in = append(in, i)
		}
		lb.mu.Unlock()
		for _, i := range in {
			lb.result(i, false, nil, refused)
		}
	}
	if st := lb.Stats(); st[1].Fails <= lbGraceFreeFails || st[2].Fails <= lbGraceFreeFails {
		t.Fatalf("setup: the refused dials did not spread: %+v", st)
	}
	n := map[string]int{}
	for k := 0; k < 100; k++ { // the peer is up: the redial burst, 100 links in ~1 s
		clock = clock.Add(10 * time.Millisecond)
		lb.dialVia(func(p LBPath) (any, error) { n[p.Local]++; return new(int), nil })
	}
	for _, l := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"} {
		if n[l] < 25 {
			t.Fatalf("start links per path %v: %s got %d of 100 (back-off from before the peer listened)", n, l, n[l])
		}
	}
	// a path that fails AFTER the peer is reachable still backs off
	bad := 0
	for k := 0; k < 300; k++ {
		clock = clock.Add(10 * time.Millisecond)
		lb.dialVia(func(p LBPath) (any, error) {
			if p.Local == "10.0.0.3" {
				bad++
				return nil, errors.New("EOF")
			}
			return new(int), nil
		})
	}
	if bad > 15 {
		t.Fatalf("%d dials to a failing path in 3 s with the peer up, want the start back-off (<= 15)", bad)
	}
}

// rel51: a whole-peer outage past the start (every link gone, every path
// refusing for a few seconds) cools no path: once the peer is back the
// rebuilt pool spreads over all paths, not onto the primary alone.
func TestLBPeerOutageNoCool(t *testing.T) {
	paths := append(twoPaths(34, 33), LBPath{Local: "10.0.0.3", Remote: "10.1.0.3", Addr: "10.1.0.3:443", Weight: 33})
	lb, _ := NewLB(paths, nil)
	clock := time.Unix(1e9, 0)
	lb.now = func() time.Time { return clock }
	var links []any
	for k := 0; k < 90; k++ {
		l, _ := lb.dialVia(func(p LBPath) (any, error) { return new(int), nil })
		links = append(links, l)
	}
	clock = clock.Add(lbStartGrace + time.Second) // past the start grace
	for _, l := range links {                     // the peer goes down: every link ends
		lb.release(l)
	}
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}
	for k := 0; k < 6; k++ { // 3 s of refused redial bursts (the pool dials in parallel)
		clock = clock.Add(500 * time.Millisecond)
		var in []int
		lb.mu.Lock()
		for c := 0; c < 30; c++ {
			i, _ := lb.acquireLocked()
			in = append(in, i)
		}
		lb.mu.Unlock()
		for _, i := range in {
			lb.result(i, false, nil, refused)
		}
	}
	for i, st := range lb.Stats() {
		if st.State == "cooling" || st.State == "probing" {
			t.Fatalf("path %d cooled by a whole-peer outage: %+v", i, lb.Stats())
		}
	}
	n := map[string]int{}
	for k := 0; k < 90; k++ { // the peer is back
		clock = clock.Add(10 * time.Millisecond)
		lb.dialVia(func(p LBPath) (any, error) { n[p.Local]++; return new(int), nil })
	}
	for _, l := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"} {
		if n[l] < 20 {
			t.Fatalf("links per path after the outage %v: %s got %d of 90", n, l, n[l])
		}
	}
	// a path that fails while the others carry links is still cooled
	bad := 0
	for k := 0; k < 30; k++ {
		clock = clock.Add(10 * time.Millisecond)
		lb.dialVia(func(p LBPath) (any, error) {
			if p.Local == "10.0.0.3" {
				bad++
				return nil, errors.New("EOF")
			}
			return new(int), nil
		})
	}
	if st := lb.Stats(); st[2].State != "cooling" {
		t.Fatalf("a failing path with the peer up was not cooled (%d dials): %+v", bad, st)
	}
}
