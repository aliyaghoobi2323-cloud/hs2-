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

// fakeCarrier is a stand-in for a dialed reverse link. end() simulates the
// link going away (the edge closing it, or the network dropping it).
type fakeCarrier struct {
	closed atomic.Bool
	done   chan struct{}
	once   sync.Once
}

func newFakeCarrier() *fakeCarrier            { return &fakeCarrier{done: make(chan struct{})} }
func (c *fakeCarrier) Close() error           { c.closed.Store(true); c.end(); return nil }
func (c *fakeCarrier) end()                   { c.once.Do(func() { close(c.done) }) }
func (c *fakeCarrier) ended() <-chan struct{} { return c.done }

// testPool is an exit pool over fake carriers, tracking which are live.
type testPool struct {
	*exitPool
	mu    sync.Mutex
	live  []*fakeCarrier
	dials atomic.Int32
}

func newTestPool(t *testing.T, min, max int) *testPool {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	tp := &testPool{}
	tp.exitPool = newExitPool(ctx, min, max, func() (dialedLink, error) {
		tp.dials.Add(1)
		c := newFakeCarrier()
		tp.mu.Lock()
		tp.live = append(tp.live, c)
		tp.mu.Unlock()
		return c, nil
	}, func(string, ...any) {})
	tp.serve = func(ctx context.Context, c dialedLink) {
		fc := c.(*fakeCarrier)
		select { // hold the link until it ends or the slot is cancelled
		case <-ctx.Done():
		case <-fc.ended():
		}
		tp.mu.Lock()
		for i, x := range tp.live {
			if x == fc {
				tp.live = append(tp.live[:i], tp.live[i+1:]...)
				break
			}
		}
		tp.mu.Unlock()
	}
	return tp
}

func (tp *testPool) slots() int {
	tp.exitPool.mu.Lock()
	defer tp.exitPool.mu.Unlock()
	return len(tp.exitPool.slots)
}
func (tp *testPool) liveCount() int {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	return len(tp.live)
}

// endOne simulates the edge closing one link.
func (tp *testPool) endOne() {
	tp.mu.Lock()
	var c *fakeCarrier
	if len(tp.live) > 0 {
		c = tp.live[0]
	}
	tp.mu.Unlock()
	if c != nil {
		c.end()
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// Growing starts slots at once, clamped to [min,max].
func TestExitPoolGrowsToTarget(t *testing.T) {
	tp := newTestPool(t, 2, 8)
	tp.setTarget(3)
	eventually(t, "3 live links", func() bool { return tp.liveCount() == 3 })
	tp.setTarget(6)
	eventually(t, "6 live links", func() bool { return tp.liveCount() == 6 })
	tp.setTarget(100) // above max -> 8
	eventually(t, "8 live links (max)", func() bool { return tp.liveCount() == 8 })
	if tp.slots() != 8 {
		t.Fatalf("slots=%d, want 8", tp.slots())
	}
}

// Shrinking never closes a link by itself (the exit cannot see which carry
// users). The pool gets smaller only as links end — the edge closes idle ones —
// and those slots retire instead of redialing. Below-min targets clamp to min.
func TestExitPoolShrinksOnlyByRetiringEndedLinks(t *testing.T) {
	tp := newTestPool(t, 2, 8)
	tp.setTarget(6)
	eventually(t, "6 live links", func() bool { return tp.liveCount() == 6 })
	dialsBefore := tp.dials.Load()

	tp.setTarget(1) // clamps to 2
	time.Sleep(100 * time.Millisecond)
	if n := tp.liveCount(); n != 6 {
		t.Fatalf("lowering the target closed links on its own: %d live, want still 6", n)
	}
	// The edge closes idle links one by one: each ended slot retires.
	for want := 5; want >= 2; want-- {
		tp.endOne()
		w := want
		eventually(t, "slot retired", func() bool { return tp.slots() == w && tp.liveCount() == w })
	}
	if d := tp.dials.Load(); d != dialsBefore {
		t.Fatalf("retired slots redialed: %d extra dials", d-dialsBefore)
	}
	// At the target, a link that drops is redialed (it is not a shrink).
	tp.endOne()
	eventually(t, "redial back to 2", func() bool { return tp.liveCount() == 2 && tp.dials.Load() == dialsBefore+1 })
}

// servePoolCtl decodes the edge's 2-byte targets and applies each to the pool.
func TestServePoolCtlApplies(t *testing.T) {
	tp := newTestPool(t, 2, 32)
	tp.setTarget(2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pr, pw := io.Pipe()
	go servePoolCtl(ctx, struct {
		io.Reader
		io.WriteCloser
	}{pr, pw}, tp.exitPool)
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], 9)
	pw.Write(b[:])
	eventually(t, "target 9 applied from the wire", func() bool {
		tp.exitPool.mu.Lock()
		defer tp.exitPool.mu.Unlock()
		return tp.exitPool.want == 9
	})
	pw.Close()
}
