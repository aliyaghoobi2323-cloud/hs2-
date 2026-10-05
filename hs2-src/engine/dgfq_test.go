package engine

import (
	"testing"
	"time"
)

func fqPkt(n int, t time.Time) qpkt {
	b := make([]byte, n)
	return qpkt{b: &b, t: t}
}

// fqPopN pops up to n packets (n < 0: until empty, as the carrier's writer
// does), returning their sizes and urgency.
func fqPopN(t *testing.T, s *fqSched, now time.Time, n int) (sizes []int, urgent []bool) {
	t.Helper()
	for i := 0; n < 0 || i < n; i++ {
		p, u, ok := s.pop(now)
		if !ok {
			break
		}
		sizes = append(sizes, len(*p.b))
		urgent = append(urgent, u)
	}
	return
}

// A packet of a flow with nothing queued goes before a download's backlog,
// and is marked urgent (the pacer's fast lane).
func TestFQInteractiveFirst(t *testing.T) {
	s := newFQSched(256)
	s.off = false
	now := time.Now()
	for i := 0; i < 100; i++ {
		s.push(fqPkt(1200, now), 1) // the download
	}
	fqPopN(t, s, now, 5)      // it has been sending: on the old list now
	s.push(fqPkt(80, now), 2) // a game packet
	sizes, urg := fqPopN(t, s, now, 1)
	if len(sizes) != 1 || sizes[0] != 80 || !urg[0] {
		t.Fatalf("next popped: sizes %v urgent %v — want the 80-byte packet, urgent", sizes, urg)
	}
	sizes, urg = fqPopN(t, s, now, 1)
	if sizes[0] != 1200 || urg[0] {
		t.Fatalf("then: %v %v — want the download, not urgent", sizes, urg)
	}
}

// Downloads share the carrier evenly by bytes, whatever their packet sizes.
func TestFQFairByBytes(t *testing.T) {
	s := newFQSched(1024)
	s.off = false
	now := time.Now()
	for i := 0; i < 300; i++ {
		s.push(fqPkt(1200, now), 1)
		s.push(fqPkt(600, now), 2)
		s.push(fqPkt(600, now), 2)
	}
	by := map[int]int{}
	for i := 0; i < 300; i++ {
		p, _, ok := s.pop(now)
		if !ok {
			t.Fatal("ran dry")
		}
		by[len(*p.b)] += len(*p.b)
	}
	a, b := float64(by[1200]), float64(by[600])
	if r := a / b; r < 0.8 || r > 1.25 {
		t.Fatalf("bytes 1200-flow %v vs 600-flow %v: ratio %.2f, want ~1", a, b, r)
	}
}

// A full queue drops the head of the flow with the most queued — not the
// newcomer of a flow with little queued.
func TestFQDropsFromTheFattest(t *testing.T) {
	s := newFQSched(10)
	s.off = false
	now := time.Now()
	for i := 0; i < 10; i++ {
		if d := s.push(fqPkt(1200, now.Add(time.Duration(i))), 1); d != nil {
			t.Fatalf("dropped before full at %d", i)
		}
	}
	d := s.push(fqPkt(80, now), 2)
	if d == nil || len(*d.b) != 1200 || !d.t.Equal(now) {
		t.Fatalf("dropped %v — want the download's head (oldest)", d)
	}
	if s.queued() != 10 {
		t.Fatalf("queued %d, want 10", s.queued())
	}
	// The fattest pushing again when full: its own oldest goes.
	if d := s.push(fqPkt(1200, now.Add(time.Hour)), 1); d == nil || d.b == nil || d.t.After(now.Add(time.Second)) {
		t.Fatalf("the fat flow's newcomer was dropped instead of its head: %v", d)
	}
}

// A flow that went the ordinary way in the last fqUrgentGap is not urgent
// (its ordinary packets may still wait in the pacer: an urgent one would
// overtake them); after the gap it is again.
func TestFQNoOvertake(t *testing.T) {
	s := newFQSched(256)
	s.off = false
	t0 := time.Now()
	for i := 0; i < 5; i++ {
		s.push(fqPkt(1200, t0), 7) // a burst: one quantum urgent, the rest ordinary
	}
	_, urg := fqPopN(t, s, t0, 5)
	if !urg[0] {
		t.Fatal("the burst's first packet was not urgent")
	}
	ordinary := false
	for _, u := range urg[1:] {
		if !u {
			ordinary = true
		}
		if u && ordinary {
			t.Fatalf("an urgent packet after an ordinary one in the same burst: %v", urg)
		}
	}
	if !ordinary {
		t.Fatalf("a 5-packet burst went all urgent: %v", urg)
	}
	t1 := t0.Add(fqUrgentGap / 2)
	s.push(fqPkt(80, t1), 7)
	if _, u, _ := s.pop(t1); u {
		t.Fatal("urgent within fqUrgentGap of an ordinary packet")
	}
	fqPopN(t, s, t1, -1)
	t2 := t1.Add(fqUrgentGap + time.Millisecond)
	s.push(fqPkt(80, t2), 7)
	if _, u, _ := s.pop(t2); !u {
		t.Fatal("not urgent after fqUrgentGap")
	}
}

