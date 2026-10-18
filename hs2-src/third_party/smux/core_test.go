package smux

import (
	"sync/atomic"
	"testing"
	"time"
)

// StepOf: half a window up to the need; past it need/2, or 1/updShare of
// the window if more, never more than half.
func TestCoreStepOf(t *testing.T) {
	const need = 2 << 20
	for _, tc := range []struct{ w, want uint32 }{
		{256 << 10, 128 << 10},
		{2 << 20, 1 << 20},
		{2<<20 + 1, 1 << 20},
		{3 << 20, 1 << 20},
		{8 << 20, 1 << 20},
		{12 << 20, 3 << 19},
		{16 << 20, 2 << 20},
	} {
		if got := StepOf(need, tc.w); got != tc.want {
			t.Errorf("StepOf(%d) = %d, want %d", tc.w, got, tc.want)
		}
	}
	// a need that is no step at all past it: never more than half
	if got := StepOf(4<<20, 5<<20); got != 2<<20 {
		t.Errorf("StepOf(4 MiB, 5 MiB) = %d, want 2 MiB (need/2), at most half", got)
	}
	if got := StepOf(8<<20, 9<<20); got != 4<<20 {
		t.Errorf("StepOf(8 MiB, 9 MiB) = %d, want need/2", got)
	}
	if got := StepOf(2<<20, 0); got != 0 {
		t.Errorf("StepOf(2 MiB, 0) = %d, want 0", got)
	}
}

// FitCore holds a window to the core's ceiling: none means the window as it
// always was (within the need); past the need it grows only while allowed,
// at most twice per update; past the ceiling it comes down to it at once,
// but no lower than MinStreamBuffer and what the stream holds; and it never
// gives more than adaptWindow wanted.
func TestCoreFitCore(t *testing.T) {
	const (
		mib  = 1 << 20
		need = 2 * mib
		min  = 64 << 10
	)
	for _, tc := range []struct {
		name                    string
		old, want, ceil, unread uint32
		growOK, turnOK          bool
		fit                     uint32
	}{
		{"no ceiling: base", 2 * mib, 4 * mib, 0, 0, true, true, need},
		{"no ceiling: base, braked", 2 * mib, 4 * mib, 0, 0, false, false, need},
		{"no ceiling: below the need", 256 << 10, 1 * mib, 0, 0, false, false, 1 * mib},
		{"no ceiling: shrink", 2 * mib, 512 << 10, 0, 0, true, true, 512 << 10},
		{"growth past the need", 2 * mib, 4 * mib, 8 * mib, 0, true, true, 4 * mib},
		{"growth at most doubles", 2 * mib, 16 * mib, 16 * mib, 0, true, true, 4 * mib},
		{"growth within the ceiling", 4 * mib, 8 * mib, 6 * mib, 0, true, true, 6 * mib},
		{"turnaround slow: held", 4 * mib, 8 * mib, 16 * mib, 0, true, false, 4 * mib},
		{"session holds growth", 4 * mib, 8 * mib, 16 * mib, 0, false, true, 4 * mib},
		{"held, but up to the need is the reader's", 1 * mib, 4 * mib, 16 * mib, 0, false, false, need},
		{"first resize jump: to the need, then doubling", 256 << 10, 16 * mib, 16 * mib, 0, true, true, need},
		{"drain: the ceiling at once", 8 * mib, 8 * mib, 3 * mib, 0, true, true, 3 * mib},
		{"drain: no lower than what it holds", 8 * mib, 8 * mib, 3 * mib, 5 * mib, true, true, 5 * mib},
		{"drain: no lower than MinStreamBuffer", 8 * mib, 8 * mib, 16 << 10, 0, true, true, min},
		{"drain: the floor never above want", 8 * mib, 4 * mib, 3 * mib, 6 * mib, true, true, 4 * mib},
		{"shrinking on its own: as wanted", 8 * mib, 3 * mib, 6 * mib, 0, true, true, 3 * mib},
		{"no change", 4 * mib, 4 * mib, 4 * mib, 0, false, false, 4 * mib},
		{"twice a huge window does not overflow", 3 << 30, ^uint32(0), ^uint32(0), 0, true, true, ^uint32(0)},
	} {
		got := FitCore(tc.old, tc.want, tc.ceil, need, min, tc.unread, tc.growOK, tc.turnOK)
		if got != tc.fit {
			t.Errorf("%s: FitCore(old %d, want %d, ceil %d, unread %d, %v, %v) = %d, want %d",
				tc.name, tc.old, tc.want, tc.ceil, tc.unread, tc.growOK, tc.turnOK, got, tc.fit)
		}
		if got > tc.want {
			t.Errorf("%s: %d past want %d", tc.name, got, tc.want)
		}
	}
}

