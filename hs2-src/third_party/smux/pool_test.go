package smux

import (
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
	p.send(cfg.MaxReceiveBuffer+200<<10, cfg.MaxFrameSize) // more than the buffer, not read
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

// What a session borrowed goes back to the pool when it closes, read or not.
func TestPoolBorrowBackOnClose(t *testing.T) {
	pool := NewWindowPool(4 << 20)
	cfg := poolConfig(pool)
	session, stream, p := newRawPeer(t, cfg)
	p.send(cfg.MaxReceiveBuffer+200<<10, cfg.MaxFrameSize)
	p.settle(stream)
	if pool.Free() == 4<<20 {
		t.Fatal("nothing borrowed")
	}
	session.Close()
	if e, sp := extraOf(session); pool.Free() != 4<<20 || e != 0 || sp != 0 {
		t.Fatalf("session closed: pool free %d extra %d spare %d", pool.Free(), e, sp)
	}
}
