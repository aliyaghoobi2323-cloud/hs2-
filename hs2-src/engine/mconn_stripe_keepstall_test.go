package engine

import (
	"sync"
	"testing"
	"time"
)

// fmMu: the managers' ticks (kicked by keepStallMarkLocked) read the fake
// marks of other test connections.
var fmMu sync.Mutex

// Tests of HS2_MCONN_STRIPE_KEEPSTALL (mconn_stripe_keepstall.go).

// fakeMarks: per sub, the stream's written and the peer's consumed counts.
type fakeMarks map[*msub]*[2]uint32

func setKeepStall(t *testing.T, on bool) fakeMarks {
	t.Helper()
	oldK, oldR, oldF, oldLz := keepStallOn.Load(), stripeResend.Load(), subAckMarks, keepStallLazy.Load()
	keepStallLazy.Store(false) // hs128: these timelines poll from the first kept chunk (lazy has its own test)
	keepStallOn.Store(on)
	stripeResend.Store(true)
	fm := fakeMarks{}
	subAckMarks = func(s *msub) (uint32, uint32, bool) {
		fmMu.Lock()
		defer fmMu.Unlock()
		if p := fm[s]; p != nil {
			return p[0], p[1], true
		}
		return 0, 0, false
	}
	oldL, oldN := subLinkAck, ackStallRTONow
	ackStallRTONow = true // hs121: these timelines predate the RTO frozen time
	// hs115: every link looks dark to TCP unless a test says otherwise
	subLinkAck = func(*msub) (linkAck, bool) {
		return linkAck{srtt: 50 * time.Millisecond, unacked: 1, rtoLoss: true}, true
	}
	t.Cleanup(func() {
		keepStallOn.Store(oldK)
		keepStallLazy.Store(oldLz)
		stripeResend.Store(oldR)
		subAckMarks = oldF
		subLinkAck = oldL
		ackStallRTONow = oldN
	})
	return fm
}

// write: s wrote c; the peer consumed everything but the last lag bytes.
func (fm fakeMarks) write(m *mConn, s *msub, now time.Time, c *mchunk, lag uint32) {
	fmMu.Lock()
	p := fm[s]
	if p == nil {
		p = new([2]uint32)
		fm[s] = p
	}
	p[0] += uint32(c.n)
	if p[0] > lag {
		p[1] = max(p[1], p[0]-lag)
	}
	fmMu.Unlock()
	m.keepSentLocked(s, now, c)
}

func keepChunk(off uint64, n int) *mchunk {
	c := getChunkN(mRecData, n)
	c.off, c.n = off, n
	c.refs.Store(1)
	c.copies = 1
	return c
}

// stalledSubKeep: the hs112 failure as the sender sees it. Sub a wrote one
// chunk (the hole) and then stalled (its path went dark); sub b went on
// writing 32 MiB at full rate. Returns whether a still holds the hole when it
// dies, i.e. whether the resend can fill it.
func stalledSubKeep(t *testing.T, on bool) (held bool, kept int) {
	fm := setKeepStall(t, on)
	m := newMConn(112, false, nil)
	a, b := &msub{m: m, idx: 1}, &msub{m: m, idx: 2}
	m.subs = []*msub{a, b}
	m.stN = 3
	t0 := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	fm.write(m, a, t0, keepChunk(1000, 16<<10), 1<<30)
	off := uint64(1000 + 16<<10)
	for i := 0; i < 2048; i++ { // 32 MiB over ~100 ms
		now := t0.Add(300*time.Millisecond + time.Duration(i)*50*time.Microsecond)
		fm.write(m, b, now, keepChunk(off, 16<<10), 1<<20)
		off += 16 << 10
	}
	held = len(a.sent) == 1 && a.sent[0].c.off == 1000
	kept = m.keptBytes
	m.releaseSentLocked()
	return held, kept
}

// The bug: the per-connection bound evicts the stalled sub's chunk (the
// oldest) within ~100 ms of the other sub's writes: the resend at the sub's
// death has nothing ("resend 0 chunks" in the lab trace).
func TestKeepStallOldRuleEvictsHole(t *testing.T) {
	if held, _ := stalledSubKeep(t, false); held {
		t.Fatal("old rule kept the stalled sub's chunk: the test no longer reproduces the bug")
	}
}

// With the key the stalled sub keeps it, and the kept bytes stay bounded.
func TestKeepStallKeepsHole(t *testing.T) {
	held, kept := stalledSubKeep(t, true)
	if !held {
		t.Fatal("stalled sub's chunk evicted")
	}
	if kept > keepStallHard {
		t.Fatalf("kept %d > hard bound %d", kept, keepStallHard)
	}
	if kept > stripeKeepBytes+(16<<10)+(16<<10) {
		t.Fatalf("kept %d: the flowing sub's chunks were not evicted to the bound", kept)
	}
}

