package engine

import (
	"testing"
	"time"
)

// b262: a switch whose target was given every chunk (cs221 missEnd low) but
// provably wrote only part of the unconsumed range resends the rest from the
// ring; with the key off (b261) it resends nothing and calls it covered.
func TestB262ClaimNeedsWritten(t *testing.T) {
	setDarkcall(t)
	oldK, oldC := cs221Darkcall.Load(), b262CoverOn.Load()
	t.Cleanup(func() { cs221Darkcall.Store(oldK); b262CoverOn.Store(oldC) })
	cs221Darkcall.Store(true)
	_, iran, exit := darkswPairDC(t)
	f := newDcFake()
	darkcallReadSet(f.read)
	const unc = 60_000
	darkcallMarksSet(func(*msub) (uint32, uint32, bool) { return unc, 0, true })
	data := randBytes(150_000, 41)
	both(t,
		func() error { return xfer(exit, data, 2000, 0) },
		func() error { return readN(iran, data) },
	)
	exit.mu.Lock()
	p := exit.primary.Load()
	var to *msub
	for _, s := range exit.rrLiveSubsLocked() {
		if s != p {
			to = s
		}
	}
	if to == nil || exit.missLost || len(p.q) != 0 || len(to.q) != 0 {
		exit.mu.Unlock()
		t.Fatalf("setup: to=%v missLost=%v", to, exit.missLost)
	}
	qStart := exit.sendOff
	from := qStart - unc
	// the hole: to was given every chunk (missEnd 0) but provably wrote only
	// from from+30000 on (the rest never reached its stream)
	run := func(on bool) (resent uint64, covered bool) {
		b262CoverOn.Store(on)
		p.dcLeft = false
		to.missEnd = 0
		to.wrLo, to.wrHi = from+30_000, qStart
		r0, c0 := darkcallStat.resent.Load(), darkcallStat.covered.Load()
		exit.dcSwitchLocked(p, to, time.Now(), 0)
		// the resend went on to: take it off again (the next run sees the
		// same state; the peer has every byte already)
		to.q = to.q[:0]
		to.qBytes = 0
		return darkcallStat.resent.Load() - r0, darkcallStat.covered.Load() > c0
	}
	if got, cov := run(false); got != 0 || !cov {
		exit.mu.Unlock()
		t.Fatalf("key off (b261): resent %d covered %v, want 0 true (the hole)", got, cov)
	}
	if got, cov := run(true); got < 30_000 || got > 34_000 || !cov { // from may sit one in-flight chunk lower
		exit.mu.Unlock()
		t.Fatalf("key on: resent %d covered %v, want ~30000 true", got, cov)
	}
	// queued (not yet written) chunks on to count as held: nothing resent
	b262CoverOn.Store(true)
	p.dcLeft = false
	to.missEnd = 0
	to.wrLo, to.wrHi = 0, 0
	c := getChunk(mRecData)
	c.n = int(unc) + 4000 // from may sit one in-flight chunk lower
	c.setOff(from - 4000)
	to.q = append(to.q, c)
	r0 := darkcallStat.resent.Load()
	exit.dcSwitchLocked(p, to, time.Now(), 0)
	got := darkcallStat.resent.Load() - r0
	to.q = to.q[:0]
	exit.mu.Unlock()
	if got != 0 {
		t.Fatalf("key on, to has [from,qStart) queued: resent %d, want 0", got)
	}
	noExtra(t, iran, exit)
	more := randBytes(40_000, 42)
	both(t,
		func() error { return xfer(exit, more, 2000, 0) },
		func() error { return readN(iran, more) },
	)
}

