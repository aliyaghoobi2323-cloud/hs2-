package engine

import (
	"bytes"
	"testing"
	"time"
)

func setB375(t *testing.T, on bool) {
	t.Helper()
	old := b375RRCover.Load()
	b375RRCover.Store(on)
	t.Cleanup(func() { b375RRCover.Store(old) })
}

// setB375Floor: HS2_B375_RINGFLOOR (0 off, 1 calls / heavy queue, 2 all).
func setB375Floor(t *testing.T, f int32) {
	t.Helper()
	old := b375RingFloor.Load()
	b375RingFloor.Store(f)
	t.Cleanup(func() { b375RingFloor.Store(old) })
}

// b375Pair: a records pair whose rings stay at 16 KB (no big growth), so a
// 30 KB in-flight backlog is past the ring (the rb372 B2 k7 shape scaled:
// 32 KB ring, 62.5 KB missing).
func b375Pair(t *testing.T, on bool) (*tNet, *mConn, *mConn, map[any]bool) {
	t.Helper()
	setB375(t, on)
	setRRRingCap(t, 16<<10)
	setRRBigMax(t, 0)
	n, iran, exit := newRRPair(t)
	set := rrLiveKeys(iran)
	if len(set) == 0 {
		t.Fatal("no live sub")
	}
	return n, iran, exit, set
}

func gateLive(m *mConn) {
	m.mu.Lock()
	for _, s := range m.rrLiveSubsLocked() {
		s.gated = true
	}
	m.mu.Unlock()
}

func waitLost(t *testing.T, m *mConn) {
	t.Helper()
	mWaitFor(t, "conn ended errMConnLost", 3*time.Second, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.ended && m.err == errMConnLost
	})
}

// The repro (backlog 01:31): the primary goes dark with 30 KB queued (not
// written) past a 16 KB ring; the exit's ACK is 30 KB below Iran's Bi.
// Key off: nocover, errMConnLost (today). Key on: byte-exact delivery from
// the retired sub's queued chunks, and the conn goes on.
func TestB375RRCoverQueued(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[on], func(t *testing.T) {
			n, iran, exit, set := b375Pair(t, on)
			blackhole(n, set)
			gateLive(iran)
			data := randBytes(30000, 11)
			if _, err := iran.Write(data); err != nil {
				t.Fatal(err)
			}
			iran.rkMu.Lock()
			held := iran.rk.n
			iran.rkMu.Unlock()
			if held >= len(data) {
				t.Fatalf("ring holds %d, the test needs it short of %d", held, len(data))
			}
			nc0, full0 := rrStat.noCover.Load(), b375Stat.full.Load()
			rrNow(t, iran, set)
			if !on {
				waitLost(t, iran)
				if rrStat.noCover.Load() == nc0 {
					t.Fatal("no nocover counted")
				}
				return
			}
			more, back := randBytes(20000, 12), randBytes(15000, 13)
			both(t,
				func() error {
					if err := readN(exit, data); err != nil {
						return err
					}
					return readN(exit, more)
				},
				func() error { return xfer(iran, more, 4000, 0) },
				func() error { return xfer(exit, back, 4000, 0) },
				func() error { return readN(iran, back) },
			)
			noExtra(t, iran, exit)
			if rrStat.noCover.Load() != nc0 || b375Stat.full.Load() == full0 {
				t.Fatalf("nocover %d->%d full %d->%d", nc0, rrStat.noCover.Load(), full0, b375Stat.full.Load())
			}
			closeAndWait(t, iran, exit)
		})
	}
}