// grown: a stream of poolConfig(pool) whose reader kept up under a ceiling
// of MaxStreamBuffer, so its window grew to it.
func grown(t *testing.T, pool *WindowPool) (*Session, *Stream, *rawPeer, *Config) {
	t.Helper()
	cfg := poolConfig(pool)
	session, stream, p := newRawPeer(t, cfg)
	stream.SetCoreCeil(uint32(cfg.MaxStreamBuffer))
	keepUp(t, p, stream, 8<<20)
	p.sync(stream)
	if w := p.lastWindow(); w != uint32(cfg.MaxStreamBuffer) {
		t.Fatalf("rig: window %d under a ceiling of MaxStreamBuffer, want it (updates %v)", w, p.updates())
	}
	return session, stream, p, cfg
}

// Under a ceiling a reader that keeps up grows its window past
// NeedStreamBuffer, at most doubling per update, to the ceiling; the excess
// comes from the pool and the session's buffer grows by just as much.
func TestCoreCeilGrowsPastNeed(t *testing.T) {
	pool := NewWindowPool(4 << 20)
	session, stream, p, cfg := grown(t, pool)
	upds := p.updates()
	for i := 2; i < len(upds); i++ {
		if upds[i][1] > uint32(cfg.NeedStreamBuffer) && upds[i][1] > 2*upds[i-1][1] {
			t.Fatalf("window more than doubled past NeedStreamBuffer: %v", upds)
		}
	}
	excess := cfg.MaxStreamBuffer - cfg.NeedStreamBuffer
	if e, sp := extraOf(session); int(e) != excess || sp != 0 || claimOf(stream) != excess {
		t.Fatalf("extra %d spare %d claim %d, want %d, 0, %d", e, sp, claimOf(stream), excess, excess)
	}
	if pool.Free() != 4<<20-excess {
		t.Fatalf("pool free %d, want %d", pool.Free(), 4<<20-excess)
	}
	if b := atomic.LoadInt32(&session.bucket); int(b) != cfg.MaxReceiveBuffer+excess-unreadOf(stream) {
		t.Fatalf("bucket %d, want MaxReceiveBuffer %d + excess %d - unread %d", b, cfg.MaxReceiveBuffer, excess, unreadOf(stream))
	}
	// a ceiling between the need and the maximum is where it stays
	_, s2, p2 := newRawPeer(t, cfg)
	s2.SetCoreCeil(uint32(cfg.NeedStreamBuffer) + 100<<10)
	keepUp(t, p2, s2, 4<<20)
	p2.sync(s2)
	if w := maxWindow(p2.updates()); w != uint32(cfg.NeedStreamBuffer)+100<<10 {
		t.Fatalf("max window %d under a ceiling of %d (updates %v)", w, cfg.NeedStreamBuffer+100<<10, p2.updates())
	}
	// SetCoreCeil holds a ceiling to MaxStreamBuffer
	s2.SetCoreCeil(^uint32(0))
	if c := s2.CoreCeil(); c != uint32(cfg.MaxStreamBuffer) {
		t.Fatalf("ceiling %d, want MaxStreamBuffer %d", c, cfg.MaxStreamBuffer)
	}
}

