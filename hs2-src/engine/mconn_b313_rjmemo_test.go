package engine

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
)

func setRJMemo(t testing.TB, on bool) {
	t.Helper()
	o := b313RJMemo.Load()
	b313RJMemo.Store(on)
	t.Cleanup(func() { b313RJMemo.Store(o) })
}

// rjPool: fpPool with paths over 3 Iran IPs x 2 exit IPs, random freshness,
// lowered links, a random avoid list and a random rs145 mark.
func rjPool(r *rand.Rand, n int, steer bool) (*LinkManager, []any, *rescueJoinMark, func()) {
	m, ls := fpPool(r, n, steer)
	now := ctrlNow()
	var low []any
	var avoid []any
	for i, l := range ls {
		l.mtr.path = fmt.Sprintf("10.0.0.%d>10.1.0.%d", r.IntN(3), 1+r.IntN(2))
		switch r.IntN(4) {
		case 0: // never heard
		case 1: // stale with an answer owed
			l.mtr.lastRd.Store(now - int64(10*rescueFreshWin))
			l.mtr.lastWr.Store(now)
		default:
			l.mtr.lastRd.Store(now)
		}
		if r.IntN(6) == 0 {
			rescueLow.Store(m.links[i].link, struct{}{})
			low = append(low, m.links[i].link)
		}
		if r.IntN(8) == 0 {
			avoid = append(avoid, avoidKey(m.links[i].link))
		}
	}
	mk := &rescueJoinMark{paths: map[string]bool{}}
	for h := 0; h < 3; h++ {
		for e := 1; e <= 2; e++ {
			if r.IntN(3) == 0 {
				mk.paths[fmt.Sprintf("10.0.0.%d>10.1.0.%d", h, e)] = true
			}
		}
	}
	return m, avoid, mk, func() {
		for _, k := range low {
			rescueLow.Delete(k)
		}
	}
}

type rjCounts struct{ fresh, freshIP, old uint64 }

func rjNow() rjCounts {
	return rjCounts{rs145Stat.pickFresh.Load(), rs145Stat.pickFreshIP.Load(), rs145Stat.pickOld.Load()}
}

// TestB313RJMemoSamePick: with the same random choices, the memo picker
// chooses the same link as the old one and counts the same rs145 outcome,
// over random pools (with and without steering).
func TestB313RJMemoSamePick(t *testing.T) {
	setFastPick(t, true)
	for trial := 0; trial < 3000; trial++ {
		r := rand.New(rand.NewPCG(uint64(trial), 313))
		m, avoid, mk, undo := rjPool(r, 1+r.IntN(24), trial%2 == 0)
		pick := func(memo bool) (*managedLink, rjCounts) {
			b313RJMemo.Store(memo)
			cr := rand.New(rand.NewPCG(uint64(trial), 7))
			pickRandHook.Store(&pickChooser{func(n int) int { return cr.IntN(n) }})
			defer pickRandHook.Store(nil)
			before := rjNow()
			m.mu.Lock()
			ml := m.pickRescueJoinLocked(avoid, nil, mk)
			m.mu.Unlock()
			a := rjNow()
			return ml, rjCounts{a.fresh - before.fresh, a.freshIP - before.freshIP, a.old - before.old}
		}
		o := b313RJMemo.Load()
		oldML, oldC := pick(false)
		newML, newC := pick(true)
		b313RJMemo.Store(o)
		undo()
		if oldML != newML || oldC != newC {
			t.Fatalf("trial %d: old %p %+v, memo %p %+v", trial, oldML, oldC, newML, newC)
		}
	}
}

// TestB313RJMemoSkips: no fresh link off the left paths: the memo picker
// runs no scan (all four skipped) and returns nil like the old one.
func TestB313RJMemoSkips(t *testing.T) {
	setRJMemo(t, true)
	r := rand.New(rand.NewPCG(1, 2))
	m, avoid, _, undo := rjPool(r, 8, false)
	defer undo()
	mk := &rescueJoinMark{paths: map[string]bool{}}
	for _, ml := range m.links {
		mk.paths[mlPath(ml)] = true
	}
	s0 := b313Stat.skipped.Load()
	m.mu.Lock()
	ml := m.pickRescueJoinLocked(avoid, nil, mk)
	m.mu.Unlock()
	if ml != nil {
		t.Fatalf("picked %v with every path left", ml)
	}
	if d := b313Stat.skipped.Load() - s0; d != 4 {
		t.Fatalf("skipped %d scans, want 4", d)
	}
}

