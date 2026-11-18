package engine

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// cs193 (HS2_MCONN_DARKCALL).

func setDarkcall(t *testing.T) {
	t.Helper()
	old, oa, og := darkcallOn.Load(), darkcallAfter(), darkcallGap()
	oR, oM := darkcallReadP.Load(), darkcallMarksP.Load()
	darkcallOn.Store(true)
	setDarkcallTimes(50*time.Millisecond, 0)
	setRRRingCap(t, 4<<20) // in step with the rescue sweeper (race-safe)
	t.Cleanup(func() {
		darkcallOn.Store(old)
		setDarkcallTimes(oa, og)
		darkcallReadP.Store(oR)
		darkcallMarksP.Store(oM)
	})
}

// dcFake: per-sub fake link evidence.
type dcFake struct {
	mu   sync.Mutex
	tcs  map[*msub]*net.TCPConn
	path map[*msub]string
	// state per sub: dark (no ack, backoff 3, rx silent), lossy (no ack,
	// backoff 3, rx fresh), noback (no ack, rx silent, backoff 0), else live
	st   map[*msub]string
	tick atomic.Uint64
}

func newDcFake() *dcFake {
	return &dcFake{tcs: map[*msub]*net.TCPConn{}, path: map[*msub]string{}, st: map[*msub]string{}}
}

func (f *dcFake) set(s *msub, st string) { f.mu.Lock(); f.st[s] = st; f.mu.Unlock() }

func (f *dcFake) read(s *msub) (*net.TCPConn, string, int64, uint64, uint32, uint8, uint32, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tc := f.tcs[s]
	if tc == nil {
		tc = &net.TCPConn{}
		f.tcs[s] = tc
	}
	p := f.path[s]
	if p == "" {
		p = "p" + string(rune('a'+len(f.path)))
		f.path[s] = p
	}
	now := ctrlNow()
	silent := now - int64(10*time.Second)
	switch f.st[s] {
	case "dark":
		return tc, p, silent, 1, 100, 3, 9, true
	case "lossy":
		return tc, p, now, 1, 100, 3, 9, true
	case "noback":
		return tc, p, silent, 1, 100, 0, 0, true
	case "stall0": // b342: no ack progress, backoff 0, no retransmits, rx fresh
		return tc, p, now, 1, 100, 0, 0, true
	}
	return tc, p, now, f.tick.Add(1), 10, 0, 0, true
}

func dcVisit(m *mConn, times int) {
	for i := 0; i < times; i++ {
		m.mu.Lock()
		m.callLikeAt = time.Now()
		rr := m.darkcallLocked(time.Now(), ctrlNow())
		m.mu.Unlock()
		if rr {
			go m.rrStart()
		}
		time.Sleep(darkcallAfter() * 3 / 2)
	}
}

// The exit's primary goes dark (writes vanish, no ack, RTO backoff, nothing
// received on its link) with a call download in flight while the other sub
// lives: the timer moves the primary and resends what the peer had not
// consumed; Iran reads every byte once, in order, and the call goes on.
func TestDarkcallSwitchResendsInFlight(t *testing.T) {
	setDarkcall(t)
	n, iran, exit := darkswPairDC(t)
	exit.mu.Lock()
	p := exit.primary.Load()
	before := exit.sendOff
	exit.mu.Unlock()
	l := exitLinkOf(n, p)
	if l == nil {
		t.Fatal("no link for the exit primary")
	}
	f := newDcFake()
	darkcallReadSet(f.read)
	darkcallMarksSet(func(s *msub) (uint32, uint32, bool) {
		if s != p {
			return 0, 0, true
		}
		u := uint32(exit.sendOff-before) + uint32(exit.sendOff-before)/100 + 64
		return u, 0, true
	})
	l.exit.drop.Store(true)
	f.set(p, "dark")
	sw0, cov0 := darkcallStat.switched.Load(), darkcallStat.covered.Load()
	data := randBytes(150_000, 21)
	got := make(chan error, 1)
	go func() { got <- readN(iran, data) }()
	if err := xfer(exit, data, 2000, 0); err != nil {
		t.Fatal(err)
	}
	dcVisit(exit, 3)
	exit.mu.Lock()
	newP := exit.primary.Load()
	exit.mu.Unlock()
	if darkcallStat.switched.Load() == sw0 {
		t.Fatal("no switch")
	}
	if newP == p {
		t.Fatal("primary still on the dark sub")
	}
	if darkcallStat.covered.Load() == cov0 {
		t.Fatal("the ring did not cover the unconsumed range")
	}
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("download stuck after the switch")
	}
	noExtra(t, iran, exit)
	more, up := randBytes(30_000, 22), randBytes(20_000, 23)
	both(t,
		func() error { return xfer(exit, more, 2000, 0) },
		func() error { return readN(iran, more) },
		func() error { return xfer(iran, up, 2000, 0) },
		func() error { return readN(exit, up) },
	)
	closeAndWait(t, iran, exit)
}

