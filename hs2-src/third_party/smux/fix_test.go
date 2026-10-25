package smux

import (
	"testing"
	"time"
)

// Fixes of red team round 1's open findings (Phase Z5).

// Item 7: a reader that stops after a fast transfer keeps what its larger
// window let in, and with it its claim on the pool, for as long as its
// connection stays open (no relay is closed for being paused). Two of them
// on two links took the whole pool, and every other link of the process
// could then neither grow a window past NeedStreamBuffer nor borrow. A
// session takes from the pool only as far as it holds no more than the pool
// has left for the others (its claims and spare together: half the pool for
// a session alone): two frozen fast apps leave room for a third link to
// grow its window and to borrow. (A fixed cap of half the pool for each
// session does not: two sessions at the cap leave nothing.)
func TestFixPoolShareLeavesRoom(t *testing.T) {
	probe := poolConfig(NewWindowPool(1))
	excess := probe.MaxStreamBuffer - probe.NeedStreamBuffer // what a grown window holds from the pool
	size := 2 * excess
	pool := NewWindowPool(size)
	frozen := func() *Session {
		cfg := poolConfig(pool)
		s, st, p := newRawPeer(t, cfg)
		st.SetCoreCeil(uint32(cfg.MaxStreamBuffer))
		keepUp(t, p, st, 4<<20)
		p.sync(st)
		p.fill(cfg.MaxFrameSize) // all its window, and its reader stops for good
		p.settle(st)
		return s
	}
	a, b := frozen(), frozen()
	for i, s := range []*Session{a, b} {
		if e, _ := extraOf(s); int(e) > size/2 {
			t.Errorf("frozen fast app %d's link holds %d of the pool, past half of its %d", i+1, e, size)
		}
	}
	ea, _ := extraOf(a)
	eb, _ := extraOf(b)
	if pool.Free() <= 0 {
		t.Fatalf("two frozen fast apps hold %d and %d: the pool has %d left for every other link", ea, eb, pool.Free())
	}

	// a third link: a reader that keeps up under a ceiling grows its window
	// past NeedStreamBuffer ...
	cfg := poolConfig(pool)
	c, sc, pc := newRawPeer(t, cfg)
	sc.SetCoreCeil(uint32(cfg.MaxStreamBuffer))
	keepUp(t, pc, sc, 4<<20)
	pc.sync(sc)
	if w := maxWindow(pc.updates()); w <= uint32(cfg.NeedStreamBuffer) {
		t.Fatalf("next to two frozen fast apps (holding %d and %d of %d), a fast reader's window stayed at %d", ea, eb, size, w)
	}
	// ... and the session borrows as a stream whose reader stopped fills it
	other, sid := pc.open(c)
	n := cfg.MaxReceiveBuffer + claimOf(sc) + 100<<10
	sendWithin(t, pc, sid, n, cfg.MaxFrameSize, "the third link, next to two frozen fast apps")
	waitUnread(t, other, n)
	if c.Borrows() == 0 {
		t.Fatal("the third link never borrowed")
	}
	t.Logf("pool %d: frozen links hold %d and %d; the third link's window %d, claim %d, borrowed %d time(s); %d left",
		size, ea, eb, maxWindow(pc.updates()), claimOf(sc), c.Borrows(), pool.Free())
}

// Item 8: a session that closes keeps the pool its streams' claims cover for
// as long as their buffers hold the data those claims let in, and gives each
// back as its stream lets the data go (Close, recycleTokens): the process's
// memory stays within what the pool accounts for while a dead link's relays
// drain (up to the engine's relayDieGrace). A stream its app never closes
// gives its claim back closeGrace after the session died, so the pool comes
// back whole whatever the apps do.
func TestFixPoolBackWhenBuffersFreed(t *testing.T) {
	defer func(g time.Duration) { closeGrace = g }(closeGrace)
	closeGrace = 300 * time.Millisecond
	const size = 4 << 20
	pool := NewWindowPool(size)
	frozenGrown := func() (*Session, *Stream) {
		session, stream, p, cfg := grown(t, pool)
		p.fill(cfg.MaxFrameSize) // all of its window, and its reader stops
		p.settle(stream)
		if unreadOf(stream) <= cfg.NeedStreamBuffer || claimOf(stream) == 0 {
			t.Fatalf("rig: unread %d claim %d", unreadOf(stream), claimOf(stream))
		}
		return session, stream
	}
	whole := func(what string, within time.Duration, sessions ...*Session) {
		t.Helper()
		deadline := time.Now().Add(within)
		for {
			ok := pool.Free() == size
			for _, s := range sessions {
				if e, sp := extraOf(s); e != 0 || sp != 0 {
					ok = false
				}
			}
			if ok {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: pool free %d of %d", what, pool.Free(), size)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	// the session closes: what its stream holds past NeedStreamBuffer stays
	// covered (the rest of the claim can cover nothing more: nothing arrives)
	s1, st1 := frozenGrown()
	need := st1.sess.config.NeedStreamBuffer
	claim := claimOf(st1)
	keep := func() int { // what the stream holds past NeedStreamBuffer, as far as its claim covered
		k := unreadOf(st1) - need
		if k > claim {
			k = claim
		}
		if k < 0 {
			k = 0
		}
		return k
	}
	s1.Close()
	if k := keep(); pool.Free() != size-k || claimOf(st1) != k {
		t.Fatalf("session closed with %d unread under a claim of %d: pool free %d (claim %d), want %d (the data is still in memory)",
			unreadOf(st1), claim, pool.Free(), claimOf(st1), size-k)
	}
	if n, _ := st1.Read(make([]byte, 64<<10)); n == 0 { // what it holds is still read after the close
		t.Fatal("nothing read after the close")
	}
	if c := claimOf(st1); pool.Free() != size-c || c != keep() {
		t.Fatalf("a read after the close: pool free %d, claim %d for %d unread", pool.Free(), c, unreadOf(st1))
	}
	// ... until the stream lets it go
	st1.Close()
	whole("the stream closed after its session", time.Second, s1)

	// a session dies and its app never closes the stream: back after closeGrace
	s2, st2 := frozenGrown()
	s2.Close()
	if pool.Free() == size {
		t.Fatal("abrupt death: the claim went back with the data still held")
	}
	whole("a stream never closed, closeGrace after its session died", closeGrace+time.Second, s2)
	if c := claimOf(st2); c != 0 {
		t.Fatalf("claim %d left on a stream after closeGrace", c)
	}
	st2.Close() // late: nothing more to give back
	if pool.Free() != size {
		t.Fatalf("a late close gave back more: pool free %d of %d", pool.Free(), size)
	}
}