// Written (vanished on the dark link) but kept for a resend: the sent lists
// cover what the ring lost. Mixed with a queued tail.
func TestB375RRCoverWrittenKept(t *testing.T) {
	o1, o2, o3 := mconnSubResend.Load(), stripeResend.Load(), keepStallOn.Load()
	t.Cleanup(func() { mconnSubResend.Store(o1); stripeResend.Store(o2); keepStallOn.Store(o3) })
	mconnSubResend.Store(true)
	stripeResend.Store(true)
	keepStallOn.Store(true)
	n, iran, exit, set := b375Pair(t, true)
	blackhole(n, set)
	w := randBytes(20000, 21)
	if _, err := iran.Write(w); err != nil {
		t.Fatal(err)
	}
	mWaitFor(t, "written", time.Second, func() bool {
		iran.mu.Lock()
		defer iran.mu.Unlock()
		for _, s := range iran.rrLiveSubsLocked() {
			if len(s.q) > 0 || s.cur != nil {
				return false
			}
		}
		return true
	})
	iran.mu.Lock()
	kept := iran.keptBytes
	iran.mu.Unlock()
	gateLive(iran)
	q := randBytes(12000, 22)
	if _, err := iran.Write(q); err != nil {
		t.Fatal(err)
	}
	rrNow(t, iran, set)
	if kept < len(w) {
		// nothing kept the written bytes: the range is truly uncoverable
		t.Logf("kept %d: not a keeping conn here; expect a clean end", kept)
		waitLost(t, iran)
		return
	}
	all := append(append([]byte(nil), w...), q...)
	more := randBytes(9000, 23)
	both(t,
		func() error {
			if err := readN(exit, all); err != nil {
				return err
			}
			return readN(exit, more)
		},
		func() error { return xfer(iran, more, 3000, 0) },
	)
	noExtra(t, iran, exit)
	closeAndWait(t, iran, exit)
}

// Truly uncoverable (the written bytes kept nowhere): the conn still ends
// cleanly with errMConnLost, key on.
func TestB375RRCoverUncoverable(t *testing.T) {
	n, iran, exit, set := b375Pair(t, true)
	_ = exit
	blackhole(n, set)
	w := randBytes(30000, 31)
	if _, err := iran.Write(w); err != nil {
		t.Fatal(err)
	}
	mWaitFor(t, "written", time.Second, func() bool {
		iran.mu.Lock()
		defer iran.mu.Unlock()
		for _, s := range iran.rrLiveSubsLocked() {
			if len(s.q) > 0 || s.cur != nil {
				return false
			}
		}
		return true
	})
	iran.mu.Lock()
	for _, s := range iran.rrLiveSubsLocked() {
		iran.abandonSentLocked(s) // nothing kept anywhere
	}
	iran.mu.Unlock()
	sh0 := b375Stat.short.Load()
	rrNow(t, iran, set)
	waitLost(t, iran)
	if b375Stat.short.Load() == sh0 {
		t.Fatal("short not counted")
	}
	buf := make([]byte, 10)
	if _, err := iran.Read(buf); err == nil {
		t.Fatal("read on an ended conn")
	}
}

// Fully covered by the ring: today's path, no extension.
func TestB375RRCoverRingEnough(t *testing.T) {
	n, iran, exit, set := b375Pair(t, true)
	blackhole(n, set)
	gateLive(iran)
	data := randBytes(8000, 41)
	if _, err := iran.Write(data); err != nil {
		t.Fatal(err)
	}
	ext0, full0 := b375Stat.ext.Load(), b375Stat.full.Load()
	rrNow(t, iran, set)
	if err := readN(exit, data); err != nil {
		t.Fatal(err)
	}
	noExtra(t, iran, exit)
	if b375Stat.full.Load() != full0 {
		t.Fatal("counted as covered by the extension")
	}
	_ = ext0 // the queued chunks may extend the snapshot; the ring alone covered Rk
	closeAndWait(t, iran, exit)
}

