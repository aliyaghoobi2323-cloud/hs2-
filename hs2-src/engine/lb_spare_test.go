package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// busyLB is an auto2 balancer over the first paths, with n links held on it
// whose retire never succeeds (every link carries users), placed by acquire
// as the dials would be.
func busyLB(t *testing.T, paths []LBPath, n int) *LB {
	t.Helper()
	withAuto2(t)
	b, err := NewLB(paths, nil)
	if err != nil {
		t.Fatal(err)
	}
	b.SetAuto(true)
	for i := 0; i < n; i++ {
		dialOne(b)
	}
	return b
}

// dialOne is one dial as the pool makes it: acquire, success, hooks.
func dialOne(b *LB) int {
	j, probe := b.acquire()
	k := new(int)
	b.result(j, probe, k, nil)
	b.attach(k, lbHooks{retire: func() bool { return false }}) // users on it: never idle
	return j
}

func linksOn(b *LB, name string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if j := b.indexLocked(name); j >= 0 {
		return b.links[j]
	}
	return -1
}

// The main tunnel: an IPv6 pair added by a reload stayed at 0 links for 10
// minutes while the IPv4 paths ran over their targets — no link was idle, so
// no rebalance retire made room, and no dial meant no placement.
func TestLB2NewPathGetsLinksWithoutIdle(t *testing.T) {
	v4 := []LBPath{
		{Local: "91.107.167.67", Remote: "81.12.35.149", Addr: "81.12.35.149:2082", Weight: 50, Primary: true},
		{Local: "91.107.167.67", Remote: "5.57.38.162", Addr: "5.57.38.162:2082", Weight: 50},
	}
	b := busyLB(t, v4, 40)
	var asked atomic.Int32
	var dials sync.WaitGroup
	b.setSpare(func(n int) { // the pool: each spare dials once, placed by acquire
		for i := 0; i < n; i++ {
			asked.Add(1)
			dials.Add(1)
			go func() { defer dials.Done(); dialOne(b) }()
		}
	})
	v6 := LBPath{Local: "2a01:4f8:1c17:f987::1", Remote: "2a05:9080:0:603:216:3eff:fefe:94d2",
		Addr: "[2a05:9080:0:603:216:3eff:fefe:94d2]:2082", Weight: 8}
	if err := b.SetPaths(append(append([]LBPath(nil), v4...), v6)); err != nil {
		t.Fatal(err)
	}
	// SetPaths asks at once (in its own goroutine); the next tick would too.
	deadline := time.Now().Add(2 * time.Second)
	for asked.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	dials.Wait()
	if got := linksOn(b, v6.Name()); got != 2 {
		t.Fatalf("new path has %d links after the reload, want 2 (spares asked: %d)", got, asked.Load())
	}
	// A tick later nothing more is asked for the minimum.
	if n := b.fillMin(); n != 0 {
		t.Fatalf("still %d short after the spares", n)
	}
	// Under its share (8% of 42 links ≈ 3.2, has 2: not a whole link under —
	// make it bigger): the rebalance, finding no idle link, asks a spare.
	b.mu.Lock()
	b.paths[b.indexLocked(v6.Name())].Weight = 40
	b.mu.Unlock()
	before := asked.Load()
	if b.rebalance2() != 0 {
		t.Fatal("retired a busy link")
	}
	dials.Wait()
	if asked.Load() != before+1 {
		t.Fatalf("rebalance with no idle link asked %d spares, want 1", asked.Load()-before)
	}
	if got := linksOn(b, v6.Name()); got != 3 {
		t.Fatalf("the spare went elsewhere: new path has %d links", got)
	}
}

// Keys off (fixed, or auto without HS2_LB_AUTO2): no spare is ever asked.
func TestLB2SpareOffWithoutKey(t *testing.T) {
	b, _ := NewLB([]LBPath{
		{Local: "k", Remote: "a", Addr: "a:1", Weight: 50, Primary: true},
		{Local: "k", Remote: "b", Addr: "b:1", Weight: 50},
	}, nil)
	b.SetAuto(true)
	for i := 0; i < 10; i++ {
		dialOne(b)
	}
	asked := 0
	b.setSpare(func(n int) { asked += n })
	b.SetPaths([]LBPath{
		{Local: "k", Remote: "a", Addr: "a:1", Weight: 50, Primary: true},
		{Local: "k", Remote: "b", Addr: "b:1", Weight: 50},
		{Local: "k", Remote: "c", Addr: "c:1", Weight: 50},
	})
	time.Sleep(50 * time.Millisecond)
	if b.fillMin() != 0 || asked != 0 {
		t.Fatalf("spares asked with the key off: %d", asked)
	}
}

// The pool side: a spare slot dials once beyond the target instead of
// retiring for being over it, and only lb2SpareMax of them at a time.
func TestExitPoolSpareDialsBeyondTarget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var dialed atomic.Int32
	hold := make(chan struct{})
	p := newExitPool(ctx, 1, 10, func() (dialedLink, error) {
		dialed.Add(1)
		return nopLink{}, nil
	}, nil)
	p.gate = newDialGate(8, func() time.Duration { return 0 })
	p.serve = func(ctx context.Context, car dialedLink) string {
		select {
		case <-hold:
		case <-ctx.Done():
		}
		return ""
	}
	p.setTarget(3)
	spareWait(t, func() bool { return dialed.Load() == 3 })
	if n := p.addSpares(5); n != lb2SpareMax {
		t.Fatalf("started %d spares, want %d (the cap)", n, lb2SpareMax)
	}
	spareWait(t, func() bool { return dialed.Load() == 3+int32(lb2SpareMax) })
	p.mu.Lock()
	slots := len(p.slots)
	p.mu.Unlock()
	if slots != 3+lb2SpareMax {
		t.Fatalf("%d slots, want %d", slots, 3+lb2SpareMax)
	}
	// Links end: the pool is over target, so their slots retire, not redial.
	close(hold)
	spareWait(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.slots) == 3
	})
}

type nopLink struct{}

func (nopLink) Close() error { return nil }

func spareWait(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The reverse edge keeps a spare the balancer dialed for a path short of two
// links, or for an address under its share, instead of closing it as surplus.
func TestEdgeKeepsWantedArrival(t *testing.T) {
	withAuto2(t)
	m, _ := pathLinks("a>x", "a>x", "a>x", "a>x", "b>x", "a>x", "b>x")
	m.shrinkShares = func(ms []*managedLink) ([]string, map[string]int) {
		return revShrinkKeys(ms, reportShares(LBReport{Auto: true, Paths: []LBPeerPath{
			{Local: "x", Remote: "a", EffPct: 50}, {Local: "x", Remote: "b", EffPct: 50}}}))
	}
	all := m.links
	m.links = all[:4] // 4 on a, none on b
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.arrivalWantedLocked(all[4]) {
		t.Fatal("a link for an empty path was not wanted")
	}
	if m.arrivalWantedLocked(all[5]) {
		t.Fatal("a fifth link on a path over its share was wanted")
	}
	m.links = append(all[:4:4], all[4]) // a:4, b:1 — b's second link
	if !m.arrivalWantedLocked(all[6]) {
		t.Fatal("b's second link was not wanted")
	}
	// Key off: never (born spare as before).
	lbAuto2 = false
	if m.arrivalWantedLocked(all[6]) {
		t.Fatal("wanted with the key off")
	}
}
