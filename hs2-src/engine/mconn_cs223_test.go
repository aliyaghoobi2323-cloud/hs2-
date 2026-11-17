package engine

import (
	"testing"
	"time"
)

func cs223Chunk(off uint64, n int) *mchunk {
	c := getChunk(mRecData)
	c.setOff(off)
	c.n = n
	c.refs.Store(1)
	c.copies = 1
	return c
}

func cs223Keys(t *testing.T) {
	t.Helper()
	o1, o2, o3, o4 := cs223CallResend.Load(), cs221Darkcall.Load(), stripeResend.Load(), mconnCallResend.Load()
	t.Cleanup(func() {
		cs223CallResend.Store(o1)
		cs221Darkcall.Store(o2)
		stripeResend.Store(o3)
		mconnCallResend.Store(o4)
	})
	cs223CallResend.Store(true)
	cs221Darkcall.Store(true)
	stripeResend.Store(true)
	mconnCallResend.Store(true)
}

// cs223: a resend onto b leaves out the chunks b was given (off >= missEnd)
// and let go with proof (off >= unprovEnd); nothing while b has a gap, a
// bulk/stripe chunk went out, or the key is off.
func TestCs223SkipDelivered(t *testing.T) {
	cs223Keys(t)
	m := &mConn{}
	b := &msub{m: m, missEnd: 1000, unprovEnd: 2000}
	mk := func() []*mchunk {
		return []*mchunk{cs223Chunk(0, 1000), cs223Chunk(1500, 500), cs223Chunk(2000, 1000), cs223Chunk(3000, 1000)}
	}
	s0 := cs223Stat.skipDel.Load()
	out := m.cs223SkipDelivered(mk(), b)
	if len(out) != 2 || out[0].off != 0 || out[1].off != 1500 || cs223Stat.skipDel.Load() != s0+2 {
		t.Fatalf("on: kept %d chunks", len(out))
	}
	fin := getChunk(mRecFin)
	fin.setOff(4000)
	if out := m.cs223SkipDelivered([]*mchunk{fin}, b); len(out) != 1 {
		t.Fatal("a FIN is never left out")
	}
	b.unprovEnd = 0 // given from 1000 on, all released with proof
	if out := m.cs223SkipDelivered(mk(), b); len(out) != 1 || out[0].off != 0 {
		t.Fatalf("missEnd bound: kept %d", len(out))
	}
	b.subGapT = time.Now()
	if out := m.cs223SkipDelivered(mk(), b); len(out) != 4 {
		t.Fatalf("gap: kept %d, want 4", len(out))
	}
	b.subGapT = time.Time{}
	m.missLost = true
	if out := m.cs223SkipDelivered(mk(), b); len(out) != 4 {
		t.Fatalf("missLost: kept %d, want 4", len(out))
	}
	m.missLost = false
	cs223CallResend.Store(false)
	if out := m.cs223SkipDelivered(mk(), b); len(out) != 4 {
		t.Fatalf("key off: kept %d, want 4", len(out))
	}
}

// cs223: dropping kept chunks without proof raises unprovEnd; the
// keep-stall ack trim of marked (acked) chunks does not.
func TestCs223DropNotesUnprov(t *testing.T) {
	m := &mConn{}
	s := &msub{m: m}
	c1, c2 := cs223Chunk(0, 100), cs223Chunk(100, 100)
	s.sent = []sentRec{{c: c1, t: time.Now()}, {c: c2, t: time.Now()}}
	m.keptBytes = 200
	keepStallStat.kept.Add(200)
	m.dropSentLocked(s, 1)
	if s.unprovEnd != 100 {
		t.Fatalf("drop: unprovEnd %d, want 100", s.unprovEnd)
	}
	m.dropSentRawLocked(s, 1)
	if s.unprovEnd != 100 || len(s.sent) != 0 {
		t.Fatalf("raw drop: unprovEnd %d sent %d", s.unprovEnd, len(s.sent))
	}
	old := subAckMarks
	t.Cleanup(func() { subAckMarks = old })
	subAckMarks = func(*msub) (uint32, uint32, bool) { return 10, 10, true }
	c3, c4 := cs223Chunk(200, 100), cs223Chunk(300, 100)
	s.sent = []sentRec{{c: c3, t: time.Now(), w: 5, marked: true}, {c: c4, t: time.Now(), w: 20, marked: true}}
	m.keptBytes = 200
	keepStallStat.kept.Add(200)
	m.keepStallTrimSubLocked(s, nil, time.Now())
	if len(s.sent) != 1 || s.unprovEnd != 100 {
		t.Fatalf("acked trim: sent %d unprovEnd %d (want 1, 100)", len(s.sent), s.unprovEnd)
	}
	m.dropSentLocked(s, 1)
}

