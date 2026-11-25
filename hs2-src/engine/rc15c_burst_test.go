package engine

import (
	"testing"
	"time"
)

func setRC15cBurst(t *testing.T, on bool, head uint64, idle time.Duration) {
	ob, oh, oi, obps := rc15cBurst, rc15cBurstBytes, rc15cBurstIdle, rc15HeavyBps
	rc15cBurst, rc15cBurstBytes, rc15cBurstIdle = on, head, idle
	rc15HeavyBps = 1 << 30 // the rate rule never heavy: isolate the burst rule
	t.Cleanup(func() { rc15cBurst, rc15cBurstBytes, rc15cBurstIdle, rc15HeavyBps = ob, oh, oi, obps })
}

// The rule alone: the first head bytes of a burst are doubled, the rest one
// copy; an idle gap with a drained primary starts a new burst; key off: all
// doubled.
func TestRC15cBurstRule(t *testing.T) {
	setRC15cBurst(t, true, 4096, 50*time.Millisecond)
	m := newMConn(93, false, nil)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rc15cBurstOneCopyLocked() {
		t.Fatal("burst head: one copy")
	}
	m.sendOff = 4000
	if m.rc15cBurstOneCopyLocked() {
		t.Fatal("4000 into the burst: one copy")
	}
	m.sendOff = 5000
	if !m.rc15cBurstOneCopyLocked() {
		t.Fatal("past the head: doubled")
	}
	m.c15Last = time.Now().Add(-100 * time.Millisecond)
	if m.rc15cBurstOneCopyLocked() {
		t.Fatal("after idle: new burst not doubled")
	}
	if m.c15Start != 5000 {
		t.Fatalf("burst start %d, want 5000", m.c15Start)
	}
	m.sendOff = 20000
	if !m.rc15cBurstOneCopyLocked() {
		t.Fatal("second burst past the head: doubled")
	}
	rc15cBurst = false
	if m.rc15cBurstOneCopyLocked() {
		t.Fatal("key off: one copy")
	}
}

// End to end, both directions, several bursts of 12 KB with idle gaps: byte
// exact, burst heads doubled, tails one copy, the hedge sub stays.
func TestRC15cBurstBytes(t *testing.T) {
	setHedge3(t, true)
	setRC15cBurst(t, true, 4096, 60*time.Millisecond)
	_, iran, exit := newHedge2Pair(t, nil)
	mWaitFor(t, "two subs", time.Second, func() bool { return liveSubs(iran) == 2 })
	exit.exitHedge3Mark(false)
	b0, o0, d0 := rc15cStat.bursts.Load(), rc15cStat.oneCopyB.Load(), hedge3Stat.dupB.Load()
	for i := 0; i < 4; i++ {
		req := randBytes(12000, uint64(40+i))
		both(t, func() error { return xfer(iran, req, 1500, 0) }, func() error { return readN(exit, req) })
		resp := randBytes(12000, uint64(60+i))
		both(t, func() error { return xfer(exit, resp, 1500, 0) }, func() error { return readN(iran, resp) })
		time.Sleep(150 * time.Millisecond)
	}
	if got := rc15cStat.bursts.Load() - b0; got < 6 {
		t.Fatalf("bursts %d, want >= 6", got)
	}
	if rc15cStat.oneCopyB.Load() == o0 {
		t.Fatal("no one-copy tail")
	}
	if hedge3Stat.dupB.Load() == d0 {
		t.Fatal("no doubled head")
	}
	for _, m := range []*mConn{iran, exit} {
		m.mu.Lock()
		h := m.h3Heavy
		m.mu.Unlock()
		if h {
			t.Fatal("heavy with the rate rule off")
		}
	}
	if liveSubs(iran) != 2 {
		t.Fatalf("iran subs %d, want 2 (hedge sub kept)", liveSubs(iran))
	}
}

// The primary killed mid-run under light bursts (each under the head, so
// doubled): no conn error, byte exact. (A one-copy tail on a dying primary
// ends the conn as the heavy one-copy path always did: "a sub died holding
// the only copy"; that is why only bytes past a burst's head go one copy.)
func TestRC15cBurstSubDeath(t *testing.T) {
	setHedge3(t, true)
	setRC15cBurst(t, true, 2048, 30*time.Millisecond)
	n, iran, exit := newHedge2Pair(t, nil)
	mWaitFor(t, "two subs", time.Second, func() bool { return liveSubs(iran) == 2 })
	exit.exitHedge3Mark(false)
	o0 := rc15cStat.oneCopyB.Load()
	for i := 0; i < 8; i++ {
		if i == 4 {
			n.link(0).iran.Close()
			n.link(0).exit.Close()
		}
		req := randBytes(1500, uint64(90+i))
		both(t, func() error { return xfer(iran, req, 500, 0) }, func() error { return readN(exit, req) })
		resp := randBytes(1800, uint64(110+i))
		both(t, func() error { return xfer(exit, resp, 600, 0) }, func() error { return readN(iran, resp) })
		time.Sleep(60 * time.Millisecond)
	}
	if rc15cStat.oneCopyB.Load() != o0 {
		t.Fatal("a light burst under the head went one copy")
	}
}
