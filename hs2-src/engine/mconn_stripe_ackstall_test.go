package engine

import (
	"testing"
	"time"
)

// Tests of the hs113 ack-stall rule (mconn_stripe_keepstall.go).

type promotedMarks struct{ w, c uint32 }

func (p promotedMarks) AckMarks() (uint32, uint32) { return p.w, p.c }

type embedsMarks struct{ promotedMarks } // like exitCountedStream{*smux.Stream, ...}

// hs112's plumbing bug: the exit's subs only have the promoted AckMarks.
func TestAckMarksOfPromoted(t *testing.T) {
	w, c, ok := ackMarksOf(embedsMarks{promotedMarks{7, 3}})
	if !ok || w != 7 || c != 3 {
		t.Fatalf("promoted AckMarks not found: %d %d %v", w, c, ok)
	}
	if _, _, ok := ackMarksOf(struct{}{}); ok {
		t.Fatal("marks on a plain value")
	}
}

func ksSubN(m *mConn, i int) *msub {
	return &msub{m: m, idx: i, kick: make(chan struct{}, 1), stripe: true}
}

// darkSetup: subs a and c are on the dark path (each wrote a chunk at t0 that
// is never consumed); b flows and its peer consumes everything it writes.
func darkSetup(t *testing.T) (fm fakeMarks, m *mConn, a, b, c *msub, t0 time.Time) {
	fm = setKeepStall(t, true)
	m = newMConn(1130, false, nil)
	a, b, c = ksSubN(m, 1), ksSubN(m, 2), ksSubN(m, 3)
	m.subs = []*msub{a, b, c}
	m.stN = 3
	t0 = time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	fm.write(m, a, t0, keepChunk(1000, 16<<10), 1<<30)
	fm.write(m, c, t0, keepChunk(1000+16<<10, 16<<10), 1<<30)
	c.qBytes = 0
	b.qBytes = 1 << 20 // the healthy sub has the LONGER queue: the old target was c
	m.ackStallTickLocked(t0)
	off := uint64(1000 + 32<<10)
	for i := 0; i < 60; i++ { // b flows for 0.6 s
		now := t0.Add(time.Duration(i) * 10 * time.Millisecond)
		fm.write(m, b, now, keepChunk(off, 16<<10), 0)
		off += 16 << 10
		m.ackStallTickLocked(now)
	}
	return
}

// The dark subs are closed within ~0.5 s and their chunks go to the sub with
// recent acks (not to the other dark sub with the shorter queue).
func TestAckStallDarkSubMovesToHealthy(t *testing.T) {
	_, m, a, b, c, t0 := darkSetup(t)
	m.mu.Lock()
	defer m.mu.Unlock()
	st, d := m.ackStallTickLocked(t0.Add(600 * time.Millisecond))
	if len(st) != 0 || d == 0 {
		t.Fatalf("stalled too early: %d (d=%v)", len(st), d)
	}
	st, _ = m.ackStallTickLocked(t0.Add(900 * time.Millisecond))
	if len(st) != 2 {
		t.Fatalf("stalled subs %d, want 2 (a and c)", len(st))
	}
	for _, x := range st {
		if x == b {
			t.Fatal("the healthy sub was called stalled")
		}
	}
	// subDied for a, as the tick's goroutine does
	a.dead = true
	m.removeSubLocked(a)
	if !m.stripeResendLocked(a) {
		t.Fatal("no resend")
	}
	if len(c.q) != 0 {
		t.Fatal("resend went to the other dark sub")
	}
	if len(b.q) == 0 || b.q[0].off != 1000 {
		t.Fatalf("hole not at the front of the healthy sub (q=%d)", len(b.q))
	}
	m.releaseSentLocked()
}

// Every sub flows (acks move): nothing is called stalled.
func TestAckStallNoFalsePositive(t *testing.T) {
	fm := setKeepStall(t, true)
	m := newMConn(1131, false, nil)
	a, b := ksSubN(m, 1), ksSubN(m, 2)
	m.subs = []*msub{a, b}
	m.stN = 3
	t0 := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	off := uint64(0)
	for i := 0; i < 300; i++ { // 3 s; the peer's acks lag by 1 MiB (smux half window)
		now := t0.Add(time.Duration(i) * 10 * time.Millisecond)
		for _, s := range []*msub{a, b} {
			fm.write(m, s, now, keepChunk(off, 16<<10), 1<<20)
			off += 16 << 10
		}
		if st, _ := m.ackStallTickLocked(now); len(st) > 0 {
			t.Fatalf("flowing sub %d called stalled at %v", st[0].idx, now.Sub(t0))
		}
	}
	m.releaseSentLocked()
}