// A window grows past NeedStreamBuffer only as far as the pool has left,
// and each update the pool falls short is counted.
func TestCoreCeilBoundedByPool(t *testing.T) {
	pool := NewWindowPool(100 << 10)
	cfg := poolConfig(pool)
	session, stream, p := newRawPeer(t, cfg)
	stream.SetCoreCeil(uint32(cfg.MaxStreamBuffer))
	keepUp(t, p, stream, 6<<20)
	p.sync(stream)
	if w := maxWindow(p.updates()); w != uint32(cfg.NeedStreamBuffer+100<<10) {
		t.Fatalf("max window %d, want NeedStreamBuffer + the pool's 100 KiB = %d (updates %v)", w, cfg.NeedStreamBuffer+100<<10, p.updates())
	}
	if e, _ := extraOf(session); e != 100<<10 || pool.Free() != 0 {
		t.Fatalf("extra %d pool free %d, want the whole pool taken", e, pool.Free())
	}
	samples, counters := session.CoreSnapshot(nil)
	if len(samples) != 1 || samples[0].PoolShort == 0 || counters.PoolShort != samples[0].PoolShort {
		t.Fatalf("pool short: samples %+v, session %d: want the stream's shortfalls counted", samples, counters.PoolShort)
	}
	// a second stream of the session finds the pool empty
	st2, sid2 := p.open(session)
	st2.SetCoreCeil(uint32(cfg.MaxStreamBuffer))
	for i := 0; i < 400; i++ {
		p.sendOn(sid2, 4096, 4096)
		waitUnread(t, st2, 4096)
		readN(t, st2, 4096, 4096)
	}
	if w := rcvWinOf(st2); w > uint32(cfg.NeedStreamBuffer) {
		t.Fatalf("second stream window %d past NeedStreamBuffer with the pool empty", w)
	}
}

// A ceiling that comes down below the window (the core drains the link)
// takes effect at the stream's next update, at once, not halving update by
// update; the claim it no longer needs goes back to the pool once its hold
// is over, and a ceiling of 0 brings the window back to NeedStreamBuffer.
func TestCoreCeilDrains(t *testing.T) {
	pool := NewWindowPool(4 << 20)
	session, stream, p, cfg := grown(t, pool)
	n0 := len(p.updates())
	ceil := uint32(cfg.NeedStreamBuffer) + 64<<10
	stream.SetCoreCeil(ceil)
	for i := 0; i < 1024 && len(p.updates()) == n0; i++ { // up to the next update
		keepUp(t, p, stream, 4096)
		p.sync(stream)
	}
	upds := p.updates()[n0:]
	if len(upds) == 0 || upds[0][1] != ceil {
		t.Fatalf("updates after the ceiling came down %v, want the first at the ceiling %d", upds, ceil)
	}
	if c := claimOf(stream); c < cfg.MaxStreamBuffer-cfg.NeedStreamBuffer {
		t.Fatalf("claim %d gone at once: what the larger window let in may still arrive", c)
	}
	stream.bufferLock.Lock()
	stream.claimDown = monoNow() - 1 // the hold is over
	stream.bufferLock.Unlock()
	keepUp(t, p, stream, 4096)
	session.returnSpare()
	if c, e := claimOf(stream), stream.sess.Extra(); c != int(ceil)-cfg.NeedStreamBuffer || e != c || pool.Free() != 4<<20-c {
		t.Fatalf("after the hold: claim %d extra %d pool free %d, want %d of the pool for the window past the need", c, e, pool.Free(), int(ceil)-cfg.NeedStreamBuffer)
	}
	stream.SetCoreCeil(0)
	keepUp(t, p, stream, 256<<10)
	p.sync(stream)
	if w := p.lastWindow(); w != uint32(cfg.NeedStreamBuffer) {
		t.Fatalf("window %d with the ceiling gone, want NeedStreamBuffer %d", w, cfg.NeedStreamBuffer)
	}
}

