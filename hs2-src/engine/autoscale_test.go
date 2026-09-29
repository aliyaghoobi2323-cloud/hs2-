package engine

import (
	"context"
	"testing"
	"time"
)

// helper: a pool of n loaded metered links.
func loadedPool(d LinkDialer, min, max, users int) *LinkManager {
	m := NewLinkManager(d, min, max, 50, nil)
	for i := 0; i < min; i++ {
		ml := &managedLink{link: newMeteredFake(), mtr: &linkMeter{}}
		ml.users.Store(1)
		m.links = append(m.links, ml)
	}
	m.users.Store(int32(users))
	m.probeCoolAt = time.Now().Add(-time.Second) // cooldown already elapsed
	return m
}

// The throughput probe grows the pool while each added link raises aggregate
// goodput, and stops at the plateau where it no longer does.
func TestAutoscaleProbesToPlateau(t *testing.T) {
	d := &fakeDialer{}
	m := loadedPool(d, 2, 6, 2)
	ctx := context.Background()

	m.aggGoodput = 1000
	m.autoscale(ctx) // first probe: add one link
	if !m.probing || m.count() != 3 || d.dials.Load() != 1 {
		t.Fatalf("first probe: probing=%v count=%d dials=%d", m.probing, m.count(), d.dials.Load())
	}

	m.aggGoodput = 1200 // +20% > probeGain: it helped -> grow again
	m.autoscale(ctx)
	if !m.probing || m.count() != 4 {
		t.Fatalf("growth: probing=%v count=%d", m.probing, m.count())
	}

	m.aggGoodput = 1210 // ~flat (< +8%): plateau -> stop
	m.autoscale(ctx)
	if m.probing {
		t.Fatal("did not stop probing at the plateau")
	}
	if m.count() != 4 {
		t.Fatalf("added a link past the plateau: count=%d", m.count())
	}
	if m.plateauSize != 4 {
		t.Fatalf("plateauSize=%d, want 4", m.plateauSize)
	}
}

// With no users, the probe never fires: the pool stays at the floor even if some
// stale goodput reading is high.
func TestAutoscaleNoGrowthWithoutUsers(t *testing.T) {
	d := &fakeDialer{}
	m := loadedPool(d, 2, 6, 0) // users = 0
	m.aggGoodput = 100000
	m.autoscale(context.Background())
	if m.probing || m.count() != 2 || d.dials.Load() != 0 {
		t.Fatalf("grew without users: probing=%v count=%d dials=%d", m.probing, m.count(), d.dials.Load())
	}
}

// The user-count floor is honored fast, independent of throughput.
func TestAutoscaleUserFloor(t *testing.T) {
	d := &fakeDialer{}
	m := NewLinkManager(d, 2, 16, 50, nil)
	// one loaded link, but 200 users -> ceil(200/50)=4 floor
	ml := &managedLink{link: newMeteredFake(), mtr: &linkMeter{}}
	ml.users.Store(200)
	m.links = append(m.links, ml)
	m.users.Store(200)
	m.autoscale(context.Background())
	if m.count() != 4 {
		t.Fatalf("user floor not met: count=%d, want 4", m.count())
	}
}

// A pool above its floor with low load shrinks — but only after the settle time,
// and only by retiring an idle link.
func TestAutoscaleShrinksWhenIdle(t *testing.T) {
	m := NewLinkManager(nil, 2, 6, 50, nil)
	for i := 0; i < 4; i++ { // 4 links, floor is 2
		m.links = append(m.links, &managedLink{link: newMeteredFake(), mtr: &linkMeter{}})
	}
	m.users.Store(0)
	m.aggGoodput = 0

	m.lowSince = time.Now() // not settled yet
	m.autoscale(context.Background())
	if m.count() != 4 {
		t.Fatalf("shrank before the settle time: count=%d", m.count())
	}

	m.lowSince = time.Now().Add(-scaleDownAfter - time.Second) // settled
	m.autoscale(context.Background())
	if m.count() != 3 {
		t.Fatalf("did not retire one idle link: count=%d, want 3", m.count())
	}
}
