package engine

import (
	"math"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/xtaci/smux"
)

func setStormPick(t *testing.T, on bool) {
	t.Helper()
	o := b325StormPick.Load()
	b325StormPick.Store(on)
	t.Cleanup(func() { b325StormPick.Store(o) })
}

// refClassPick is pickLockedFP with only the class part of the key compared
// (every link of the best class ties): under oldChoices it yields exactly
// the set of links the storm pick may choose from (best tier, best class).
func refClassPick(m *LinkManager, limit int) *managedLink {
	drawn := m.steerDrawLocked(false, func(ml *managedLink) bool {
		return ml.aliveCheap() && m.infoReadyLocked(ml) && ml.serving() && (limit <= 0 || int(ml.users.Load()) < limit) && !stripeOn(ml)
	})
	for tier := 0; tier < 3; tier++ {
		var chosen *managedLink
		var best pickKey
		ties := 0
		for _, ml := range m.links {
			if !ml.aliveCheap() || !m.infoReadyLocked(ml) {
				continue
			}
			if limit > 0 && int(ml.users.Load()) >= limit || stripeGuardSkip(ml, limit) {
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
			k.heavy = pickClass && (ml.heavy > 0 || stripeOn(ml))
			k.pressed = k.pressed || stripeGuardPressed(ml)
			k.path = m.steerRankLocked(ml, drawn)
			k.cutNew = m.steerCutNewLocked(ml)
			if stripeOn(ml) {
				k.path = stripeSteerRank
			}
			switch {
			case chosen == nil || k.classLess(best):
				chosen, best, ties = ml, k, 1
			case !best.classLess(k):
				ties++
				if pickTie(ties) {
					chosen = ml
				}
			}
		}
		if chosen != nil {
			return chosen
		}
	}
	return nil
}

// Validity: over thousands of random pools (tiers, pressure, heavy, steering
// with cut paths and turned-off links, dead links via the cached flag), every
// link the storm pick can choose (all its random choices walked) is in the
// best tier and best class the old pick ranks first; and a light link is
// never left for a heavy one in the same path rank.
func TestB325StormPickValid(t *testing.T) {
	r := rand.New(rand.NewPCG(325, 1))
	pools, picks := 0, 0
	for trial := 0; trial < 2500; trial++ {
		steer := trial%2 == 1
		m, _ := fpPool(r, 2+r.IntN(14), steer)
		for _, ml := range m.links {
			if r.IntN(3) == 0 {
				f := new(atomic.Bool)
				f.Store(r.IntN(6) == 0)
				ml.sessDead = f
			}
		}
		pools++
		for step := 0; step < 3; step++ {
			ref := oldChoices(t, m, func() *managedLink { return refClassPick(m, 0) })
			var got map[*managedLink]bool
			if len(m.links) <= 6 { // every random choice walked (the reservoir branches n ways per member)
				got = oldChoices(t, m, func() *managedLink { ml, _ := m.pickStormFP(0); return ml })
			} else { // larger pools: 40 random draws
				got = map[*managedLink]bool{}
				m.mu.RLock()
				for k := 0; k < 40; k++ {
					if ml, _ := m.pickStormFP(0); ml != nil {
						got[ml] = true
					}
				}
				m.mu.RUnlock()
			}
			if len(ref) == 0 != (len(got) == 0) {
				t.Fatalf("trial %d: storm found %d choices, ref %d", trial, len(got), len(ref))
			}
			for ml := range got {
				picks++
				if !ref[ml] {
					t.Fatalf("trial %d step %d: storm can choose link %d, allowed only %v", trial, step, ml.id, ids(ref))
				}
				if p := ml.sessDead; p != nil && p.Load() {
					t.Fatalf("trial %d: dead link %d chosen", trial, ml.id)
				}
			}
			// the old exact pick is always a member of the allowed set too
			old := oldChoices(t, m, func() *managedLink { return m.pickLocked(0) })
			for ml := range old {
				if ml.sessDead == nil || !ml.sessDead.Load() {
					if !ref[ml] {
						t.Fatalf("trial %d: old pick %d outside the class set %v", trial, ml.id, ids(ref))
					}
				}
			}
			// place one (serial fast path) and go on from the new state
			if ml, fp := m.pickStormFP(0); ml != nil {
				m.placeFast(ml, fp)
			}
		}
	}
	t.Logf("pools=%d checked choices=%d", pools, picks)
}

// sessDeadOf: an mtcpLink's cached flag is exactly its Alive().
func TestB325SessDeadFlag(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	go func() { _, _ = smux.Server(b, nil) }()
	s, err := smux.Client(a, nil)
	if err != nil {
		t.Fatal(err)
	}
	l := &mtcpLink{sess: s}
	f := sessDeadOf(l)
	if f == nil {
		t.Fatal("no flag for an mtcpLink")
	}
	ml := &managedLink{link: l, sessDead: f}
	if !ml.aliveCheap() || !l.Alive() {
		t.Fatal("open link not alive")
	}
	s.Close()
	if ml.aliveCheap() || l.Alive() {
		t.Fatal("closed link alive")
	}
	if sessDeadOf(&mtcpLink{}) != nil || sessDeadOf(newMCLink(0)) != nil {
		t.Fatal("flag for a link without a session")
	}
}

// balRun places n users one at a time through the serial fast path (old or
// storm pick) with random releases (steady ~mean users per link) and a
// sample fold every tick picks; returns the time-averaged max/mean users
// per link and the max-min spread at the end.
func balRun(t *testing.T, storm bool, nLinks, n int, seed uint64) (ratio float64, spread int) {
	setStormPick(t, storm)
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
		if storm {
			ml, fp = m.pickStormFP(0)
		} else {
			ml, fp = m.pickLockedFP(0)
		}
		if ml == nil || !m.placeFast(ml, fp) {
			m.mu.RUnlock()
			t.Fatal("serial pick failed")
		}
		m.mu.RUnlock()
		live = append(live, u{m.releaseFor(ml)})
		if i%nLinks == nLinks-1 {
			sampleFold(m)
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

// Balance: over 100k placements with churn on a 300-link pool, the storm
// pick's max/mean users per link stays within 10 % of the old exact pick's.
func TestB325StormPickBalance(t *testing.T) {
	const X = 0.10
	for _, nl := range []int{300, 40} {
		o, os := balRun(t, false, nl, 100000, 1)
		s, ss := balRun(t, true, nl, 100000, 1)
		t.Logf("links=%d max/mean old %.4f storm %.4f (%+.2f %%), end spread old %d storm %d", nl, o, s, 100*(s/o-1), os, ss)
		if s > o*(1+X) {
			t.Fatalf("links=%d: storm max/mean %.4f > old %.4f +%.0f %%", nl, s, o, 100*X)
		}
	}
}

// Concurrency (-race): 32 pickers x 400 user connections through pickHeld
// with the storm pick, releases interleaved, sample folds running: every
// placement counted exactly once; releases restore the pool to zero.
func TestB325StormPickConcurrent(t *testing.T) {
	setFastPick(t, true)
	setStormPick(t, true)
	ls := make([]*mcLink, 300)
	for i := range ls {
		ls[i] = newMCLink(capPortTags | capMConn)
		ls[i].mtr.path = "10.0.0.1>10.1.0.1"
	}
	m := mcManager(ls...)
	m.perLink = 400
	r0, f0 := fastPickStat.heldRetry.Load(), fastPickStat.heldFallback.Load()
	var wg sync.WaitGroup
	var placed atomic.Int64
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				sampleFold(m)
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
				placed.Add(1)
				if i%3 == 0 {
					rel()
					rel() // idempotent
				} else {
					rels[g] = append(rels[g], rel)
				}
			}
		}(g)
	}
	wg.Wait()
	close(stop)
	var users int64
	for _, ml := range m.links {
		users += int64(ml.users.Load())
	}
	if users != int64(m.users.Load()) {
		t.Fatalf("link users %d, pool %d", users, m.users.Load())
	}
	want := int64(0)
	for _, rs := range rels {
		want += int64(len(rs))
	}
	if users != want {
		t.Fatalf("users %d, want %d", users, want)
	}
	for _, rs := range rels {
		for _, rel := range rs {
			rel()
		}
	}
	if m.users.Load() != 0 {
		t.Fatalf("pool users %d after all releases", m.users.Load())
	}
	t.Logf("placed=%d heldretry=%d heldfallback=%d line=%q", placed.Load(), fastPickStat.heldRetry.Load()-r0, fastPickStat.heldFallback.Load()-f0, b325Line())
}

// No allocation in the storm pick (pool read-locked, no steering).
func TestB325StormPickNoAlloc(t *testing.T) {
	ls := make([]*mcLink, 300)
	for i := range ls {
		ls[i] = newMCLink(capPortTags | capMConn)
	}
	m := mcManager(ls...)
	n := testing.AllocsPerRun(200, func() {
		m.mu.RLock()
		m.pickStormFP(0)
		m.mu.RUnlock()
	})
	if n != 0 {
		t.Fatalf("storm pick allocates %.1f per run", n)
	}
}

func BenchmarkB325Pick(b *testing.B) {
	for _, storm := range []bool{false, true} {
		name := "old"
		if storm {
			name = "storm"
		}
		b.Run(name, func(b *testing.B) {
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
					var ml *managedLink
					var fp int32
					if storm {
						ml, fp = m.pickStormFP(0)
					} else {
						ml, fp = m.pickLockedFP(0)
					}
					m.placeFast(ml, fp)
					m.mu.RUnlock()
				}
			})
		})
	}
}
