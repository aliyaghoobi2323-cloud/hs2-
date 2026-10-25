package smux

import (
	"io"
	"sync"
	"testing"
	"time"
)

// Cover is the need plus the step of that window: the need is what the peer
// has in flight under it; within MaxStreamBuffer.
func TestCoverOf(t *testing.T) {
	for _, need := range []uint32{256 << 10, 2 << 20, 2<<20 + 1, 3 << 20} {
		w := Cover(need, 16<<20)
		if w-StepOf(need, w) != need {
			t.Errorf("Cover(%d) = %d, step %d: %d in flight, want the need", need, w, StepOf(need, w), w-StepOf(need, w))
		}
		if StepOf(need, w) != StepOf(need, need) {
			t.Errorf("Cover(%d) = %d: step %d, %d at the need: more updates per byte", need, w, StepOf(need, w), StepOf(need, need))
		}
	}
	if w := Cover(2<<20, 2<<20+512<<10); w != 2<<20+512<<10 {
		t.Errorf("Cover within MaxStreamBuffer: %d", w)
	}
}

// RanUp: a pause at the limit of the stream's hit gap and three of its
// frames' time at its rate over the cycle, or more.
func TestCoverRanUp(t *testing.T) {
	const ms = time.Millisecond
	for _, tc := range []struct {
		name              string
		pause, gap, cycle time.Duration
		read              uint64
		frame             int
		want              bool
	}{
		{"no pause", 0, 2 * ms, 75 * ms, 1 << 20, 16 << 10, false},
		{"under the hit gap", ms, 2 * ms, 4 * ms, 1 << 20, 16 << 10, false},
		{"a held peer at 112 Mbit/s: 15 ms", 15 * ms, 2 * ms, 75 * ms, 1 << 20, 16 << 10, true},
		{"three frames at its rate", 3516 * time.Microsecond, 2 * ms, 75 * ms, 1 << 20, 16 << 10, true},
		{"under three frames at its rate", 3 * ms, 2 * ms, 75 * ms, 1 << 20, 16 << 10, false},
		{"a 30 Mbit/s stream flowing on: 5 ms between frames", 5 * ms, 2 * ms, 280 * ms, 1 << 20, 16 << 10, false},
		{"under the core's own gap", 10 * ms, 20 * ms, 75 * ms, 1 << 20, 16 << 10, false},
		{"nothing read", 3 * ms, 2 * ms, 75 * ms, 0, 16 << 10, true},
	} {
		if got := RanUp(tc.pause, tc.gap, tc.cycle, tc.read, tc.frame); got != tc.want {
			t.Errorf("%s: RanUp = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// CoverNext: granted at a cycle that ran up to a window of the need or more
// while the reader keeps up and the session may grow; kept while the reader
// keeps up, until coverMiss updates in a row did not run up to it.
func TestCoverNext(t *testing.T) {
	const need = 2 << 20
	cover := Cover(need, 16<<20)
	for _, tc := range []struct {
		name                  string
		covered               bool
		miss                  uint8
		old, want             uint32
		ranUp, growOK, turnOK bool
		covered2              bool
		miss2                 uint8
	}{
		{"ran up at the need: granted", false, 0, need, 2 * need, true, true, true, true, 0},
		{"ran up under the need: not yet", false, 0, need / 2, 2 * need, true, true, true, false, 0},
		{"ran up, reader lags: none", false, 0, need, need, true, true, true, false, 0},
		{"ran up, the session may not grow", false, 0, need, 2 * need, true, false, true, false, 0},
		{"ran up, turnaround past the reference", false, 0, need, 2 * need, true, true, false, false, 0},
		{"no run up: none", false, 0, need, 2 * need, false, true, true, false, 0},
		{"covered, ran up: kept, misses reset", true, 7, cover, 2 * cover, true, false, false, true, 0},
		{"covered, no run up: a miss", true, 7, cover, 2 * cover, false, true, true, true, 8},
		{"covered, reader holds its pace: kept", true, 0, cover, cover, false, true, true, true, 1},
		{"covered, the last miss", true, coverMiss - 1, cover, 2 * cover, false, true, true, false, 0},
		{"covered, reader lags: gone", true, 0, cover, need, true, true, true, false, 0},
	} {
		c, m := CoverNext(tc.covered, tc.miss, tc.old, tc.want, need, tc.ranUp, tc.growOK, tc.turnOK)
		if c != tc.covered2 || m != tc.miss2 {
			t.Errorf("%s: CoverNext = %v, %d, want %v, %d", tc.name, c, m, tc.covered2, tc.miss2)
		}
	}
}

// heldRounds runs rounds of a peer an update behind (behind: the round trip
// of a path longer than a step of reads) that sends all that lets it frame by
// frame, each read as it arrives, and then waits pause at the limit for the
// next update; it returns what each round moved.
func heldRounds(t *testing.T, p *rawPeer, s *Stream, rounds int, pause time.Duration) []int {
	t.Helper()
	var moved []int
	for i := 0; i < rounds; i++ {
		p.sync(s)
		n := 0
		for r := p.behind(); r > 0; r -= 4096 {
			k := r
			if k > 4096 {
				k = 4096
			}
			p.send(k, 4096)
			p.settle(s)
			readN(t, s, k, 4096)
			n += k
		}
		moved = append(moved, n)
		time.Sleep(pause)
	}
	p.sync(s)
	return moved
}

// The half-window lock and its cover. A peer held by the window (an update
// behind, as over a round trip longer than a step of reads) moves a step
// per round trip at the need of 256 KiB -- the window less a step, half of
// it -- and the need per round trip once the stream is covered: the window
// is 1.5 times the need, its step the need's (no more updates per byte), the
// half need past it held from the pool, and all of it back in the pool once
// the stream is gone.
func TestCoverHalfWindowLock(t *testing.T) {
	const rounds, last = 24, 8
	avg := func(m []int) int {
		s := 0
		for _, v := range m[len(m)-last:] {
			s += v
		}
		return s / last
	}
	perMiB := func(upds [][2]uint32) float64 {
		n := len(upds) - 2 // past the first two (the start window, the first resize)
		return float64(n) / (float64(upds[len(upds)-1][0]-upds[1][0]) / (1 << 20))
	}
	// without a pool: no cover, the window at the need
	cfg0 := poolConfig(nil)
	need := uint32(cfg0.NeedStreamBuffer)
	_, s0, p0 := newRawPeer(t, cfg0)
	m0 := heldRounds(t, p0, s0, rounds, 10*time.Millisecond)
	if w := maxWindow(p0.updates()); w != need {
		t.Fatalf("no pool: window %d, want the need %d (updates %v)", w, need, p0.updates())
	}
	lock := int(need - StepOf(need, need))
	if a := avg(m0); a < lock*9/10 || a > lock*11/10 {
		t.Fatalf("no pool: %d moved per round trip, want the window less a step, %d (rounds %v)", a, lock, m0)
	}

	pool := NewWindowPool(4 << 20)
	cfg := poolConfig(pool)
	session, s, p := newRawPeer(t, cfg)
	m := heldRounds(t, p, s, rounds, 10*time.Millisecond)
	cover := Cover(need, uint32(cfg.MaxStreamBuffer))
	if w := maxWindow(p.updates()); w != cover {
		t.Fatalf("window %d, want the cover %d (updates %v)", w, cover, p.updates())
	}
	if a := avg(m); a < int(need)*9/10 || a > int(need)*11/10 {
		t.Fatalf("covered: %d moved per round trip, want the need %d (rounds %v, %v without a pool)", a, need, m, m0)
	}
	if u, u0 := perMiB(p.updates()), perMiB(p0.updates()); u > u0*1.05 {
		t.Fatalf("%.2f updates per MiB covered, %.2f at the need", u, u0)
	}
	excess := int(cover - need)
	if e, _ := extraOf(session); claimOf(s) != excess || int(e) != excess || pool.Free() != 4<<20-excess {
		t.Fatalf("claim %d, extra %d, pool free %d: want the cover's %d from the pool", claimOf(s), e, pool.Free(), excess)
	}
	t.Logf("per round trip: %d at the need, %d covered (window %d, %.2f updates per MiB)", avg(m0), avg(m), cover, perMiB(p.updates()))

	// the pool back in full: the stream closed, then the session
	s.Close()
	session.Close()
	deadline := time.Now().Add(5 * time.Second)
	for pool.Free() != 4<<20 {
		if time.Now().After(deadline) {
			e, sp := extraOf(session)
			t.Fatalf("closed: pool free %d of %d (extra %d, spare %d)", pool.Free(), 4<<20, e, sp)
		}
		time.Sleep(time.Millisecond)
	}
}

// A covered stream whose peer no longer waits at its window (its source
// slowed down, or the path is full with the need in flight: the data flows
// on past each limit) goes back to the need after coverMiss updates, and the
// pool gets the cover's part back once the claim's hold is over. (A frame
// that comes late at a limit, a scheduler's pause under -race, reads as a
// cycle that ran up to the window and holds the cover a while longer: the
// need is held to come within 4·coverMiss updates, never before coverMiss.)
func TestCoverGoesWhenTheWindowNoLongerHolds(t *testing.T) {
	pool := NewWindowPool(4 << 20)
	cfg := poolConfig(pool)
	session, s, p := newRawPeer(t, cfg)
	need := uint32(cfg.NeedStreamBuffer)
	heldRounds(t, p, s, 8, 10*time.Millisecond)
	if w := p.lastWindow(); w != Cover(need, uint32(cfg.MaxStreamBuffer)) {
		t.Fatalf("window %d, want the cover (updates %v)", w, p.updates())
	}
	before := len(p.updates())
	step := int(StepOf(need, Cover(need, uint32(cfg.MaxStreamBuffer))))
	keepUp(t, p, s, (coverMiss-1)*step)
	p.sync(s)
	if w := p.lastWindow(); w == need {
		t.Fatalf("the need after %d updates, before coverMiss %d", len(p.updates())-before, coverMiss)
	}
	for i := 0; i < 3*coverMiss+2 && p.lastWindow() != need; i++ {
		keepUp(t, p, s, step)
		p.sync(s)
	}
	if w := p.lastWindow(); w != need {
		t.Fatalf("window %d after %d updates that did not run up to it, want the need %d", w, len(p.updates())-before, need)
	}
	// the claim outlives the window by its hold (what was sent under it may
	// still come), then goes back as the stream reads on
	s.bufferLock.Lock()
	s.claimDown = monoNow() - 1
	s.bufferLock.Unlock()
	keepUp(t, p, s, 4096)
	if e, sp := extraOf(session); claimOf(s) != 0 || e != 0 || sp != 0 || pool.Free() != 4<<20 {
		t.Fatalf("claim %d extra %d spare %d pool free %d: the cover's part not back", claimOf(s), e, sp, pool.Free())
	}
}

// Without the pool's room the cover falls back to the window as it was: an
// empty pool (HS2_TUNE_SMUX_POOL=0) leaves the need and the half-window lock,
// counting each window it could not cover; a pool with less left gives what
// it has; and a session that may not grow (the window core's growOK) gets
// none.
func TestCoverFallsBackWithoutThePool(t *testing.T) {
	cfg := poolConfig(NewWindowPool(0))
	need := uint32(cfg.NeedStreamBuffer)
	session, s, p := newRawPeer(t, cfg)
	m := heldRounds(t, p, s, 12, 10*time.Millisecond)
	if w := maxWindow(p.updates()); w != need {
		t.Fatalf("empty pool: window %d, want the need %d", w, need)
	}
	if lock := int(need - StepOf(need, need)); m[len(m)-1] != lock {
		t.Fatalf("empty pool: %d moved in the last round trip, want %d (rounds %v)", m[len(m)-1], lock, m)
	}
	if _, c := session.CoreSnapshot(nil); c.PoolShort == 0 || c.Extra != 0 {
		t.Fatalf("empty pool: counters %+v, want windows the pool could not cover counted and no extra", c)
	}

	pool := NewWindowPool(64 << 10)
	cfg = poolConfig(pool)
	_, s, p = newRawPeer(t, cfg)
	heldRounds(t, p, s, 12, 10*time.Millisecond)
	// a session alone takes up to half of what the pool has (FreeFor), and
	// a cover no more than the covers' share (a quarter: coverShare)
	if w := maxWindow(p.updates()); w != need+16<<10 || claimOf(s) != 16<<10 {
		t.Fatalf("a short pool: window %d claim %d, want the need and the 16 KiB the covers' share of it has", w, claimOf(s))
	}

	cfg = poolConfig(NewWindowPool(4 << 20))
	session, s, p = newRawPeer(t, cfg)
	session.SetCoreRef(0, 0, false)
	heldRounds(t, p, s, 12, 10*time.Millisecond)
	if w := maxWindow(p.updates()); w != need {
		t.Fatalf("growOK off: window %d, want the need %d", w, need)
	}

	// the cover off in the config (HS2_TUNE_SMUX_COVER=0), or taken away by
	// the window core (SetCover): the need, and a cover held goes at the
	// next update
	cfg = poolConfig(NewWindowPool(4 << 20))
	cfg.NoCover = true
	_, s, p = newRawPeer(t, cfg)
	heldRounds(t, p, s, 12, 10*time.Millisecond)
	if w := maxWindow(p.updates()); w != need {
		t.Fatalf("NoCover: window %d, want the need %d", w, need)
	}
	cfg = poolConfig(NewWindowPool(4 << 20))
	session, s, p = newRawPeer(t, cfg)
	heldRounds(t, p, s, 8, 10*time.Millisecond)
	if w := p.lastWindow(); w != Cover(need, uint32(cfg.MaxStreamBuffer)) {
		t.Fatalf("window %d before SetCover(false), want the cover", w)
	}
	session.SetCover(false)
	heldRounds(t, p, s, 4, 10*time.Millisecond)
	if w := p.lastWindow(); w != need {
		t.Fatalf("SetCover(false): window %d, want the need %d", w, need)
	}
}

// A ceiling of the window core bounds the cover as any window: under it the
// window stops at the ceiling; past it the window grows on from the cover
// as the core lets it.
func TestCoverUnderACeiling(t *testing.T) {
	pool := NewWindowPool(4 << 20)
	cfg := poolConfig(pool)
	need := uint32(cfg.NeedStreamBuffer)
	_, s, p := newRawPeer(t, cfg)
	s.SetCoreCeil(need + 64<<10)
	heldRounds(t, p, s, 12, 10*time.Millisecond)
	if w := maxWindow(p.updates()); w != need+64<<10 {
		t.Fatalf("under a ceiling of %d: window %d", need+64<<10, w)
	}
	_, s2, p2 := newRawPeer(t, cfg)
	s2.SetCoreCeil(uint32(cfg.MaxStreamBuffer))
	heldRounds(t, p2, s2, 16, 10*time.Millisecond)
	if w := maxWindow(p2.updates()); w <= Cover(need, uint32(cfg.MaxStreamBuffer)) {
		t.Fatalf("under a ceiling of MaxStreamBuffer: window %d, want past the cover (updates %v)", w, p2.updates())
	}
}

// CoverPause: a pause counts when the update that ended it came back within
// coverTurn of the stream's previous turnaround (or it has none yet).
func TestCoverPause(t *testing.T) {
	const ms = time.Millisecond
	for _, tc := range []struct {
		turn, prev time.Duration
		want       bool
	}{
		{80 * ms, 0, true},
		{80 * ms, 75 * ms, true},
		{150 * ms, 75 * ms, true},
		{151 * ms, 75 * ms, false},
		{1331 * ms, 170 * ms, false}, // a PROBE_RTT's stall on 100/20
	} {
		if got := CoverPause(tc.turn, tc.prev); got != tc.want {
			t.Errorf("CoverPause(%v, %v) = %v, want %v", tc.turn, tc.prev, got, tc.want)
		}
	}
}

// A pause at a limit whose update came back far later than the stream's
// turnarounds -- the sender stopped for its own reasons -- is no run up to
// the window for the cover; the core's hit is as before.
func TestCoverStallIsNoRunUp(t *testing.T) {
	for _, stall := range []bool{false, true} {
		s := coreBare(4096)
		s.arrive(64 << 10)
		s.readAndAnnounce(64<<10, 1<<20)
		s.core.turnSeen = int64(20 * time.Millisecond)
		recv := s.core.readTot.Load() + uint64(s.unread)
		s.arrive(int(s.core.ring[s.core.head].lim - recv)) // up to the limit ...
		time.Sleep(hitGap + 2*time.Millisecond)            // ... a pause ...
		at := monoNow() - int64(25*time.Millisecond)
		if stall {
			at = monoNow() - int64(time.Second)
		}
		s.core.ring[s.core.head].at = at
		s.arrive(4096) // ... past it
		if !s.core.hit {
			t.Fatalf("stall %v: no hit", stall)
		}
		if counted := s.core.pause > 0; counted == stall {
			t.Fatalf("stall %v: pause %v counted for the cover %v", stall, time.Duration(s.core.pause), counted)
		}
	}
}

// The cover's counters: granted, gone as its peer no longer waits (miss),
// taken by the core, gone with an idle stream's claim and with a closed
// stream; and the count of streams holding pool memory for a cover, the
// session's and the pool's, which drops once the claim's hold is over.
func TestCoverCounters(t *testing.T) {
	pool := NewWindowPool(4 << 20)
	cfg := poolConfig(pool)
	need := uint32(cfg.NeedStreamBuffer)
	step := int(StepOf(need, Cover(need, uint32(cfg.MaxStreamBuffer))))
	session, s, p := newRawPeer(t, cfg)
	released := func(what string) {
		t.Helper()
		s.bufferLock.Lock()
		s.claimDown = monoNow() - 1 // the claim's hold is over
		s.reclaim(cfg)
		s.bufferLock.Unlock()
		if c := session.CoverStats(); c.Now != 0 || pool.CoverStreams() != 0 || pool.Covers() != 0 {
			t.Fatalf("%s, the claim let go: %+v, pool covers %d holding %d", what, c, pool.CoverStreams(), pool.Covers())
		}
	}
	heldRounds(t, p, s, 8, 10*time.Millisecond)
	if c := session.CoverStats(); c.Grants != 1 || c.Now != 1 || pool.CoverStreams() != 1 || pool.Covers() != claimOf(s) {
		t.Fatalf("held: %+v, pool covers %d of %d bytes (claim %d), want one granted", c, pool.CoverStreams(), pool.Covers(), claimOf(s))
	}
	for i := 0; i < 4*coverMiss && p.lastWindow() != need; i++ {
		keepUp(t, p, s, step)
		p.sync(s)
	}
	if c := session.CoverStats(); c.Miss != 1 {
		t.Fatalf("flowing on: %+v, want one gone for misses", c)
	}
	released("flowing on")
	heldRounds(t, p, s, 8, 10*time.Millisecond)
	session.SetCover(false)
	heldRounds(t, p, s, 2, 10*time.Millisecond)
	session.SetCover(true)
	if c := session.CoverStats(); c.Grants != 2 || c.Taken != 1 {
		t.Fatalf("taken by the core: %+v", c)
	}
	released("taken")
	heldRounds(t, p, s, 8, 10*time.Millisecond)
	s.bufferLock.Lock()
	s.readAt = monoNow() - int64(claimIdle)
	s.bufferLock.Unlock()
	session.sweepClaims()
	if c := session.CoverStats(); c.Grants != 3 || c.Idle != 1 {
		t.Fatalf("idle: %+v", c)
	}
	released("idle")
	_, s2, p2 := newRawPeer(t, cfg)
	heldRounds(t, p2, s2, 8, 10*time.Millisecond)
	if pool.CoverStreams() != 1 {
		t.Fatalf("a second link's cover: pool covers %d", pool.CoverStreams())
	}
	s2.Close()
	if c := s2.sess.CoverStats(); c.Closed != 1 || c.Now != 0 || pool.CoverStreams() != 0 {
		t.Fatalf("closed: %+v, pool covers %d", c, pool.CoverStreams())
	}
	if c := session.CoverStats(); c.Lost() != 3 {
		t.Fatalf("%+v: %d lost, want 3", c, c.Lost())
	}
}

// The covers of all the sessions sharing a pool hold at most a quarter of
// it (coverShare): with a cover held on every one of eight links, a link
// whose readers stopped (three of them at once) still borrows the room it
// would have with no cover anywhere (here half the pool), where covers
// within each session's share alone (FreeFor) would have left it 128 KiB.
func TestCoverLeavesBorrowItsRoom(t *testing.T) {
	pool := NewWindowPool(1 << 20)
	cfg := poolConfig(pool)
	capped := uint32(0)
	for i := 0; i < 8; i++ {
		session, s, p := newRawPeer(t, cfg)
		heldRounds(t, p, s, 8, 5*time.Millisecond)
		capped += session.CoverStats().Capped
	}
	if int64(pool.Covers()) > pool.size/coverShare || capped == 0 || pool.Free() < 3<<18 {
		t.Fatalf("covers hold %d of the pool (%d covered, %d capped), free %d: want at most a quarter, the rest free",
			pool.Covers(), pool.CoverStreams(), capped, pool.Free())
	}
	t.Logf("eight held links: %d covered holding %d B, %d windows capped, pool free %d", pool.CoverStreams(), pool.Covers(), capped, pool.Free())
	session, _, p := newRawPeer(t, cfg)
	var sids []uint32
	for i := 0; i < 3; i++ {
		_, sid := p.open(session)
		sids = append(sids, sid)
	}
	n := cfg.MaxReceiveBuffer + 512<<10 // past the session's buffer by half the pool, not read
	for _, sid := range sids {
		sendWithin(t, p, sid, n/len(sids), cfg.MaxFrameSize, "three readers stopped next to eight covered links")
	}
	if session.Borrows() == 0 {
		t.Fatal("no borrow")
	}
}

// With the cover off (Config.NoCover: HS2_TUNE_SMUX_COVER=0) a stream held
// by its window gets the window updates it gets with no pool at all, update
// for update: the need, as before the cover.
func TestCoverOffIsTheWindowsAsBefore(t *testing.T) {
	off := poolConfig(NewWindowPool(4 << 20))
	off.NoCover = true
	_, s, p := newRawPeer(t, off)
	heldRounds(t, p, s, 16, 5*time.Millisecond)
	_, s0, p0 := newRawPeer(t, poolConfig(nil))
	heldRounds(t, p0, s0, 16, 5*time.Millisecond)
	got, want := p.updates(), p0.updates()
	if len(got) != len(want) {
		t.Fatalf("%d updates with the cover off, %d with no pool:\n%v\n%v", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("update %d: %v with the cover off, %v with no pool", i, got[i], want[i])
		}
	}
}

// ---- a real pair through a delayed, paced path ------------------------------

// pacedPipe is one direction of an emulated path for the cover: a sender
// paced as BBR paces once it left STARTUP -- its gain cycle (1.25, 0.75,
// then 1 for six round trips) on the most it delivered in a round trip of
// the last ten -- within the path's rate, then a one-way delay. Writes never
// block: what the pacing has not let out yet waits in the pipe, as in a
// sender's socket. paced false: the path's rate alone (the way back).
type pacedPipe struct {
	mu      sync.Mutex
	cond    *sync.Cond
	q       []pacedChunk
	lastDep time.Time
	t0      time.Time
	rate    float64 // the path's own, bytes/s
	rtt     time.Duration
	delay   time.Duration
	paced   bool
	slots   map[int64]float64 // bytes leaving in each round trip since t0
	closed  bool
}

type pacedChunk struct {
	at time.Time
	b  []byte
}

func newPacedPipe(rate float64, rtt time.Duration, paced bool) *pacedPipe {
	p := &pacedPipe{rate: rate, rtt: rtt, delay: rtt / 2, paced: paced, t0: time.Now(), slots: map[int64]float64{}}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// pace is the rate the sender paces at now. The caller holds mu.
func (p *pacedPipe) pace(now time.Time) float64 {
	cur := int64(now.Sub(p.t0) / p.rtt)
	if !p.paced || cur < 4 {
		return p.rate // STARTUP: the path's rate
	}
	bw := 0.0
	for k := cur - 10; k < cur; k++ {
		if v := p.slots[k]; v > bw {
			bw = v
		}
	}
	for k := range p.slots {
		if k < cur-10 {
			delete(p.slots, k)
		}
	}
	gain := 1.0
	switch cur % 8 {
	case 0:
		gain = 1.25
	case 1:
		gain = 0.75
	}
	if bw == 0 {
		return p.rate
	}
	if r := gain * bw / p.rtt.Seconds(); r < p.rate {
		return r
	}
	return p.rate
}

func (p *pacedPipe) write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, io.ErrClosedPipe
	}
	now := time.Now()
	dep := now
	if p.lastDep.After(dep) {
		dep = p.lastDep
	}
	dep = dep.Add(time.Duration(float64(len(b)) / p.pace(now) * float64(time.Second)))
	p.lastDep = dep
	p.slots[int64(dep.Sub(p.t0)/p.rtt)] += float64(len(b))
	p.q = append(p.q, pacedChunk{dep.Add(p.delay), append([]byte(nil), b...)})
	p.cond.Broadcast()
	return len(b), nil
}

func (p *pacedPipe) read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		if p.closed {
			return 0, io.EOF
		}
		if len(p.q) == 0 {
			p.cond.Wait()
			continue
		}
		c := &p.q[0]
		if d := time.Until(c.at); d > 0 {
			p.mu.Unlock()
			time.Sleep(d)
			p.mu.Lock()
			continue
		}
		n := copy(b, c.b)
		if c.b = c.b[n:]; len(c.b) == 0 {
			p.q = p.q[1:]
		}
		return n, nil
	}
}

func (p *pacedPipe) close() {
	p.mu.Lock()
	p.closed = true
	p.cond.Broadcast()
	p.mu.Unlock()
}

type pacedEnd struct{ in, out *pacedPipe }

func (e pacedEnd) Read(b []byte) (int, error)  { return e.in.read(b) }
func (e pacedEnd) Write(b []byte) (int, error) { return e.out.write(b) }
func (e pacedEnd) Close() error                { e.in.close(); e.out.close(); return nil }

// pacedDownload runs one download over a fresh path of rtt from an exit
// paced as BBR to an edge of cfg reading at once, and returns its rate over
// [warm, warm+span], bytes/s.
func pacedDownload(t *testing.T, cfg *Config, rate float64, rtt, warm, span time.Duration) float64 {
	t.Helper()
	down, up := newPacedPipe(rate, rtt, true), newPacedPipe(1<<30, rtt, false)
	edge, err := Client(pacedEnd{in: down, out: up}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ecfg := adaptiveConfig()
	exit, err := Server(pacedEnd{in: up, out: down}, ecfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { edge.Close(); exit.Close(); down.close(); up.close() }()
	st, err := edge.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		x, err := exit.AcceptStream()
		if err != nil {
			return
		}
		buf := make([]byte, 64<<10)
		for {
			if _, err := x.Write(buf); err != nil {
				return
			}
		}
	}()
	var mu sync.Mutex
	var got int64
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, err := st.Read(buf)
			mu.Lock()
			got += int64(n)
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	read := func() int64 {
		mu.Lock()
		defer mu.Unlock()
		return got
	}
	time.Sleep(warm)
	a, t1 := read(), time.Now()
	time.Sleep(span)
	return float64(read()-a) / time.Since(t1).Seconds()
}

// Through a path of 50 ms whose sender paces as BBR does once it left
// STARTUP, a download at the need of 256 KiB moves about the window less a
// step per round trip (the half-window lock: 128 KiB, ~21 Mbit/s), and with
// the cover about the need (~42 Mbit/s), on the same path, from the same
// sender: the receiver alone makes the difference.
func TestCoverThroughAPacedPath(t *testing.T) {
	if testing.Short() {
		t.Skip("real time: seconds per download")
	}
	const rtt = 50 * time.Millisecond
	locked := poolConfig(nil)
	need := float64(locked.NeedStreamBuffer)
	rate := 4 * need / rtt.Seconds() // the path: four needs per round trip
	r0 := pacedDownload(t, locked, rate, rtt, 2*time.Second, 2*time.Second)
	covered := poolConfig(NewWindowPool(4 << 20))
	r1 := pacedDownload(t, covered, rate, rtt, 2*time.Second, 2*time.Second)
	lock := (need - float64(StepOf(uint32(need), uint32(need)))) / rtt.Seconds()
	t.Logf("at the need %.1f Mbit/s (the window less a step per round trip: %.1f), covered %.1f (the need per round trip: %.1f)",
		r0*8/1e6, lock*8/1e6, r1*8/1e6, need/rtt.Seconds()*8/1e6)
	if r0 > 1.4*lock {
		t.Errorf("at the need: %.0f B/s, want about the window less a step per round trip, %.0f", r0, lock)
	}
	if r1 < 0.8*need/rtt.Seconds() || r1 < 1.5*r0 {
		t.Errorf("covered: %.0f B/s, want about the need per round trip, %.0f (%.0f at the need)", r1, need/rtt.Seconds(), r0)
	}
}
