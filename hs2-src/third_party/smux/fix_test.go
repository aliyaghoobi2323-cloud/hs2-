package smux

import (
	"testing"
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
