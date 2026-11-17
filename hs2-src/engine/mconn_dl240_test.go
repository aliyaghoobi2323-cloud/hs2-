package engine

import (
	"testing"
	"time"
)

// dl240 (HS2_DL240_YOUNG, exit): a young stripe sub at its probation budget
// (unread >= max(dl240Floor, consumed)) takes no chunk while another sub has
// room; it still takes one when it is the only sub with room; an old sub,
// one under budget, and the key off are unchanged.
func TestDL240YoungProbation(t *testing.T) {
	old, oldK := stripeQTarget.Load(), dl240Young.Load()
	oldMarks := subAckMarks
	t.Cleanup(func() { stripeQTarget.Store(old); dl240Young.Store(oldK); subAckMarks = oldMarks })
	stripeQTarget.Store(0)
	withStripeSteer(t, false)
	m, p := gateConn(1)
	m.primary.Store(p)
	if m.iran {
		t.Fatal("gateConn: want the exit side")
	}
	now := time.Now()
	for _, s := range m.subs {
		s.rw = exitCountedStream{c: &exitConn{mtr: &linkMeter{}}}
		s.lastPush = now
		s.lastRecv.Store(monoNano(now))
	}
	y := m.subs[1]
	if y == p || !y.stripe {
		t.Fatal("want subs[1] a stripe sub, not the primary")
	}
	var w, c uint32
	subAckMarks = func(s *msub) (uint32, uint32, bool) {
		if s == y {
			return w, c, true
		}
		return 0, 0, true
	}
	send := func(n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			ch := getChunk(mRecData)
			ch.n = 100
			if r := m.stripeSendLocked(ch); r != stripeSent {
				t.Fatalf("chunk %d: %d, want sent", i, r)
			}
			ch.unref()
		}
	}
	drain := func() {
		for _, s := range m.subs {
			for _, ch := range s.q {
				ch.unref()
			}
			s.q, s.qBytes = nil, 0
		}
	}
	y.stJoinAt = now
	w, c = 600<<10, 0 // young, 600 KiB unread > the 512 KiB floor
	dl240Young.Store(false)
	send(20)
	if len(y.q) == 0 {
		t.Fatal("key off: the young sub got nothing")
	}
	drain()
	dl240Young.Store(true)
	h0 := dl240Stat.held.Load()
	send(20)
	if len(y.q) != 0 || dl240Stat.held.Load() == h0 {
		t.Fatalf("young over budget: got %d chunks", len(y.q))
	}
	drain()
	// proven delivery: consumed 4 MiB, 600 KiB unread is under it
	w, c = 4<<20+600<<10, 4<<20
	send(20)
	if len(y.q) == 0 {
		t.Fatal("young under its consumed budget: got nothing")
	}
	drain()
	// old enough: no probation
	w, c = 600<<10, 0
	y.stJoinAt = now.Add(-dl240Age - time.Millisecond)
	send(20)
	if len(y.q) == 0 {
		t.Fatal("past dl240Age: got nothing")
	}
	drain()
	// the only sub with room: the young sub still takes it (no wait)
	y.stJoinAt = now
	p.dead = true
	f0 := dl240Stat.fallback.Load()
	send(5)
	if len(y.q) == 0 || dl240Stat.fallback.Load() == f0 {
		t.Fatalf("only sub: got %d chunks", len(y.q))
	}
	p.dead = false
	drain()
}
