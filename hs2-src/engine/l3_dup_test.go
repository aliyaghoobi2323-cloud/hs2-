package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

func yes() bool { return true }
func no() bool  { return false }

// dupLink is a lane link with a queue whose peer does (cap) or does not read
// TypeDataDup.
func dupLink(id uint32, heavy int32, cap bool) *l3Link {
	l := laneLink(id, heavy)
	l.q = make(chan qpkt, l3QueueLen)
	if cap {
		l.dupOK = yes
	} else {
		l.dupOK = no
	}
	return l
}

// dupSet is a set with the lane and the dup on.
func dupSet(links ...*l3Link) *l3Set {
	var clock int64 = 1
	s := laneSet(&clock, links...)
	s.dup = true
	return s
}

// queued drains a link's queue.
func queued(l *l3Link) []qpkt {
	var out []qpkt
	for {
		select {
		case p := <-l.q:
			out = append(out, p)
		default:
			return out
		}
	}
}

// pumpOne runs pumpTun's per-packet step for pkt.
func pumpOne(s *l3Set, pkt []byte) {
	b := append([]byte(nil), pkt...)
	bp := &b
	l, l2 := s.pickDup(b)
	if l2 != nil {
		s.sendDup(bp, l, l2)
		return
	}
	if l != nil {
		l.enqueue(bp)
	}
}

// (1) With two links and the dup on, every ping goes out on both, with the
// same id, the second copy flagged; the old pick sent it once.
func TestL3DupPingOnBothLinks(t *testing.T) {
	a, b := dupLink(1, 0, true), dupLink(2, 0, true)
	s := dupSet(a, b)
	before := obs.l3DupSent.Load()
	for i := 0; i < 500; i++ {
		p := icmpPacket(byte(i>>8), byte(i))
		pumpOne(s, p)
		qa, qb := queued(a), queued(b)
		if len(qa) != 1 || len(qb) != 1 {
			t.Fatalf("ping %d queued %d/%d times, want 1/1", i, len(qa), len(qb))
		}
		x, y := qa[0], qb[0]
		first, second := x, y
		if x.id&l3DupSecondBit != 0 {
			first, second = y, x
		}
		if !x.dup || !y.dup || first.id&l3DupSecondBit != 0 || second.id != first.id|l3DupSecondBit {
			t.Fatalf("ping %d: ids %x/%x dup %v/%v", i, x.id, y.id, x.dup, y.dup)
		}
		if !bytes.Equal(*x.b, p) || !bytes.Equal(*y.b, p) || x.b == y.b {
			t.Fatalf("ping %d: copies differ or share a buffer", i)
		}
	}
	if got := obs.l3DupSent.Load() - before; got < 500 {
		t.Fatalf("dup sent counter moved by %d, want >= 500", got)
	}
	// one live link: one plain copy
	b.dead.Store(true)
	pumpOne(s, icmpPacket(0, 1))
	if q := queued(a); len(q) != 1 || q[0].dup {
		t.Fatalf("one live link: %d copies (dup %v)", len(q), len(q) > 0 && q[0].dup)
	}
}

// The second link avoids a link carrying a download when it can.
func TestL3DupSecondPrefersLight(t *testing.T) {
	links := []*l3Link{dupLink(1, 0, true), dupLink(2, 3, true), dupLink(3, 0, true)}
	s := dupSet(links...)
	for i := 0; i < 1000; i++ {
		l, l2 := s.pickDup(icmpPacket(byte(i>>8), byte(i)))
		if l == nil || l2 == nil || l == l2 || l.isHeavy() || l2.isHeavy() {
			t.Fatalf("ping %d: links %p %p", i, l, l2)
		}
	}
	// only one light link: the second is the heavy one
	links[2].dead.Store(true)
	if l, l2 := s.pickDup(icmpPacket(9, 9)); l != links[0] || l2 != links[1] {
		t.Fatal("no second link when only a heavy one is left")
	}
}

