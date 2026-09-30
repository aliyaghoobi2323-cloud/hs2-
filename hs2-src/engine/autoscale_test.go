package engine

import (
	"context"
	"testing"
	"time"
)

// These tests exercise the ACTUATOR — LinkManager.autoscale moving the real pool
// toward the autopilot's target. The controller's own decision logic is tested
// separately and deterministically in autopilot_test.go.

// helper: a pool of n loaded metered links, with the controller primed so its
// cooldown has already elapsed.
func loadedPool(d LinkDialer, min, max, users int) *LinkManager {
	m := NewLinkManager(d, min, max, 50, nil)
	for i := 0; i < min; i++ {
		ml := &managedLink{link: newMeteredFake(), mtr: &linkMeter{}}
		ml.users.Store(1)
		m.links = append(m.links, ml)
	}
	m.users.Store(int32(users))
	m.ap.coolUntil = time.Now().Add(-time.Second)
	return m
}

// When the pool is saturated and another link keeps raising goodput, autoscale
// grows the real pool one link per tick and stops at the plateau.
func TestAutoscaleGrowsWhileSaturated(t *testing.T) {
	d := &fakeDialer{}
	m := loadedPool(d, 2, 6, 2)
	ctx := context.Background()
	m.poolSaturated = true

	m.aggGoodput = 1000
	m.autoscale(ctx) // first probe: add one link
	if m.count() != 3 || d.dials.Load() != 1 {
		t.Fatalf("first probe: count=%d dials=%d", m.count(), d.dials.Load())
	}
	m.aggGoodput = 1200 // +20% > probeGain: keep growing
	m.autoscale(ctx)
	if m.count() != 4 {
		t.Fatalf("growth: count=%d, want 4", m.count())
	}
	m.aggGoodput = 1210 // ~flat: plateau -> stop
	m.autoscale(ctx)
	if m.count() != 4 {
		t.Fatalf("added a link past the plateau: count=%d", m.count())
	}
	if m.ap.ceilingSize != 4 {
		t.Fatalf("ceilingSize=%d, want 4", m.ap.ceilingSize)
	}
}

// With no users the pool never grows past its floor, whatever a stale goodput
// reading says (saturation is false without load).
func TestAutoscaleNoGrowthWithoutUsers(t *testing.T) {
	d := &fakeDialer{}
	m := loadedPool(d, 2, 6, 0) // users = 0
	m.aggGoodput = 100000
	m.poolSaturated = false
	m.autoscale(context.Background())
	if m.count() != 2 || d.dials.Load() != 0 {
		t.Fatalf("grew without users: count=%d dials=%d", m.count(), d.dials.Load())
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

// A pool above its floor with no load shrinks — but only after the settle time,
// and only by retiring an idle link.
func TestAutoscaleShrinksWhenIdle(t *testing.T) {
	m := NewLinkManager(nil, 2, 6, 50, nil)
	for i := 0; i < 4; i++ { // 4 links, floor is 2
		m.links = append(m.links, &managedLink{link: newMeteredFake(), mtr: &linkMeter{}})
	}
	m.users.Store(0)
	m.aggGoodput = 0
	m.poolSaturated = false

	m.ap.lowSince = time.Now() // not settled yet
	m.autoscale(context.Background())
	if m.count() != 4 {
		t.Fatalf("shrank before the settle time: count=%d", m.count())
	}
	m.ap.lowSince = time.Now().Add(-scaleDownAfter - time.Second) // settled
	m.autoscale(context.Background())
	if m.count() != 3 {
		t.Fatalf("did not retire one idle link: count=%d, want 3", m.count())
	}
}

// The published target (read by the reverse exit) tracks the autopilot decision.
func TestAutoscalePublishesTarget(t *testing.T) {
	d := &fakeDialer{}
	m := loadedPool(d, 2, 8, 2)
	m.poolSaturated = true
	m.aggGoodput = 1000
	m.autoscale(context.Background())
	if got := int(m.target.Load()); got != m.count() {
		t.Fatalf("published target %d != pool size %d", got, m.count())
	}
}