// b262: a ring that restarted empty (memory budget) holds none of the
// unconsumed range; b261 called that covered, b262 reports it short.
func TestB262RingShortNotCovered(t *testing.T) {
	setDarkcall(t)
	oldK, oldC := cs221Darkcall.Load(), b262CoverOn.Load()
	t.Cleanup(func() { cs221Darkcall.Store(oldK); b262CoverOn.Store(oldC) })
	cs221Darkcall.Store(true)
	_, iran, exit := darkswPairDC(t)
	f := newDcFake()
	darkcallReadSet(f.read)
	const unc = 60_000
	darkcallMarksSet(func(*msub) (uint32, uint32, bool) { return unc, 0, true })
	data := randBytes(150_000, 43)
	both(t,
		func() error { return xfer(exit, data, 2000, 0) },
		func() error { return readN(iran, data) },
	)
	exit.mu.Lock()
	p := exit.primary.Load()
	var to *msub
	for _, s := range exit.rrLiveSubsLocked() {
		if s != p {
			to = s
		}
	}
	if to == nil || !exit.rscCap {
		exit.mu.Unlock()
		t.Fatalf("setup: to=%v rscCap=%v", to, exit.rscCap)
	}
	run := func(on bool) (resent uint64, covered bool, short uint64) {
		b262CoverOn.Store(on)
		p.dcLeft = false
		to.missEnd = exit.sendOff // to was given nothing: no claim
		exit.rkMu.Lock()
		exit.rk.release() // the ring restarted empty at rkEnd
		exit.rk.on = true
		exit.rkMu.Unlock()
		r0, c0, s0 := darkcallStat.resent.Load(), darkcallStat.covered.Load(), b262Stat.ringShort.Load()+b262Stat.ringLag.Load()
		exit.dcSwitchLocked(p, to, time.Now(), 0)
		return darkcallStat.resent.Load() - r0, darkcallStat.covered.Load() > c0, b262Stat.ringShort.Load() + b262Stat.ringLag.Load() - s0
	}
	if got, cov, _ := run(false); got != 0 || !cov {
		exit.mu.Unlock()
		t.Fatalf("key off (b261): resent %d covered %v, want 0 true", got, cov)
	}
	if got, cov, sh := run(true); got != 0 || cov || sh != 1 {
		exit.mu.Unlock()
		t.Fatalf("key on: resent %d covered %v short +%d, want 0 false +1", got, cov, sh)
	}
	exit.mu.Unlock()
	noExtra(t, iran, exit)
}

func TestB262NoteWritten(t *testing.T) {
	s := &msub{}
	mk := func(off uint64, n int) *mchunk {
		c := getChunk(mRecData)
		c.n = n
		c.setOff(off)
		return c
	}
	s.noteWrittenLocked(mk(0, 100), []*mchunk{mk(100, 50)})
	if s.wrLo != 0 || s.wrHi != 150 {
		t.Fatalf("run [%d,%d), want [0,150)", s.wrLo, s.wrHi)
	}
	s.noteWrittenLocked(mk(400, 100), nil) // a gap: a new run
	if s.wrLo != 400 || s.wrHi != 500 {
		t.Fatalf("after gap [%d,%d), want [400,500)", s.wrLo, s.wrHi)
	}
	s.noteWrittenLocked(mk(300, 100), nil) // a resend just below: extends down
	if s.wrLo != 300 || s.wrHi != 500 {
		t.Fatalf("after resend [%d,%d), want [300,500)", s.wrLo, s.wrHi)
	}
	if h := b262HoldsFrom(s, 500); h != 300 {
		t.Fatalf("holds from %d, want 300", h)
	}
	s.q = append(s.q, mk(500, 100))
	if h := b262HoldsFrom(s, 600); h != 300 {
		t.Fatalf("written+queued holds from %d, want 300", h)
	}
	if h := b262HoldsFrom(s, 700); h != 700 {
		t.Fatalf("gap below 700: holds from %d, want 700", h)
	}
}

// b262 (lab1 YT1 0u3): with the new primary over its queue bound and the
// dark old primary with room, a chunk must not go out on the dark sub alone
// (b261 did: a 22-byte hole only the dark link held). The write waits until
// the live sub has room, then the chunk rides it.
func TestB262NoOnlyCopyOnDark(t *testing.T) {
	setDarkcall(t)
	oldC := b262CoverOn.Load()
	t.Cleanup(func() { b262CoverOn.Store(oldC) })
	_, iran, exit := darkswPairDC(t)
	warm := randBytes(20_000, 44)
	both(t,
		func() error { return xfer(exit, warm, 2000, 0) },
		func() error { return readN(iran, warm) },
	)
	for _, on := range []bool{false, true} {
		b262CoverOn.Store(on)
		exit.mu.Lock()
		p := exit.primary.Load()
		var to *msub
		for _, s := range exit.rrLiveSubsLocked() {
			if s != p {
				to = s
			}
		}
		if to == nil || exit.dupOff {
			exit.mu.Unlock()
			t.Fatalf("setup: to=%v dupOff=%v", to, exit.dupOff)
		}
		// p is the dark sub a switch left; to holds a resend over its bound
		p.dcLeft = true
		saved := to.qBytes
		to.qBytes = mSubQueue + mSubQueueLite + 1
		off := exit.sendOff
		exit.mu.Unlock()
		done := make(chan error, 1)
		go func() { _, err := exit.Write([]byte("tiny call frame 22 b..")); done <- err }()
		time.Sleep(100 * time.Millisecond)
		exit.mu.Lock()
		// p got the chunk (queued or written) while to had no room: the
		// copy is p's alone
		onlyDark := p.txEnd > off && to.txEnd <= off
		to.qBytes = saved
		p.dcLeft = false
		exit.sendCond.Broadcast()
		exit.mu.Unlock()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if on && onlyDark {
			t.Fatal("key on: a chunk went out on the dark sub alone")
		}
		if !on && !onlyDark {
			t.Fatal("key off (b261): want the chunk on the dark sub alone (the hole)")
		}
		if err := readN(iran, []byte("tiny call frame 22 b..")); err != nil {
			t.Fatalf("on=%v: read: %v", on, err)
		}
	}
	noExtra(t, iran, exit)
}