// (4) A link whose peer does not read TypeDataDup never gets one.
func TestL3DupOnlyToCapablePeer(t *testing.T) {
	a, b := dupLink(1, 0, false), dupLink(2, 0, false)
	s := dupSet(a, b)
	for i := 0; i < 500; i++ {
		pumpOne(s, icmpPacket(byte(i>>8), byte(i)))
	}
	for _, l := range []*l3Link{a, b} {
		for _, p := range queued(l) {
			if p.dup {
				t.Fatal("TypeDataDup queued for a peer without capL3Dup")
			}
		}
	}
	// mixed: a capable first link never pairs with an incapable second
	c, d := dupLink(3, 0, true), dupLink(4, 0, false)
	s = dupSet(c, d)
	for i := 0; i < 500; i++ {
		pumpOne(s, icmpPacket(byte(i>>8), byte(i)))
	}
	for _, p := range queued(d) {
		if p.dup {
			t.Fatal("TypeDataDup queued for a peer without capL3Dup")
		}
	}
	// the info bit is announced only with the key on
	defer func(v bool) { l3DupOn = v }(l3DupOn)
	l3DupOn = false
	if l3DupCap() != 0 || (KharejConfig{}).info(1).Caps&capL3Dup != 0 {
		t.Fatal("capL3Dup announced with HS2_L3_DUP off")
	}
	l3DupOn = true
	if l3DupCap() != capL3Dup || (KharejConfig{}).info(1).Caps&capL3Dup == 0 {
		t.Fatal("capL3Dup not announced with HS2_L3_DUP on")
	}
	pi := &peerInfo{V2: true, Caps: capPortTags | capL3Quiet}
	if pi.l3Dup() {
		t.Fatal("older peer read as dup-capable")
	}
	pi.Caps |= capL3Dup
	if !pi.l3Dup() || peerL3Dup(nil)() {
		t.Fatal("capL3Dup not read back")
	}
}

