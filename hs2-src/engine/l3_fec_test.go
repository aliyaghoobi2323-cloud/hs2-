package engine

import (
	"context"
	"encoding/binary"
	"sync"
	"testing"
	"time"
)

// fecSet is a set with the lane and the FEC on (a test clock).
func fecSet(clock *int64, links ...*l3Link) *l3Set {
	s := laneSet(clock, links...)
	s.fec = &l3Fec{}
	return s
}

// callPacket is UDP packet seq of a call flow (size bytes, seq in the body).
func callPacket(seq, size int) []byte {
	p := udpPacket(40000, 3478, size)
	binary.BigEndian.PutUint32(p[28:], uint32(seq))
	for i := 32; i < size; i++ {
		p[i] = byte(seq + i)
	}
	return p
}

// pumpFec runs pumpTun's per-packet step; it reports whether the FEC took it.
func pumpFec(s *l3Set, pkt []byte) bool {
	b := append([]byte(nil), pkt...)
	bp := &b
	l, l2, fp, fec := s.pickAll(b)
	switch {
	case fec:
		s.sendFec(bp, fp)
	case l2 != nil:
		s.sendDup(bp, l, l2)
	case l != nil:
		l.enqueue(bp)
	}
	return fec
}

// (1) A call flow over six light links: five carry one data packet of each
// group, the sixth the group's parity, which is the XOR of the five; the
// extra bytes are about a fifth of the flow.
func TestL3FecStripesWithParity(t *testing.T) {
	var clock int64 = 1
	var ls []*l3Link
	for i := 1; i <= 6; i++ {
		ls = append(ls, dupLink(uint32(i), 0, true))
	}
	s := fecSet(&clock, ls...)
	var data, par int
	for i := 0; i < 50; i++ {
		clock += int64(4 * time.Millisecond)
		if !pumpFec(s, callPacket(i, 1100)) {
			t.Fatalf("packet %d not striped", i)
		}
	}
	perLink := map[*l3Link][]qpkt{}
	for _, l := range ls {
		perLink[l] = queued(l)
	}
	var parLink *l3Link
	for l, q := range perLink {
		for _, p := range q {
			if !p.dup {
				t.Fatal("striped packet not sent as TypeDataDup")
			}
			if (*p.b)[0] == l3FecMagic {
				par++
				if parLink != nil && parLink != l {
					t.Fatal("parity on two links")
				}
				parLink = l
				if k := int((*p.b)[1]); k != l3FecMaxK {
					t.Fatalf("k=%d, want %d", k, l3FecMaxK)
				}
			} else {
				data++
			}
		}
	}
	if data != 50 || par != 10 {
		t.Fatalf("data %d parity %d, want 50/10", data, par)
	}
	for l, q := range perLink {
		if l != parLink && len(q) != 10 {
			t.Fatalf("a data link carried %d packets, want 10", len(q))
		}
	}
	// the overhead counter: parity bytes ~ data/5
	pb, db := s.fec.parB.Load(), s.fec.dataB.Load()
	if pct := 100 * float64(pb) / float64(db); pct < 19 || pct > 23 {
		t.Fatalf("parity is %.1f%% of the flow", pct)
	}
	// an old receiver's TUN refuses the parity packet: not IPv4/IPv6
	if l3FecMagic>>4 == 4 || l3FecMagic>>4 == 6 {
		t.Fatal("parity magic looks like an IP version")
	}
}