func benchRJ(b *testing.B, memo bool, fresh bool) {
	setRJMemo(b, memo)
	r := rand.New(rand.NewPCG(5, 6))
	m, avoid, mk, undo := rjPool(r, 64, true)
	defer undo()
	if !fresh {
		for _, ml := range m.links {
			ml.mtr.lastRd.Store(0)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.mu.Lock()
		m.pickRescueJoinLocked(avoid, nil, mk)
		m.mu.Unlock()
	}
}

func BenchmarkB313RJOld(b *testing.B)         { benchRJ(b, false, true) }
func BenchmarkB313RJMemo(b *testing.B)        { benchRJ(b, true, true) }
func BenchmarkB313RJOldNoFresh(b *testing.B)  { benchRJ(b, false, false) }
func BenchmarkB313RJMemoNoFresh(b *testing.B) { benchRJ(b, true, false) }

// TestB313RJFastSamePick: the read-locked core (no memo) picks what the old
// write-locked pick does, with the fast-path count of that link.
func TestB313RJFastSamePick(t *testing.T) {
	setFastPick(t, true)
	setRJMemo(t, false)
	for trial := 0; trial < 2000; trial++ {
		r := rand.New(rand.NewPCG(uint64(trial), 314))
		m, avoid, mk, undo := rjPool(r, 1+r.IntN(24), trial%2 == 1)
		hook := func() {
			cr := rand.New(rand.NewPCG(uint64(trial), 9))
			pickRandHook.Store(&pickChooser{func(n int) int { return cr.IntN(n) }})
		}
		hook()
		m.mu.Lock()
		oldML := m.pickRescueJoinLocked(avoid, nil, mk)
		m.mu.Unlock()
		hook()
		m.mu.RLock()
		newML, fp, _ := m.rjPickCore(avoid, nil, mk, false)
		m.mu.RUnlock()
		pickRandHook.Store(nil)
		undo()
		if oldML != newML {
			t.Fatalf("trial %d: old %p, fast %p", trial, oldML, newML)
		}
		if newML != nil && fp != newML.fpPicks.Load() {
			t.Fatalf("trial %d: fp %d, link has %d", trial, fp, newML.fpPicks.Load())
		}
	}
}

// TestB313RJFastConcurrent: 32 goroutines place rescue joins at once through
// pickPlaneCall with the key on (and the old path half the time): every
// placement is counted once (users, fpPicks + picks), releases undo them.
func TestB313RJFastConcurrent(t *testing.T) {
	setFastPick(t, true)
	o := b313RJFast.Load()
	defer b313RJFast.Store(o)
	for round := 0; round < 4; round++ {
		b313RJFast.Store(round%2 == 0)
		setRJMemo(t, round >= 2)
		r := rand.New(rand.NewPCG(uint64(round), 315))
		m, ls := fpPool(r, 16, round%2 == 1)
		now := ctrlNow()
		for i, l := range ls {
			l.mtr.path = fmt.Sprintf("10.0.0.%d>10.1.0.1", i%4)
			l.mtr.lastRd.Store(now)
		}
		base := map[*managedLink]int32{}
		var users0 int32
		for _, ml := range m.links {
			base[ml] = ml.users.Load()
			users0 += ml.users.Load()
		}
		picks0 := 0
		for _, ml := range m.links {
			picks0 += ml.picks + int(ml.fpPicks.Load())
		}
		const g, per = 32, 40
		rels := make(chan func(), g*per)
		var wg sync.WaitGroup
		for i := 0; i < g; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < per; j++ {
					mk := rescueJoinMark{paths: map[string]bool{"10.0.0.0>10.1.0.1": true}}
					if _, rel, ok := m.pickPlaneCall(planeLight, []any{mk}); ok {
						rels <- rel
					}
				}
			}()
		}
		wg.Wait()
		close(rels)
		n := 0
		var rs []func()
		for rel := range rels {
			n++
			rs = append(rs, rel)
		}
		picks := 0
		var users int32
		for _, ml := range m.links {
			picks += ml.picks + int(ml.fpPicks.Load())
			users += ml.users.Load()
			if mlPath(ml) == "10.0.0.0>10.1.0.1" && ml.users.Load() != base[ml] {
				t.Fatalf("round %d: a join landed on a left path", round)
			}
		}
		if n != g*per || picks-picks0 != n || int(users-users0) != n {
			t.Fatalf("round %d: placed %d (want %d), picks +%d, users +%d", round, n, g*per, picks-picks0, users-users0)
		}
		for _, rel := range rs {
			rel()
		}
		users = 0
		for _, ml := range m.links {
			users += ml.users.Load()
		}
		if users != users0 {
			t.Fatalf("round %d: users %d after releases, want %d", round, users, users0)
		}
	}
}