// Hard bound: every sub stalled, far over the bound: the oldest go anyway.
func TestKeepStallHardBound(t *testing.T) {
	fm := setKeepStall(t, true)
	m := newMConn(113, false, nil)
	var subs []*msub
	for i := 0; i < 60; i++ {
		subs = append(subs, &msub{m: m, idx: i})
	}
	m.subs = subs
	m.stN = 3
	t0 := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	off := uint64(0)
	for i, s := range subs { // each sub writes 2 MiB, then stalls for good
		for j := 0; j < 128; j++ {
			fm.write(m, s, t0.Add(time.Duration(i)*time.Second), keepChunk(off, 16<<10), 1<<30)
			off += 16 << 10
		}
	}
	if m.keptBytes > keepStallHard {
		t.Fatalf("kept %d > hard bound %d", m.keptBytes, keepStallHard)
	}
	m.releaseSentLocked()
}

// The stall then death end to end: the resend after the stall carries the
// hole (the resend's chunk list starts at it).
func TestKeepStallResendCarriesHole(t *testing.T) {
	fm := setKeepStall(t, true)
	m := newMConn(114, false, nil)
	a, b, c := &msub{m: m, idx: 1, kick: make(chan struct{}, 1)}, &msub{m: m, idx: 2, kick: make(chan struct{}, 1)}, &msub{m: m, idx: 3, kick: make(chan struct{}, 1)}
	m.subs = []*msub{a, b, c}
	m.stN = 3
	t0 := time.Now()
	m.mu.Lock()
	fm.write(m, a, t0, keepChunk(5000, 16<<10), 1<<30)
	off := uint64(5000 + 16<<10)
	for i := 0; i < 2048; i++ {
		fm.write(m, b, t0.Add(300*time.Millisecond), keepChunk(off, 16<<10), 1<<20)
		off += 16 << 10
	}
	a.dead = true
	m.removeSubLocked(a)
	if !m.stripeResendLocked(a) {
		m.mu.Unlock()
		t.Fatal("no resend")
	}
	tgt := b
	if len(c.q) > 0 {
		tgt = c
	}
	if len(tgt.q) == 0 || tgt.q[0].off != 5000 {
		m.mu.Unlock()
		t.Fatalf("resend does not start at the hole (q=%d)", len(tgt.q))
	}
	m.mu.Unlock()
}

// Receiver: a full reorder buffer takes more only for an old hole, and only
// up to gapRoomBytes.
func TestGapRoom(t *testing.T) {
	setKeepStall(t, true)
	now := time.Now()
	r := &mrecv{oooMax: 64 << 20, oooBytes: 64 << 20, ooo: []rchunk{{off: 10}}}
	r.lastProgress = now.Add(-100 * time.Millisecond)
	if r.gapRoomLocked(now, 16<<10) {
		t.Fatal("room for a young hole")
	}
	r.lastProgress = now.Add(-time.Second) // hs131: an ordinary lossy-path stall, no room yet
	if r.gapRoomLocked(now, 16<<10) {
		t.Fatal("room for a 1 s hole (hs131: rooms only past gapRoomAfter 1.5 s)")
	}
	r.lastProgress = now.Add(-2 * time.Second)
	if !r.gapRoomLocked(now, 16<<10) {
		t.Fatal("no room for an old hole")
	}
	r.oooBytes = 64<<20 + gapRoomBytes
	if r.gapRoomLocked(now, 16<<10) {
		t.Fatal("room over gapRoomBytes")
	}
	keepStallOn.Store(false)
	r.oooBytes = 64 << 20
	if r.gapRoomLocked(now, 16<<10) {
		t.Fatal("room with the key off")
	}
}

// hs113: with the gap room used up, a chunk inside the buffer's span is
// still taken for an old hole; one beyond it is not.
func TestGapFill(t *testing.T) {
	setKeepStall(t, true)
	now := time.Now()
	r := &mrecv{ooo: []rchunk{{off: 1000, b: make([]byte, 100)}, {off: 5000, b: make([]byte, 100)}}}
	r.lastProgress = now.Add(-2 * time.Second)
	if !r.gapFillLocked(now, 3000) {
		t.Fatal("chunk inside the span refused")
	}
	if r.gapFillLocked(now, 6000) {
		t.Fatal("chunk beyond the span taken")
	}
	r.lastProgress = now
	if r.gapFillLocked(now, 3000) {
		t.Fatal("taken for a young hole")
	}
}