// A sub that only wrote a little and waits for the peer's lazy ack while
// nothing else proves the receiver reads newer data is left alone.
func TestAckStallNeedsProof(t *testing.T) {
	fm := setKeepStall(t, true)
	m := newMConn(1132, false, nil)
	a, b := ksSubN(m, 1), ksSubN(m, 2)
	m.subs = []*msub{a, b}
	m.stN = 3
	t0 := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	fm.write(m, a, t0, keepChunk(0, 16<<10), 1<<30)
	fm.write(m, b, t0, keepChunk(16<<10, 16<<10), 1<<30)
	m.ackStallTickLocked(t0)
	if st, _ := m.ackStallTickLocked(t0.Add(2 * time.Second)); len(st) > 0 {
		t.Fatal("stalled without proof")
	}
	m.releaseSentLocked()
}

// Old peer / no marks (or the key off): the rule does nothing.
func TestAckStallOldPeerAndKeyOff(t *testing.T) {
	_, m, _, _, _, t0 := darkSetup(t)
	m.mu.Lock()
	keepStallOn.Store(false)
	if st, d := m.ackStallTickLocked(t0.Add(time.Second)); len(st) > 0 || d != 0 {
		t.Fatal("rule ran with the key off")
	}
	if m.ackStallTargetLocked(t0) != nil {
		t.Fatal("target choice changed with the key off")
	}
	m.releaseSentLocked()
	m.mu.Unlock()

	setKeepStall(t, true) // no fake marks: subAckMarks says ok=false
	m2 := newMConn(1133, false, nil)
	a, b := ksSubN(m2, 1), ksSubN(m2, 2)
	m2.subs = []*msub{a, b}
	m2.stN = 3
	m2.mu.Lock()
	defer m2.mu.Unlock()
	m2.keepSentLocked(a, t0, keepChunk(0, 16<<10))
	m2.keepSentLocked(b, t0.Add(time.Second), keepChunk(16<<10, 16<<10))
	if st, _ := m2.ackStallTickLocked(t0.Add(3 * time.Second)); len(st) > 0 {
		t.Fatal("stalled without marks")
	}
	m2.releaseSentLocked()
}

// One round per connection per ackStallGap.
func TestAckStallGap(t *testing.T) {
	fm, m, a, b, _, t0 := darkSetup(t)
	m.mu.Lock()
	defer m.mu.Unlock()
	if st, _ := m.ackStallTickLocked(t0.Add(900 * time.Millisecond)); len(st) != 2 {
		t.Fatalf("stalled %d", len(st))
	}
	a.ks.stalled = false // pretend a is back
	fm.write(m, b, t0.Add(950*time.Millisecond), keepChunk(1<<30, 16<<10), 0)
	if st, _ := m.ackStallTickLocked(t0.Add(1000 * time.Millisecond)); len(st) != 0 {
		t.Fatal("second round within ackStallGap")
	}
	m.releaseSentLocked()
}

// Memory: the global cap bounds what all connections keep together.
func TestKeepStallGlobalCap(t *testing.T) {
	fm := setKeepStall(t, true)
	base := keepStallStat.kept.Load()
	var ms []*mConn
	t0 := time.Now()
	for k := 0; k < 8; k++ {
		m := newMConn(uint64(1140+k), false, nil)
		subs := []*msub{ksSubN(m, 1), ksSubN(m, 2)}
		m.subs = subs
		m.stN = 3
		m.mu.Lock()
		off := uint64(0)
		for j := 0; j < 2600; j++ { // ~40 MiB unacked each (under the per-conn hard bound)
			fm.write(m, subs[j%2], t0, keepChunk(off, 16<<10), 1<<30)
			off += 16 << 10
		}
		m.mu.Unlock()
		ms = append(ms, m)
	}
	if got := keepStallStat.kept.Load() - base; got > keepStallGlobal+(16<<10) {
		t.Fatalf("kept %d over every connection > global cap %d", got, keepStallGlobal)
	}
	for _, m := range ms {
		m.mu.Lock()
		if m.keptBytes > keepStallHard {
			t.Fatalf("conn kept %d > %d", m.keptBytes, keepStallHard)
		}
		m.releaseSentLocked()
		m.mu.Unlock()
	}
	if got := keepStallStat.kept.Load() - base; got != 0 {
		t.Fatalf("kept counter %d after release", got)
	}
}

// Receiver: the resend's copy of a chunk that did arrive is dropped (no
// duplicate delivery).
func TestAckStallResendNoDuplicate(t *testing.T) {
	m := newMConn(1150, false, nil)
	s := &msub{m: m}
	data := randBytes(64<<10, 5)
	put := func(o, e int) {
		c := getChunk(mRecData)
		copy(c.buf[:], data[o:e])
		m.deliver(s, uint64(o), c, e-o)
	}
	put(16<<10, 32<<10) // arrived on a healthy sub
	put(32<<10, 48<<10)
	put(16<<10, 32<<10) // the resend's copy
	put(0, 16<<10)      // the hole, via the healthy sub
	put(0, 16<<10)      // and the dark sub's late copy
	put(48<<10, 64<<10)
	m.recvFin(uint64(len(data)))
	if err := readAllCheck(m, data); err != nil {
		t.Fatal(err)
	}
	m.end(errMConnClosed, false)
}

