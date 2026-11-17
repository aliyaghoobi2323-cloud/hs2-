package engine

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
)

func setFastPick(t *testing.T, on bool) {
	t.Helper()
	o := lmFastPick.Load()
	lmFastPick.Store(on)
	t.Cleanup(func() { lmFastPick.Store(o) })
}

// fpPool builds a random pool of n mconn links on up to 4 paths: random
// pressure, load, heavy streams, tiers (retiring / degraded / draining /
// suspect), placements and users. steer: an active HS2_LB_STEER report
// with random shares, factors, cut paths and turned-off links.
func fpPool(r *rand.Rand, n int, steer bool) (*LinkManager, []*mcLink) {
	ls := make([]*mcLink, n)
	for i := range ls {
		ls[i] = newMCLink(capPortTags | capMConn)
		ls[i].mtr.path = fmt.Sprintf("10.0.0.%d>10.1.0.1", r.IntN(4))
	}
	m := mcManager(ls...)
	for _, ml := range m.links {
		ml.pressed = r.IntN(4) == 0
		ml.flowing = r.IntN(6)
		ml.heavy = r.IntN(3) * r.IntN(2)
		ml.retiring = r.IntN(6) == 0
		ml.degraded = r.IntN(8) == 0
		ml.draining = r.IntN(10) == 0
		ml.suspect = r.IntN(12) == 0
		ml.picks = r.IntN(4)
		for k := range ml.pickHist {
			ml.pickHist[k] = r.IntN(5)
		}
		ml.users.Store(int32(r.IntN(400)))
		m.users.Add(ml.users.Load())
	}
	if steer {
		st := newSteerState()
		st.active = true
		st.repShare, st.repFactor, st.repCut, st.repLoss = map[string]float64{}, map[string]float64{}, map[string]bool{}, map[string]float64{}
		for p := 0; p < 4; p++ {
			k := fmt.Sprintf("10.0.0.%d>10.1.0.1", p)
			st.repShare[k] = r.Float64() * 40
			st.repFactor[k] = 0.3 + 0.7*r.Float64()
			st.repCut[k] = r.IntN(5) == 0
			st.repLoss[k] = r.Float64() * 0.03
		}
		m.steer = st
		for _, ml := range m.links {
			ml.steer.off = r.IntN(10) == 0
		}
	}
	return m, ls
}

// oldChoices walks every random choice of the old (write-locked) picker
// (pickRandHook: each tie-break both ways, each steering draw over every path
// with weight) without placing: exactly the set of links it can choose from
// this state. rel69h: replaces 300 random samples, which could miss a
// low-weight link (a -race flake in r248).
func oldChoices(t *testing.T, m *LinkManager, pick func() *managedLink) map[*managedLink]bool {
	t.Helper()
	set := map[*managedLink]bool{}
	d := &choiceWalk{}
	pickRandHook.Store(&pickChooser{d.choose})
	defer pickRandHook.Store(nil)
	m.mu.Lock()
	defer m.mu.Unlock()
	for runs := 1; ; runs++ {
		if runs > 1<<16 {
			t.Fatalf("old picker: more than %d choice paths", 1<<16)
		}
		d.pos = 0
		if ml := pick(); ml != nil {
			set[ml] = true
		}
		if !d.next() {
			return set
		}
	}
}

// choiceWalk enumerates every sequence of choices of a deterministic
// program (depth first): choose answers from the current script and extends
// it with 0; next moves to the following script.
type choiceWalk struct {
	script, lim []int
	pos         int
}

func (d *choiceWalk) choose(n int) int {
	if d.pos < len(d.script) {
		v := d.script[d.pos]
		d.pos++
		return min(v, n-1)
	}
	d.script = append(d.script, 0)
	d.lim = append(d.lim, n)
	d.pos++
	return 0
}

func (d *choiceWalk) next() bool {
	d.script, d.lim = d.script[:d.pos], d.lim[:d.pos]
	for i := len(d.script) - 1; i >= 0; i-- {
		if d.script[i]+1 < d.lim[i] {
			d.script[i]++
			d.script, d.lim = d.script[:i+1], d.lim[:i+1]
			return true
		}
	}
	return false
}

// sampleFold is the pick bookkeeping of one sampleHealth tick.
func sampleFold(m *LinkManager) {
	m.mu.Lock()
	for _, ml := range m.links {
		ml.foldFastPicksLocked()
		copy(ml.pickHist[1:], ml.pickHist[:len(ml.pickHist)-1])
		ml.pickHist[0], ml.picks = ml.picks, 0
	}
	m.mu.Unlock()
}