// cs223: a given-up call sub queues its chunks on the target now, without
// what the target holds; its death then leaves out what it gave while the
// target lives.
func TestCs223GiveUpResend(t *testing.T) {
	cs223Keys(t)
	m := &mConn{lite: true, callLikeAt: time.Now()}
	if !m.callKeepLocked() {
		t.Fatal("setup: not a call keep conn")
	}
	s := &msub{m: m, idx: 0}
	b := &msub{m: m, idx: 1, missEnd: 1 << 40} // joined late: given nothing below
	c0, c1, c2 := cs223Chunk(0, 1000), cs223Chunk(1000, 1000), cs223Chunk(2000, 1000)
	s.sent = []sentRec{{c: c0, t: time.Now()}, {c: c1, t: time.Now()}}
	s.q = []*mchunk{c2}
	b.q = []*mchunk{c1} // b already carries c1
	m.subs = []*msub{s, b}
	g0 := cs223Stat.gaveC.Load()
	if n := m.cs223GiveUpResendLocked(s, b); n != 2 {
		t.Fatalf("give-up: queued %d, want 2", n)
	}
	if len(b.q) != 3 || b.q[0] != c0 || b.q[1] != c2 || b.q[2] != c1 || cs223Stat.gaveC.Load() != g0+2 {
		t.Fatalf("b.q %v", b.q)
	}
	if c0.copies != 2 || c2.copies != 2 || b.qBytes != 2000 || b.txEnd != 3000 {
		t.Fatalf("copies %d/%d qBytes %d txEnd %d", c0.copies, c2.copies, b.qBytes, b.txEnd)
	}
	if s.gaveTo != b || s.gaveEnd != 3000 {
		t.Fatalf("gaveTo %v gaveEnd %d", s.gaveTo, s.gaveEnd)
	}
	// at s's death: only what came after the give-up
	c3 := cs223Chunk(3000, 500)
	out := cs223SkipGave([]*mchunk{c0, c1, c2, c3}, s)
	if len(out) != 1 || out[0] != c3 {
		t.Fatalf("death: %d chunks, want only the newer one", len(out))
	}
	b.dead = true
	if out := cs223SkipGave([]*mchunk{c0, c1, c2, c3}, s); len(out) != 4 {
		t.Fatalf("target dead: %d chunks, want 4", len(out))
	}
	b.dead = false
	// target retired / same sub / not a call / key off: nothing
	if m.cs223GiveUpResendLocked(s, s) != 0 {
		t.Fatal("same sub")
	}
	b.retired = true
	if m.cs223GiveUpResendLocked(s, b) != 0 {
		t.Fatal("retired target")
	}
	b.retired = false
	m.callLikeAt = time.Time{}
	if m.cs223GiveUpResendLocked(s, b) != 0 {
		t.Fatal("not a call")
	}
	m.callLikeAt = time.Now()
	cs223CallResend.Store(false)
	if m.cs223GiveUpResendLocked(s, b) != 0 {
		t.Fatal("key off")
	}
	if out := cs223SkipGave([]*mchunk{c0, c3}, s); len(out) != 2 {
		t.Fatal("key off: death skip")
	}
}

// cs223: a call sub that goes dark while not the primary queues on the
// primary the chunks the primary lacks (not those the switch resent onto it,
// not those it holds); a dark primary is passed over for a live sub.
func TestCs223DarkResend(t *testing.T) {
	cs223Keys(t)
	old := cs223DarkResend.Load()
	t.Cleanup(func() { cs223DarkResend.Store(old) })
	cs223DarkResend.Store(true)
	m := &mConn{lite: true, callLikeAt: time.Now()}
	s := &msub{m: m, idx: 1}
	p := &msub{m: m, idx: 3, missEnd: 1 << 40}
	o := &msub{m: m, idx: 2, missEnd: 1 << 40}
	m.subs = []*msub{s, o, p}
	m.primary.Store(p)
	c0, c1, c2 := cs223Chunk(0, 1000), cs223Chunk(1000, 1000), cs223Chunk(2000, 1000)
	s.sent = []sentRec{{c: c0, t: time.Now()}, {c: c1, t: time.Now()}, {c: c2, t: time.Now()}}
	p.rsLo, p.rsHi = 0, 2000 // the switch resent [0,2000) onto p
	d0 := cs223Stat.darkRes.Load()
	m.cs223DarkResendLocked(s)
	if len(p.q) != 1 || p.q[0] != c2 || cs223Stat.darkRes.Load() != d0+1 || len(o.q) != 0 {
		t.Fatalf("p.q %d o.q %d", len(p.q), len(o.q))
	}
	// the primary is dark: the other live sub takes them
	p.dc.isDark = true
	s.gaveTo, s.gaveEnd = nil, 0
	m.cs223DarkResendLocked(s)
	if len(o.q) != 3 {
		t.Fatalf("o.q %d, want 3", len(o.q))
	}
	cs223DarkResend.Store(false)
	n := len(o.q)
	m.cs223DarkResendLocked(s)
	if len(o.q) != n {
		t.Fatal("key off")
	}
}
