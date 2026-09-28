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