// Serially, the fast path chooses only links the old path could have chosen
// from the same state (same order, same tiers, same steering), and counts
// them the same way (picks, users), folds included — on random pools.
func TestLMFastPickEquivalence(t *testing.T) {
	for _, dp := range []bool{false, true} {
		od := dupPathsOn
		dupPathsOn = dp
		t.Cleanup(func() { dupPathsOn = od })
		r := rand.New(rand.NewPCG(162, uint64(len(fmt.Sprint(dp)))))
		for trial := 0; trial < 60; trial++ {
			steer := trial%2 == 1
			m, ls := fpPool(r, 2+r.IntN(30), steer)
			setFastPick(t, true)
			for step := 0; step < 40; step++ {
				var avoid []any
				for _, l := range ls {
					if r.IntN(8) == 0 {
						avoid = append(avoid, Link(l))
					}
				}
				var p mPlane = planeLight
				if r.IntN(3) == 0 {
					p = planeBulk
				}
				held := r.IntN(2) == 0
				var allowed map[*managedLink]bool
				if held {
					allowed = oldChoices(t, m, func() *managedLink { return m.pickLocked(0) })
				} else {
					pool := m.exitInfo.Load()
					allowed = oldChoices(t, m, func() *managedLink {
						ml, _, _ := m.pickPlaneLockedFP(p, avoid, pool)
						return ml
					})
				}
				var l Link
				var ok bool
				if held {
					l, _, ok, _ = m.pickHeld()
				} else {
					l, _, ok = m.pickPlane(p, avoid)
				}
				if !ok {
					if len(allowed) != 0 {
						t.Fatalf("trial %d step %d: fast found no link, old had %d", trial, step, len(allowed))
					}
					continue
				}
				var got *managedLink
				for _, ml := range m.links {
					if ml.link == l {
						got = ml
					}
				}
				if !allowed[got] {
					t.Fatalf("trial %d step %d held=%v plane=%v: fast chose link %d, old chooses only %v", trial, step, held, p, got.id, ids(allowed))
				}
				if r.IntN(10) == 0 {
					sampleFold(m)
				}
			}
			var fp, users int64
			for _, ml := range m.links {
				fp += int64(ml.fpPicks.Load())
				users += int64(ml.users.Load())
			}
			if users != int64(m.users.Load()) {
				t.Fatalf("users %d, pool says %d", users, m.users.Load())
			}
			sampleFold(m)
			for _, ml := range m.links {
				if ml.fpPicks.Load() != 0 {
					t.Fatal("fold left fast picks")
				}
			}
			_ = fp
		}
	}
}

func ids(s map[*managedLink]bool) []int {
	var o []int
	for ml := range s {
		o = append(o, ml.id)
	}
	return o
}

// A concurrent burst on the fast path spreads exactly as the write-locked
// Pick does (TestPickSpreadsBurst): each placement is checked against the
// count the scan ranked with, so no two land on a link that only looked free.
func TestLMFastPickBurstSpread(t *testing.T) {
	setFastPick(t, true)
	for _, plane := range []bool{false, true} {
		ls := make([]*mcLink, 8)
		for i := range ls {
			ls[i] = newMCLink(capPortTags | capMConn)
		}
		m := mcManager(ls...)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 8*12; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				var ok bool
				if plane {
					_, _, ok = m.pickPlane(planeBulk, nil)
				} else {
					_, _, ok, _ = m.pickHeld()
				}
				if !ok {
					t.Error("no link")
				}
			}()
		}
		close(start)
		wg.Wait()
		for _, ml := range m.links {
			if n := ml.curPicks(); n != 12 || ml.users.Load() != 12 {
				t.Fatalf("plane=%v link %d: %d picks %d users, want 12 each", plane, ml.id, n, ml.users.Load())
			}
		}
	}
}