// darkswPairDC: cs189's two-sub records pair.
func darkswPairDC(t *testing.T) (*tNet, *mConn, *mConn) {
	t.Helper()
	n, iran, exit := newRRPair(t)
	if len(rrLiveKeys(iran)) < 2 {
		sub, key, err := n.open(planeLight, nil)
		if err != nil {
			t.Fatal(err)
		}
		iran.mu.Lock()
		s := iran.addSubLocked(sub, key, false)
		iran.mu.Unlock()
		iran.startSub(s)
	}
	mWaitFor(t, "two exit subs", 2*time.Second, func() bool {
		exit.mu.Lock()
		defer exit.mu.Unlock()
		return len(exit.rrLiveSubsLocked()) >= 2
	})
	return n, iran, exit
}

// exitLinkOf: the tLink whose exit end s rides.
func exitLinkOf(n *tNet, s *msub) *tLink {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, l := range n.links {
		if mSub(l.exit) == s.rw {
			return l
		}
	}
	return nil
}

// Evidence gates: a lossy link (backoff but something received), a silent
// link without RTO backoff, a dark primary whose only candidate is on the
// same path, and a connection that is not a call never act; the gap holds
// a second action back.
func TestDarkcallEvidenceGates(t *testing.T) {
	setDarkcall(t)
	setDc254(t, false) // the rel69i rule: a lossy (rx fresh) link never acts; dc254 tests it on
	_, iran, exit := darkswPairDC(t)
	f := newDcFake()
	darkcallReadSet(f.read)
	darkcallMarksSet(func(*msub) (uint32, uint32, bool) { return 0, 0, true })
	exit.mu.Lock()
	p := exit.primary.Load()
	var o *msub
	for _, s := range exit.rrLiveSubsLocked() {
		if s != p {
			o = s
		}
	}
	exit.mu.Unlock()
	sw0, d0 := darkcallStat.switched.Load(), darkcallStat.dark.Load()
	for _, st := range []string{"lossy", "noback"} {
		f.set(p, st)
		dcVisit(exit, 4)
		if darkcallStat.switched.Load() != sw0 || darkcallStat.dark.Load() != d0 {
			t.Fatalf("%s link counted dark / switched", st)
		}
	}
	// same path candidate
	f.mu.Lock()
	f.path[o] = f.path[p]
	f.mu.Unlock()
	f.set(p, "dark")
	nc0 := darkcallStat.noCand.Load()
	dcVisit(exit, 3)
	if darkcallStat.switched.Load() != sw0 {
		t.Fatal("switched to a sub on the same path")
	}
	if darkcallStat.noCand.Load() == nc0 {
		t.Fatal("no-candidate not counted")
	}
	// not a call: not visited
	f.mu.Lock()
	f.path[o] = "other"
	f.mu.Unlock()
	v0 := darkcallStat.visits.Load()
	exit.mu.Lock()
	exit.callLikeAt, exit.callOn = time.Time{}, false
	exit.darkcallLocked(time.Now(), ctrlNow())
	exit.mu.Unlock()
	if darkcallStat.visits.Load() != v0 {
		t.Fatal("a non-call connection was visited")
	}
	// now it acts, once; the gap holds the next one
	darkcallGapNs.Store(int64(time.Hour))
	exit.mu.Lock()
	exit.dc.last = time.Time{}
	exit.mu.Unlock()
	dcVisit(exit, 3)
	if darkcallStat.switched.Load() != sw0+1 {
		t.Fatalf("switches %d, want 1", darkcallStat.switched.Load()-sw0)
	}
	// (cs194: a DARK target may be left inside the gap, see
	// TestDarkcallAgainWhenTargetDark; a lossy one may not)
	f.set(o, "lossy")
	f.set(p, "")
	dcVisit(exit, 3)
	if darkcallStat.switched.Load() != sw0+1 {
		t.Fatal("acted twice inside the gap")
	}
	closeAndWait(t, iran, exit)
}

// In-flight cap: a call sub with more unconsumed bytes than the cap takes no
// chunk; the next visit under the cap lets it go again. A stale cap is
// ignored.
func TestDarkcallInflightCap(t *testing.T) {
	setDarkcall(t)
	setDc254(t, false) // a lossy primary would be switched (dc254), which clears its cap
	_, iran, exit := darkswPairDC(t)
	f := newDcFake()
	darkcallReadSet(f.read)
	var unc atomic.Uint32
	darkcallMarksSet(func(*msub) (uint32, uint32, bool) { return unc.Load(), 0, true })
	exit.mu.Lock()
	p := exit.primary.Load()
	exit.mu.Unlock()
	unc.Store(uint32(darkcallCapMin) + 1)
	f.set(p, "lossy") // fx212: the cap needs a link without ack progress
	dcVisit(exit, 2)
	exit.mu.Lock()
	if p.fits(100) {
		exit.mu.Unlock()
		t.Fatal("capped sub still fits")
	}
	p.dc.fullAt -= int64(2 * time.Second)
	if !p.fits(100) {
		exit.mu.Unlock()
		t.Fatal("stale cap still blocks")
	}
	exit.mu.Unlock()
	unc.Store(100)
	dcVisit(exit, 1)
	exit.mu.Lock()
	ok := p.fits(100) && !p.dc.full
	exit.mu.Unlock()
	if !ok {
		t.Fatal("cap not released")
	}
	closeAndWait(t, iran, exit)
}

