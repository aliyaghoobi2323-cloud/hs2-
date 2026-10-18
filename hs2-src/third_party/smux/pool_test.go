package smux

import (
	"encoding/binary"
	"sync/atomic"
	"testing"
	"time"
)

// poolConfig: the adaptive test config with windows up to 256 KiB on a
// reader keeping up alone and up to 1 MiB under a ceiling (core.go), in a
// 512 KiB session buffer that grows from pool (nil: no pool).
func poolConfig(pool *WindowPool) *Config {
	config := adaptiveConfig()
	config.MaxReceiveBuffer = 512 << 10
	config.MaxStreamBuffer = 1 << 20
	config.NeedStreamBuffer = 256 << 10
	config.WindowPool = pool
	if pool == nil {
		config.MaxStreamBuffer = 512 << 10
	}
	return config
}

func extraOf(s *Session) (extra, spare int32) {
	return atomic.LoadInt32(&s.extra), atomic.LoadInt32(&s.spare)
}

func claimOf(st *Stream) int {
	st.bufferLock.Lock()
	defer st.bufferLock.Unlock()
	return st.claim
}

func maxWindow(upds [][2]uint32) uint32 {
	var w uint32
	for _, u := range upds {
		if u[1] > w {
			w = u[1]
		}
	}
	return w
}

// trySendOn is rawPeer.sendOn returning its error rather than failing the
// test: it may run on after the test gave up on it (sendWithin).
func (p *rawPeer) trySendOn(sid uint32, n, frameSize int) error {
	for n > 0 {
		sz := n
		if sz > frameSize {
			sz = frameSize
		}
		frame := make([]byte, headerSize+sz)
		frame[0] = p.ver
		frame[1] = cmdPSH
		binary.LittleEndian.PutUint16(frame[2:], uint16(sz))
		binary.LittleEndian.PutUint32(frame[4:], sid)
		if _, err := p.conn.Write(frame); err != nil {
			return err
		}
		if sid == p.sid {
			p.mu.Lock()
			p.sent += uint32(sz)
			p.mu.Unlock()
		}
		n -= sz
	}
	return nil
}

// sendWithin sends n bytes on stream sid and fails the test unless the
// session takes them all in within 5 s: one that waits for room instead of
// borrowing it never would.
func sendWithin(t *testing.T, p *rawPeer, sid uint32, n, frameSize int, what string) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- p.trySendOn(sid, n, frameSize) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: %d bytes not taken in within 5 s", what, n)
	}
}