// The extension itself: contiguous pieces extend, a gap stops it, the
// snapshot's bytes stay, overlaps are byte-exact.
func TestB375ExtendSnap(t *testing.T) {
	setB375(t, true)
	src := randBytes(1000, 51)
	m := &mConn{}
	m.rrSnap, m.rrSnap0 = append([]byte(nil), src[800:1000]...), 800
	ps := []b375Piece{
		{off: 600, b: src[600:750]},
		{off: 700, b: src[700:850]}, // overlaps the snapshot
		{off: 300, b: src[300:500]}, // gap [500, 600): not reached
		{off: 950, b: src[950:1000]},
	}
	m.b375ExtendSnapLocked(ps, 1000)
	if m.rrSnap0 != 600 || !bytes.Equal(m.rrSnap, src[600:1000]) {
		t.Fatalf("snap0 %d len %d", m.rrSnap0, len(m.rrSnap))
	}
	// no ring snapshot: pieces must reach bi themselves
	m2 := &mConn{}
	m2.rrSnap0 = ^uint64(0)
	m2.b375ExtendSnapLocked([]b375Piece{{off: 0, b: src[0:500]}, {off: 500, b: src[500:1000]}}, 1000)
	if m2.rrSnap0 != 0 || !bytes.Equal(m2.rrSnap, src) {
		t.Fatalf("snap0 %d", m2.rrSnap0)
	}
	m3 := &mConn{}
	m3.rrSnap0 = ^uint64(0)
	m3.b375ExtendSnapLocked([]b375Piece{{off: 0, b: src[0:500]}}, 1000)
	if m3.rrSnap0 != ^uint64(0) || m3.rrSnap != nil {
		t.Fatal("a range not reaching bi was taken")
	}
	// key off: nothing collected
	b375RRCover.Store(false)
	m.b375HoldLocked([]*msub{{q: []*mchunk{b375TestChunk(0, src[:100])}}}, 1000)
	if len(m.b375Hold) != 0 {
		t.Fatal("hold with the key off")
	}
}

// The ring cap: the in-flight floor and the young window's rate.
func TestB375RingCap(t *testing.T) {
	setB375(t, true)
	setB375Floor(t, 2)
	r := &rescueRing{rec: true, floor: true}
	if c := r.capB(); c != rrRingCap {
		t.Fatalf("cap %d", c)
	}
	r.infl = 200 << 10
	if c := r.capB(); c != 200<<10 {
		t.Fatalf("infl cap %d", c)
	}
	r.infl = 100 << 20
	if c := r.capB(); c != max(rrRingMax, rrRingCap) {
		t.Fatalf("capped %d", c)
	}
	r.infl = 0
	r.winT0, r.winN = monoNano(time.Now()), 100<<10 // 100 KB in the first window
	if c := r.capB(); c < 100<<10 {
		t.Fatalf("young cap %d", c)
	}
	r.floor = false // RINGFLOOR=1 and the conn is no call / no heavy queue
	r.infl = 200 << 10
	if c := r.capB(); c != rrRingCap {
		t.Fatalf("unmarked cap %d", c)
	}
	r.floor = true
	b375RingFloor.Store(0) // RRCOVER alone: no floor
	if c := r.capB(); c != rrRingCap {
		t.Fatalf("floor off cap %d", c)
	}
}

// RINGFLOOR=1 marks only calls and conns with a heavy queue; 0 marks none.
func TestB375FloorGate(t *testing.T) {
	setB375(t, true)
	for _, tc := range []struct {
		f      int32
		call   bool
		q      int
		marked bool
	}{{0, true, 64 << 10, false}, {1, false, 8 << 10, false}, {1, true, 0, true}, {1, false, 32 << 10, true}, {2, false, 0, true}} {
		setB375Floor(t, tc.f)
		m := &mConn{}
		m.subs = []*msub{{qBytes: tc.q}}
		m.rk.call = tc.call
		m.b375InflLocked()
		if m.rk.floor != tc.marked || (m.rk.infl > 0) != tc.marked {
			t.Fatalf("%+v: floor %v infl %d", tc, m.rk.floor, m.rk.infl)
		}
	}
}