// hs115: the ack rule alone is not enough — a sub whose TCP link still gets
// its bytes acked (slow because of its queue, or the peer's lazy smux window
// update), or has nothing in flight, is left alone; once its link's
// bytes_acked freezes with data in flight it is closed.
func TestAckStallTCPVeto(t *testing.T) {
	_, m, _, b, _, t0 := darkSetup(t)
	la := linkAck{srtt: 50 * time.Millisecond, acked: 1000, unacked: 64 << 10}
	subLinkAck = func(*msub) (linkAck, bool) { return la, true }
	m.mu.Lock()
	defer m.mu.Unlock()
	v0 := keepStallStat.vetoTCP.Load()
	// candidates from 800 ms; the link keeps getting acked
	for i := 80; i <= 200; i += 10 {
		la.acked += 1 << 20
		if st, _ := m.ackStallTickLocked(t0.Add(time.Duration(i) * 10 * time.Millisecond)); len(st) != 0 {
			t.Fatalf("closed %d subs whose TCP link is acked (at %d0 ms)", len(st), i)
		}
	}
	if keepStallStat.vetoTCP.Load() == v0 {
		t.Fatal("veto not counted")
	}
	// nothing in flight (the peer does not read it: smux window shut)
	la.unacked = 0
	if st, _ := m.ackStallTickLocked(t0.Add(2500 * time.Millisecond)); len(st) != 0 {
		t.Fatal("closed a sub with nothing in flight")
	}
	// the link freezes: closed after max(1 s, 8 x srtt) of no TCP progress (hs119)
	la.unacked = 64 << 10
	m.ackStallTickLocked(t0.Add(2600 * time.Millisecond)) // frozen since 2500 ms
	if st, _ := m.ackStallTickLocked(t0.Add(3400 * time.Millisecond)); len(st) != 0 {
		t.Fatal("closed before ackStallTCPMin")
	}
	st, _ := m.ackStallTickLocked(t0.Add(3650 * time.Millisecond))
	if len(st) != 2 {
		t.Fatalf("stalled subs %d, want 2", len(st))
	}
	for _, x := range st {
		if x == b {
			t.Fatal("the healthy sub was called stalled")
		}
	}
	m.releaseSentLocked()
}

// hs115: an RTO's loss recovery on the frozen link closes at once; a long
// srtt without it waits 8 x srtt (hs119; was 4).
func TestAckStallRelativeTime(t *testing.T) {
	_, m, _, _, _, t0 := darkSetup(t)
	subLinkAck = func(*msub) (linkAck, bool) { return linkAck{srtt: 400 * time.Millisecond, acked: 5, unacked: 1}, true }
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ackStallTickLocked(t0.Add(900 * time.Millisecond)) // frozen window starts
	if st, _ := m.ackStallTickLocked(t0.Add(4000 * time.Millisecond)); len(st) != 0 {
		t.Fatalf("closed %d subs before 8 x srtt", len(st))
	}
	if st, _ := m.ackStallTickLocked(t0.Add(4150 * time.Millisecond)); len(st) != 2 {
		t.Fatalf("stalled %d after 8 x srtt, want 2", len(st))
	}
	m.releaseSentLocked()
}

// hs115: a sub that acks in coarse steps gets a longer stall time.
func TestAckStallGapScales(t *testing.T) {
	x := &msub{}
	x.ks.gap = 300 * time.Millisecond
	if d := x.ackStallTime(); d != ackStallAfter {
		t.Fatalf("stall time %v, want the floor", d)
	}
	x.ks.gap = 600 * time.Millisecond
	if d := x.ackStallTime(); d != 1200*time.Millisecond {
		t.Fatalf("stall time %v, want 1.2s", d)
	}
	x.ks.gap = 10 * time.Second
	if d := x.ackStallTime(); d != ackStallMax {
		t.Fatalf("stall time %v, want the cap", d)
	}
}

// hs115: no socket to ask: ackStallNoSockK x the time.
func TestAckStallNoSocket(t *testing.T) {
	_, m, _, _, _, t0 := darkSetup(t)
	subLinkAck = func(*msub) (linkAck, bool) { return linkAck{}, false }
	m.mu.Lock()
	defer m.mu.Unlock()
	if st, _ := m.ackStallTickLocked(t0.Add(1500 * time.Millisecond)); len(st) != 0 {
		t.Fatalf("closed %d subs without a socket before 3x", len(st))
	}
	if st, _ := m.ackStallTickLocked(t0.Add(2500 * time.Millisecond)); len(st) != 2 {
		t.Fatalf("stalled %d, want 2", len(st))
	}
	m.releaseSentLocked()
}