func TestPoolVerifyConfig(t *testing.T) {
	pool := NewWindowPool(1 << 20)
	cases := []struct {
		name string
		edit func(c *Config)
		ok   bool
	}{
		{"on", func(c *Config) {}, true},
		{"no pool", func(c *Config) { c.WindowPool = nil; c.MaxStreamBuffer = 512 << 10 }, true},
		{"no pool, stream over session", func(c *Config) { c.WindowPool = nil }, false},
		{"need over session", func(c *Config) { c.NeedStreamBuffer = 768 << 10 }, false},
		{"need over max", func(c *Config) { c.NeedStreamBuffer = 2 << 20 }, false},
		{"need under min", func(c *Config) { c.NeedStreamBuffer = 8 << 10 }, false},
		{"need negative", func(c *Config) { c.NeedStreamBuffer = -1 }, false},
		{"need without adaptive", func(c *Config) { c.MinStreamBuffer, c.StreamLagTarget = 0, 0 }, false},
		{"pool without need", func(c *Config) { c.NeedStreamBuffer = 0; c.MaxStreamBuffer = 512 << 10 }, false},
		{"need equals max", func(c *Config) { c.WindowPool, c.MaxStreamBuffer, c.NeedStreamBuffer = nil, 512<<10, 512<<10 }, true},
	}
	for _, tc := range cases {
		c := poolConfig(pool)
		tc.edit(c)
		if err := VerifyConfig(c); (err == nil) != tc.ok {
			t.Errorf("%s: VerifyConfig = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

// Without a ceiling a reader that keeps up grows its window to
// NeedStreamBuffer and no further, exactly as a window whose
// MaxStreamBuffer is NeedStreamBuffer does, update for update, and takes
// nothing from the pool.
func TestPoolNoCeilingStaysAtNeed(t *testing.T) {
	pool := NewWindowPool(4 << 20)
	cfg := poolConfig(pool)
	session, stream, p := newRawPeer(t, cfg)
	keepUp(t, p, stream, 6<<20)
	p.sync(stream)
	if w := maxWindow(p.updates()); w != uint32(cfg.NeedStreamBuffer) {
		t.Fatalf("max window %d, want NeedStreamBuffer %d (updates %v)", w, cfg.NeedStreamBuffer, p.updates())
	}
	if e, _ := extraOf(session); e != 0 || pool.Free() != 4<<20 {
		t.Fatalf("extra %d, pool free %d: a window within NeedStreamBuffer took from the pool", e, pool.Free())
	}
	base := adaptiveConfig()
	base.MaxStreamBuffer = cfg.NeedStreamBuffer
	_, bs, bp := newRawPeer(t, base)
	keepUp(t, bp, bs, 6<<20)
	bp.sync(bs)
	got, want := p.updates(), bp.updates()
	if len(got) != len(want) {
		t.Fatalf("%d updates, %d with MaxStreamBuffer at the need:\n%v\n%v", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("update %d: %v, %v with MaxStreamBuffer at the need:\n%v\n%v", i, got[i], want[i], got, want)
		}
	}
}

// A session whose buffer runs full (what was in flight when its readers
// slowed down) borrows from the pool, whatever its windows (here every one
// within NeedStreamBuffer), up to borrowMax times MaxReceiveBuffer more, so
// its other streams keep moving; past that it waits as before; what it
// borrowed goes back once it is free again.
func TestPoolBorrowWhenFull(t *testing.T) {
	pool := NewWindowPool(4 << 20)
	cfg := poolConfig(pool)
	session, stream, p := newRawPeer(t, cfg)
	sendWithin(t, p, p.sid, cfg.MaxReceiveBuffer+200<<10, cfg.MaxFrameSize, "more than the buffer, not read")
	p.settle(stream)
	e, sp := extraOf(session)
	if e < 200<<10 || e > int32(borrowMax*cfg.MaxReceiveBuffer) || sp != e {
		t.Fatalf("buffer past MaxReceiveBuffer %d (spare %d), want what it took in past it, as spare", e, sp)
	}
	if b, n := session.Borrows(), session.Parks(); b == 0 || b > n {
		t.Fatalf("borrows %d of %d parks, want some, at most all", b, n)
	}
	other, sid := p.open(session)
	p.sendOn(sid, 64<<10, cfg.MaxFrameSize)
	waitUnread(t, other, 64<<10)
	readN(t, other, 64<<10, 16<<10)

	// past borrowMax+1 times MaxReceiveBuffer it waits: nothing more is taken in
	sent := make(chan struct{})
	go func() { p.send(borrowMax*cfg.MaxReceiveBuffer, cfg.MaxFrameSize); close(sent) }()
	time.Sleep(50 * time.Millisecond)
	if e, _ := extraOf(session); e > int32(borrowMax*cfg.MaxReceiveBuffer) {
		t.Fatalf("borrowed %d, more than %d times MaxReceiveBuffer %d", e, borrowMax, cfg.MaxReceiveBuffer)
	}
	if u := unreadOf(stream); u > (borrowMax+1)*cfg.MaxReceiveBuffer+cfg.MaxFrameSize {
		t.Fatalf("took in %d, past %d times MaxReceiveBuffer and a frame", u, borrowMax+1)
	}
	go func() { // read it all: the rest comes in, and the buffer goes back
		buf := make([]byte, 64<<10)
		for {
			if _, err := stream.Read(buf); err != nil {
				return
			}
		}
	}()
	select {
	case <-sent:
	case <-time.After(5 * time.Second):
		t.Fatal("the rest never came in once read")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		e, sp := extraOf(session)
		if e == 0 && sp == 0 && pool.Free() == 4<<20 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the reads: extra %d spare %d pool free %d", e, sp, pool.Free())
		}
		time.Sleep(time.Millisecond)
	}
}

// Without a pool a session that runs full waits, as it always did.
func TestPoolNoBorrowWithoutPool(t *testing.T) {
	cfg := poolConfig(nil)
	cfg.NeedStreamBuffer = 0
	session, stream, p := newRawPeer(t, cfg)
	sent := make(chan struct{})
	go func() { p.send(cfg.MaxReceiveBuffer+64<<10, cfg.MaxFrameSize); close(sent) }()
	waitUnread(t, stream, cfg.MaxReceiveBuffer)
	time.Sleep(20 * time.Millisecond)
	if u := unreadOf(stream); u > cfg.MaxReceiveBuffer+cfg.MaxFrameSize {
		t.Fatalf("took in %d with a buffer of %d and no pool", u, cfg.MaxReceiveBuffer)
	}
	if e, _ := extraOf(session); e != 0 || session.Borrows() != 0 || session.Parks() == 0 {
		t.Fatalf("extra %d borrows %d parks %d, want 0, 0 and some", e, session.Borrows(), session.Parks())
	}
	readN(t, stream, cfg.MaxReceiveBuffer+64<<10, 64<<10)
	<-sent
}

// A session that closes while its reader is out of buffer borrows nothing
// after it has given the pool back: what it took then would never return.
func TestPoolNoBorrowOnceClosed(t *testing.T) {
	pool := NewWindowPool(4 << 20)
	cfg := poolConfig(pool)
	session, _, _ := newRawPeer(t, cfg)
	session.Close()
	if session.borrow() {
		t.Fatal("a closed session borrowed")
	}
	if e, sp := extraOf(session); e != 0 || sp != 0 || pool.Free() != 4<<20 {
		t.Fatalf("after close: extra %d spare %d pool free %d", e, sp, pool.Free())
	}
}

// What a session borrowed goes back to the pool when it closes as far as
// its buffer is free, and the rest as its streams let their data go (here:
// closed; read, or closeGrace later: TestRed2BorrowedDataKeepsItsPoolAfterClose).
func TestPoolBorrowBackOnClose(t *testing.T) {
	pool := NewWindowPool(4 << 20)
	cfg := poolConfig(pool)
	session, stream, p := newRawPeer(t, cfg)
	sendWithin(t, p, p.sid, cfg.MaxReceiveBuffer+200<<10, cfg.MaxFrameSize, "more than the buffer, not read")
	p.settle(stream)
	if pool.Free() == 4<<20 {
		t.Fatal("nothing borrowed")
	}
	session.Close()
	if held, past := 4<<20-pool.Free(), unreadOf(stream)-cfg.MaxReceiveBuffer; held != past {
		t.Fatalf("session closed with %d unread past its buffer: the pool holds %d for it", past, held)
	}
	stream.Close()
	if e, sp := extraOf(session); pool.Free() != 4<<20 || e != 0 || sp != 0 {
		t.Fatalf("stream let go: pool free %d extra %d spare %d", pool.Free(), e, sp)
	}
}

// What came in under a larger window stays covered until it is read: when a
// brake brings the window down while that data is unread, the session keeps
// the buffer it grew by (its spare) as long as giving it back would leave
// less than a quarter of MaxReceiveBuffer free, and gives it back as it is
// read.
func TestPoolSpareHeldWhileBuffered(t *testing.T) {
	pool := NewWindowPool(4 << 20)
	session, stream, p, cfg := grown(t, pool)
	if claimOf(stream) == 0 {
		t.Fatal("no claim to begin with")
	}
	other, sid := p.open(session)
	p.sendOn(sid, 200<<10, 4096) // the other stream holds over a third of MaxReceiveBuffer
	waitUnread(t, other, 200<<10)
	p.send(100<<10, 4096) // the reader falls behind ...
	time.Sleep(5 * time.Millisecond)
	for i := 0; i < 150; i++ { // ... and stays behind: braked
		p.send(4096, 4096)
		time.Sleep(100 * time.Microsecond)
	}
	p.settle(stream)
	if session.Brakes() == 0 {
		t.Fatal("no brake: the test did not test")
	}
	stream.bufferLock.Lock()
	stream.claimDown = monoNow() - 1 // the claim's hold is over
	stream.bufferLock.Unlock()
	room := int32(cfg.MaxReceiveBuffer / spareRoom)
	var maxSpare int32
	for u := unreadOf(stream); u > 0; u = unreadOf(stream) {
		if u > 16<<10 {
			u = 16 << 10
		}
		readN(t, stream, u, u)
		b := atomic.LoadInt32(&session.bucket)
		e, sp := extraOf(session)
		if sp > maxSpare {
			maxSpare = sp
		}
		if b <= 0 {
			t.Fatalf("bucket %d with %d unread: the buffer shrank under data it took in (extra %d spare %d)", b, unreadOf(stream), e, sp)
		}
		if sp > 0 && b > room+16<<10 {
			t.Fatalf("spare %d kept with %d free (room %d)", sp, b, room)
		}
	}
	if maxSpare == 0 {
		t.Fatal("no spare kept while the data that came in under the larger window was unread")
	}
	readN(t, other, 200<<10, 16<<10)
	if e, sp := extraOf(session); e != int32(claimOf(stream)) || sp != 0 || pool.Free() != 4<<20-claimOf(stream) {
		t.Fatalf("after the backlog: extra %d spare %d claim %d pool free %d", e, sp, claimOf(stream), pool.Free())
	}
}

// A reader that stops after a fast transfer holds what its larger window let
// in. That stays its own claim past NeedStreamBuffer, however its window
// comes down (here: idle, swept), so the other streams of the session keep
// the room they would have with every window within NeedStreamBuffer; it
// goes back to the pool as the reader takes it.
func TestPoolClaimCoversWhatAStoppedReaderHolds(t *testing.T) {
	pool := NewWindowPool(4 << 20)
	session, stream, p, cfg := grown(t, pool)
	p.fill(cfg.MaxFrameSize) // all its window, then the reader stops
	p.settle(stream)
	held := unreadOf(stream)
	if held <= cfg.NeedStreamBuffer {
		t.Fatalf("holds %d: no larger window to begin with", held)
	}
	stream.bufferLock.Lock()
	stream.readAt = monoNow() - int64(claimIdle)
	stream.bufferLock.Unlock()
	session.sweepClaims() // the window comes down ...
	stream.bufferLock.Lock()
	stream.claimDown = monoNow() - 1 // ... and its hold is over
	stream.bufferLock.Unlock()
	session.sweepClaims()
	if c := claimOf(stream); c < held-cfg.NeedStreamBuffer {
		t.Fatalf("claim %d under the %d it holds past NeedStreamBuffer", c, held-cfg.NeedStreamBuffer)
	}
	// the other streams have all of MaxReceiveBuffer but NeedStreamBuffer
	other, sid := p.open(session)
	p.sendOn(sid, cfg.MaxReceiveBuffer-cfg.NeedStreamBuffer-cfg.MaxFrameSize, 4096)
	waitUnread(t, other, cfg.MaxReceiveBuffer-cfg.NeedStreamBuffer-cfg.MaxFrameSize)
	if b := atomic.LoadInt32(&session.bucket); b <= 0 {
		t.Fatalf("bucket %d: the stopped reader took the others' room", b)
	}
	readN(t, other, cfg.MaxReceiveBuffer-cfg.NeedStreamBuffer-cfg.MaxFrameSize, 16<<10)
	readN(t, stream, held, 16<<10) // it reads again: the claim keeps only what its window needs
	stream.bufferLock.Lock()
	stream.claimDown = monoNow() - 1
	stream.reclaim(cfg)
	win := int(stream.rcvWin)
	stream.bufferLock.Unlock()
	session.returnSpare()
	want := win - cfg.NeedStreamBuffer
	if want < 0 {
		want = 0
	}
	if e, sp := extraOf(session); claimOf(stream) != want || e != int32(want) || sp != 0 || pool.Free() != 4<<20-want {
		t.Fatalf("after the reads: claim %d (window %d) extra %d spare %d pool free %d", claimOf(stream), win, e, sp, pool.Free())
	}
}

// Closing a stream gives everything back to the pool; closing its session
// gives back what its streams do not hold, and the rest as they let it go
// (or closeGrace later: TestFixPoolBackWhenBuffersFreed).
func TestPoolBackOnClose(t *testing.T) {
	pool := NewWindowPool(4 << 20)
	session, stream, p, cfg := grown(t, pool)
	p.fill(cfg.MaxFrameSize)
	p.settle(stream)
	if pool.Free() == 4<<20 {
		t.Fatal("nothing taken from the pool")
	}
	stream.Close()
	if e, sp := extraOf(session); pool.Free() != 4<<20 || e != 0 || sp != 0 {
		t.Fatalf("stream closed: pool free %d extra %d spare %d", pool.Free(), e, sp)
	}

	session2, stream2, p2, _ := grown(t, pool)
	p2.fill(cfg.MaxFrameSize)
	p2.settle(stream2)
	if pool.Free() == 4<<20 {
		t.Fatal("nothing taken from the pool")
	}
	session2.Close()
	if held := unreadOf(stream2) - cfg.NeedStreamBuffer; pool.Free() != 4<<20-held || claimOf(stream2) != held {
		t.Fatalf("session closed: pool free %d (claim %d), want all of it back but what the stream holds past NeedStreamBuffer, %d",
			pool.Free(), claimOf(stream2), held)
	}
	free := pool.Free()
	if n, _ := stream2.Read(make([]byte, 4096)); n == 0 { // what it holds is still read after the close
		t.Fatal("nothing read after the close")
	}
	if pool.Free() < free {
		t.Fatalf("a read after the close took %d from the pool", free-pool.Free())
	}
	stream2.Close()
	if e, sp := extraOf(session2); pool.Free() != 4<<20 || e != 0 || sp != 0 {
		t.Fatalf("its stream closed after the session: pool free %d extra %d spare %d", pool.Free(), e, sp)
	}
}

// A stream that has read nothing for claimIdle (a connection kept open after
// a fast transfer) gives its window past NeedStreamBuffer back at the
// keepalive tick, with an update: the peer may then send it no more than
// NeedStreamBuffer, whatever its reader does when the next transfer comes.
// Its claim covers what was on its way for claimHold turnarounds, then goes
// back to the pool, and the stream takes from it again what its ceiling
// lets it grow to once it reads again.
func TestPoolIdleClaimSwept(t *testing.T) {
	pool := NewWindowPool(4 << 20)
	session, stream, p, cfg := grown(t, pool)
	claim := claimOf(stream)
	if claim == 0 || p.room() <= cfg.NeedStreamBuffer {
		t.Fatalf("claim %d, room %d: no grown window to begin with", claim, p.room())
	}
	session.sweepClaims()
	if claimOf(stream) != claim {
		t.Fatalf("a stream that just read lost its claim (%d of %d)", claimOf(stream), claim)
	}
	stream.bufferLock.Lock()
	stream.readAt = monoNow() - int64(claimIdle)
	stream.bufferLock.Unlock()
	session.sweepClaims()
	deadline := time.Now().Add(5 * time.Second)
	for p.lastWindow() != uint32(cfg.NeedStreamBuffer) {
		if time.Now().After(deadline) {
			t.Fatalf("idle: window %d at the peer, want NeedStreamBuffer %d (updates %v)", p.lastWindow(), cfg.NeedStreamBuffer, p.updates())
		}
		time.Sleep(time.Millisecond)
	}
	if r := p.room(); r > cfg.NeedStreamBuffer {
		t.Fatalf("idle: the peer may still send %d, more than NeedStreamBuffer %d", r, cfg.NeedStreamBuffer)
	}
	if claimOf(stream) != claim {
		t.Fatalf("idle: claim %d of %d gone before its hold: what is on its way would land uncovered", claimOf(stream), claim)
	}
	stream.bufferLock.Lock()
	stream.claimDown = monoNow() + int64(time.Hour) // within the hold: nothing
	stream.bufferLock.Unlock()
	session.sweepClaims()
	if claimOf(stream) != claim {
		t.Fatalf("claim %d of %d gone within its hold", claimOf(stream), claim)
	}
	stream.bufferLock.Lock()
	stream.claimDown = monoNow() - 1 // the hold is over
	stream.bufferLock.Unlock()
	session.sweepClaims()
	if e, sp := extraOf(session); claimOf(stream) != 0 || e != 0 || sp != 0 || pool.Free() != 4<<20 {
		t.Fatalf("idle: claim %d extra %d spare %d pool free %d, want all back in the pool", claimOf(stream), e, sp, pool.Free())
	}
	keepUp(t, p, stream, 4<<20) // reads again, under the same ceiling
	p.sync(stream)
	if claimOf(stream) == 0 || p.lastWindow() <= uint32(cfg.NeedStreamBuffer) {
		t.Fatalf("reading again: claim %d window %d, want the larger window back", claimOf(stream), p.lastWindow())
	}
}

// A stream holding a grown window does not keep its session from borrowing:
// its claim covers what that window lets in, not what was in flight for the
// other streams when their readers slowed down. (Counted against the limit,
// a fast user's claim on a real link -- 1 Gbit/s, 100 ms -- left its
// session one borrow in 247 times out of buffer, next to a dozen readers
// that slowed down at once, and every stream stalled 10 s.)
func TestPoolBorrowNextToAGrownWindow(t *testing.T) {
	pool := NewWindowPool(4 << 20)
	session, stream, p, cfg := grown(t, pool)
	claim := claimOf(stream)
	if claim < cfg.MaxReceiveBuffer {
		t.Fatalf("claim %d: the grown window holds less than MaxReceiveBuffer from the pool", claim)
	}
	// another stream whose reader stopped gets the session's whole buffer,
	// the room the claim adds to it, and more
	other, sid := p.open(session)
	n := cfg.MaxReceiveBuffer + claim + 200<<10
	sendWithin(t, p, sid, n, cfg.MaxFrameSize, "next to a grown window")
	waitUnread(t, other, n)
	if session.Borrows() == 0 {
		t.Fatal("no borrow")
	}
	if c := claimOf(stream); c != claim {
		t.Fatalf("claim %d, was %d", c, claim)
	}
	if _, sp := extraOf(session); sp > int32(borrowMax*cfg.MaxReceiveBuffer) {
		t.Fatalf("spare %d past %d times MaxReceiveBuffer %d", sp, borrowMax, cfg.MaxReceiveBuffer)
	}
}