// (5) Key off: no dup state, the pick is the plain one and the frames on the
// wire are byte for byte the old TypeData frames.
func TestL3DupOffUnchanged(t *testing.T) {
	defer func(a, b bool) { l3DupOn, l3LaneOn = a, b }(l3DupOn, l3LaneOn)
	l3DupOn, l3LaneOn = false, false
	s := newL3Set()
	if s.dup || s.lane != nil {
		t.Fatal("dup or lane set up with both keys off")
	}
	links := []*l3Link{dupLink(1, 0, true), dupLink(2, 0, true)}
	for _, l := range links {
		s.add(l)
	}
	for i := 0; i < 1000; i++ {
		p := icmpPacket(byte(i>>8), byte(i))
		if l, l2 := s.pickDup(p); l2 != nil || l != rendezvous(links, flowHash(p), false) {
			t.Fatal("key off: not the plain pick")
		}
	}
	l3LaneOn = true // the lane alone does not dup
	s = newL3Set()
	if s.dup {
		t.Fatal("dup on with only HS2_L3_LANE")
	}
	for _, l := range links {
		s.add(l)
	}
	if _, l2 := s.pickDup(icmpPacket(1, 1)); l2 != nil {
		t.Fatal("lane alone sent a ping twice")
	}
	l3DupOn = true
	if s := newL3Set(); !s.dup || s.lane == nil {
		t.Fatal("HS2_L3_DUP on: no dup or no lane")
	}

	// writeLoop bytes for plain packets: exactly AppendFrame's
	car := &capPkt{}
	l := newL3Link(car)
	l.ka = time.Hour
	var pool sync.Pool
	var drops atomic.Uint64
	pkts := [][]byte{icmpPacket(1, 2), udpPacket(5, 6, 200)}
	for _, p := range pkts {
		b := append([]byte(nil), p...)
		l.enqueue(&b)
	}
	go l.writeLoop(&pool, &drops)
	defer l.markDead()
	var want []byte
	for _, p := range pkts {
		want = tlscarrier.AppendFrame(want, core.TypeData, p)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !bytes.Equal(car.bytes(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("wire bytes %x, want %x", car.bytes(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// capPkt records what is written to it.
type capPkt struct {
	mu sync.Mutex
	b  []byte
}

func (c *capPkt) WriteRaw(b []byte) error {
	c.mu.Lock()
	c.b = append(c.b, b...)
	c.mu.Unlock()
	return nil
}
func (c *capPkt) bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.b...)
}
func (c *capPkt) ReadFrameReuse() (byte, []byte, error) { select {} }
func (c *capPkt) SetReadDeadline(time.Time)             {}
func (c *capPkt) Close() error                          { return nil }

// (6) A bulk flow is never sent twice; small UDP is.
func TestL3DupBulkFlowOnce(t *testing.T) {
	a, b := dupLink(1, 0, true), dupLink(2, 0, true)
	s := dupSet(a, b)
	for i := 0; i < 300; i++ {
		for k := 0; k < 30; k++ {
			if _, l2 := s.pickDup(udpPacket(uint16(i), 443, 1400)); l2 != nil {
				t.Fatalf("bulk flow %d packet %d sent twice", i, k)
			}
		}
	}
	if _, l2 := s.pickDup(udpPacket(7, 27015, 100)); l2 == nil {
		t.Fatal("small udp not sent twice")
	}
}

// (2) The receiver passes the first copy of each id, drops the second, keeps
// the order, and survives wrap and a sender restart.
func TestL3DedupWindow(t *testing.T) {
	var d l3Dedup
	start := uint32(l3DupIDMask - 100) // wraps inside the run
	for i := uint32(0); i < 20000; i++ {
		id := (start + i) & l3DupIDMask
		if !d.first(id) {
			t.Fatalf("id %d: first copy dropped", id)
		}
		if d.first(id) {
			t.Fatalf("id %d: second copy passed", id)
		}
		if i >= 50 && d.first((id-50)&l3DupIDMask) {
			t.Fatalf("id %d: late copy 50 back passed", id)
		}
	}
	// out of order inside the window: each passes once
	top := (start + 20000) & l3DupIDMask
	for _, k := range []uint32{5, 1, 3, 2, 4} {
		if !d.first((top + k) & l3DupIDMask) {
			t.Fatalf("+%d dropped", k)
		}
	}
	for _, k := range []uint32{1, 2, 3, 4, 5} {
		if d.first((top + k) & l3DupIDMask) {
			t.Fatalf("+%d passed twice", k)
		}
	}
	// a jump ahead past the window forgets everything older
	if !d.first((top + 10000) & l3DupIDMask) {
		t.Fatal("jump ahead dropped")
	}
	// a sender restart (far behind): start over, nothing lost
	far := (top + 10000 - 1<<30) & l3DupIDMask
	for i := uint32(0); i < 100; i++ {
		if !d.first((far + i) & l3DupIDMask) {
			t.Fatalf("after restart: id %d dropped", i)
		}
	}
	// malformed payload
	if d.accept([]byte{1, 2}) != nil {
		t.Fatal("short payload accepted")
	}
}

// chanPkt is one end of an in-memory framed link; delay holds every write
// (a link stuck in TCP loss recovery).
type chanPkt struct {
	in, out   chan []byte
	delay     time.Duration
	done      chan struct{}
	closeOnce sync.Once
	buf       []byte
}

func chanPktPair(delayAB time.Duration) (*chanPkt, *chanPkt) {
	ab, ba := make(chan []byte, 1024), make(chan []byte, 1024)
	a := &chanPkt{in: ba, out: ab, delay: delayAB, done: make(chan struct{})}
	b := &chanPkt{in: ab, out: ba, done: make(chan struct{})}
	return a, b
}

func (c *chanPkt) WriteRaw(b []byte) error {
	if c.delay > 0 {
		select {
		case <-time.After(c.delay):
		case <-c.done:
			return errors.New("closed")
		}
	}
	select {
	case c.out <- append([]byte(nil), b...):
		return nil
	case <-c.done:
		return errors.New("closed")
	}
}

func (c *chanPkt) ReadFrameReuse() (byte, []byte, error) {
	for len(c.buf) < 7 {
		select {
		case b := <-c.in:
			c.buf = append(c.buf, b...)
		case <-c.done:
			return 0, nil, errors.New("closed")
		}
	}
	n := int(c.buf[1])<<16 | int(c.buf[2])<<8 | int(c.buf[3])
	ft, p := c.buf[0], c.buf[7:7+n]
	c.buf = c.buf[7+n:]
	return ft, p, nil
}
func (c *chanPkt) SetReadDeadline(time.Time) {}
func (c *chanPkt) Close() error {
	c.closeOnce.Do(func() { close(c.done) })
	return nil
}

// chanTun is a TUN fed from and draining to channels.
type chanTun struct {
	in  chan []byte
	out chan []byte
}

func (c *chanTun) MTU() int { return 1380 }
func (c *chanTun) Read(p []byte) (int, error) {
	b, ok := <-c.in
	if !ok {
		return 0, errors.New("closed")
	}
	return copy(p, b), nil
}
func (c *chanTun) Write(p []byte) (int, error) {
	c.out <- append([]byte(nil), p...)
	return len(p), nil
}

// (3) Two real l3Sets over two in-memory links, one of them stuck for 300 ms
// per write: with the dup every ping arrives in time, once, in order, intact;
// without it the pings on the stuck link are late (the test bites).
func TestL3DupEndToEndStuckLink(t *testing.T) {
	for _, dup := range []bool{true, false} {
		late, got := dupRun(t, dup)
		if dup && (late > 0 || got != 40) {
			t.Fatalf("dup on: %d of %d pings late", late, got)
		}
		if !dup && late == 0 {
			t.Fatal("dup off: no ping late — the stuck link carried none, the test does not bite")
		}
	}
}

func dupRun(t *testing.T, dup bool) (late, got int) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tx, rx := &l3Set{lane: newL3Lane(), dup: dup}, &l3Set{}
	txTun := &chanTun{in: make(chan []byte), out: make(chan []byte, 1)}
	rxTun := &chanTun{in: make(chan []byte), out: make(chan []byte, 256)}
	for i, delay := range []time.Duration{300 * time.Millisecond, 0} {
		a, b := chanPktPair(delay)
		la, lb := newL3Link(a), newL3Link(b)
		la.id, la.ka, lb.ka = uint32(i+1)*0x9e3779b9, time.Hour, time.Hour
		la.dupOK = yes
		tx.add(la)
		rx.add(lb)
		go l3WriteOnly(tx, la)
		go rx.linkToTun(ctx, lb, rxTun)
		defer la.markDead()
		defer lb.markDead()
	}
	go tx.pumpTun(ctx, txTun)
	const n = 40
	sent := make(map[string]time.Time)
	order := make(map[string]int)
	for i := 0; i < n; i++ {
		p := icmpPacket(byte(i), byte(i*7)) // many flows, both links
		binary.BigEndian.PutUint16(p[40:], uint16(i))
		sent[string(p)] = time.Now()
		order[string(p)] = i
		txTun.in <- p
		time.Sleep(2 * time.Millisecond)
	}
	timeout := time.After(3 * time.Second)
	seen := map[string]bool{}
	lastPerFlow := map[uint32]int{}
	for got < n {
		select {
		case p := <-rxTun.out:
			t0, ok := sent[string(p)]
			if !ok {
				t.Fatalf("dup=%v: corrupt packet", dup)
			}
			if seen[string(p)] {
				t.Fatalf("dup=%v: packet delivered twice", dup)
			}
			seen[string(p)] = true
			if h := flowHash(p); order[string(p)] < lastPerFlow[h] {
				t.Fatalf("dup=%v: reordered inside a flow", dup)
			} else {
				lastPerFlow[h] = order[string(p)]
			}
			if time.Since(t0) > 150*time.Millisecond {
				late++
			}
			got++
		case <-timeout:
			if dup {
				t.Fatalf("dup on: %d of %d pings arrived", got, n)
			}
			return late + n - got, got
		}
	}
	// nothing more arrives (the second copies were dropped)
	select {
	case <-rxTun.out:
		t.Fatalf("dup=%v: an extra packet arrived", dup)
	case <-time.After(400 * time.Millisecond):
	}
	return late, got
}

// l3WriteOnly runs a sender link's writer (its reader is not needed).
func l3WriteOnly(s *l3Set, l *l3Link) { l.writeLoop(&s.pool, &s.drops) }
