package engine

import (
	"testing"
	"time"
)

func setHedge2(t *testing.T, on bool) {
	old := liteHedge
	liteHedge = on
	t.Cleanup(func() { liteHedge = old })
}

// newHedge2Pair: a lite pair whose hedge link (link 1) opens only once
// release is closed, so the request is written before the hedge joins.
func newHedge2Pair(t *testing.T, release chan struct{}) (*tNet, *mConn, *mConn) {
	t.Helper()
	setLite(t, true)
	resetLiteBudget()
	n := &tNet{exit: newMConnExit(11)}
	n.hook = func(l *tLink) {
		if l.key == 1 && release != nil {
			<-release
		}
	}
	iran, err := newMConnIran(11, n.open)
	if err != nil {
		t.Fatal(err)
	}
	closeAtEnd(t, iran, n.exit)
	return n, iran, n.exit
}

// Byte-0 replay: the request is written on the first sub before the hedge
// joins; the first link then stalls (its writes held, as a link in loss
// recovery); the hedge sub delivers the request from byte 0, and the
// response comes back.
func TestB455HedgeReplayByteZero(t *testing.T) {
	setHedge2(t, true)
	rel := make(chan struct{})
	n, iran, exit := newHedge2Pair(t, rel)
	g := make(chan struct{})
	n.link(0).iran.gate.Store(&g) // the first link sends nothing from now on
	t.Cleanup(func() { close(g) })
	r0 := hedgeStat.replays.Load()
	req := randBytes(900, 5)
	if _, err := iran.Write(req); err != nil {
		t.Fatal(err)
	}
	close(rel) // the hedge joins after the request was queued
	both(t, func() error { return nil }, func() error { return readN(exit, req) })
	if hedgeStat.replays.Load() == r0 {
		t.Fatal("no replay counted")
	}
	iran.mu.Lock()
	var hs *msub
	for _, s := range iran.subs {
		if s.idx == 1 {
			hs = s
		}
	}
	iran.mu.Unlock()
	if hs == nil || hs.missEnd != 0 || !hs.lastHole.IsZero() {
		t.Fatalf("hedge sub hole marks: %+v", hs)
	}
	resp := randBytes(2000, 6)
	both(t, func() error { return xfer(exit, resp, 1000, 0) }, func() error { return readN(iran, resp) })
}

// Without the replay (key off) the stalled first link holds the request.
func TestB455HedgeOffNoReplay(t *testing.T) {
	setHedge2(t, false)
	n, iran, exit := newHedge2Pair(t, nil)
	g := make(chan struct{})
	n.link(0).iran.gate.Store(&g)
	req := randBytes(900, 7)
	if _, err := iran.Write(req); err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() { got <- readN(exit, req) }()
	select {
	case <-got:
		t.Fatal("request arrived over a stalled link with the key off")
	case <-time.After(300 * time.Millisecond):
	}
	close(g)
	if err := <-got; err != nil {
		t.Fatal(err)
	}
}

// Dedup: both subs carry the replayed bytes; the reader sees them once and
// in order (readN compares every byte; a duplicate would shift it).
func TestB455HedgeDedup(t *testing.T) {
	setHedge2(t, true)
	rel := make(chan struct{})
	_, iran, exit := newHedge2Pair(t, rel)
	req := randBytes(5000, 8)
	if _, err := iran.Write(req[:3000]); err != nil {
		t.Fatal(err)
	}
	close(rel)
	mWaitFor(t, "hedge joined", time.Second, func() bool { return liveSubs(iran) == 2 })
	both(t, func() error { _, err := iran.Write(req[3000:]); return err }, func() error { return readN(exit, req) })
	more := randBytes(700, 9)
	both(t, func() error { _, err := iran.Write(more); return err }, func() error { return readN(exit, more) })
}

// Budget: a capped lite budget gets no hedge (so no replay); replayed bytes
// count as duplicates when it is open.
func TestB455HedgeBudget(t *testing.T) {
	setHedge2(t, true)
	setLite(t, true)
	resetLiteBudget()
	t.Cleanup(resetLiteBudget)
	m := newMConn(12, true, nil)
	m.mu.Lock()
	m.early.until = time.Now().Add(time.Second)
	for i := 0; i < 3; i++ {
		c := getChunk(mRecData)
		c.n = 1000
		c.setOff(m.sendOff)
		m.sendOff += 1000
		m.earlyKeepLocked(c)
		c.refs.Add(1) // the queue's ref
	}
	if m.early.bytes != 3000 {
		m.mu.Unlock()
		t.Fatalf("early %d", m.early.bytes)
	}
	now := time.Now()
	liteBud.add(now.Unix(), 1000, 900) // capped
	s := &msub{m: m, kick: make(chan struct{}, 1)}
	if m.earlyReplayLocked(s, now) || len(s.q) != 0 {
		m.mu.Unlock()
		t.Fatal("replay while capped")
	}
	resetLiteBudget()
	liteBud.add(now.Unix(), 20000, 0) // traffic, no duplicates yet
	if !m.earlyReplayLocked(s, now) || len(s.q) != 3 {
		m.mu.Unlock()
		t.Fatal("no replay with the budget open")
	}
	m.earlyReleaseLocked()
	m.mu.Unlock()
	if !liteBud.capped(now.Unix()) {
		// 3000 dup bytes on 20000: 15 % > 5 %, the budget sees the replay
		t.Fatal("replayed bytes not counted in the budget")
	}
}

// Heavy: past hedgeBytes the buffer is released and a later join gets no
// replay (it is marked missing as before); a bulk or striped connection
// never buffers.
func TestB455HedgeNoHeavy(t *testing.T) {
	setHedge2(t, true)
	setLite(t, true)
	m := newMConn(13, true, nil)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.early.until = time.Now().Add(time.Second)
	for m.sendOff <= hedgeBytes {
		c := getChunk(mRecData)
		c.n = mRecMax - mDataHdr
		c.setOff(m.sendOff)
		m.sendOff += uint64(c.n)
		m.earlyKeepLocked(c)
	}
	if !m.early.off || len(m.early.chunks) != 0 {
		t.Fatalf("heavy conn still buffered: off=%v n=%d", m.early.off, len(m.early.chunks))
	}
	s := &msub{m: m, kick: make(chan struct{}, 1)}
	if m.earlyReplayLocked(s, time.Now()) {
		t.Fatal("replay on a heavy conn")
	}
	b := newMConn(14, true, nil)
	b.mode = planeBulk
	b.early.until = time.Now().Add(time.Second)
	c := getChunk(mRecData)
	c.n = 100
	b.earlyKeepLocked(c)
	if len(b.early.chunks) != 0 {
		t.Fatal("bulk conn buffered")
	}
}

// A downloading (bulky) hedged connection's hedge sub uses the short idle
// limit past the window.
func TestB455HedgeBulkyIdle(t *testing.T) {
	setHedge2(t, true)
	_, iran, exit := newHedge2Pair(t, nil)
	mWaitFor(t, "two subs", time.Second, func() bool { return liveSubs(iran) == 2 })
	resp := randBytes(hedgeBytes+4000, 10)
	both(t, func() error { return xfer(exit, resp, 4000, 0) }, func() error { return readN(iran, resp) })
	iran.mu.Lock()
	h := iran.hedgeSub.Load()
	iran.hedgeEnd = time.Now().Add(-time.Millisecond)
	lim := iran.hedgeIdleLocked(h, time.Now(), mLiteIdle)
	iran.mu.Unlock()
	if lim != hedgeIdle {
		t.Fatalf("bulky hedge idle %v", lim)
	}
}