// The trace's shape without a keeping conn: 20 KB written into the dark
// link (kept nowhere) then 12 KB queued, past a 16 KB base ring. Key on: the
// ring's in-flight floor (one lite queue + the queue) held the written part
// and the queue covers the rest. Key off: nocover.
func TestB375RRCoverRingInfl(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    int32
		call bool
		on   bool // covered
	}{{"cover-only", 0, false, false}, {"floor-all", 2, false, true}, {"floor-call", 1, true, true}, {"floor-call-nocall", 1, false, false}} {
		on := tc.on
		t.Run(tc.name, func(t *testing.T) {
			setB375(t, true)
			setB375Floor(t, tc.f)
			setRRRingCap(t, 16<<10)
			n, iran, exit := newRRPair(t)
			if tc.call {
				iran.mu.Lock()
				iran.callOn = true
				iran.mu.Unlock()
			}
			set := rrLiveKeys(iran)
			blackhole(n, set)
			w := randBytes(20000, 61)
			if _, err := iran.Write(w); err != nil {
				t.Fatal(err)
			}
			mWaitFor(t, "written", time.Second, func() bool {
				iran.mu.Lock()
				defer iran.mu.Unlock()
				for _, s := range iran.rrLiveSubsLocked() {
					if len(s.q) > 0 || s.cur != nil {
						return false
					}
				}
				return true
			})
			iran.mu.Lock()
			for _, s := range iran.rrLiveSubsLocked() {
				iran.abandonSentLocked(s) // the written bytes are kept nowhere
			}
			iran.mu.Unlock()
			gateLive(iran)
			q := randBytes(12000, 62)
			if _, err := iran.Write(q); err != nil {
				t.Fatal(err)
			}
			rrNow(t, iran, set)
			if !on {
				waitLost(t, iran)
				return
			}
			all := append(append([]byte(nil), w...), q...)
			if err := readN(exit, all); err != nil {
				t.Fatal(err)
			}
			noExtra(t, iran, exit)
			closeAndWait(t, iran, exit)
		})
	}
}

// TestRescueRstBeyondRing's shape with the key on: the 8 KB base ring's
// in-flight floor holds the 40 KB, the RRST rescue covers it and the conn
// goes on byte-exact; 300 KB is past any floor and still ends.
func TestB375RstNowCovered(t *testing.T) {
	for _, sz := range []int{40000, 300000} {
		t.Run(itoa(sz), func(t *testing.T) {
			setB375(t, true)
			setB375Floor(t, 2)
			setRst(t, true, true)
			oldSecs := rrRateSecs
			rrRateSecs = 0
			t.Cleanup(func() { rrRateSecs = oldSecs })
			setRRRingCap(t, 8<<10)
			n, iran, exit := newRstPair(t)
			req, resp := randBytes(sz, 71), randBytes(sz, 72)
			rstKillPrimary(t, n, iran, exit, req, resp)
			if sz > 100000 {
				mWaitFor(t, "both ends", rstGrace+2*time.Second, func() bool { return rstEnded(iran) && rstEnded(exit) })
				return
			}
			both(t,
				func() error { return readN(exit, req) },
				func() error { return readN(iran, resp) },
			)
			noExtra(t, iran, exit)
			if rstEnded(iran) || rstEnded(exit) {
				t.Fatalf("conn ended: iran %v exit %v", iran.err, exit.err)
			}
			closeAndWait(t, iran, exit)
		})
	}
}

// b375TestChunk: a queued DATA chunk (one queue reference) holding b at off.
func b375TestChunk(off uint64, b []byte) *mchunk {
	c := getChunkN(mRecData, mDataHdr+len(b))
	c.n = copy(c.buf[mDataHdr:], b)
	c.setOff(off)
	c.refs.Store(1)
	return c
}