// Key off: the timer reads nothing.
func TestDarkcallOff(t *testing.T) {
	setDarkcall(t)
	darkcallOn.Store(false)
	called := false
	darkcallReadSet(func(*msub) (*net.TCPConn, string, int64, uint64, uint32, uint8, uint32, bool) {
		called = true
		return nil, "", 0, 0, 0, 0, 0, false
	})
	// the loop body: off skips the tick
	if darkcallOn.Load() {
		darkcallTick(time.Now(), ctrlNow())
	}
	if called {
		t.Fatal("read with the key off")
	}
}

// cs194: HS2_MCONN_DARKCALL_TRACE with the key off observes only: a dark
// primary is seen (counted, traced) but neither switched nor capped.
func TestDarkcallTraceObserveOnly(t *testing.T) {
	setDarkcall(t)
	darkcallOn.Store(false)
	ot := darkcallTrace.Load()
	darkcallTrace.Store(true)
	t.Cleanup(func() { darkcallTrace.Store(ot) })
	if !darkcallRunning() {
		t.Fatal("trace alone does not run the timer")
	}
	_, iran, exit := darkswPairDC(t)
	f := newDcFake()
	darkcallReadSet(f.read)
	darkcallMarksSet(func(*msub) (uint32, uint32, bool) { return uint32(darkcallCapMin) + 1, 0, true })
	exit.mu.Lock()
	p := exit.primary.Load()
	exit.mu.Unlock()
	f.set(p, "dark")
	d0, s0 := darkcallStat.dark.Load(), darkcallStat.switched.Load()
	dcVisit(exit, 4)
	exit.mu.Lock()
	still, full, traced := exit.primary.Load() == p, p.dc.full, !exit.dc.trAt.IsZero()
	exit.mu.Unlock()
	if !still || full || darkcallStat.switched.Load() != s0 {
		t.Fatalf("observe-only acted: same primary %v full %v", still, full)
	}
	if darkcallStat.dark.Load() == d0 || !traced {
		t.Fatal("observe-only saw nothing")
	}
	closeAndWait(t, iran, exit)
}

func TestDarkcallAfterClamp(t *testing.T) {
	for _, c := range []struct {
		ms   int
		want time.Duration
	}{{1000, time.Second}, {700, 700 * time.Millisecond}, {10, 300 * time.Millisecond}, {99999, 5 * time.Second}} {
		if got := darkcallAfterMs(c.ms); got != c.want {
			t.Fatalf("%d ms: %v want %v", c.ms, got, c.want)
		}
	}
}

// cs194: inside the 3 s gap the timer acts again only when the sub the last
// switch went to is dark itself; otherwise the gap holds.
func TestDarkcallAgainWhenTargetDark(t *testing.T) {
	setDarkcall(t)
	darkcallGapNs.Store(int64(time.Hour))
	_, iran, exit := darkswPairDC(t)
	f := newDcFake()
	darkcallReadSet(f.read)
	darkcallMarksSet(func(*msub) (uint32, uint32, bool) { return 0, 0, true })
	exit.mu.Lock()
	p := exit.primary.Load()
	var o *msub
	for _, s := range exit.rrLiveSubsLocked() {
		if s != p {
			o = s
		}
	}
	exit.mu.Unlock()
	f.set(p, "dark")
	dcVisit(exit, 3)
	exit.mu.Lock()
	first := exit.primary.Load() == o
	exit.mu.Unlock()
	if !first {
		t.Fatal("no first switch")
	}
	// the old primary heals, the target is lossy (not dark): the gap holds
	f.set(p, "")
	f.set(o, "lossy")
	dcVisit(exit, 3)
	exit.mu.Lock()
	held := exit.primary.Load() == o
	exit.mu.Unlock()
	if !held {
		t.Fatal("acted inside the gap without a dark target")
	}
	a0 := darkcallStat.again.Load()
	f.set(o, "dark")
	dcVisit(exit, 3)
	exit.mu.Lock()
	back := exit.primary.Load() == p
	exit.mu.Unlock()
	if !back || darkcallStat.again.Load() == a0 {
		t.Fatal("dark target not left inside the gap")
	}
	closeAndWait(t, iran, exit)
}

func TestDarkcallPathDiv(t *testing.T) {
	for _, c := range []struct {
		a, b string
		n    int
	}{{"10.60.4.1>10.70.1.2", "10.60.4.1>10.70.2.2", 1}, {"10.60.4.1>10.70.1.2", "10.60.5.1>10.70.2.2", 2},
		{"10.60.4.1>10.70.1.2", "10.60.4.1>10.70.1.2", 0}, {"pa", "pb", 0}, {"[fd00::1]:1>x", "y>x", 1}} {
		if n := dcPathDiv(c.a, c.b); n != c.n {
			t.Fatalf("%s vs %s: %d want %d", c.a, c.b, n, c.n)
		}
	}
}