// b262: with 3 subs the call pair takes subs in turn; when the only fitting
// pick is the dark sub a switch left, a live sub with room joins it, and
// with no live sub with room the write waits (k 0) instead of giving the
// dark sub the only copy. Key off: the lone dark pick stands (b261).
func TestB262CallPairNotOnlyDark(t *testing.T) {
	oldCall, oldC, oldCs := mconnCall.Load(), b262CoverOn.Load(), cs221Darkcall.Load()
	t.Cleanup(func() { mconnCall.Store(oldCall); b262CoverOn.Store(oldC); cs221Darkcall.Store(oldCs) })
	mconnCall.Store(true)
	cs221Darkcall.Store(false)
	mk := func() (*mConn, *msub, *msub, *msub) {
		m := &mConn{lite: true}
		a, b, c := &msub{m: m, kick: make(chan struct{}, 1)}, &msub{m: m, kick: make(chan struct{}, 1)}, &msub{m: m, kick: make(chan struct{}, 1)}
		m.subs = []*msub{a, b, c}
		return m, a, b, c
	}
	send := func(m *mConn) (bool, bool, *mchunk) {
		ch := getChunkN(mRecData, 22)
		ch.n = 22
		ok, wait := m.callPairLocked(ch)
		return ok, wait, ch
	}
	// a dark (switch left it) with room; b full; c full but live
	for _, on := range []bool{false, true} {
		b262CoverOn.Store(on)
		m, a, b, c := mk()
		a.dcLeft = true
		b.qBytes, c.qBytes = mSubQueueLite, mSubQueueLite
		ok, wait, _ := send(m)
		if !on {
			if !ok || a.qBytes != 22 {
				t.Fatalf("key off: ok %v wait %v a.q %d, want the lone dark copy (b261)", ok, wait, a.qBytes)
			}
			continue
		}
		if ok || !wait || a.qBytes != 0 {
			t.Fatalf("key on, no live room: ok %v wait %v a.q %d, want wait", ok, wait, a.qBytes)
		}
		// c gets room: the chunk rides c too (a may keep its copy)
		c.qBytes = 0
		if ok, _, _ := send(m); !ok || c.qBytes != 22 {
			t.Fatalf("key on, c has room: ok %v c.q %d, want a copy on c", ok, c.qBytes)
		}
		// a and b dark, c live, all with room: the in-turn pair (a, b)
		// is both dark; c replaces b
		m1, a1, b1, c1 := mk()
		a1.dcLeft, b1.dc.txDark = true, true
		w0 := b262Stat.darkPair.Load()
		if ok, _, _ := send(m1); !ok || c1.qBytes != 22 || b1.qBytes != 0 || a1.qBytes != 22 {
			t.Fatalf("two dark picks: ok %v q a/b/c %d/%d/%d, want a and c", ok, a1.qBytes, b1.qBytes, c1.qBytes)
		}
		if b262Stat.darkPair.Load() != w0+1 {
			t.Fatal("darkpair not counted")
		}
		// every sub dark: unchanged (nothing better to wait for)
		m2, a2, b2, c2 := mk()
		a2.dcLeft, b2.dc.txDark, c2.dc.isDark = true, true, true
		if ok, _, _ := send(m2); !ok {
			t.Fatal("all dark: refused")
		}
	}
}