// (2) What the FEC leaves alone: TCP flows, interactive flows, flows above
// l3FecMaxRate, and any flow with fewer than four light dup links.
func TestL3FecScope(t *testing.T) {
	var clock int64 = 1
	mk := func(n, heavy int) []*l3Link {
		var ls []*l3Link
		for i := 1; i <= n; i++ {
			h := int32(0)
			if i <= heavy {
				h = 1
			}
			ls = append(ls, dupLink(uint32(i), h, true))
		}
		return ls
	}
	s := fecSet(&clock, mk(6, 0)...)
	tcp := tcpPacket(1000, 443)
	tcp = append(tcp, make([]byte, 1100-len(tcp))...)
	if pumpFec(s, tcp) {
		t.Fatal("a TCP flow was striped")
	}
	if pumpFec(s, udpPacket(53, 53, 100)) {
		t.Fatal("an interactive (small) flow was striped")
	}
	s = fecSet(&clock, mk(6, 3)...) // three light links only
	if pumpFec(s, callPacket(0, 1100)) {
		t.Fatal("striped over fewer than four light links")
	}
	s = fecSet(&clock, mk(4, 0)...) // four: k=3
	for i := 0; i < 3; i++ {
		if !pumpFec(s, callPacket(i, 1100)) {
			t.Fatal("not striped over four light links")
		}
	}
	if s.fec.parity.Load() != 1 {
		t.Fatal("no parity after three packets with k=3")
	}
	// a fast flow: striped in its first second, then plain
	s = fecSet(&clock, mk(6, 0)...)
	for i := 0; i < 1000; i++ { // 1000 x 1100 B in one second = 8.8 Mbit/s
		clock += int64(time.Millisecond)
		pumpFec(s, callPacket(i, 1100))
	}
	clock += int64(2 * time.Millisecond)
	if pumpFec(s, callPacket(1001, 1100)) {
		t.Fatal("a flow above l3FecMaxRate was still striped")
	}
	// key off: newL3Set has no FEC, and a call flow takes the old pick
	defer func(a bool) { l3FecOn = a }(l3FecOn)
	l3FecOn = false
	if newL3Set().fec != nil {
		t.Fatal("FEC state with the key off")
	}
	off := laneSet(&clock, mk(6, 0)...)
	for i := 0; i < 100; i++ {
		p := callPacket(i, 1100)
		if l, l2, _, fec := off.pickAll(p); fec || l2 != nil || l != rendezvous(off.links, flowHash(p), false) {
			t.Fatal("key off: not the plain pick")
		}
	}
}

// (3) The global budget: past the burst, groups go without parity.
func TestL3FecBudget(t *testing.T) {
	var c l3Fec
	now := int64(1)
	n := 0
	for c.take(1200, now) {
		n++
	}
	if want := l3FecBurst / 1200; n != want {
		t.Fatalf("burst gave %d parity packets, want %d", n, want)
	}
	now += int64(time.Second)
	n = 0
	for c.take(1200, now) {
		n++
	}
	if want := l3FecBurst / 1200; n != want { // the refill stops at the burst
		t.Fatalf("one second refilled %d, want %d", n, want)
	}
	now += int64(100 * time.Millisecond)
	n = 0
	for c.take(1200, now) {
		n++
	}
	if want := l3FecBudget / 10 / 1200; n < want-1 || n > want+1 {
		t.Fatalf("100 ms refilled %d, want ~%d", n, want)
	}
}