// Past NeedStreamBuffer a window grows only while the stream's latest
// turnaround is within the session's reference and brake margin, and the
// session lets it grow; up to NeedStreamBuffer it grows as it always did.
func TestCoreGrowthBrake(t *testing.T) {
	pool := NewWindowPool(4 << 20)
	cfg := poolConfig(pool)
	for _, tc := range []struct {
		name           string
		ref, brake     time.Duration
		growOK, beyond bool
	}{
		{"turnaround past the reference", time.Nanosecond, 0, true, false},
		{"session holds growth", 0, 0, false, false},
		{"turnaround within it", time.Hour, 0, true, true},
		{"within the brake margin", time.Nanosecond, time.Hour, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session, stream, p := newRawPeer(t, cfg)
			session.SetCoreRef(tc.ref, tc.brake, tc.growOK)
			keepUp(t, p, stream, 1<<20) // at the need: turnarounds come in
			if turnOf(stream) <= 0 {
				t.Fatal("no turnaround at the need")
			}
			stream.SetCoreCeil(uint32(cfg.MaxStreamBuffer))
			keepUp(t, p, stream, 4<<20)
			p.sync(stream)
			w := maxWindow(p.updates())
			if tc.beyond != (w > uint32(cfg.NeedStreamBuffer)) || w < uint32(cfg.NeedStreamBuffer) {
				t.Fatalf("max window %d (need %d), want past it: %v (updates %v)", w, cfg.NeedStreamBuffer, tc.beyond, p.updates())
			}
			if !tc.beyond && claimOf(stream) != 0 {
				t.Fatalf("held at the need, yet claims %d", claimOf(stream))
			}
		})
	}
}

// Past NeedStreamBuffer an update leaves each step -- NeedStreamBuffer/2,
// or 1/updShare of the window if more -- not each half window, and no more
// updates leave per byte read than without a ceiling.
func TestCoreNoExtraUpdates(t *testing.T) {
	pool := NewWindowPool(4 << 20)
	cfg := poolConfig(pool)
	cfg.MaxStreamBuffer = 2 << 20 // 8 times the need: steps of 1/updShare
	const n = 8 << 20
	count := func(ceil uint32) [][2]uint32 {
		_, stream, p := newRawPeer(t, cfg)
		stream.SetCoreCeil(ceil)
		keepUp(t, p, stream, n)
		p.sync(stream)
		return p.updates()
	}
	base, core := count(0), count(uint32(cfg.MaxStreamBuffer))
	if maxWindow(core) != uint32(cfg.MaxStreamBuffer) {
		t.Fatalf("the window did not grow (updates %v)", core)
	}
	if len(core) > len(base) {
		t.Fatalf("%d updates for %d bytes under a ceiling, %d without", len(core), n, len(base))
	}
	past := 0
	for i := 1; i < len(core); i++ {
		w, step := core[i-1][1], core[i][0]-core[i-1][0]
		if w <= uint32(cfg.NeedStreamBuffer) {
			continue
		}
		past++
		want := StepOf(uint32(cfg.NeedStreamBuffer), w)
		// keepUp reads 4 KiB at a time: a step ends at the read that reaches it
		if step < want || step >= want+4096 {
			t.Fatalf("update %d: %d read since the previous one under a window of %d, want %d (updates %v)", i, step, w, want, core)
		}
	}
	if past < 8 {
		t.Fatalf("only %d updates past NeedStreamBuffer: %v", past, core)
	}
}

// turnOf is the stream's latest turnaround, without resetting it.
func turnOf(st *Stream) time.Duration {
	st.bufferLock.Lock()
	defer st.bufferLock.Unlock()
	return time.Duration(st.core.turnSeen)
}