// Many goroutines open (pickHeld, pickPlane light/bulk, with avoid) and
// close while the pool churns under the write lock: links come and go,
// change tier and pressure, samples fold the fast counts, a refill hold
// starts and ends, steering is on. Run under -race. Every placement is
// released exactly once and the counts balance.
func TestLMFastPickConcurrentChurn(t *testing.T) {
	setFastPick(t, true)
	r := rand.New(rand.NewPCG(7, 162))
	m, _ := fpPool(r, 40, true)
	for _, ml := range m.links {
		m.users.Add(-ml.users.Load())
		ml.users.Store(0)
	}
	var stop atomic.Bool
	var placed, released atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			rr := rand.New(rand.NewPCG(uint64(g), 1))
			var rels []func()
			for !stop.Load() {
				var rel func()
				var ok bool
				var l Link
				switch rr.IntN(3) {
				case 0:
					var w *holdWaiter
					l, rel, ok, w = m.pickHeld()
					if w != nil {
						m.cancelHeld(w)
					}
				case 1:
					l, rel, ok = m.pickPlane(planeLight, nil)
					if ok {
						var r2 func()
						var ok2 bool
						_, r2, ok2 = m.pickPlane(planeLight, []any{l})
						if ok2 {
							placed.Add(1)
							rels = append(rels, r2)
						}
					}
				default:
					_, rel, ok = m.pickPlane(planeBulk, nil)
				}
				_ = l
				if ok {
					placed.Add(1)
					rels = append(rels, rel)
				}
				if len(rels) > 20 {
					i := rr.IntN(len(rels))
					rels[i]()
					rels[i]() // idempotent
					released.Add(1)
					rels = append(rels[:i], rels[i+1:]...)
				}
			}
			for _, rel := range rels {
				rel()
				released.Add(1)
			}
		}(g)
	}
	// churn: the pool's own goroutine
	var dropped []*managedLink
	for i := 0; i < 400; i++ {
		m.mu.Lock()
		switch r.IntN(6) {
		case 0:
			if len(m.links) > 4 {
				k := r.IntN(len(m.links))
				dropped = append(dropped, m.links[k])
				m.links = append(m.links[:k:k], m.links[k+1:]...)
			}
		case 1:
			l := newMCLink(capPortTags | capMConn)
			l.mtr.path = fmt.Sprintf("10.0.0.%d>10.1.0.1", r.IntN(4))
			m.linkSeq++
			m.links = append(m.links, &managedLink{link: l, id: 1000 + m.linkSeq, mtr: l.mtr})
		case 2:
			if len(m.links) > 0 {
				ml := m.links[r.IntN(len(m.links))]
				ml.pressed, ml.degraded, ml.retiring = !ml.pressed, r.IntN(4) == 0, r.IntN(5) == 0
				ml.flowing, ml.heavy = r.IntN(6), r.IntN(2)
			}
		case 3:
			for _, ml := range m.links {
				ml.foldFastPicksLocked()
				copy(ml.pickHist[1:], ml.pickHist[:len(ml.pickHist)-1])
				ml.pickHist[0], ml.picks = ml.picks, 0
			}
		case 4:
			m.hold.on = !m.hold.on && r.IntN(3) == 0
			if !m.hold.on {
				for _, w := range m.hold.queue {
					w.ch <- nil
				}
				m.hold.queue = nil
			}
		case 5:
			m.steer.active = !m.steer.active
		}
		m.mu.Unlock()
		if i%50 == 0 {
			sampleFold(m)
		}
	}
	stop.Store(true)
	wg.Wait()
	m.mu.Lock()
	m.hold.on = false
	m.mu.Unlock()
	if placed.Load() != released.Load() {
		t.Fatalf("placed %d released %d", placed.Load(), released.Load())
	}
	if m.users.Load() != 0 {
		t.Fatalf("pool users %d after every release", m.users.Load())
	}
	for _, ml := range append(m.links, dropped...) {
		if ml.users.Load() != 0 {
			t.Fatalf("link %d users %d after every release", ml.id, ml.users.Load())
		}
	}
	if fastPickStat.held.Load()+fastPickStat.plane.Load() == 0 {
		t.Fatal("fast path never placed")
	}
	t.Logf("fast held=%d plane=%d retry=%d fallback=%d placed=%d", fastPickStat.held.Load(), fastPickStat.plane.Load(),
		fastPickStat.retry.Load(), fastPickStat.fallback.Load(), placed.Load())
}

// lk165: stripeOn lock-free under the key reports exactly what the map says,
// through random holds and releases.
func TestLMFastPickStripeOnEquivalence(t *testing.T) {
	oldK := stripeK.Load()
	stripeK.Store(4)
	t.Cleanup(func() { stripeK.Store(oldK) })
	r := rand.New(rand.NewPCG(165, 1))
	m, _ := fpPool(r, 30, false)
	held := map[*managedLink]int{}
	for i := 0; i < 5000; i++ {
		ml := m.links[r.IntN(len(m.links))]
		d := 1
		if r.IntN(2) == 0 {
			d = -1 - r.IntN(2) // over-release too: clamped at 0 like the map
		}
		stripeHold(ml, d)
		if held[ml] += d; held[ml] <= 0 {
			delete(held, ml)
		}
		for _, x := range m.links {
			lmFastPick.Store(true)
			fast := stripeOn(x)
			lmFastPick.Store(false)
			slow := stripeOn(x)
			if fast != slow || slow != (held[x] > 0) {
				t.Fatalf("step %d link %d: fast %v map %v want %v", i, x.id, fast, slow, held[x] > 0)
			}
		}
	}
	for ml, n := range held {
		stripeHold(ml, -n)
	}
}