// (4) End to end, two real sets over six in-memory links, one stuck 300 ms
// per write: with the FEC every packet of the call arrives once, intact and
// within 150 ms (the stuck link's share is rebuilt from parity).
func TestL3FecEndToEndStuckLink(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tx, rx := &l3Set{lane: newL3Lane(), fec: &l3Fec{}}, &l3Set{}
	txTun := &chanTun{in: make(chan []byte), out: make(chan []byte, 1)}
	rxTun := &chanTun{in: make(chan []byte), out: make(chan []byte, 1024)}
	var rxl []*l3Link
	for i := 0; i < 6; i++ {
		a, b := chanPktPair(0)
		la, lb := newL3Link(a), newL3Link(b)
		la.id, la.ka, lb.ka = uint32(i+1)*0x9e3779b9, time.Hour, time.Hour
		la.dupOK = yes
		tx.add(la)
		rx.add(lb)
		rxl = append(rxl, lb)
		defer la.markDead()
		defer lb.markDead()
	}
	// the flow's first data link is the stuck one
	var ls [l3FecMaxK + 1]*l3Link
	fecLinks(tx.links, flowHash(callPacket(0, 1000)), &ls)
	ls[0].car.(*chanPkt).delay = 300 * time.Millisecond
	for i, la := range tx.links {
		go l3WriteOnly(tx, la)
		go rx.linkToTun(ctx, rxl[i], rxTun)
	}
	go tx.pumpTun(ctx, txTun)
	const n = 200
	pkts := map[string]int{}
	for i := 0; i < n; i++ {
		pkts[string(callPacket(i, 1000+i%200))] = i
	}
	var mu sync.Mutex
	sent := make([]time.Time, n)
	go func() { // the call, read back while it is sent
		for i := 0; i < n; i++ {
			mu.Lock()
			sent[i] = time.Now()
			mu.Unlock()
			txTun.in <- callPacket(i, 1000+i%200)
			time.Sleep(2 * time.Millisecond)
		}
	}()
	timeout := time.After(3 * time.Second)
	seen := map[string]bool{}
	late, got := 0, 0
	for got < n {
		select {
		case p := <-rxTun.out:
			i, ok := pkts[string(p)]
			if !ok {
				t.Fatalf("corrupt packet (%d bytes)", len(p))
			}
			mu.Lock()
			t0 := sent[i]
			mu.Unlock()
			if seen[string(p)] {
				t.Fatal("packet delivered twice")
			}
			seen[string(p)] = true
			if time.Since(t0) > 150*time.Millisecond {
				late++
			}
			got++
		case <-timeout:
			t.Fatalf("%d of %d packets arrived", got, n)
		}
	}
	// the last group may lack its parity (the flow stopped mid-group): allow
	// those few to be late
	if late > l3FecMaxK {
		t.Fatalf("%d of %d packets late", late, n)
	}
	r := rx.fecRx.Load()
	if r == nil || r.recovered.Load() == 0 {
		t.Fatal("nothing rebuilt: the stuck link carried none, the test does not bite")
	}
	select {
	case p := <-rxTun.out:
		t.Fatalf("an extra packet arrived (%d bytes)", len(p))
	case <-time.After(400 * time.Millisecond):
	}
}

// (5) The receiver: a parity packet with one missing member rebuilds it; one
// with two waits until one of them comes; the late original is a repeat.
func TestL3FecReceiverRepair(t *testing.T) {
	s := &l3Set{}
	tun := &chanTun{out: make(chan []byte, 16)}
	pk := [][]byte{callPacket(1, 1100), callPacket(2, 900), callPacket(3, 1200)}
	ids := []uint32{100, 101, 102}
	par := func() []byte {
		x := make([]byte, 1200)
		var lx uint16
		for _, p := range pk {
			for i, c := range p {
				x[i] ^= c
			}
			lx ^= uint16(len(p))
		}
		b := []byte{l3FecMagic, 3}
		for _, id := range ids {
			b = binary.BigEndian.AppendUint32(b, id)
		}
		b = binary.BigEndian.AppendUint16(b, lx)
		return append(b, x...)
	}
	deliver := func(id uint32, p []byte) {
		f := binary.BigEndian.AppendUint32(nil, id)
		if q := s.rx.accept(append(f, p...)); q != nil {
			s.fecRecv(id, q, tun)
		}
	}
	// a parity first (activates the receiver), nothing of its group yet
	deliver(99, par())
	deliver(ids[0], pk[0])
	if len(tun.out) != 1 {
		t.Fatalf("%d packets out, want 1", len(tun.out))
	}
	<-tun.out
	deliver(ids[2], pk[2]) // now only ids[1] is missing: rebuilt
	if len(tun.out) != 2 {
		t.Fatalf("%d packets out, want 2 (one rebuilt)", len(tun.out))
	}
	<-tun.out
	if got := <-tun.out; string(got) != string(pk[1]) {
		t.Fatal("rebuilt packet differs")
	}
	deliver(ids[1], pk[1]) // the late original
	if len(tun.out) != 0 {
		t.Fatal("late original delivered after its rebuild")
	}
}