// The turnaround of an update is how long after it was queued the first
// byte past the limit the peer had before it arrives: a peer that holds that
// byte back a while shows it; data within the old limit does not.
func TestCoreTurnaround(t *testing.T) {
	cfg := adaptiveConfig()
	session, stream, p := newRawPeer(t, cfg)
	p.send(4096, 4096)
	p.settle(stream)
	readN(t, stream, 4096, 4096) // the first update: the limit moves past the 256 KiB the peer started with
	p.sync(stream)
	p.send(initialPeerWindow-4096, 4096) // up to the old limit: no turnaround yet
	p.settle(stream)
	if d := turnOf(stream); d != 0 {
		t.Fatalf("turnaround %v from data within the old limit", d)
	}
	const hold = 30 * time.Millisecond
	time.Sleep(hold)
	p.send(4096, 4096) // past it
	p.settle(stream)
	d := turnOf(stream)
	if d < hold || d > hold+2*time.Second {
		t.Fatalf("turnaround %v, want %v and a little", d, hold)
	}
	samples, _ := session.CoreSnapshot(nil)
	if len(samples) != 1 || samples[0].TurnLast != d || samples[0].TurnMin != d || samples[0].ID != stream.ID() {
		t.Fatalf("snapshot %+v, want the turnaround %v", samples, d)
	}
	samples, _ = session.CoreSnapshot(samples)
	if len(samples) != 1 || samples[0].TurnLast != 0 || samples[0].TurnMin != 0 {
		t.Fatalf("second snapshot %+v: the turnarounds did not restart", samples)
	}
	if turnOf(stream) != d {
		t.Fatal("a snapshot reset the turnaround growth is judged on")
	}
}

// behind is how much more the peer's second-latest update lets it send: a
// peer one update behind, as over a path whose round trip is longer than a
// step of reads.
func (p *rawPeer) behind() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	lim := uint32(initialPeerWindow)
	if n := len(p.upds); n >= 2 {
		lim = p.upds[n-2][0] + p.upds[n-2][1]
	}
	return int(int32(lim - p.sent))
}

// coreBare is a stream outside any session's goroutines, for the sampler.
func coreBare(frameSize int) *Stream {
	s := &Stream{frameSize: frameSize, sess: &Session{config: adaptiveConfig()}}
	s.core.limHigh = initialPeerWindow
	return s
}

// arrive is data the stream receives and does not read.
func (s *Stream) arrive(n int) {
	s.bufferLock.Lock()
	s.unread += n
	s.turned()
	s.bufferLock.Unlock()
}

// read takes n of what the stream holds and announces window.
func (s *Stream) readAndAnnounce(n int, window uint32) {
	s.bufferLock.Lock()
	s.unread -= n
	s.core.readTot.Add(uint64(n))
	s.announced(window)
	s.bufferLock.Unlock()
}

// A cycle is a hit when its data paused at the limit the peer had before
// the update that ends it (the peer used all of its window and waited for
// the update), not when the receiver's own backlog reached the latest
// limit: a stream whose data flows on past the limit is no hit.
func TestCoreHitIsAPauseAtTheLimit(t *testing.T) {
	const w = 1 << 20
	s := coreBare(4096)
	s.arrive(64 << 10)
	s.readAndAnnounce(64<<10, w) // the first mark, at the limit the peer started with
	recv := s.core.readTot.Load() + uint64(s.unread)
	s.arrive(int(s.core.ring[s.core.head].lim - recv)) // up to it ...
	time.Sleep(hitGap + time.Millisecond)              // ... a pause ...
	s.arrive(4096)                                     // ... past it
	s.readAndAnnounce(64<<10, w)
	if h, u := s.core.hitN.Load(), s.core.updN.Load(); h != 1 || u != 2 {
		t.Fatalf("a pause at the limit: %d hit(s) in %d update(s), want 1 in 2", h, u)
	}
	// on past the next mark's limit with no pause
	recv = s.core.readTot.Load() + uint64(s.unread)
	s.arrive(int(s.core.ring[s.core.head].lim-recv) + 4096)
	s.readAndAnnounce(64<<10, w)
	// a backlog up to the latest limit, with no pause at the earlier one
	recv = s.core.readTot.Load() + uint64(s.unread)
	s.arrive(int(s.core.limHigh - recv))
	s.readAndAnnounce(64<<10, w)
	if h, u := s.core.hitN.Load(), s.core.updN.Load(); h != 1 || u != 4 {
		t.Fatalf("no pause: %d hit(s) in %d update(s), want still 1 in 4", h, u)
	}
	if s.core.turnLast.Load() == 0 {
		t.Fatal("no turnaround taken")
	}
}

