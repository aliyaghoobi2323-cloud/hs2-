package engine

import (
	"sync/atomic"
	"testing"
	"time"
)

// stallLink is a lane link whose socket view reports in-flight data; acked
// and backoff are what the test sets.
func stallLink(id uint32) (*l3Link, *atomic.Uint64, *atomic.Uint32) {
	l := laneLink(id, 0)
	var acked atomic.Uint64
	var backoff atomic.Uint32
	l.txStat = func() (tcpStat, bool) {
		return tcpStat{unacked: 3, bytesAcked: acked.Load(), backoff: uint8(backoff.Load()), rttUs: 80000}, true
	}
	return l, &acked, &backoff
}

// A ping flow pinned to a link whose TCP stops getting acks (data-dark path)
// leaves it within the stall window; with the key off it stays there.
func TestB278LaneLeavesStuckLink(t *testing.T) {
	for _, on := range []bool{true, false} {
		old := b278On.Load()
		b278On.Store(on)
		var clock int64 = int64(time.Hour)
		links := make([]*l3Link, 0, 4)
		acks := make([]*atomic.Uint64, 0, 4)
		for i := uint32(1); i <= 4; i++ {
			l, a, _ := stallLink(i)
			links = append(links, l)
			acks = append(acks, a)
		}
		s := laneSet(&clock, links...)
		ping := icmpPacket(0, 2)
		first := s.pick(ping)
		var fi int
		for i, l := range links {
			if l == first {
				fi = i
			}
		}
		// every link but the first keeps getting acks; the first does not
		var moved *l3Link
		for step := 0; step < 40; step++ {
			clock += int64(50 * time.Millisecond)
			for i, a := range acks {
				if i != fi {
					a.Add(1000)
				}
			}
			if l := s.pick(ping); l != first {
				moved = l
				break
			}
		}
		if on && moved == nil {
			t.Fatalf("key on: ping stayed on the stuck link for 2 s")
		}
		if on && moved.txStuck(clock) {
			t.Fatalf("key on: moved to a stuck link")
		}
		if !on && moved != nil {
			t.Fatalf("key off: ping moved (behaviour must be as before)")
		}
		b278On.Store(old)
	}
}

// RTO backoff marks a link stuck at once; the dup's second copy skips it.
func TestB278DupSecondSkipsBackoff(t *testing.T) {
	old := b278On.Load()
	defer b278On.Store(old)
	b278On.Store(true)
	var clock int64 = int64(time.Hour)
	var links []*l3Link
	var bos []*atomic.Uint32
	for i := uint32(1); i <= 5; i++ {
		l, _, bo := stallLink(i)
		l.dupOK = func() bool { return true }
		links = append(links, l)
		bos = append(bos, bo)
	}
	h := uint32(12345)
	first := links[0]
	second := dupSecond(links, h, first)
	for i, l := range links {
		if l == second {
			bos[i].Store(2)
		}
	}
	got := b278Second(links, h, first, second, clock)
	if got == nil || got == second || got == first {
		t.Fatalf("second copy not moved off the backoff link: %v", got)
	}
	if got.txStuck(clock) {
		t.Fatalf("second copy on a stuck link")
	}
	// a link with nothing in flight is never stuck
	idle := laneLink(9, 0)
	idle.txStat = func() (tcpStat, bool) { return tcpStat{backoff: 3}, true }
	if idle.txStuck(clock) {
		t.Fatalf("link with nothing in flight called stuck")
	}
	// a link read rarely (1 pps ping) with a packet just in flight and no
	// ack since the last read is not stuck: that read did not watch the
	// window; with reads every 50 ms the same socket is stuck after 400 ms
	rare, _, _ := stallLink(11)
	if rare.txStuck(clock) || rare.txStuck(clock+int64(time.Second)) || rare.txStuck(clock+int64(2*time.Second)) {
		t.Fatalf("rarely read idle link called stuck")
	}
	c := clock + int64(2*time.Second)
	for i := 0; i < 9; i++ {
		c += int64(50 * time.Millisecond)
		rare.txStuck(c)
	}
	if !rare.txStuck(c + int64(50*time.Millisecond)) {
		t.Fatalf("link read every 50 ms with no ack for 500 ms not stuck")
	}
	// no socket view: never stuck
	if laneLink(10, 0).txStuck(clock) {
		t.Fatalf("link without txStat called stuck")
	}
}

// The dup's second copy leaves the first copy's path when another path has a
// link (lab3 FZ3: both copies on the cut path); key off: as before.
func TestB278DupSecondOtherPath(t *testing.T) {
	old := b278On.Load()
	defer b278On.Store(old)
	var links []*l3Link
	for i := uint32(1); i <= 6; i++ {
		l, _, _ := stallLink(i)
		l.dupOK = func() bool { return true }
		l.path = "p1"
		if i == 6 {
			l.path = "p2"
		}
		links = append(links, l)
	}
	var clock int64 = int64(time.Hour)
	first := links[0]
	for h := uint32(0); h < 64; h++ {
		second := dupSecond(links, h, first)
		b278On.Store(true)
		if got := b278Second(links, h, first, second, clock); got == nil || got.path != "p2" {
			t.Fatalf("h %d: second copy on %v, want path p2", h, got)
		}
		b278On.Store(false)
		if got := b278Second(links, h, first, second, clock); got != second {
			t.Fatalf("key off changed the second copy")
		}
	}
}