// The fixed rrStart: references, no copy; the answer reads the held chunks
// only when the ring falls short, and every reference is dropped after.
func TestB375HoldZeroCopy(t *testing.T) {
	setB375(t, true)
	src := make([]byte, 4000)
	for i := range src {
		src[i] = byte(i*7 + 3)
	}
	mk := func() (*mConn, []*mchunk) {
		var cs []*mchunk
		for o := 0; o < 4000; o += 500 {
			cs = append(cs, b375TestChunk(uint64(o), src[o:o+500]))
		}
		s1 := &msub{q: cs[4:6], cur: cs[6], curMore: []*mchunk{cs[7]}}
		s2 := &msub{q: []*mchunk{cs[4]}, sent: []sentRec{{c: cs[0]}, {c: cs[1]}, {c: cs[2]}, {c: cs[3]}}}
		m := &mConn{}
		m.rrBi = 4000
		m.b375HoldLocked([]*msub{s1, s2}, 4000)
		return m, cs
	}
	m, cs := mk()
	if len(m.b375Hold) != 8 {
		t.Fatalf("held %d", len(m.b375Hold))
	}
	for i, c := range cs {
		if c.refs.Load() != 2 {
			t.Fatalf("chunk %d refs %d", i, c.refs.Load())
		}
	}
	// ring covers [3000, 4000); Rk 1200 needs the hold
	m.rrSnap, m.rrSnap0 = append([]byte(nil), src[3000:]...), 3000
	m.b375CoverLocked(1200)
	if m.rrSnap0 != 0 || !bytes.Equal(m.rrSnap, src) || len(m.b375Hold) != 0 {
		t.Fatalf("snap0 %d len %d hold %d", m.rrSnap0, len(m.rrSnap), len(m.b375Hold))
	}
	for i, c := range cs {
		if c.refs.Load() != 1 {
			t.Fatalf("chunk %d refs %d after release", i, c.refs.Load())
		}
	}
	// ring covers Rk: no copy, only the release
	m, cs = mk()
	m.rrSnap, m.rrSnap0 = append([]byte(nil), src[3000:]...), 3000
	m.b375CoverLocked(3500)
	if m.rrSnap0 != 3000 || len(m.b375Hold) != 0 || cs[0].refs.Load() != 1 {
		t.Fatalf("snap0 %d", m.rrSnap0)
	}
	// end while waiting releases too
	m, cs = mk()
	m.b375ReleaseLocked()
	if cs[3].refs.Load() != 1 {
		t.Fatal("not released")
	}
}

// rrStart's cost under m.mu: the old copy versus the references.
func b375BenchSubs(nq, nsent int) []*msub {
	src := make([]byte, mChunkMax)
	var subs []*msub
	off := uint64(0)
	for k := 0; k < 2; k++ {
		s := &msub{}
		for i := 0; i < nsent; i++ {
			s.sent = append(s.sent, sentRec{c: b375TestChunk(off, src)})
			off += uint64(len(src))
		}
		for i := 0; i < nq; i++ {
			s.q = append(s.q, b375TestChunk(off, src))
			off += uint64(len(src))
		}
		subs = append(subs, s)
	}
	return subs
}

func b375OldCopy(old []*msub, bi uint64) []b375Piece {
	seen := map[*mchunk]bool{}
	var out []b375Piece
	add := func(c *mchunk) {
		if c == nil || c.kind != mRecData || c.n <= 0 || seen[c] || c.off >= bi {
			return
		}
		seen[c] = true
		n := min(uint64(c.n), bi-c.off)
		out = append(out, b375Piece{off: c.off, b: append([]byte(nil), c.buf[mDataHdr:mDataHdr+int(n)]...)})
	}
	for _, s := range old {
		for _, c := range s.q {
			add(c)
		}
		add(s.cur)
		for _, c := range s.curMore {
			add(c)
		}
		for _, r := range s.sent {
			add(r.c)
		}
	}
	return out
}

func BenchmarkB375RRStartOldCopy(b *testing.B) {
	subs := b375BenchSubs(40, 40) // ~2.6 MB held by two retired heavy subs
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = b375OldCopy(subs, ^uint64(0)>>1)
	}
}

func BenchmarkB375RRStartHold(b *testing.B) {
	b375RRCover.Store(true)
	subs := b375BenchSubs(40, 40)
	m := &mConn{}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		m.b375HoldLocked(subs, ^uint64(0)>>1)
		m.b375ReleaseLocked()
	}
}