// The sampler keeps turnRing updates in flight: past that the newest go
// unsampled (dropping the oldest would drop each before its turnaround came
// in), and it takes them again once the ring has room; an update that does
// not raise the highest limit (a smaller window) starts no turnaround; and
// one sample per update, oldest first.
func TestCoreTurnRing(t *testing.T) {
	const w = 1 << 20
	s := coreBare(4096)
	s.arrive(6 * 64 << 10)
	for i := 0; i < 6; i++ { // six steps of 64 KiB, none sampled yet
		s.readAndAnnounce(64<<10, w)
	}
	if s.core.n != turnRing || s.core.limHigh != 6*64<<10+w {
		t.Fatalf("ring holds %d, highest limit %d", s.core.n, s.core.limHigh)
	}
	// the ring's marks, 40, 30, 20 and 10 ms old
	now := monoNow()
	for i := 0; i < turnRing; i++ {
		s.core.ring[(s.core.head+i)%turnRing].at = now - int64(40-10*i)*int64(time.Millisecond)
	}
	lims := []uint64{}
	for i := 0; i < turnRing; i++ {
		lims = append(lims, s.core.ring[(s.core.head+i)%turnRing].lim)
	}
	if lims[0] != initialPeerWindow {
		t.Fatalf("first mark at limit %d, want the %d the peer started with", lims[0], initialPeerWindow)
	}
	recv := s.core.readTot.Load() + uint64(s.unread)
	s.arrive(int(lims[1] - recv + 1)) // past the first two marks' limits
	if d := time.Duration(s.core.turnLast.Load()); s.core.n != turnRing-2 || d < 30*time.Millisecond || d > time.Second {
		t.Fatalf("after two marks: ring %d, last %v, want 2 and ~30ms", s.core.n, d)
	}
	if m := time.Duration(s.core.turnMin.Load()); m < 30*time.Millisecond || m > time.Duration(s.core.turnLast.Load()) {
		t.Fatalf("least %v, last %v", m, time.Duration(s.core.turnLast.Load()))
	}
	recv = s.core.readTot.Load() + uint64(s.unread)
	s.arrive(int(s.core.limHigh - recv + 1)) // past every limit: the rest, and nothing for the two dropped
	if s.core.n != 0 {
		t.Fatalf("ring %d after everything arrived", s.core.n)
	}
	if d := time.Duration(s.core.turnSeen); d < 10*time.Millisecond || d > time.Second {
		t.Fatalf("latest %v, want the youngest mark's ~10ms", d)
	}
	// a smaller window raises no limit: no mark
	s.readAndAnnounce(64<<10, 64<<10)
	if s.core.n != 0 {
		t.Fatal("an update that raised no limit started a turnaround")
	}
	// room again: marks again
	s.readAndAnnounce(64<<10, 2*w)
	if s.core.n != 1 {
		t.Fatalf("ring %d after a raising update with room, want 1", s.core.n)
	}
}

