package engine

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCarrier is a stand-in for a dialed reverse link. end() simulates the edge
// closing it cleanly; endWith(reason) the link going away for another reason
// (the network dropping it).
type fakeCarrier struct {
	closed atomic.Bool
	done   chan struct{}
	once   sync.Once
	why    string // why the link ended, as serve reports it
}

func newFakeCarrier() *fakeCarrier            { return &fakeCarrier{done: make(chan struct{})} }
func (c *fakeCarrier) Close() error           { c.closed.Store(true); c.end(); return nil }
func (c *fakeCarrier) ended() <-chan struct{} { return c.done }

// end: the link ends the way the edge closes one (clean TLS close -> EOF).
func (c *fakeCarrier) end() { c.endWith("read: " + reasonPeerClosed) }

// endWith: the link ends for the given reason (e.g. a network reset).
func (c *fakeCarrier) endWith(why string) {
	c.once.Do(func() { c.why = why; close(c.done) })
}

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
	tp.serve = func(ctx context.Context, c dialedLink) string {
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
		return fc.why
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
func (tp *testPool) endOne() { tp.endOneWith("read: " + reasonPeerClosed) }

// endOneWith ends one live link for the given reason.
func (tp *testPool) endOneWith(why string) {
	tp.mu.Lock()
	var c *fakeCarrier
	if len(tp.live) > 0 {
		c = tp.live[0]
	}
	tp.mu.Unlock()
	if c != nil {
		c.endWith(why)
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

// The reverse exit's log must tell the edge shrinking the pattern (a clean
// close of an idle link — normal) from a link lost to the network while the
// pool is above target (a fault). Both retire the slot; only the words differ,
// and the loss carries its reason. (Field report: a link killed with `ss -K`
// was logged as "retired — pattern shrinking".)
func TestExitPoolLogTellsShrinkFromLoss(t *testing.T) {
	tp := newTestPool(t, 2, 8)
	var mu sync.Mutex
	var logs []string
	tp.log = func(f string, a ...any) { mu.Lock(); logs = append(logs, fmt.Sprintf(f, a...)); mu.Unlock() }
	has := func(sub string) func() bool {
		return func() bool {
			mu.Lock()
			defer mu.Unlock()
			for _, l := range logs {
				if strings.Contains(l, sub) {
					return true
				}
			}
			return false
		}
	}
	tp.setTarget(4)
	eventually(t, "4 live links", func() bool { return tp.liveCount() == 4 })
	tp.setTarget(2) // the pool is now above target

	tp.endOne() // the edge closes an idle link
	eventually(t, "shrink logged as the edge's decision", has("retired — the edge shrank the pattern (now 3)"))

	const reset = "read: reset by the network or the other server"
	tp.endOneWith(reset) // the network kills a link
	eventually(t, "loss logged as a loss, with its reason", has("lost ("+reset+") — not redialed, pool above target (now 2)"))
	if has("retired — the edge shrank the pattern (now 2)")() {
		t.Fatal("a network loss was logged as the edge shrinking the pattern")
	}

	// At target: a lost link is redialed, and the log says why it went down.
	tp.endOneWith(reset)
	eventually(t, "redial logged with its reason", has("exit link down (slot"))
	eventually(t, "redial back to 2", func() bool { return tp.liveCount() == 2 })
	if !has(reset + "; now 1); redial")() {
		mu.Lock()
		t.Fatalf("redial line lacks the reason:\n%s", strings.Join(logs, "\n"))
	}
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

// ---- the exit pool against the v2 reverse edge --------------------------------

// carrierLink is the edge's side of a fake exit carrier: alive until the
// carrier ends, and closing it ends the carrier (the exit sees its link go).
type carrierLink struct{ c *fakeCarrier }

func (l *carrierLink) OpenStream() (stream, error) { return nil, nil }
func (l *carrierLink) Active() int32               { return 0 }
func (l *carrierLink) Close() error                { return l.c.Close() }
func (l *carrierLink) Alive() bool {
	select {
	case <-l.c.ended():
		return false
	default:
		return true
	}
}

// edgeExitPair couples an exit pool of fake carriers with a reverse edge
// (accept-mode LinkManager on a fake clock), wired as acceptReverseLinks
// wires them: every carrier the exit dials arrives at the edge through
// AddLink, and leaves it through DropLink when it ends. The exit's target is
// set by the test, standing in for kindPool.
type edgeExitPair struct {
	tp  *testPool
	lm  *LinkManager
	clk *v2Clock
}

func newEdgeExitPair(t *testing.T, edgeT, exitWarm int) *edgeExitPair {
	tp := newTestPool(t, 2, 8)
	lm, clk, _ := newV2Manager(nil, 2, 8, true)
	lm.setTarget(edgeT)
	hold := tp.serve
	tp.serve = func(ctx context.Context, c dialedLink) string {
		l := &carrierLink{c: c.(*fakeCarrier)}
		lm.AddLink(l, "exit")
		why := hold(ctx, c) // until the carrier ends or its slot is cancelled
		lm.DropLink(l, "exit")
		return why
	}
	tp.setTarget(exitWarm)
	return &edgeExitPair{tp: tp, lm: lm, clk: clk}
}

// entries returns the edge's alive serving and retiring entries.
func (p *edgeExitPair) entries() (serving, retiring []*managedLink) {
	p.lm.mu.RLock()
	defer p.lm.mu.RUnlock()
	for _, ml := range p.lm.links {
		switch {
		case !ml.link.Alive() || ml.degraded || ml.draining:
		case ml.retiring:
			retiring = append(retiring, ml)
		default:
			serving = append(serving, ml)
		}
	}
	return
}

func (p *edgeExitPair) state(S, R, slots int) func() bool {
	return func() bool {
		s, r := poolCounts(p.lm)
		return s == S && r == R && p.tp.slots() == slots && p.tp.liveCount() == slots
	}
}

// The exit's slots track the edge's physical links, serving + retiring: links
// it dialed beyond the edge's target are born retiring, and as the edge closes
// each one once it is empty the exit retires that slot — no redial, ever.
func TestExitPoolSlotsTrackEdgeServingPlusRetiring(t *testing.T) {
	p := newEdgeExitPair(t, 3, 8) // edge wants 3; exit comes up warm at 8
	eventually(t, "8 links at the edge: 3 serving + 5 born retiring", p.state(3, 5, 8))
	p.tp.setTarget(p.lm.Target()) // the exit learns the edge's target
	_, retiring := p.entries()
	retiring[0].users.Store(1) // two surplus links hold connections
	retiring[1].users.Store(1)
	p.clk.Advance(bornSpareGrace + time.Second) // born spare: kept for its grace first
	for i := 0; i < 3; i++ {
		p.lm.drainTick()
	}
	eventually(t, "3 empty surplus links closed and their slots retired: 5 = 3 + 2", p.state(3, 2, 5))

	retiring[0].users.Store(0)
	retiring[1].users.Store(0)
	p.lm.drainTick()
	eventually(t, "the held links closed once empty: 3 = 3 + 0", p.state(3, 0, 3))
	time.Sleep(50 * time.Millisecond)
	if d := p.tp.dials.Load(); d != 8 {
		t.Fatalf("the exit redialed %d link(s) the edge retired", d-8)
	}
}

// A serving link that dies while the edge has retiring links: the exit, above
// its target, retires that slot instead of redialing, and the edge brings a
// retiring link back into service — no dial on either side.
func TestExitPoolServingDeathWithRetiringRetiresSlot(t *testing.T) {
	p := newEdgeExitPair(t, 3, 5)
	eventually(t, "5 links: 3 serving + 2 retiring", p.state(3, 2, 5))
	p.tp.setTarget(3)
	serving, retiring := p.entries()
	for _, ml := range retiring {
		ml.users.Store(1)
	}
	serving[0].link.(*carrierLink).c.end() // lost on the network
	eventually(t, "slot retired, link dropped: 4 = 2 + 2", p.state(2, 2, 4))
	p.lm.reconcile(context.Background(), 3)
	if s, r := poolCounts(p.lm); s != 3 || r != 1 || p.tp.slots() != s+r {
		t.Fatalf("after reconcile: %d serving + %d retiring, %d exit slots; want 3 + 1 = 4", s, r, p.tp.slots())
	}
	time.Sleep(50 * time.Millisecond)
	if d := p.tp.dials.Load(); d != 5 {
		t.Fatalf("%d redial(s) although a retiring link could take over", d-5)
	}
}

// With no retiring link a serving link that dies is redialed, and the new link
// arrives at the edge serving.
func TestExitPoolServingDeathWithoutRetiringRedials(t *testing.T) {
	p := newEdgeExitPair(t, 3, 3)
	eventually(t, "3 serving links", p.state(3, 0, 3))
	p.tp.setTarget(3)
	serving, _ := p.entries()
	serving[0].link.(*carrierLink).c.end()
	eventually(t, "redialed back to 3 serving", func() bool { return p.state(3, 0, 3)() && p.tp.dials.Load() == 4 })
}
