package engine

import (
	"math"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
)

func setPickList(t testing.TB, on bool) {
	t.Helper()
	o := b327PickList.Load()
	b327PickList.Store(on)
	t.Cleanup(func() { b327PickList.Store(o) })
}

// refAllowed is the set of links the old pick's best tier and best class
// holds for some steering draw (any path, or none): refClassPick's set
// without walking its random choices.
func refAllowed(m *LinkManager) map[*managedLink]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	set := map[*managedLink]bool{}
	draws := map[string]bool{"": true}
	for _, ml := range m.links {
		draws[mlPath(ml)] = true
	}
	for tier := 0; tier < 3; tier++ {
		for d := range draws {
			var best []*managedLink
			var bk pickKey
			for _, ml := range m.links {
				if !ml.aliveCheap() || !m.infoReadyLocked(ml) {
					continue
				}
				switch tier {
				case 0:
					if ml.retiring || ml.degraded || ml.draining || ml.suspect {
						continue
					}
				case 1:
					if ml.degraded || ml.draining || ml.suspect {
						continue
					}
				}
				picks := ml.picks + int(ml.fpPicks.Load())
				k := newPickKey(ml.pressed, ml.flowing, picks, picks+ml.histPicks(), int(ml.users.Load()), m.perLink)
				k.heavy = pickClass && ml.heavy > 0
				k.path = m.steerRankLocked(ml, d)
				k.cutNew = m.steerCutNewLocked(ml)
				switch {
				case best == nil || k.classLess(bk):
					best, bk = []*managedLink{ml}, k
				case !bk.classLess(k):
					best = append(best, ml)
				}
			}
			for _, ml := range best {
				set[ml] = true
			}
		}
		if len(set) > 0 {
			return set
		}
	}
	return set
}

// sampleFoldBump is sampleFold plus the epoch bump sampleHealth makes.
func sampleFoldBump(m *LinkManager) {
	sampleFold(m)
	m.mu.Lock()
	m.b327Bump()
	m.mu.Unlock()
}

// keyNow is ml's current pick key (tier 0, no stripes), path rank for drawn.
func keyNow(m *LinkManager, ml *managedLink, drawn string) pickKey {
	k, _, _ := m.b327Key(ml, drawn)
	return k
}

// Validity: over thousands of random pools (tiers, pressure, heavy, steering
// with cut paths and turned-off links, dead links, cached and uncached steer
// factors), every list pick is in the best tier and best class of the full
// scan (refClassPick's allowed set), never dead, and at most b327Slack more
// loaded than the lightest link of that class; placements and folds go on
// between picks so the list goes stale and is rebuilt.
func TestB327PickListValid(t *testing.T) {
	setPickList(t, true)
	r := rand.New(rand.NewPCG(327, 1))
	pools, checked, fromList := 0, 0, 0
	for trial := 0; trial < 3000; trial++ {
		steer := trial%2 == 1
		m, _ := fpPool(r, 2+r.IntN(60), steer)
		if trial%4 == 3 {
			m.mu.Lock()
			m.b327FacTickLocked() // (a): factors from the per-link cache
			m.mu.Unlock()
		}
		for _, ml := range m.links {
			if r.IntN(3) == 0 {
				f := new(atomic.Bool)
				f.Store(r.IntN(6) == 0)
				ml.sessDead = f
			}
		}
		pools++
		for step := 0; step < 12; step++ {
			if r.IntN(5) == 0 {
				sampleFoldBump(m)
			}
			if r.IntN(7) == 0 { // a link dies lock-free
				ml := m.links[r.IntN(len(m.links))]
				f := new(atomic.Bool)
				f.Store(true)
				m.mu.Lock()
				ml.sessDead = f
				m.mu.Unlock()
			}
			ref := refAllowed(m)
			m.mu.RLock()
			ml, fp, ok := m.pickListFP()
			m.mu.RUnlock()
			if !ok {
				m.mu.RLock()
				ml, fp = m.pickLockedFP(0)
				m.mu.RUnlock()
			} else {
				fromList++
				checked++
				if !ref[ml] {
					t.Fatalf("trial %d step %d: list chose link %d, allowed only %v", trial, step, ml.id, ids(ref))
				}
				if !ml.aliveCheap() {
					t.Fatalf("trial %d: dead link %d chosen", trial, ml.id)
				}
				if ml.retiring || ml.degraded || ml.draining || ml.suspect {
					t.Fatalf("trial %d: list chose a non-serving link %d", trial, ml.id)
				}
				// load within the slack of the lightest allowed link of the
				// same steering group and class (the drawn path is random per
				// call, so ref is the union over draws: compare inside a group,
				// where the path rank is the same for every draw)
				minLoad := math.MaxInt
				ck := keyNow(m, ml, "")
				for o := range ref {
					if o.steer.off != ml.steer.off || !ml.steer.off && mlPath(o) != mlPath(ml) {
						continue
					}
					if k := keyNow(m, o, ""); !k.classLess(ck) && !ck.classLess(k) {
						minLoad = min(minLoad, k.load)
					}
				}
				if ck.load > minLoad+b327Slack {
					t.Fatalf("trial %d step %d: list load %d > class min %d + %d", trial, step, ck.load, minLoad, b327Slack)
				}
			}
			if ml != nil {
				m.mu.RLock()
				m.placeFast(ml, fp)
				m.mu.RUnlock()
			}
		}
	}
	if fromList < 1000 {
		t.Fatalf("list decided only %d picks", fromList)
	}
	t.Logf("pools=%d list picks checked=%d line=%q", pools, checked, b327Line())
}

