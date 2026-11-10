package engine

import (
	"context"
	"math"
	"strings"
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

// HS2_LB_FILL_PCT: slots already over the target (links the edge holds
// retiring) no longer block spares; the cap is target+N%, with at most
// lb2SpareMax spares pending at once.
func TestExitPoolFillPctSparesPastRetiring(t *testing.T) {
	old := lbFillPct
	defer func() { lbFillPct = old }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var dialed atomic.Int32
	hold := make(chan struct{})
	p := newExitPool(ctx, 1, 100, func() (dialedLink, error) {
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
	p.setTarget(20)
	spareWait(t, func() bool { return dialed.Load() == 20 })
	p.setTarget(16) // 4 slots now over the target: links the edge holds retiring
	lbFillPct = 0
	if n := p.addSpares(1); n != 0 {
		t.Fatalf("key off: started %d spares past retiring slots, want 0", n)
	}
	lbFillPct = 25 // cap 16+4 = 20: full
	if n := p.addSpares(1); n != 0 {
		t.Fatalf("at the cap: started %d", n)
	}
	lbFillPct = 50 // cap 24
	if n := p.addSpares(5); n != lb2SpareMax {
		t.Fatalf("started %d spares, want %d (pending at most)", n, lb2SpareMax)
	}
	spareWait(t, func() bool { return dialed.Load() == 20+int32(lb2SpareMax) })
	spareWait(t, func() bool { // dialed: no longer pending
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, s := range p.slots {
			if s.spare {
				return false
			}
		}
		return true
	})
	for i := 0; i < 10; i++ {
		p.addSpares(1)
		spareWait(t, func() bool {
			p.mu.Lock()
			defer p.mu.Unlock()
			for _, s := range p.slots {
				if s.spare {
					return false
				}
			}
			return true
		})
	}
	p.mu.Lock()
	slots := len(p.slots)
	p.mu.Unlock()
	if slots != 24 {
		t.Fatalf("%d slots, want the cap 24", slots)
	}
	close(hold) // links end: over target, their slots retire
	spareWait(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.slots) == 16
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

// prodPaths is the main tunnel: 6 Iran IPv4 × 2 kharej IPv4, and one IPv6 pair.
func prodPaths() (ps []LBPath, bad13, mid35 []string, v6 string) {
	kh := []string{"116.202.5.113", "91.107.167.67"}
	bad := []string{"81.12.35.149", "37.32.47.180", "5.160.110.230"}
	mid := []string{"5.57.38.162", "85.133.195.138", "85.133.250.165"}
	for _, ir := range append(append([]string(nil), bad...), mid...) {
		for _, k := range kh {
			ps = append(ps, LBPath{Local: k, Remote: ir, Addr: ir + ":2082", Weight: 7 * 50, Primary: len(ps) == 0})
			if hasStr(bad, ir) {
				bad13 = append(bad13, k+">"+ir)
			} else {
				mid35 = append(mid35, k+">"+ir)
			}
		}
	}
	v6p := LBPath{Local: "2a01:4f8:1c17:f987::1", Remote: "2a05:9080:0:603:216:3eff:fefe:94d2",
		Addr: "[2a05:9080:0:603:216:3eff:fefe:94d2]:2082", Weight: 7 * 100}
	return append(ps, v6p), bad13, mid35, v6p.Name()
}

func prodReadings(b *LB, bad13, mid35 []string, v6 string, lossMid float64) map[int]q2Reading {
	rs := map[int]q2Reading{}
	for j, p := range b.paths {
		switch {
		case hasStr(bad13, p.Name()):
			rs[j] = q2Reading{loss: 0.13}
		case hasStr(mid35, p.Name()):
			rs[j] = q2Reading{loss: lossMid}
		case p.Name() == v6:
			rs[j] = q2Reading{loss: 0.02}
		}
	}
	return rs
}

// The main tunnel at the time of the report: 3 Iran IPs at 13%, 3 at 3.5%,
// IPv6 at 2%. Only the 13% IPs are cut (their IP verdict, not the kharej
// IPs'); the 3.5% ones keep a fair share; IPv6 takes at most half.
func TestLB2ProdMediocreKeptAndCapped(t *testing.T) {
	withAuto2(t)
	ps, bad13, mid35, v6 := prodPaths()
	b, _ := NewLB(ps, nil)
	b.SetAuto(true)
	b.SetSides("Kharej", "Iran")
	now := time.Unix(1000, 0)
	b.now = func() time.Time { return now }
	for i := 0; i < 40; i++ {
		now = now.Add(5 * time.Second)
		b.adapt2(prodReadings(b, bad13, mid35, v6, 0.035))
	}
	sh := lbShares(b.paths, b.mult)
	var sBad, sMid, sV6 float64
	for j, p := range b.paths {
		q := b.judge.p[p.Name()]
		switch {
		case hasStr(bad13, p.Name()):
			sBad += sh[j]
			if !q.cut {
				t.Errorf("%s at 13%% not cut", p.Name())
			}
		case hasStr(mid35, p.Name()):
			sMid += sh[j]
			if q.cut {
				t.Errorf("%s at 3.5%% cut (%s)", p.Name(), q.reason)
			}
		default:
			sV6 += sh[j]
		}
	}
	if sV6 > 100*lb2ShareMax+0.5 {
		t.Errorf("IPv6 takes %.1f%% of the links, cap %.0f%%", sV6, 100*lb2ShareMax)
	}
	if sMid < 30 {
		t.Errorf("the 3.5%% IPs keep only %.1f%%", sMid)
	}
	if sBad > 12 { // six cut paths at the floor (0.1 each)
		t.Errorf("the 13%% IPs keep %.1f%%", sBad)
	}
	for _, v := range b.Verdicts() {
		if strings.HasPrefix(v, "Kharej IP") {
			t.Errorf("confounded kharej verdict: %q", v)
		}
	}
	if v := b.Verdicts(); len(v) != 3 || !strings.HasPrefix(v[0], "Iran IP ") || !strings.HasSuffix(v[0], "lossy on every Kharej IP") {
		t.Errorf("verdicts %q, want the three 13%% Iran IPs", v)
	}
	t.Logf("shares: 13%% IPs %.1f%%, 3.5%% IPs %.1f%%, IPv6 %.1f%%", sBad, sMid, sV6)
}

// A path cut at 13% recovers once it runs at 3.5% beside a 2% best path (the
// recovery bar is relative, like the cut): before, it had to fall under 1.5%
// and the main tunnel's mediocre IPs stayed cut for good.
func TestLB2ProdRecoversRelative(t *testing.T) {
	withAuto2(t)
	ps, bad13, mid35, v6 := prodPaths()
	b, _ := NewLB(ps, nil)
	b.SetAuto(true)
	now := time.Unix(1000, 0)
	b.now = func() time.Time { return now }
	all13 := append(append([]string(nil), bad13...), mid35...)
	for i := 0; i < 10; i++ { // everything IPv4 at 13%: all cut
		now = now.Add(5 * time.Second)
		b.adapt2(prodReadings(b, all13, nil, v6, 0))
	}
	if !b.judge.p[mid35[0]].cut {
		t.Fatal("not cut at 13%")
	}
	start := now
	for i := 0; i < 120 && b.judge.p[mid35[0]].cut; i++ {
		now = now.Add(5 * time.Second)
		b.adapt2(prodReadings(b, bad13, mid35, v6, 0.035))
	}
	if b.judge.p[mid35[0]].cut {
		t.Fatalf("a 3.5%% path still cut %s after it came down from 13%%", now.Sub(start))
	}
	t.Logf("3.5%% path recovered %s after the drop", now.Sub(start))
}

// A cut path whose links all ended gets its two probe links back at once.
func TestLB2CutPathKeepsProbeLinks(t *testing.T) {
	withAuto2(t)
	ps, bad13, mid35, v6 := prodPaths()
	b, _ := NewLB(ps, nil)
	b.SetAuto(true)
	for i := 0; i < 60; i++ {
		dialOne(b)
	}
	now := time.Unix(1000, 0)
	b.now = func() time.Time { return now }
	for i := 0; i < 10; i++ {
		now = now.Add(5 * time.Second)
		b.adapt2(prodReadings(b, bad13, mid35, v6, 0.035))
	}
	// the cut path's links end (released)
	b.mu.Lock()
	cut := b.indexLocked(bad13[0])
	var gone []any
	for k, i := range b.held {
		if i == cut {
			gone = append(gone, k)
		}
	}
	b.mu.Unlock()
	for _, k := range gone {
		b.release(k)
	}
	asked := 0
	b.setSpare(func(n int) { asked = n })
	if n := b.fillMin(); n < 2 || asked < 2 {
		t.Fatalf("cut path at 0 links: %d spares asked", asked)
	}
}

func TestCapWeights(t *testing.T) {
	out := capWeights([]float64{80, 10, 10}, []float64{0.5, 1, 1})
	tot := out[0] + out[1] + out[2]
	if math.Abs(out[0]/tot-0.5) > 1e-6 {
		t.Fatalf("capped share %.3f", out[0]/tot)
	}
	out = capWeights([]float64{80, 0}, []float64{0.5, 1}) // nothing to give it to
	if out[0] != 80 {
		t.Fatalf("lone path changed: %v", out)
	}
}

func TestKeepPathMin(t *testing.T) {
	_, ls := pathLinks("a>x", "a>x", "a>x", "b>x", "b>x")
	m, _ := pathLinks()
	_ = m
	serving := make([]*managedLink, 0)
	mm, _ := pathLinks("a>x", "a>x", "a>x", "b>x", "b>x")
	serving = append(serving, mm.links...)
	_ = ls
	drop := []bool{true, true, false, true, true}
	keepPathMin(serving, drop)
	if drop[0] && drop[1] || drop[3] || drop[4] {
		t.Fatalf("drop %v: a keeps 2, b keeps both", drop)
	}
}

// Peak hour on the main tunnel: the six IPv4 Iran IPs at 4.5–14%, IPv6 at
// 2.3%. The kharej cut the IPv4 paths while the edge, judging by itself, fell
// back to "all paths lossy: shares as set" and sent 4% of the new
// connections to IPv6. Now: not "all lossy" (IPv6 is clearly better), the
// edge steers by the dialer's report (one source of truth), and IPv6 gets
// more than its configured share but at most lb2ShareMax.
func TestLB2PeakBothSidesAgree(t *testing.T) {
	withAuto2(t)
	kh := []string{"116.202.5.113", "91.107.167.67"}
	iran := []string{"81.12.35.149", "37.32.47.180", "5.160.110.230", "5.57.38.162", "85.133.195.138", "85.133.250.165"}
	loss := map[string]float64{"81.12.35.149": 0.14, "37.32.47.180": 0.12, "5.160.110.230": 0.10,
		"5.57.38.162": 0.08, "85.133.195.138": 0.06, "85.133.250.165": 0.045}
	var ps []LBPath
	for _, ir := range iran {
		for _, k := range kh {
			ps = append(ps, LBPath{Local: k, Remote: ir, Addr: ir + ":2082", Weight: 50, Primary: len(ps) == 0})
		}
	}
	v6 := LBPath{Local: "2a01:4f8:1c17:f987::1", Remote: "2a05:9080:0:603:216:3eff:fefe:94d2", Addr: "[x]:2082", Weight: 100}
	ps = append(ps, v6)
	b, _ := NewLB(ps, nil)
	b.SetAuto(true)
	b.SetSides("Kharej", "Iran")
	now := time.Unix(1000, 0)
	b.now = func() time.Time { return now }
	for i := 0; i < 60; i++ {
		now = now.Add(5 * time.Second)
		rs := map[int]q2Reading{}
		for j, p := range b.paths {
			if p.Name() == v6.Name() {
				rs[j] = q2Reading{loss: 0.023}
			} else {
				rs[j] = q2Reading{loss: loss[p.Remote]}
			}
		}
		b.adapt2(rs)
	}
	cfgV6 := 100.0 / float64(12*50+100) * 100 // its configured share, %
	sh := lbShares(b.paths, b.mult)
	v6Share := sh[len(sh)-1]
	for _, s := range b.Stats() {
		if strings.HasPrefix(s.AutoReason, "all paths lossy") {
			t.Fatalf("%s: %q — IPv6 at 2.3%% is clearly better", s.Path, s.AutoReason)
		}
	}
	if v6Share <= cfgV6 || v6Share > 100*lb2ShareMax+0.5 {
		t.Fatalf("kharej: IPv6 share %.1f%%, want over its configured %.1f%% and at most %.0f%%", v6Share, cfgV6, 100*lb2ShareMax)
	}
	seen := map[string]bool{}
	for _, v := range b.Verdicts() {
		if seen[v] || strings.HasPrefix(v, "Kharej IP") {
			t.Fatalf("verdicts %q: duplicated or confounded", b.Verdicts())
		}
		seen[v] = true
	}
	// The edge: two links on every path, keyed as the edge sees them
	// (Iran IP > kharej IP), the dialer's report fresh.
	var keys []string
	for _, p := range b.paths {
		k := p.Remote + ">" + p.Local
		keys = append(keys, k, k)
	}
	m, ls := pathLinks(keys...)
	m.steer = newSteerState()
	rep := b.report()
	rep.At = time.Now()
	m.SteerReport(rep)
	m.mu.Lock()
	m.steerTickLocked(time.Now(), 2*time.Second)
	if !m.steer.active {
		m.mu.Unlock()
		t.Fatal("edge not steering with a fresh report")
	}
	cnt := map[string]int{}
	const n = 20000
	for i := 0; i < n; i++ {
		cnt[m.steerDrawLocked(false, func(*managedLink) bool { return true })]++
	}
	m.mu.Unlock()
	_ = ls
	edgeV6 := 100 * float64(cnt[v6.Remote+">"+v6.Local]) / n
	if math.Abs(edgeV6-v6Share) > 3 {
		t.Fatalf("the sides disagree: kharej gives IPv6 %.1f%%, the edge sends it %.1f%% of new connections", v6Share, edgeV6)
	}
	t.Logf("IPv6: configured %.1f%%, kharej %.1f%%, edge new connections %.1f%%; verdicts %q", cfgV6, v6Share, edgeV6, b.Verdicts())
	// Stale report: no steering (the configured spread), shown as such.
	m.mu.Lock()
	m.steer.rep.At = time.Now().Add(-time.Minute)
	m.steerTickLocked(time.Now(), 2*time.Second)
	active := m.steer.active
	m.mu.Unlock()
	if active {
		t.Fatal("still steering by a stale report")
	}
	if _, r, _ := m.steerRows(); r[keys[0]] != "report stale" {
		t.Fatalf("stale report shown as %q", r[keys[0]])
	}
}

// HS2_LB_SPARE_PER_ROUND (with FILL_PCT): a rebalance with no idle link asks
// up to K spares at once, never more than the path is under its share.
func TestLB2SparePerRound(t *testing.T) {
	old := lbSparePerRound
	lbSparePerRound = 3
	defer func() { lbSparePerRound = old }()
	v4 := []LBPath{
		{Local: "91.107.167.67", Remote: "81.12.35.149", Addr: "81.12.35.149:2082", Weight: 50, Primary: true},
		{Local: "91.107.167.67", Remote: "5.57.38.162", Addr: "5.57.38.162:2082", Weight: 50},
	}
	b := busyLB(t, v4, 40)
	var asked atomic.Int32
	b.setSpare(func(n int) { asked.Add(int32(n)) })
	v6 := LBPath{Local: "2a01:4f8:1c17:f987::1", Remote: "2a05:9080:0:603:216:3eff:fefe:94d2",
		Addr: "[2a05:9080:0:603:216:3eff:fefe:94d2]:2082", Weight: 40}
	if err := b.SetPaths(append(append([]LBPath(nil), v4...), v6)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // SetPaths' own fillMin ask
	before := asked.Load()
	if b.rebalance2() != 0 {
		t.Fatal("retired a busy link")
	}
	if got := asked.Load() - before; got != 3 {
		t.Fatalf("rebalance asked %d spares, want 3", got)
	}
}