// The turnaround counts bytes in 64 bits: it does not mind numRead crossing
// 2^32.
func TestCoreTurnaroundWrap(t *testing.T) {
	cfg := adaptiveConfig()
	session, stream, p := newRawPeer(t, cfg)
	min := uint32(cfg.MinStreamBuffer)
	base := uint32(0) - min/2
	stream.bufferLock.Lock()
	stream.numRead = base
	stream.rcvWin = min
	stream.adapted = true
	stream.bufferLock.Unlock()
	p.mu.Lock()
	p.sent, p.consumed, p.window = base, base, min
	p.mu.Unlock()
	for total := 0; total < 2<<20; total += 4096 {
		p.sendInWindow(4096, cfg.MaxFrameSize)
		readN(t, stream, 4096, 4096)
	}
	samples, _ := session.CoreSnapshot(nil)
	if len(samples) != 1 || samples[0].TurnLast <= 0 || samples[0].Upds < 4 || samples[0].Read != 2<<20 {
		t.Fatalf("across the wrap: %+v, want turnarounds, updates and 2 MiB read", samples)
	}
}

// A stream that closes with updates in flight leaves the snapshots; the
// session's own counts go on.
func TestCoreClosedStreamLeavesSnapshot(t *testing.T) {
	cfg := adaptiveConfig()
	session, stream, p := newRawPeer(t, cfg)
	keepUp(t, p, stream, 1<<20)
	p.sync(stream)
	if samples, c := session.CoreSnapshot(nil); len(samples) != 1 || c.Rcvd != 1<<20 {
		t.Fatalf("open: %+v, received %d", samples, c.Rcvd)
	}
	stream.Close()
	if samples, c := session.CoreSnapshot(nil); len(samples) != 0 || c.Rcvd != 1<<20 {
		t.Fatalf("closed: %+v, received %d", samples, c.Rcvd)
	}
}

// hitN counts the cycles in which the peer sent up to the window it was
// told of and waited there (held by the window: its data paused at the
// limit for hitGap or more); lagN those in which the reader left more than
// StreamLagTarget/4 unread throughout.
func TestCoreHitsAndLags(t *testing.T) {
	cfg := adaptiveConfig()
	cfg.MaxStreamBuffer = 256 << 10 // stays at the start window
	session, stream, p := newRawPeer(t, cfg)
	// a reader that keeps up, a peer an update behind (the round trip of a
	// long path) that sends all its window allows and waits there
	for i := 0; i < 20; i++ {
		p.sync(stream)
		for n := p.behind(); n > 0; n -= 4096 {
			p.send(4096, 4096)
			p.settle(stream)
			readN(t, stream, 4096, 4096)
		}
		time.Sleep(hitGap + time.Millisecond) // waiting for the update in flight
	}
	p.sync(stream)
	s, _ := session.CoreSnapshot(nil)
	if len(s) != 1 || s[0].Upds < 16 || s[0].Hits < s[0].Upds-3 || s[0].Lags != 0 {
		t.Fatalf("window-held peer: %+v, want nearly every update a hit, and no lag", s)
	}
	// a reader that keeps a backlog over StreamLagTarget/4 throughout (its
	// window shrinks under need/4 and no hit brings it into a snapshot: the
	// counts are read off the stream)
	_, stream3, p3 := newRawPeer(t, cfg)
	p3.send(128<<10, 4096)
	for i := 0; i < 64; i++ {
		p3.send(16<<10, 4096)
		p3.settle(stream3)
		readN(t, stream3, 16<<10, 4096)
	}
	if u, l := stream3.core.updN.Load(), stream3.core.lagN.Load(); u < 8 || l < u-2 {
		t.Fatalf("reader holding a backlog: %d lagging of %d updates, want nearly every one", l, u)
	}
	// a peer that never reaches the window, a reader that keeps up
	session2, stream2, p2 := newRawPeer(t, cfg)
	keepUp(t, p2, stream2, 2<<20)
	p2.sync(stream2)
	s2, _ := session2.CoreSnapshot(nil)
	if len(s2) != 1 || s2[0].Upds < 8 || s2[0].Hits > 1 || s2[0].Lags != 0 {
		t.Fatalf("free-running peer, reader keeping up: %+v, want no hits and no lags", s2)
	}
}