// Light never on heavy: with one light serving link among heavy ones on the
// same path, the list always returns the light one while it is in the class.
func TestB327PickListLightFirst(t *testing.T) {
	setPickList(t, true)
	oc := pickClass
	pickClass = true
	defer func() { pickClass = oc }()
	ls := make([]*mcLink, 40)
	for i := range ls {
		ls[i] = newMCLink(capPortTags | capMConn)
		ls[i].mtr.path = "10.0.0.1>10.1.0.1"
	}
	m := mcManager(ls...)
	m.perLink = 1 << 20
	for i, ml := range m.links {
		ml.heavy = 1
		if i == 33 {
			ml.heavy = 0
			ml.flowing = 50 // much more loaded, but light
		}
	}
	for k := 0; k < 200; k++ {
		m.mu.RLock()
		ml, fp, ok := m.pickListFP()
		if !ok {
			ml, fp = m.pickLockedFP(0)
		}
		m.placeFast(ml, fp)
		m.mu.RUnlock()
		if ml.id != m.links[33].id {
			t.Fatalf("pick %d: heavy link %d chosen over the light one", k, ml.id)
		}
	}
}

// balRun327 is balRun with the list pick (falling back to the full scan as
// pickHeldFast does) and folds that bump the epoch.
func balRun327(t *testing.T, list bool, nLinks, n int, seed uint64) (ratio float64, spread int) {
	setPickList(t, list)
	r := rand.New(rand.NewPCG(seed, 7))
	ls := make([]*mcLink, nLinks)
	for i := range ls {
		ls[i] = newMCLink(capPortTags | capMConn)
		ls[i].mtr.path = "10.0.0.1>10.1.0.1"
	}
	m := mcManager(ls...)
	m.perLink = 400
	type u struct{ rel func() }
	var live []u
	target := nLinks * 100
	sum, cnt := 0.0, 0
	for i := 0; i < n; i++ {
		for len(live) > target || (len(live) > 0 && r.IntN(3) == 0 && len(live) > target/2) {
			j := r.IntN(len(live))
			live[j].rel()
			live[j] = live[len(live)-1]
			live = live[:len(live)-1]
			if len(live) <= target {
				break
			}
		}
		m.mu.RLock()
		var ml *managedLink
		var fp int32
		ok := false
		if list {
			ml, fp, ok = m.pickListFP()
		}
		if !ok {
			ml, fp = m.pickLockedFP(0)
		}
		if ml == nil || !m.placeFast(ml, fp) {
			m.mu.RUnlock()
			t.Fatal("serial pick failed")
		}
		m.mu.RUnlock()
		live = append(live, u{m.releaseFor(ml)})
		if i%nLinks == nLinks-1 {
			sampleFoldBump(m)
		}
		if i > n/5 && i%1000 == 0 {
			mx, tot := 0, 0
			for _, ml := range m.links {
				v := int(ml.users.Load())
				mx = max(mx, v)
				tot += v
			}
			sum += float64(mx) / (float64(tot) / float64(nLinks))
			cnt++
		}
	}
	mn, mx := math.MaxInt, 0
	for _, ml := range m.links {
		v := int(ml.users.Load())
		mn, mx = min(mn, v), max(mx, v)
	}
	return sum / float64(cnt), mx - mn
}

