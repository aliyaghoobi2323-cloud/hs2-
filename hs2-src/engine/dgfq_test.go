package engine

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
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
		p, _, u, ok := s.pop(now)
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
		p, _, _, ok := s.pop(now)
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
	if _, _, u, _ := s.pop(t1); u {
		t.Fatal("urgent within fqUrgentGap of an ordinary packet")
	}
	fqPopN(t, s, t1, -1)
	t2 := t1.Add(fqUrgentGap + time.Millisecond)
	s.push(fqPkt(80, t2), 7)
	if _, _, u, _ := s.pop(t2); !u {
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
		p, _, u, ok := s.pop(now)
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
		p, _, _, ok := s.pop(now)
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
		p, _, u, _ := s.pop(now)
		got[len(*p.b)]++
		if u {
			t.Fatal("a heavy flow back after a pause was sent urgent")
		}
	}
	if got[1200] < 8 {
		t.Fatalf("the download got %d of 20 turns after the heavy flow came back (%v) — want about half", got[1200], got)
	}
}

// On a carrier that counts its data queue (LaneMark / LaneDrained), a flow's
// packet goes urgent only once everything it sent the ordinary way has left
// that queue — however long that takes (the floor rate, a policer cap): the
// review's case, a lone packet 100 ms after an ordinary one still queued.
func TestFQUrgentWaitsForTheDataQueue(t *testing.T) {
	s := newFQSched(256)
	s.off = false
	var queued, left uint64
	s.mark = func() uint64 { return queued }
	s.drained = func(m uint64) bool { return left >= m }
	now := time.Now()
	s.push(fqPkt(1500, now), 5)
	s.push(fqPkt(100, now), 5)
	_, f, u, _ := s.pop(now) // the quantum: urgent
	if !u {
		t.Fatal("first packet not urgent")
	}
	_, f, u, _ = s.pop(now) // credit spent: ordinary
	if u {
		t.Fatal("second packet urgent past the quantum")
	}
	queued = 7 // the carrier took it: its data queue has had 7 shards
	s.noteSlow(f, s.mark())
	fqPopN(t, s, now, -1)
	later := now.Add(300 * time.Millisecond) // long past fqUrgentGap
	s.push(fqPkt(100, later), 5)
	if _, _, u, _ := s.pop(later); u {
		t.Fatal("urgent while the flow's ordinary packet was still in the data queue")
	}
	left = 7 // it left (the next ordinary send moved the mark to 7 again)
	s.noteSlow(5, 7)
	fqPopN(t, s, later, -1)
	s.push(fqPkt(100, later), 5)
	if _, _, u, _ := s.pop(later); !u {
		t.Fatal("not urgent once the data queue had passed the flow's mark")
	}
}

// A flow whose queue never empties does not grow its array: what was popped
// is reclaimed (the review measured 64 MB after 2M packets).
func TestFQFlowQueueStaysBounded(t *testing.T) {
	s := newFQSched(256)
	s.off = false
	now := time.Now()
	for i := 0; i < 50; i++ {
		s.push(fqPkt(100, now), 1)
	}
	for i := 0; i < 200000; i++ {
		s.push(fqPkt(100, now), 1)
		s.pop(now)
	}
	s.mu.Lock()
	f := s.flows[1]
	n, c := f.len(), cap(f.q)
	s.mu.Unlock()
	if n != 50 || c > 1024 {
		t.Fatalf("queue %d packets in an array of %d after 200k push/pop — want 50 in a small array", n, c)
	}
}

// laneCar is a fake carrier with the pacer's two lanes and its data-queue
// counters: what the writer sends where, and when the test says the data
// queue has drained.
type laneCar struct {
	*dgFakeCarrier
	queued, left atomic.Uint64
	lanes        chan bool // true: fast lane
}

func (c *laneCar) SendFrame(ft byte, p []byte) error {
	if ft == core.TypeData {
		c.queued.Add(1)
		c.lanes <- false
	}
	return c.dgFakeCarrier.SendFrame(ft, p)
}
func (c *laneCar) SendUrgent(p []byte) error {
	c.lanes <- true
	return c.dgFakeCarrier.SendFrame(core.TypeData, p)
}
func (c *laneCar) LaneMark() uint64          { return c.queued.Load() }
func (c *laneCar) LaneDrained(m uint64) bool { return c.left.Load() >= m }

// The writer records the carrier's mark after each ordinary packet, so a
// flow's next packet takes the fast lane only once the data queue has passed
// it — end to end through dgLink.writeLoop.
func TestDgWriterKeepsAFlowsOrderAcrossLanes(t *testing.T) {
	inner, _ := newDgFakePair()
	car := &laneCar{dgFakeCarrier: inner, lanes: make(chan bool, 16)}
	l := newDgLink(car, time.Now())
	l.fq.off = false
	p := newDgPool(newFakeTUN(1400), 2, 8, 8, func(string, ...any) {})
	go l.writeLoop(&p.pool, &p.drops, &p.dropAged, &p.sentPkts)
	defer l.markDead()
	next := func() bool {
		t.Helper()
		select {
		case u := <-car.lanes:
			return u
		case <-time.After(2 * time.Second):
			t.Fatal("nothing sent")
			return false
		}
	}
	send := func(n int) {
		b := make([]byte, n)
		if ok, _ := l.enqueue(&b, 5, time.Now()); !ok {
			t.Fatal("enqueue refused")
		}
	}
	send(1500)
	send(100)
	if !next() || next() {
		t.Fatal("want the quantum urgent, then the rest ordinary")
	}
	time.Sleep(10 * time.Millisecond)
	send(100) // its ordinary packet has not left the data queue (left 0 < mark 1)
	if next() {
		t.Fatal("urgent ahead of the flow's ordinary packet still in the data queue")
	}
	car.left.Store(car.queued.Load())
	time.Sleep(10 * time.Millisecond)
	send(100)
	if !next() {
		t.Fatal("not urgent once the data queue had passed the flow's packets")
	}
}
