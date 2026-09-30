package engine

import (
	"sync"
	"testing"
)

type fakeLink struct{ alive bool }

func (f *fakeLink) OpenStream() (stream, error) { return nil, nil }
func (f *fakeLink) Active() int32               { return 0 } // streams not opened yet
func (f *fakeLink) Alive() bool                 { return f.alive }
func (f *fakeLink) Close() error                { return nil }

// A burst of connections picked before any stream opens must spread evenly.
func TestPickSpreadsBurst(t *testing.T) {
	m := NewLinkManager(nil, 4, 4, 50, nil)
	links := make([]*fakeLink, 4)
	for i := range links {
		links[i] = &fakeLink{alive: true}
		m.links = append(m.links, &managedLink{link: links[i], id: i})
	}
	got := map[Link]int{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, _, ok := m.Pick()
			if !ok {
				t.Error("no link")
				return
			}
			mu.Lock()
			got[l]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	for i, l := range links {
		if got[l] != 2 {
			t.Fatalf("link %d got %d of 8 connections, want 2 (%v)", i, got[l], got)
		}
	}
}

// Releasing frees the slot; dead links are skipped.
func TestPickReleaseAndDead(t *testing.T) {
	m := NewLinkManager(nil, 2, 2, 50, nil)
	a, b := &fakeLink{alive: true}, &fakeLink{alive: false}
	m.links = []*managedLink{{link: a}, {link: b}}
	for i := 0; i < 3; i++ {
		l, rel, ok := m.Pick()
		if !ok || l != a {
			t.Fatal("expected the only live link")
		}
		rel()
		rel() // idempotent
	}
	if m.links[0].users.Load() != 0 || m.users.Load() != 0 {
		t.Fatalf("counts not released: %d %d", m.links[0].users.Load(), m.users.Load())
	}
}

// pickPool builds a manager over always-alive fake links, one per state func.
func pickPool(states ...func(*managedLink)) (*LinkManager, []*managedLink) {
	m := NewLinkManager(nil, 1, 32, 8, nil)
	for i, st := range states {
		ml := &managedLink{link: &fakeLink{alive: true}, id: i}
		if st != nil {
			st(ml)
		}
		m.links = append(m.links, ml)
	}
	return m, m.links
}

// New users go to serving links only, and among those to unpressed ones —
// however busy the unpressed link is. The fallbacks, in order: a pressed
// serving link, then a retiring link (better than refusing the user), then a
// degraded or draining one.
func TestPickSkipsRetiringDegradedPressed(t *testing.T) {
	m, ls := pickPool(
		func(ml *managedLink) { ml.retiring = true },
		func(ml *managedLink) { ml.degraded = true },
		func(ml *managedLink) { ml.draining = true },
		func(ml *managedLink) { ml.pressed = true },
		func(ml *managedLink) { ml.flowing = 20; ml.users.Store(100) }, // busy but free
	)
	retiring, degraded, draining, pressed, plain := ls[0], ls[1], ls[2], ls[3], ls[4]
	for i := 0; i < 50; i++ {
		l, rel, ok := m.Pick()
		if !ok || l != plain.link {
			t.Fatalf("pick %d: got link %v, want the only unpressed serving link", i, l)
		}
		rel()
	}
	kill := func(ml *managedLink) { ml.link.(*fakeLink).alive = false }
	kill(plain)
	if l, _, _ := m.Pick(); l != pressed.link {
		t.Fatal("with no unpressed serving link, a pressed serving link must come next")
	}
	kill(pressed)
	if l, _, _ := m.Pick(); l != retiring.link {
		t.Fatal("with no serving link, a retiring link must come before degraded/draining ones")
	}
	kill(retiring)
	if l, _, ok := m.Pick(); !ok || (l != degraded.link && l != draining.link) {
		t.Fatal("last resort: a degraded or draining link")
	}
	kill(degraded)
	kill(draining)
	if _, _, ok := m.Pick(); ok {
		t.Fatal("picked a dead link")
	}
}

// A fresh link (nothing flowing, no users) takes the next flows even when the
// other links have fewer active flows than per_link allows.
func TestPickFreshLinkWins(t *testing.T) {
	m, ls := pickPool(
		func(ml *managedLink) { ml.flowing = 1; ml.users.Store(30) },
		nil, // fresh
	)
	for i := 0; i < 2; i++ {
		if l, _, _ := m.Pick(); l != ls[1].link {
			t.Fatalf("pick %d did not go to the fresh link", i)
		}
	}
	// Now 2 placed on it this tick vs 1 flowing on the old one: the old one.
	if l, _, _ := m.Pick(); l != ls[0].link {
		t.Fatal("the fresh link kept taking flows past the old link's load")
	}
}

// Connections placed within one tick count as load (picks), so a burst spreads
// by flowing + picks, not only by the flows measured at the last sample; the
// next sample clears the picks.
func TestPickBurstSpreadsWithinTick(t *testing.T) {
	m, ls := pickPool(
		nil,
		func(ml *managedLink) { ml.flowing = 2 },
		func(ml *managedLink) { ml.flowing = 2 },
	)
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := map[Link]int{}
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, _, ok := m.Pick()
			if !ok {
				t.Error("no link")
				return
			}
			mu.Lock()
			got[l]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	// Loads (flowing+picks, users): A 0→1→2, then B and C (2,0) beat A (2,2),
	// then A (2,2) beats B and C (3,1).
	if got[ls[0].link] != 3 || got[ls[1].link] != 1 || got[ls[2].link] != 1 {
		t.Fatalf("burst of 5 spread as %d/%d/%d, want 3/1/1", got[ls[0].link], got[ls[1].link], got[ls[2].link])
	}
	m.sampleHealth()
	for i, ml := range ls {
		if ml.picks != 0 {
			t.Fatalf("link %d: picks=%d after a sample, want 0", i, ml.picks)
		}
	}
}

// Among links with equal load, the one with fewer open connections wins —
// always, not by chance; only a full tie is broken at random.
func TestPickTiesByOpenConnections(t *testing.T) {
	for i := 0; i < 50; i++ {
		m, ls := pickPool(
			func(ml *managedLink) { ml.flowing = 1; ml.users.Store(5) },
			func(ml *managedLink) { ml.flowing = 1; ml.users.Store(2) },
		)
		if l, _, _ := m.Pick(); l != ls[1].link {
			t.Fatalf("trial %d: equal load, 5 vs 2 open connections: picked the busier link", i)
		}
	}
	seen := map[int]bool{}
	for i := 0; i < 200 && len(seen) < 2; i++ {
		m, ls := pickPool(nil, nil)
		l, _, _ := m.Pick()
		for j, ml := range ls {
			if ml.link == l {
				seen[j] = true
			}
		}
	}
	if len(seen) != 2 {
		t.Fatal("a full tie always went to the same link (ties must be broken at random)")
	}
}