// Balance (b325's test): the list pick's time-averaged max/mean users per
// link stays within 10 % of the old exact pick's.
func TestB327PickListBalance(t *testing.T) {
	const X = 0.10
	for _, nl := range []int{300, 40} {
		b0 := b327Stat.list.Load()
		bb := b327Stat.build.Load()
		o, os := balRun327(t, false, nl, 100000, 1)
		s, ss := balRun327(t, true, nl, 100000, 1)
		t.Logf("links=%d max/mean old %.4f list %.4f (%+.2f %%), end spread old %d list %d; list picks %d builds %d", nl, o, s, 100*(s/o-1), os, ss,
			b327Stat.list.Load()-b0, b327Stat.build.Load()-bb)
		if s > o*(1+X) {
			t.Fatalf("links=%d: list max/mean %.4f > old %.4f +%.0f %%", nl, s, o, 100*X)
		}
	}
}

// Concurrency (-race): 32 pickers x 400 through pickHeld with the list on,
// releases interleaved, folds+bumps racing: counts exact, releases to zero.
func TestB327PickListConcurrent(t *testing.T) {
	setFastPick(t, true)
	setPickList(t, true)
	ls := make([]*mcLink, 300)
	for i := range ls {
		ls[i] = newMCLink(capPortTags | capMConn)
		ls[i].mtr.path = "10.0.0.1>10.1.0.1"
	}
	m := mcManager(ls...)
	m.perLink = 400
	l0 := b327Stat.list.Load()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				sampleFoldBump(m)
			}
		}
	}()
	rels := make([][]func(), 32)
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 400; i++ {
				_, rel, ok, _ := m.pickHeld()
				if !ok {
					t.Error("no pick")
					return
				}
				if i%3 == 0 {
					rel()
					rel()
				} else {
					rels[g] = append(rels[g], rel)
				}
			}
		}(g)
	}
	wg.Wait()
	close(stop)
	var users, want int64
	for _, ml := range m.links {
		users += int64(ml.users.Load())
	}
	for _, rs := range rels {
		want += int64(len(rs))
	}
	if users != int64(m.users.Load()) || users != want {
		t.Fatalf("link users %d, pool %d, want %d", users, m.users.Load(), want)
	}
	for _, rs := range rels {
		for _, rel := range rs {
			rel()
		}
	}
	if m.users.Load() != 0 {
		t.Fatalf("pool users %d after all releases", m.users.Load())
	}
	if b327Stat.list.Load() == l0 {
		t.Fatal("list never used")
	}
	t.Logf("line=%q", b327Line())
}

// No allocation in the list pick, rebuilds included.
func TestB327PickListNoAlloc(t *testing.T) {
	setPickList(t, true)
	ls := make([]*mcLink, 300)
	for i := range ls {
		ls[i] = newMCLink(capPortTags | capMConn)
	}
	m := mcManager(ls...)
	m.perLink = 1 << 20
	n := testing.AllocsPerRun(500, func() {
		m.mu.RLock()
		ml, fp, ok := m.pickListFP()
		if ok {
			m.placeFast(ml, fp)
		}
		m.mu.RUnlock()
	})
	if n != 0 {
		t.Fatalf("list pick allocates %.2f per run", n)
	}
}

func BenchmarkB327Pick(b *testing.B) {
	for _, list := range []bool{false, true} {
		name := "full"
		if list {
			name = "list"
		}
		b.Run(name, func(b *testing.B) {
			setPickList(b, list)
			ls := make([]*mcLink, 300)
			for i := range ls {
				ls[i] = newMCLink(capPortTags | capMConn)
			}
			m := mcManager(ls...)
			m.perLink = 1 << 30
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					m.mu.RLock()
					ok := false
					var ml *managedLink
					var fp int32
					if list {
						ml, fp, ok = m.pickListFP()
					}
					if !ok {
						ml, fp = m.pickLockedFP(0)
					}
					m.placeFast(ml, fp)
					m.mu.RUnlock()
				}
			})
		})
	}
}

// b334: HS2_B327_PICKLIST defaults to on (code default; the envdef table
// lists it for both sides), and "=0" still turns it off.
func TestB327PickListDefaultOn(t *testing.T) {
	if !b327PickListDefault {
		t.Fatal("HS2_B327_PICKLIST must default to on")
	}
	t.Setenv("HS2_B327_PICKLIST", "")
	if !envBoolDefault("HS2_B327_PICKLIST", b327PickListDefault) {
		t.Fatal("unset HS2_B327_PICKLIST must read as on")
	}
	t.Setenv("HS2_B327_PICKLIST", "0")
	if envBoolDefault("HS2_B327_PICKLIST", b327PickListDefault) {
		t.Fatal("HS2_B327_PICKLIST=0 must read as off")
	}
}
