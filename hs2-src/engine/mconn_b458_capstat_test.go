package engine

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestB458CapStat: with the cap full a pick waits and is counted (waiting
// gauge, wait time); a latency-class pick skips the cap; the edge's latency
// class follows the mConn's stamp; joins are counted by kind.
func TestB458CapStat(t *testing.T) {
	defer b457SetPickCap(0)
	b457SetPickCap(1)
	w0, _, _ := b458Stat.wait.snap()
	x0 := b458Stat.exempt.Load()
	r1 := b457PickAcquire(context.Background())
	// exempt: no slot needed, returns at once
	done := make(chan struct{})
	go func() { b457PickAcquireEx(context.Background(), true)(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("exempt pick blocked on a full cap")
	}
	if b458Stat.exempt.Load() != x0+1 {
		t.Fatal("exempt not counted")
	}
	got := make(chan struct{})
	go func() { b457PickAcquire(context.Background())(); close(got) }()
	deadline := time.Now().Add(2 * time.Second)
	for b458Stat.waiting.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("waiting gauge never rose")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	r1()
	<-got
	w1, ms, mx := b458Stat.wait.snap()
	if w1 != w0+1 || mx < 15 || ms < 15 {
		t.Fatalf("wait counters n %d->%d total %d max %d", w0, w1, ms, mx)
	}
	if b458Stat.waiting.Load() != 0 || b458Stat.peak.Load() < 1 {
		t.Fatalf("waiting %d peak %d", b458Stat.waiting.Load(), b458Stat.peak.Load())
	}

	e := &mconnEdge{}
	if e.b458Latency() {
		t.Fatal("no mConn: not latency class")
	}
	m := &mConn{}
	e.b458MC.Store(m)
	if e.b458Latency() {
		t.Fatal("never call-like: not latency class")
	}
	m.b458Stamp(time.Now())
	if !e.b458Latency() {
		t.Fatal("call-like now: latency class")
	}
	m.b458Stamp(time.Now().Add(-callRescueTTL - time.Second))
	if e.b458Latency() {
		t.Fatal("stale stamp: not latency class")
	}

	n0, _, _ := b458Stat.joinRescue.snap()
	f0, _, _ := b458Stat.joinFail.snap()
	l0, _, _ := b458Stat.joinLat.snap()
	b458Join(time.Now().Add(-5*time.Millisecond), true, true, true)
	b458Join(time.Now(), false, false, true)
	n1, _, _ := b458Stat.joinRescue.snap()
	f1, _, _ := b458Stat.joinFail.snap()
	l1, _, _ := b458Stat.joinLat.snap()
	if n1 != n0+1 || f1 != f0+1 || l1 != l0+1 {
		t.Fatalf("join counters rescue %d->%d fail %d->%d lat %d->%d", n0, n1, f0, f1, l0, l1)
	}
	if !strings.HasPrefix(b458Line(), "b458: capwait n=") {
		t.Fatal(b458Line())
	}
}
