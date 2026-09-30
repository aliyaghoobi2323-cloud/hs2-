package engine

import (
	"context"
	"encoding/binary"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCarrier is a stand-in for a dialed reverse link.
type fakeCarrier struct{ closed atomic.Bool }

func (c *fakeCarrier) Close() error { c.closed.Store(true); return nil }

// The exit pool dials up to its target, and grows and shrinks as the target
// (which the edge sends) changes — never below min, never above max.
func TestExitPoolTracksTarget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var dials atomic.Int32
	var live sync.Map // *fakeCarrier -> struct{}
	dial := func() (dialedLink, error) {
		dials.Add(1)
		c := &fakeCarrier{}
		live.Store(c, struct{}{})
		return c, nil
	}
	pool := newExitPool(ctx, 2, 8, dial, func(string, ...any) {})
	pool.serve = func(ctx context.Context, c dialedLink) {
		<-ctx.Done() // hold the link until the slot is cancelled
		live.Delete(c)
	}

	countSlots := func() int { pool.mu.Lock(); defer pool.mu.Unlock(); return len(pool.slots) }
	waitSlots := func(n int) {
		t.Helper()
		for i := 0; i < 200; i++ {
			if countSlots() == n {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("pool did not reach %d slots (have %d)", n, countSlots())
	}

	pool.setTarget(3)
	waitSlots(3)
	pool.setTarget(6)
	waitSlots(6)
	pool.setTarget(1) // below min -> clamps to 2
	waitSlots(2)
	pool.setTarget(100) // above max -> clamps to 8
	waitSlots(8)

	// Shrinking cancels slots; each cancelled slot closes its carrier.
	pool.setTarget(2)
	waitSlots(2)
	time.Sleep(80 * time.Millisecond)
	stillOpen := 0
	live.Range(func(k, _ any) bool {
		if !k.(*fakeCarrier).closed.Load() {
			stillOpen++
		}
		return true
	})
	if stillOpen != 2 {
		t.Fatalf("after shrinking to 2, %d carriers still open (want 2)", stillOpen)
	}
}

// servePoolCtl decodes the edge's 2-byte targets and applies each to the pool.
func TestServePoolCtlApplies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := newExitPool(ctx, 2, 32, func() (dialedLink, error) { return &fakeCarrier{}, nil }, func(string, ...any) {})
	pool.serve = func(ctx context.Context, c dialedLink) { <-ctx.Done() }
	pool.setTarget(2)

	pr, pw := io.Pipe()
	go servePoolCtl(ctx, struct {
		io.Reader
		io.WriteCloser
	}{pr, pw}, pool)

	send := func(n int) {
		var b [2]byte
		binary.BigEndian.PutUint16(b[:], uint16(n))
		pw.Write(b[:])
	}
	send(9)
	deadline := time.Now().Add(2 * time.Second)
	for {
		pool.mu.Lock()
		w := pool.want
		pool.mu.Unlock()
		if w == 9 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("target from the wire was not applied: want 9, got %d", w)
		}
		time.Sleep(10 * time.Millisecond)
	}
	pw.Close()
}
