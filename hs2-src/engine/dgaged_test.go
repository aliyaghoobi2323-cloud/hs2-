package engine

import (
	"testing"
	"time"
)

// A packet that ages out of a carrier's send queue (it waited longer than
// dgSojourn behind a writer blocked in the pacer) must count as PRESSURE, the
// same as a queue-full drop: the carrier could not drain within the sojourn, so
// it is at its limit. Before this, only queue-full drops set droppedAt, and a
// starved carrier shedding thousands of aged packets still read as "not at its
// limit" — the autopilot then shrank the pool under exactly the load that
// needed it (both field servers: huge drop_aged, "0 of 8 at their limit").
func TestDgAgedDropCountsAsPressure(t *testing.T) {
	p := newDgPool(newFakeTUN(1400), 2, 8, 8, func(string, ...any) {})
	car, _ := newDgFakePair()
	l := newDgLink(car, time.Now())
	p.set = append(p.set, l)
	go l.writeLoop(&p.pool, &p.drops, &p.dropAged, &p.sentPkts)
	defer l.markDead()

	if l.droppedAt.Load() != 0 {
		t.Fatal("droppedAt set before any drop")
	}

	// Enqueue a packet stamped well past the sojourn: the writer must age it
	// out rather than send it.
	b := make([]byte, 100)
	stale := time.Now().Add(-(dgSojourn + 60*time.Millisecond))
	if !l.enqueue(&b, 1, stale) {
		t.Fatal("enqueue of a single packet failed (queue should be empty)")
	}
	if !waitFor(t, 2*time.Second, func() bool { return p.dropAged.Load() == 1 }) {
		t.Fatalf("the stale packet was not aged out (dropAged=%d)", p.dropAged.Load())
	}
	if l.sentPkts.Load() != 0 {
		t.Fatalf("the stale packet was sent (%d) instead of aged out", l.sentPkts.Load())
	}

	// The aged drop recorded pressure...
	at := l.droppedAt.Load()
	if at == 0 {
		t.Fatal("an aged drop did not record pressure (droppedAt still 0)")
	}
	if d := time.Since(time.Unix(0, at)); d < 0 || d > time.Second {
		t.Fatalf("droppedAt is not recent: %s ago", d)
	}

	// ...and the autopilot sample reads the carrier as pressed on the next
	// tick, which is what lets the pool grow instead of shrinking.
	s := p.sampleHealth()
	if len(s.links) != 1 || !s.links[0].pressed {
		t.Fatalf("after an aged drop the carrier is not pressed in the sample: %+v", s.links)
	}
}