// lk165: stripeOn and stripeHold from many goroutines (run with -race).
func TestLMFastPickStripeOnRace(t *testing.T) {
	setFastPick(t, true)
	oldK := stripeK.Load()
	stripeK.Store(4)
	t.Cleanup(func() { stripeK.Store(oldK) })
	m, _ := fpPool(rand.New(rand.NewPCG(165, 2)), 16, false)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			rr := rand.New(rand.NewPCG(uint64(g), 3))
			for i := 0; i < 3000; i++ {
				ml := m.links[rr.IntN(len(m.links))]
				if g%2 == 0 {
					stripeHold(ml, 1)
					_ = stripeOn(ml)
					stripeHold(ml, -1)
				} else {
					_ = stripeOn(ml)
				}
			}
		}(g)
	}
	wg.Wait()
	for _, ml := range m.links {
		if stripeOn(ml) {
			t.Fatalf("link %d still striped after balanced holds", ml.id)
		}
	}
}

// steerDist draws n times and returns each path's share.
func steerDist(m *LinkManager, light bool, cand func(*managedLink) bool, n int) map[string]float64 {
	cnt := map[string]float64{}
	for i := 0; i < n; i++ {
		cnt[m.steerDrawLocked(light, cand)]++
	}
	for k := range cnt {
		cnt[k] /= float64(n)
	}
	return cnt
}

// lk165: the cached draw has the old draw's distribution — over random pools,
// steer reports, candidate sets (some leaving one path or none), light and
// not — and follows a steer tick and a changed pool at once.
func TestLMFastPickSteerDrawDistribution(t *testing.T) {
	r := rand.New(rand.NewPCG(165, 4))
	const n = 40000
	worst := 0.0
	for it := 0; it < 30; it++ {
		m, _ := fpPool(r, 20+r.IntN(80), true)
		if it%2 == 1 {
			spreadPaths(r, m, 12)
		}
		lim := int32(r.IntN(400))
		cand := func(ml *managedLink) bool { return ml.users.Load() < lim && !ml.degraded }
		for round := 0; round < 3; round++ {
			for _, light := range []bool{false, true} {
				// exact: the same set of possible draws and weights
				lmFastPick.Store(false)
				old := steerDist(m, light, cand, n)
				lmFastPick.Store(true)
				nw := steerDist(m, light, cand, n)
				lmFastPick.Store(false)
				for k := range mergeKeys(old, nw) {
					d := math.Abs(old[k] - nw[k])
					worst = math.Max(worst, d)
					// two draws of n: sd of the difference ≤ sqrt(2·0.25/n) ≈ 0.0035
					if d > 0.02 || (old[k] == 0) != (nw[k] == 0) && d > 0.002 {
						t.Fatalf("it %d round %d light %v path %q: old %.4f cached %.4f", it, round, light, k, old[k], nw[k])
					}
				}
			}
			// a steer tick moves the shares; the pool changes
			m.mu.Lock()
			for k := range m.steer.repShare {
				m.steer.repShare[k] = r.Float64() * 40
				m.steer.repCut[k] = r.IntN(5) == 0
			}
			for _, ml := range m.links {
				ml.steer.off = r.IntN(10) == 0
			}
			m.steerGen.Add(1)
			if round == 1 && len(m.links) > 3 {
				// filtered in place then a link on a new path: same length, same array
				k := r.IntN(len(m.links))
				kept := m.links[:0]
				for i, ml := range m.links {
					if i != k {
						kept = append(kept, ml)
					}
				}
				l := newMCLink(capPortTags | capMConn)
				l.mtr.path = "10.0.0.9>10.1.0.1"
				m.links = append(kept, &managedLink{link: l, id: 999, mtr: l.mtr})
				m.steer.repShare[l.mtr.path] = 50
			}
			m.mu.Unlock()
		}
	}
	t.Logf("worst share difference old vs cached: %.4f", worst)
}

func mergeKeys(a, b map[string]float64) map[string]bool {
	k := map[string]bool{}
	for x := range a {
		k[x] = true
	}
	for x := range b {
		k[x] = true
	}
	return k
}