// CoreSnapshot samples only streams with a window of a quarter of the need
// or more, or that ran up to their window since the previous one.
func TestCoreSnapshotPicksLargeStreams(t *testing.T) {
	cfg := poolConfig(NewWindowPool(1 << 20))
	session, big, p := newRawPeer(t, cfg)
	keepUp(t, p, big, 1<<20)
	small, sid := p.open(session)
	p.sendOn(sid, 4096, 4096)
	waitUnread(t, small, 4096)
	readN(t, small, 4096, 4096) // one update of the start window, then held small
	small.bufferLock.Lock()
	small.rcvWin = uint32(cfg.MinStreamBuffer)
	small.bufferLock.Unlock()
	small.sendWindowUpdate()
	samples, c := session.CoreSnapshot(make([]CoreSample, 8))
	if len(samples) != 1 || samples[0].ID != big.ID() || samples[0].Win != uint32(cfg.NeedStreamBuffer) {
		t.Fatalf("snapshot %+v, want the large stream alone", samples)
	}
	if c.Rcvd != 1<<20+4096 || c.Parks != session.Parks() || c.Borrows != 0 || c.Extra != 0 {
		t.Fatalf("counters %+v", c)
	}
}

// A braked reader that only paused gets its window back at once (restore),
// but no more than its ceiling now allows: NeedStreamBuffer without one.
func TestCoreRestoreCapped(t *testing.T) {
	for _, ceil := range []uint32{256<<10 + 64<<10, 0} {
		pool := NewWindowPool(4 << 20)
		_, stream, p, cfg := grown(t, pool)
		stream.bufferLock.Lock()
		stream.preBrake, stream.brakeAt = stream.rcvWin, monoNow() // a brake run began ...
		stream.setWindow(cfg, uint32(cfg.MinStreamBuffer))         // ... and took the window down
		stream.bufferLock.Unlock()
		stream.SetCoreCeil(ceil)
		p.send(4096, 4096)
		p.settle(stream)
		readN(t, stream, unreadOf(stream), 4096) // all it holds: it only paused
		p.sync(stream)
		want := ceil
		if want == 0 {
			want = uint32(cfg.NeedStreamBuffer)
		}
		if w := p.lastWindow(); w != want {
			t.Fatalf("ceiling %d: window %d restored, want %d (updates %v)", ceil, w, want, p.updates())
		}
	}
}

// A stream's own hit gap (SetCoreHitGap) replaces hitGap when longer: a pause
// at the limit shorter than it is no hit, one past it is; a gap under hitGap
// leaves hitGap in force.
func TestCoreHitGapPerStream(t *testing.T) {
	const w = 1 << 20
	cycle := func(s *Stream, pause time.Duration) {
		recv := s.core.readTot.Load() + uint64(s.unread)
		s.arrive(int(s.core.ring[s.core.head].lim - recv)) // up to the limit ...
		time.Sleep(pause)                                  // ... a pause ...
		s.arrive(4096)                                     // ... past it
		s.readAndAnnounce(64<<10, w)
	}
	s := coreBare(4096)
	s.arrive(64 << 10)
	s.readAndAnnounce(64<<10, w)
	s.SetCoreHitGap(30 * time.Millisecond)
	cycle(s, 5*time.Millisecond) // past hitGap, under the stream's own gap
	if h := s.core.hitN.Load(); h != 0 {
		t.Fatalf("a 5 ms pause under a 30 ms gap: %d hit(s), want 0", h)
	}
	cycle(s, 35*time.Millisecond)
	if h := s.core.hitN.Load(); h != 1 {
		t.Fatalf("a 35 ms pause under a 30 ms gap: %d hit(s), want 1", h)
	}
	s.SetCoreHitGap(time.Millisecond) // under hitGap: hitGap holds
	cycle(s, hitGap+time.Millisecond)
	if h := s.core.hitN.Load(); h != 2 {
		t.Fatalf("a pause past hitGap with a 1 ms gap: %d hit(s), want 2", h)
	}
}