// HS2_DG_FQ=0: one FIFO — arrival order, the newcomer dropped when full,
// never urgent.
func TestFQOffIsTheFIFO(t *testing.T) {
	s := newFQSched(4)
	s.off = true
	now := time.Now()
	for i, f := range []uint32{1, 2, 1, 3} {
		s.push(fqPkt(100+i, now), f)
	}
	if d := s.push(fqPkt(999, now), 9); d == nil || len(*d.b) != 999 {
		t.Fatalf("full FIFO dropped %v, want the newcomer", d)
	}
	sizes, urg := fqPopN(t, s, now, 4)
	for i, n := range sizes {
		if n != 100+i || urg[i] {
			t.Fatalf("FIFO order/urgency: %v %v", sizes, urg)
		}
	}
}

// Idle flows' records are forgotten after fqForget; queued ones never.
func TestFQForgetsIdleFlows(t *testing.T) {
	s := newFQSched(256)
	s.off = false
	t0 := time.Now()
	for f := uint32(1); f <= 50; f++ {
		s.push(fqPkt(80, t0), f)
	}
	fqPopN(t, s, t0, -1)
	s.push(fqPkt(80, t0), 999)
	s.pop(t0.Add(fqForget + 2*time.Second)) // sweeps; flow 999's packet popped after
	s.mu.Lock()
	n := len(s.flows)
	s.mu.Unlock()
	if n > 1 {
		t.Fatalf("%d flow records kept after fqForget, want at most the one just served", n)
	}
}

// A sparse flow sharing the carrier with several downloads gets every packet
// out first and urgent — not only the first after an idle spell (fq_codel
// kept a flow that just emptied on its old list until its turn came round).
func TestFQSparseFlowStaysAhead(t *testing.T) {
	s := newFQSched(1024)
	s.off = false
	now := time.Now()
	for f := uint32(1); f <= 10; f++ { // ten downloads, always backlogged
		for i := 0; i < 60; i++ {
			s.push(fqPkt(1200, now), f)
		}
	}
	fqPopN(t, s, now, 30)
	for k := 0; k < 20; k++ { // a game: 80 bytes every 16 ms
		now = now.Add(16 * time.Millisecond)
		s.push(fqPkt(80, now), 99)
		p, u, ok := s.pop(now)
		if !ok || len(*p.b) != 80 || !u {
			t.Fatalf("game packet %d: popped %d bytes urgent=%v — want it first, urgent", k, len(*p.b), u)
		}
		fqPopN(t, s, now, 2) // the downloads move on meanwhile
	}
}

// A heavy flow paced just under the carrier's rate (its queue empties between
// packets) does not keep the head start: past fqSparseRate it takes turns
// with the downloads, which keep their share.
func TestFQHeavySmoothFlowDoesNotStarve(t *testing.T) {
	s := newFQSched(1024)
	s.off = false
	now := time.Now()
	for i := 0; i < 400; i++ {
		s.push(fqPkt(1200, now), 1) // a download, backlogged
	}
	got := map[int]int{}
	for k := 0; k < 300; k++ { // a smooth 1200-byte flow arriving each 1 ms: ~9.6 Mbit/s
		now = now.Add(time.Millisecond)
		s.push(fqPkt(1201, now), 2)
		p, _, ok := s.pop(now)
		if !ok {
			t.Fatal("ran dry")
		}
		got[len(*p.b)]++
	}
	t.Logf("turns: download %d, smooth heavy flow %d", got[1200], got[1201])
	if got[1200] < 135 {
		t.Fatalf("the download got %d of 300 turns against a smooth heavy flow (%v) — want about half", got[1200], got)
	}
}

// A heavy flow back after a short pause (its bucket still spent) takes its
// turn with the backlog: no head start, not urgent.
func TestFQHeavyFlowBackFromPauseHasNoHeadStart(t *testing.T) {
	s := newFQSched(1024)
	s.off = false
	now := time.Now()
	for i := 0; i < 200; i++ {
		s.push(fqPkt(1200, now), 1) // a download, backlogged
	}
	for i := 0; i < 20; i++ {
		s.push(fqPkt(1201, now), 2) // a burst of 24 KB: past fqSparseBurst
	}
	for k := 0; k < 100; k++ { // serve until flow 2 has emptied and left the lists
		s.pop(now)
	}
	s.mu.Lock()
	f2 := s.flows[2]
	gone := f2 != nil && f2.len() == 0 && f2.list == fqNone
	s.mu.Unlock()
	if !gone {
		t.Fatal("setup: flow 2 still queued or listed")
	}
	now = now.Add(5 * time.Millisecond)
	for i := 0; i < 20; i++ {
		s.push(fqPkt(1201, now), 2)
	}
	s.mu.Lock()
	list := s.flows[2].list
	s.mu.Unlock()
	if list != fqOld {
		t.Fatalf("a heavy flow back after 5 ms went on list %d, want the old list (no head start)", list)
	}
	got := map[int]int{}
	for k := 0; k < 20; k++ {
		p, u, _ := s.pop(now)
		got[len(*p.b)]++
		if u {
			t.Fatal("a heavy flow back after a pause was sent urgent")
		}
	}
	if got[1200] < 8 {
		t.Fatalf("the download got %d of 20 turns after the heavy flow came back (%v) — want about half", got[1200], got)
	}
}