// b262 HS2_B262_WAIT_MS: a bounded darkwait lets the chunk go out as b261's
// (on the dark sub) once the bound passes, and counts it (give).
func TestB262WaitBound(t *testing.T) {
	setDarkcall(t)
	oldC, oldW := b262CoverOn.Load(), b262WaitMax
	t.Cleanup(func() { b262CoverOn.Store(oldC); b262WaitMax = oldW })
	b262CoverOn.Store(true)
	b262WaitMax = 60 * time.Millisecond
	_, iran, exit := darkswPairDC(t)
	warm := randBytes(20_000, 45)
	both(t,
		func() error { return xfer(exit, warm, 2000, 0) },
		func() error { return readN(iran, warm) },
	)
	exit.mu.Lock()
	p := exit.primary.Load()
	var to *msub
	for _, s := range exit.rrLiveSubsLocked() {
		if s != p {
			to = s
		}
	}
	if to == nil || exit.dupOff {
		exit.mu.Unlock()
		t.Fatalf("setup: to=%v dupOff=%v", to, exit.dupOff)
	}
	p.dcLeft = true
	saved := to.qBytes
	to.qBytes = mSubQueue + mSubQueueLite + 1
	exit.mu.Unlock()
	g0, n0 := b262Stat.dwGive.Load(), b262Stat.dwN.Load()
	var h0 [5]uint64
	for i := range h0 {
		h0[i] = b262Stat.dwHist[i].Load()
	}
	msg := []byte("tiny call frame 22 b..")
	done := make(chan error, 1)
	t0 := time.Now()
	go func() { _, err := exit.Write(msg); done <- err }()
	select {
	case <-done:
		t.Fatal("returned before the bound")
	case <-time.After(30 * time.Millisecond):
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bounded wait did not end")
	}
	el := time.Since(t0)
	exit.mu.Lock()
	to.qBytes = saved
	p.dcLeft = false
	exit.sendCond.Broadcast()
	exit.mu.Unlock()
	if el < 55*time.Millisecond {
		t.Fatalf("waited %v, want >= the 60 ms bound", el)
	}
	if b262Stat.dwGive.Load() != g0+1 || b262Stat.dwN.Load() != n0+1 {
		t.Fatalf("give %d ep %d, want +1 each", b262Stat.dwGive.Load()-g0, b262Stat.dwN.Load()-n0)
	}
	// the episode (60 ms .. 2 s) lands in exactly one histogram bucket, not the < 10 ms one
	var dh uint64
	for i := range h0 {
		dh += b262Stat.dwHist[i].Load() - h0[i]
	}
	if dh != 1 || b262Stat.dwHist[0].Load() != h0[0] {
		t.Fatalf("histogram: %d episodes counted (want 1), <10 ms bucket +%d", dh, b262Stat.dwHist[0].Load()-h0[0])
	}
	if err := readN(iran, msg); err != nil {
		t.Fatal(err)
	}
	noExtra(t, iran, exit)
}

// TestB262RingMissLag: a ring miss inside the peer's consumed-report lag
// (lab1 ZC1: lite conns, unc from a consumed count reported every 1 MB) is
// ringlag, not ringshort; one past from + the lag is ringshort.
func TestB262RingMissLag(t *testing.T) {
	cases := []struct {
		from, start, hi uint64
		proven          bool
	}{
		{0, 167, 814, false},                          // ZC1: an 814-byte conn, need [0,814)
		{251672, 342901, 269059, false},               // ZC1: 4 KB/s conn, 17 KB below hi
		{0, b262ReportLag, b262ReportLag + 10, false}, // exactly the lag: unproven
		{0, b262ReportLag + 1, b262ReportLag + 10, true},
		{100, 3 << 20, 2 << 20, true}, // hi bounds the miss: 2 MB - 100 > 1 MB
	}
	for _, c := range cases {
		if got := b262RingMissProven(c.from, c.start, c.hi); got != c.proven {
			t.Fatalf("from %d start %d hi %d: proven %v, want %v", c.from, c.start, c.hi, got, c.proven)
		}
	}
	s0, l0, k0 := b262Stat.ringShort.Load(), b262Stat.ringLag.Load(), b262Stat.ringLagKB.Load()
	b262RingMissLocked(251672, 342901, 269059)
	b262RingMissLocked(0, 4<<20, 3<<20)
	if s, l, k := b262Stat.ringShort.Load()-s0, b262Stat.ringLag.Load()-l0, b262Stat.ringLagKB.Load()-k0; s != 1 || l != 1 || k != (269059-251672)>>10 {
		t.Fatalf("counters short +%d lag +%d (%d KB), want +1 +1 (%d KB)", s, l, k, (269059-251672)>>10)
	}
}

// b266: HS2_B262_WAIT_MS defaults to a 500 ms bound (an unbounded darkwait
// held Kharej sends up to 5 s in lab1); =0 still gives the unbounded wait.
func TestB262WaitDefault(t *testing.T) {
	if b262WaitDefaultMs != 500 {
		t.Fatalf("default %d ms, want 500", b262WaitDefaultMs)
	}
	for v, want := range map[string]int{"": 500, " ": 500, "x": 500, "-3": 500, "0": 0, "300": 300, " 750 ": 750} {
		if got := b262WaitEnv(v); got != want {
			t.Fatalf("HS2_B262_WAIT_MS=%q: %d, want %d", v, got, want)
		}
	}
}