// lk165: the cache's weights equal the uncached computation for the same
// candidate paths (deterministic, no sampling).
func TestLMFastPickSteerDrawWeightsExact(t *testing.T) {
	setFastPick(t, true)
	r := rand.New(rand.NewPCG(165, 5))
	for it := 0; it < 200; it++ {
		m, _ := fpPool(r, 10+r.IntN(60), true)
		if it%2 == 1 {
			spreadPaths(r, m, 12) // more paths than the old 8-path cap (the lab has 10)
		}
		lim := int32(r.IntN(400))
		cand := func(ml *managedLink) bool { return ml.users.Load() < lim }
		for _, light := range []bool{false, true} {
			m.steerDrawLocked(light, cand)
			c := m.steerC.Load()
			if c == nil {
				t.Fatal("no cache built")
			}
			present := map[string]bool{}
			for _, ml := range m.links {
				if p := mlPath(ml); p != "" && !ml.steer.off && cand(ml) {
					present[p] = true
				}
			}
			var keys []string
			var mask uint64
			for k, p := range c.paths {
				if present[p] {
					keys = append(keys, p)
					mask |= 1 << uint(k)
				}
			}
			if len(keys) < 2 {
				continue
			}
			if light {
				mask |= 1 << 63
			}
			v, ok := c.w.Load(mask)
			if !ok {
				t.Fatalf("it %d: no weights cached for %v", it, keys)
			}
			w := v.(*steerW)
			ws, tot := m.steerWeightsLocked(light, keys)
			if fmt.Sprint(w.keys) != fmt.Sprint(keys) || tot != w.tot && math.Abs(tot-w.tot) > 1e-9 {
				t.Fatalf("it %d: cached %v %v tot %v, want %v %v tot %v", it, w.keys, w.ws, w.tot, keys, ws, tot)
			}
			for i := range ws {
				if math.Abs(ws[i]-w.ws[i]) > 1e-9 {
					t.Fatalf("it %d: weight %d cached %v want %v", it, i, w.ws, ws)
				}
			}
		}
	}
}

// lk165: draws under the read lock from many goroutines while the steer
// ticks and the pool changes (run with -race).
func TestLMFastPickSteerDrawRace(t *testing.T) {
	setFastPick(t, true)
	r := rand.New(rand.NewPCG(165, 6))
	m, _ := fpPool(r, 60, true)
	var stop atomic.Bool
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for !stop.Load() {
				m.mu.RLock()
				_ = m.steerDrawLocked(g%2 == 0, func(ml *managedLink) bool { return ml.users.Load() < 300 })
				m.mu.RUnlock()
			}
		}(g)
	}
	for i := 0; i < 300; i++ {
		m.mu.Lock()
		switch r.IntN(3) {
		case 0:
			for k := range m.steer.repShare {
				m.steer.repShare[k] = r.Float64() * 40
			}
			m.steerGen.Add(1)
		case 1:
			if len(m.links) > 4 {
				k := r.IntN(len(m.links))
				m.links = append(m.links[:k:k], m.links[k+1:]...)
			}
		default:
			l := newMCLink(capPortTags | capMConn)
			l.mtr.path = fmt.Sprintf("10.0.0.%d>10.1.0.1", r.IntN(6))
			m.linkSeq++
			m.links = append(m.links, &managedLink{link: l, id: 2000 + m.linkSeq, mtr: l.mtr})
		}
		m.mu.Unlock()
	}
	stop.Store(true)
	wg.Wait()
}

// spreadPaths puts m's links on n paths, each in the steer report.
func spreadPaths(r *rand.Rand, m *LinkManager, n int) {
	for _, ml := range m.links {
		ml.mtr.path = fmt.Sprintf("10.0.0.%d>10.1.0.1", r.IntN(n))
	}
	for p := 0; p < n; p++ {
		k := fmt.Sprintf("10.0.0.%d>10.1.0.1", p)
		m.steer.repShare[k] = r.Float64() * 40
		m.steer.repFactor[k] = 0.3 + 0.7*r.Float64()
		m.steer.repCut[k] = r.IntN(5) == 0
		m.steer.repLoss[k] = r.Float64() * 0.03
	}
	m.steerGen.Add(1)
}

// lk165: with more than 8 paths the cache is used, not rebuilt per draw.
func TestLMFastPickSteerDrawManyPathsCached(t *testing.T) {
	setFastPick(t, true)
	r := rand.New(rand.NewPCG(165, 7))
	m, _ := fpPool(r, 300, true)
	spreadPaths(r, m, 10)
	cand := func(*managedLink) bool { return true }
	m.steerDrawLocked(false, cand)
	c := m.steerC.Load()
	if c == nil || len(c.paths) < 9 {
		t.Fatalf("no cache for 10 paths: %+v", c)
	}
	for i := 0; i < 100; i++ {
		m.steerDrawLocked(i%2 == 0, cand)
	}
	if m.steerC.Load() != c {
		t.Fatal("cache rebuilt without a tick or a pool change")
	}
}
