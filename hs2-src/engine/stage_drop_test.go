package engine

import (
	"sync/atomic"
	"testing"
	"time"
)

// dropCar counts the queue drops the pool reports to the carrier.
type dropCar struct {
	*dgFakeCarrier
	drops atomic.Int64
}

func (c *dropCar) NoteQueueDrop() { c.drops.Add(1) }

// Every packet the carrier's send queue drops — queue full (the fattest
// flow's head) or aged past dgSojourn behind a slow writer — is reported to
// the carrier's rate model (udpcarrier.Conn.NoteQueueDrop): it is how a
// sender whose CPU, not its rate, holds it back leaves startup.
func TestDgQueueDropsReachTheCarrier(t *testing.T) {
	inner, _ := newDgFakePair()
	car := &dropCar{dgFakeCarrier: inner}
	l := newDgLink(car, time.Now())
	now := time.Now()
	for i := 0; i < dgQueueLen+10; i++ {
		b := make([]byte, 1200)
		l.enqueue(&b, uint32(i%3), now)
	}
	if got := car.drops.Load(); got != 10 {
		t.Fatalf("queue full: %d drops reported, want 10", got)
	}
	// The writer finds the rest aged past the sojourn.
	p := newDgPool(newFakeTUN(1400), 2, 8, 8, func(string, ...any) {})
	l.fq.limit = dgQueueLen
	time.Sleep(dgSojourn + 10*time.Millisecond)
	go l.writeLoop(&p.pool, &p.drops, &p.dropAged, &p.sentPkts)
	defer l.markDead()
	deadline := time.Now().Add(2 * time.Second)
	for car.drops.Load() < 10+dgQueueLen && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := car.drops.Load(); got != 10+dgQueueLen {
		t.Fatalf("aged: %d drops reported, want %d", got, 10+dgQueueLen)
	}
}