// hs115: Iran (an upload's sender) runs the rule too.
func TestAckStallIranSide(t *testing.T) {
	_, m, _, _, _, t0 := darkSetup(t)
	m.iran = true
	m.mu.Lock()
	defer m.mu.Unlock()
	if st, _ := m.ackStallTickLocked(t0.Add(900 * time.Millisecond)); len(st) != 2 {
		t.Fatalf("Iran: stalled %d, want 2", len(st))
	}
	m.releaseSentLocked()
}

// hs121: the RTO path needs the frozen time too — an RTO's loss recovery on a
// link with data in flight closes only after bytes_acked stayed frozen for
// max(1 s, 8 x srtt); a link that moves restarts it; nothing in flight never closes.
func TestAckStallRTONeedsFrozen(t *testing.T) {
	_, m, _, b, _, t0 := darkSetup(t)
	ackStallRTONow = false
	la := linkAck{srtt: 50 * time.Millisecond, acked: 7, unacked: 1, rtoLoss: true}
	subLinkAck = func(*msub) (linkAck, bool) { return la, true }
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ackStallTickLocked(t0.Add(900 * time.Millisecond)) // candidates, frozen window starts
	if st, _ := m.ackStallTickLocked(t0.Add(1500 * time.Millisecond)); len(st) != 0 {
		t.Fatalf("RTO closed %d subs after 600 ms frozen", len(st))
	}
	la.acked++ // the link moved: a new window
	m.ackStallTickLocked(t0.Add(1600 * time.Millisecond))
	if st, _ := m.ackStallTickLocked(t0.Add(2500 * time.Millisecond)); len(st) != 0 {
		t.Fatalf("RTO closed %d subs 900 ms after the link moved", len(st))
	}
	la.unacked = 0 // nothing in flight
	if st, _ := m.ackStallTickLocked(t0.Add(2700 * time.Millisecond)); len(st) != 0 {
		t.Fatal("closed a sub with nothing in flight")
	}
	la.unacked = 1 // the window restarted at 2700 ms (nothing in flight)
	if st, _ := m.ackStallTickLocked(t0.Add(3600 * time.Millisecond)); len(st) != 0 {
		t.Fatal("closed before ackStallTCPMin")
	}
	st, _ := m.ackStallTickLocked(t0.Add(3750 * time.Millisecond))
	if len(st) != 2 {
		t.Fatalf("stalled subs %d after 1 s frozen with RTO, want 2", len(st))
	}
	for _, x := range st {
		if x == b {
			t.Fatal("the healthy sub was called stalled")
		}
	}
	m.releaseSentLocked()
}

// hs123 (hs121 qu8on2): a sub that wrote long ago and has nothing queued is
// not dark — its frozen link (a retransmit tail shared with others) and its
// peer's lazy ack do not close it; the same sub writing again is a candidate.
func TestAckStallIdleSubNotCandidate(t *testing.T) {
	fm, m, a, b, c, t0 := darkSetup(t)
	la := linkAck{srtt: 50 * time.Millisecond, acked: 7, unacked: 1}
	subLinkAck = func(*msub) (linkAck, bool) { return la, true }
	m.mu.Lock()
	defer m.mu.Unlock()
	off := uint64(1 << 30)
	at := func(d time.Duration) []*msub {
		now := t0.Add(d)
		fm.write(m, b, now, keepChunk(off, 16<<10), 0) // b flows: the proof
		off += 16 << 10
		st, _ := m.ackStallTickLocked(now)
		return st
	}
	for d := 600 * time.Millisecond; d < 6100*time.Millisecond; d += 100 * time.Millisecond {
		at(d) // skip past ackStallActive without a tick close (the rate check aside)
		if d == 600*time.Millisecond {
			// pretend a and c were idle from the start: no recent write
			a.ks.lastW, c.ks.lastW = t0.Add(-time.Minute), t0.Add(-time.Minute)
		}
	}
	for d := 6100 * time.Millisecond; d < 9*time.Second; d += 100 * time.Millisecond {
		if st := at(d); len(st) != 0 {
			t.Fatalf("idle sub %d closed at %v", st[0].idx, d)
		}
	}
	a.qBytes = 16 << 10 // a has bytes waiting again: it is a candidate
	var st []*msub
	for d := 9 * time.Second; d < 11*time.Second && len(st) == 0; d += 100 * time.Millisecond {
		st = at(d)
	}
	if len(st) != 1 || st[0] != a {
		t.Fatalf("writing sub not closed: %v", len(st))
	}
	m.releaseSentLocked()
}
