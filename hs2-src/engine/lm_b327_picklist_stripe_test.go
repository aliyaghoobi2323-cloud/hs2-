package engine

import (
	"math"
	"math/rand/v2"
	"sync/atomic"
	"testing"
)

// stripeKey is ml's current pick key exactly as pickLockedFP ranks it with
// stripes on (heavy class + stripeSteerRank for a stripe holder, pressed
// under the resource guard).
func stripeKey(m *LinkManager, ml *managedLink, d string) pickKey {
	picks := ml.picks + int(ml.fpPicks.Load())
	k := newPickKey(ml.pressed, ml.flowing, picks, picks+ml.histPicks(), int(ml.users.Load()), m.perLink)
	k.heavy = pickClass && (ml.heavy > 0 || stripeOn(ml))
	k.pressed = k.pressed || stripeGuardPressed(ml)
	k.path = m.steerRankLocked(ml, d)
	k.cutNew = m.steerCutNewLocked(ml)
	if stripeOn(ml) {
		k.path = stripeSteerRank
	}
	return k
}

// refAllowedStripe is refAllowed with stripe-held links ranked as
// pickLockedFP ranks them: the set the old pick's best tier and best class
// holds for some steering draw.
func refAllowedStripe(m *LinkManager) map[*managedLink]bool {
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
				k := stripeKey(m, ml, d)
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

func setStripes(t *testing.T, k int32, guard bool) {
	t.Helper()
	ok, og, of := stripeK.Load(), stripeResGuard.Load(), lmFastPick.Load()
	stripeK.Store(k)
	stripeResGuard.Store(guard)
	lmFastPick.Store(true)
	t.Cleanup(func() { stripeK.Store(ok); stripeResGuard.Store(og); lmFastPick.Store(of) })
}

// 07328dd5: with stripes on (the lab/prod config), random pools where many
// links hold stripes that start and end lock-free between picks (no epoch
// bump, as in production): every list pick stays inside the old pick's
// allowed set, never dead or non-serving, within b327Slack of the lightest
// link of its group and class, and stripe holders are ranked exactly as the
// full scan ranks them (heavy class, stripeSteerRank, pressed under the
// resource guard).
func TestB327PickListStripeValid(t *testing.T) {
	setPickList(t, true)
	for gi, guard := range []bool{false, true} {
		setStripes(t, 4, guard)
		r := rand.New(rand.NewPCG(3270, uint64(gi)))
		var held []*managedLink
		checked, fromStripe, ends := 0, 0, 0
		for trial := 0; trial < 1500; trial++ {
			steer := trial%2 == 1
			m, _ := fpPool(r, 2+r.IntN(60), steer)
			if steer && trial%4 == 3 { // the steer tick runs only with steering state
				m.mu.Lock()
				m.b327FacTickLocked()
				m.mu.Unlock()
			}
			frac := []int{0, 2, 4, 10}[r.IntN(4)] // stripe-held share: none, 1/2, 1/4, 1/10
			for _, ml := range m.links {
				if frac > 0 && r.IntN(frac) == 0 {
					stripeHold(ml, 1)
					held = append(held, ml)
				}
			}
			for step := 0; step < 12; step++ {
				if r.IntN(5) == 0 {
					sampleFoldBump(m)
				}
				// stripes end and start lock-free (no epoch bump)
				if len(held) > 0 && r.IntN(2) == 0 {
					i := r.IntN(len(held))
					stripeHold(held[i], -1)
					held[i] = held[len(held)-1]
					held = held[:len(held)-1]
					ends++
				}
				if r.IntN(3) == 0 {
					ml := m.links[r.IntN(len(m.links))]
					stripeHold(ml, 1)
					held = append(held, ml)
				}
				if r.IntN(9) == 0 { // a link dies lock-free
					ml := m.links[r.IntN(len(m.links))]
					f := new(atomic.Bool)
					f.Store(true)
					m.mu.Lock()
					ml.sessDead = f
					m.mu.Unlock()
				}
				ref := refAllowedStripe(m)
				m.mu.RLock()
				ml, fp, ok := m.pickListFP()
				m.mu.RUnlock()
				if !ok {
					m.mu.RLock()
					ml, fp = m.pickLockedFP(0)
					m.mu.RUnlock()
				} else {
					checked++
					if !ref[ml] {
						t.Fatalf("guard=%v trial %d step %d: list chose link %d (stripe %v), allowed only %v", guard, trial, step, ml.id, stripeOn(ml), ids(ref))
					}
					if !ml.aliveCheap() || ml.retiring || ml.degraded || ml.draining || ml.suspect {
						t.Fatalf("guard=%v trial %d: list chose dead/non-serving link %d", guard, trial, ml.id)
					}
					if stripeOn(ml) {
						fromStripe++
					}
					// stripe holders have their own rank (stripeSteerRank, the same
					// for every draw): compare among stripe holders, else inside
					// the steering group
					ck := stripeKey(m, ml, "")
					minLoad := math.MaxInt
					for o := range ref {
						if stripeOn(o) != stripeOn(ml) {
							continue
						}
						if !stripeOn(ml) && (o.steer.off != ml.steer.off || !ml.steer.off && mlPath(o) != mlPath(ml)) {
							continue
						}
						if k := stripeKey(m, o, ""); !k.classLess(ck) && !ck.classLess(k) {
							minLoad = min(minLoad, k.load)
						}
					}
					if ck.load > minLoad+b327Slack {
						t.Fatalf("guard=%v trial %d step %d: list load %d > class min %d + %d", guard, trial, step, ck.load, minLoad, b327Slack)
					}
				}
				if ml != nil {
					m.mu.RLock()
					m.placeFast(ml, fp)
					m.mu.RUnlock()
				}
			}
			for _, ml := range held {
				stripeHold(ml, -1)
			}
			held = held[:0]
		}
		if checked < 1000 || ends < 1000 {
			t.Fatalf("guard=%v: list decided only %d picks, %d lock-free stripe ends", guard, checked, ends)
		}
		t.Logf("guard=%v list picks checked=%d (stripe holder chosen %d) lock-free stripe ends=%d%s", guard, checked, fromStripe, ends, b327Line())
	}
}

// A stripe that ends lock-free after the build makes its link the best one:
// the list (which keeps every stripe holder as a candidate) picks it, as the
// full scan does, with no epoch bump.
func TestB327PickListStripeEnds(t *testing.T) {
	setPickList(t, true)
	setStripes(t, 4, true)
	oc := pickClass
	pickClass = true
	t.Cleanup(func() { pickClass = oc })
	ls := make([]*mcLink, 40)
	for i := range ls {
		ls[i] = newMCLink(capPortTags | capMConn)
		ls[i].mtr.path = "10.0.0.1>10.1.0.1"
	}
	m := mcManager(ls...)
	m.perLink = 1 << 20
	for _, ml := range m.links {
		ml.heavy = 1 // every link heavy ...
	}
	light := m.links[17]
	light.heavy = 0 // ... except one light link that holds a stripe
	stripeHold(light, 1)
	m.mu.RLock()
	ml, _, ok := m.pickListFP()
	m.mu.RUnlock()
	if !ok {
		stripeHold(light, -1)
		t.Fatalf("list unusable")
	}
	if ml == light {
		stripeHold(light, -1)
		t.Fatalf("stripe holder chosen while held (heavy class + stripe rank)")
	}
	ep := m.b327.epoch.Load()
	stripeHold(light, -1) // ends lock-free: now the only light link
	for k := 0; k < 50; k++ {
		m.mu.RLock()
		ml, fp, ok := m.pickListFP()
		m.mu.RUnlock()
		if !ok {
			t.Fatalf("pick %d: list unusable", k)
		}
		if ml != light {
			t.Fatalf("pick %d: list chose %d, the light link whose stripe ended is %d", k, ml.id, light.id)
		}
		m.mu.RLock()
		m.placeFast(ml, fp)
		m.mu.RUnlock()
	}
	if m.b327.epoch.Load() != ep {
		t.Fatalf("epoch moved")
	}
	m.mu.RLock()
	fs, _ := m.pickLockedFP(0)
	m.mu.RUnlock()
	if fs != light {
		t.Fatalf("full scan chose %d", fs.id)
	}
}

// More stripe holders than the list keeps (b327Str): the list is unusable
// (every pick takes the full scan) until the pool changes.
func TestB327PickListStripeOverflow(t *testing.T) {
	setPickList(t, true)
	setStripes(t, 4, false)
	r := rand.New(rand.NewPCG(3271, 1))
	m, _ := fpPool(r, 60, false)
	var held []*managedLink
	defer func() {
		for _, ml := range held {
			stripeHold(ml, -1)
		}
	}()
	for _, ml := range m.links {
		if ml.aliveCheap() && !ml.retiring && !ml.degraded && !ml.draining && !ml.suspect {
			stripeHold(ml, 1)
			held = append(held, ml)
		}
	}
	if len(held) <= b327Str {
		t.Skipf("only %d serving links", len(held))
	}
	f0 := b327Stat.full.Load()
	for k := 0; k < 5; k++ {
		m.mu.RLock()
		_, _, ok := m.pickListFP()
		m.mu.RUnlock()
		if ok {
			t.Fatalf("list used with %d stripe holders > %d", len(held), b327Str)
		}
	}
	if d := b327Stat.full.Load() - f0; d != 5 {
		t.Fatalf("full=%d", d)
	}
}

// Quiet vs storm placement quality (st327 quiet p99 question): per list pick,
// how much more loaded (flowing + picks) the chosen link is than the
// lightest link of the same class the full scan would take, with flowing
// re-drawn at every sample (active flows come and go) and picksPerSample
// placements between samples.
func b327Excess(t *testing.T, picksPerSample int) (used, picks int, sumEx int, pile int) {
	setPickList(t, true)
	r := rand.New(rand.NewPCG(3272, uint64(picksPerSample)))
	ls := make([]*mcLink, 300)
	for i := range ls {
		ls[i] = newMCLink(capPortTags | capMConn)
		ls[i].mtr.path = "10.0.0.1>10.1.0.1"
	}
	m := mcManager(ls...)
	m.perLink = 400
	// Under -race the full size (400 samples x up to 300 picks, each checked
	// against all 300 links) takes far beyond the test timeout: 20 samples
	// keep every config and check, at 1/20 of the picks.
	samples := 400
	if b327RaceOn {
		samples = 20
	}
	for s := 0; s < samples; s++ {
		m.mu.Lock()
		for _, ml := range m.links {
			ml.flowing = r.IntN(4)
			ml.heavy = r.IntN(5) / 4
		}
		m.mu.Unlock()
		sampleFoldBump(m)
		per := map[*managedLink]int{}
		for p := 0; p < picksPerSample; p++ {
			m.mu.RLock()
			ml, fp, ok := m.pickListFP()
			if ok {
				ck := keyNow(m, ml, "")
				mn := math.MaxInt
				for _, o := range m.links {
					if k := keyNow(m, o, ""); o.aliveCheap() && !k.classLess(ck) && !ck.classLess(k) {
						mn = min(mn, k.load)
					}
				}
				picks++
				if ck.load > mn {
					used++
					sumEx += ck.load - mn
				}
			} else {
				ml, fp = m.pickLockedFP(0)
			}
			m.placeFast(ml, fp)
			m.mu.RUnlock()
			per[ml]++
			pile = max(pile, per[ml])
		}
	}
	return
}

func TestB327PickListExcess(t *testing.T) {
	o, ok := b327Slack, b327K
	t.Cleanup(func() { b327Slack, b327K = o, ok })
	for _, k := range []int{8, 16} {
		b327K = k
		for _, sl := range []int{1, 0} {
			b327Slack = sl
			for _, pps := range []int{1, 3, 30, 300} {
				b0 := b327Stat.build.Load()
				u, p, s, pile := b327Excess(t, pps)
				nb := b327Stat.build.Load() - b0
				t.Logf("k=%d slack=%d picks/sample=%d list picks=%d builds=%d (%.1f picks/build) heavier than the class min: %d (%.2f %%), total excess %d, max placements on one link in a sample %d",
					k, sl, pps, p, nb, float64(p)/float64(max(nb, 1)), u, 100*float64(u)/float64(max(p, 1)), s, pile)
				if sl == 0 && u != 0 {
					t.Fatalf("slack 0: %d list picks above the class min", u)
				}
			}
		}
	}

}
