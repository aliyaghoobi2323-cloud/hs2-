package engine

import (
	"encoding/binary"
	"testing"
	"time"
)

// xs261 (mconn_xs261.go).

func setXS261(t *testing.T, on, peer bool) {
	t.Helper()
	old, oldP := xs261On.Load(), xsPeerP.Load()
	xs261On.Store(on)
	xsPeerSet(func(*msub) bool { return peer })
	t.Cleanup(func() {
		xs261On.Store(old)
		xsPeerP.Store(oldP)
	})
}

// iranTwin: the Iran end of the link the exit sub s rides.
func iranTwin(t *testing.T, n *tNet, iran *mConn, s *msub) *msub {
	t.Helper()
	l := exitLinkOf(n, s)
	if l == nil {
		t.Fatal("no link")
	}
	iran.mu.Lock()
	defer iran.mu.Unlock()
	for _, x := range iran.subs {
		if x.rw == mSub(l.iran) {
			return x
		}
	}
	t.Fatal("no iran twin")
	return nil
}

func primaryOf(m *mConn) *msub {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.primary.Load()
}

// The exit moves a call off a txDark primary: it sends XS on the new
// primary and Iran (which saw nothing wrong) moves its own primary onto the
// same link; data still flows both ways once each, in order.
func TestXS261SwitchMovesPeer(t *testing.T) {
	setDarkcall(t)
	setDc254(t, true)
	setXS261(t, true, true)
	n, iran, exit := darkswPairDC(t)
	f := newDcFake()
	darkcallReadSet(f.read)
	darkcallMarksSet(func(*msub) (uint32, uint32, bool) { return 0, 0, true })
	p := primaryOf(exit)
	ip := iranTwin(t, n, iran, p)
	if primaryOf(iran) != ip {
		t.Skip("pair starts on different links")
	}
	f.set(p, "lossy")
	s0, a0 := xsStat.sentSw.Load(), xsStat.acted.Load()
	dcVisit(exit, 3)
	np := primaryOf(exit)
	if np == p {
		t.Fatal("exit did not switch")
	}
	if xsStat.sentSw.Load() == s0 {
		t.Fatal("no XS sent on the switch")
	}
	want := iranTwin(t, n, iran, np)
	mWaitFor(t, "iran follows", 2*time.Second, func() bool { return primaryOf(iran) == want })
	if xsStat.acted.Load() == a0 {
		t.Fatal("act not counted")
	}
	f.set(p, "")
	data, up := randBytes(60_000, 31), randBytes(40_000, 32)
	both(t,
		func() error { return xfer(exit, data, 2000, 0) },
		func() error { return readN(iran, data) },
		func() error { return xfer(iran, up, 2000, 0) },
		func() error { return readN(exit, up) },
	)
	noExtra(t, iran, exit)
	closeAndWait(t, iran, exit)
}

// onXS: idempotent (already primary), stale seq ignored, rate-limited,
// unknown (dead/retired) sub ignored; each acts on this conn only.
func TestXS261OnXSRules(t *testing.T) {
	setDarkcall(t)
	setXS261(t, true, true)
	_, iran, exit := darkswPairDC(t)
	_, iran2, exit2 := darkswPairDC(t)
	iran.mu.Lock()
	live := iran.rrLiveSubsLocked()
	iran.mu.Unlock()
	p := primaryOf(iran)
	var o *msub
	for _, s := range live {
		if s != p {
			o = s
		}
	}
	if o == nil {
		t.Fatal("no second sub")
	}
	other0 := primaryOf(iran2)
	a0, sm0, st0, r0 := xsStat.acted.Load(), xsStat.same.Load(), xsStat.stale.Load(), xsStat.rate.Load()
	iran.onXS(o, 10)
	if primaryOf(iran) != o || xsStat.acted.Load() != a0+1 {
		t.Fatal("no act")
	}
	if primaryOf(iran2) != other0 {
		t.Fatal("another conn moved")
	}
	iran.onXS(o, 10) // stale
	if xsStat.stale.Load() != st0+1 {
		t.Fatal("stale seq not ignored")
	}
	iran.onXS(o, 11) // idempotent
	if xsStat.same.Load() != sm0+1 || primaryOf(iran) != o {
		t.Fatal("same-primary not idempotent")
	}
	iran.onXS(p, 12) // within xsActGap
	if xsStat.rate.Load() != r0+1 || primaryOf(iran) != o {
		t.Fatal("rate limit")
	}
	iran.mu.Lock()
	p.retired = true
	iran.dc.xs.actAt = time.Time{}
	iran.mu.Unlock()
	i0 := xsStat.ign.Load()
	iran.onXS(p, 13) // not live
	if xsStat.ign.Load() != i0+1 || primaryOf(iran) != o {
		t.Fatal("a retired sub was taken")
	}
	iran.mu.Lock()
	p.retired = false
	iran.mu.Unlock()
	closeAndWait(t, iran, exit, iran2, exit2)
}

// Compatibility: a peer that did not announce capExtMConnXS is never sent
// XS (an older build ends the conn on an unknown record); the key off sends
// nothing and an XS that arrives anyway is read and ignored without harm.
func TestXS261Compat(t *testing.T) {
	setDarkcall(t)
	setDc254(t, true)
	setXS261(t, true, false)
	_, iran, exit := darkswPairDC(t)
	f := newDcFake()
	darkcallReadSet(f.read)
	darkcallMarksSet(func(*msub) (uint32, uint32, bool) { return 0, 0, true })
	p := primaryOf(exit)
	f.set(p, "lossy")
	s0, nc0 := xsStat.sentSw.Load(), xsStat.noCap.Load()
	dcVisit(exit, 3)
	if primaryOf(exit) == p {
		t.Fatal("no switch")
	}
	if xsStat.sentSw.Load() != s0 || xsStat.noCap.Load() == nc0 {
		t.Fatal("XS sent to a peer without the cap")
	}
	// key off: nothing sent; a raw XS record on the wire is parsed + ignored
	xs261On.Store(false)
	exit.mu.Lock()
	if exit.xsSendLocked(exit.primary.Load(), time.Now().Add(time.Hour), false) {
		exit.mu.Unlock()
		t.Fatal("key off sent")
	}
	c := getChunk(mRecXS)
	binary.BigEndian.PutUint64(c.buf[1:9], 99)
	exit.primary.Load().push(c)
	exit.mu.Unlock()
	rx0 := xsStat.rx.Load()
	mWaitFor(t, "XS read", 2*time.Second, func() bool { return xsStat.rx.Load() > rx0 })
	f.set(p, "")
	data, up := randBytes(30_000, 41), randBytes(20_000, 42)
	both(t,
		func() error { return xfer(exit, data, 2000, 0) },
		func() error { return readN(iran, data) },
		func() error { return xfer(iran, up, 2000, 0) },
		func() error { return readN(exit, up) },
	)
	closeAndWait(t, iran, exit)
}

// The cap bit follows the key.
func TestXS261InfoExt(t *testing.T) {
	oM, oL := mconnOn.Load(), mconnLite.Load()
	mconnOn.Store(true)
	mconnLite.Store(true)
	t.Cleanup(func() { mconnOn.Store(oM); mconnLite.Store(oL) })
	setXS261(t, true, true)
	if infoExt()&capExtMConnXS == 0 {
		t.Fatal("cap not announced")
	}
	xs261On.Store(false)
	if infoExt()&capExtMConnXS != 0 {
		t.Fatal("cap announced with the key off")
	}
}

// The silent-receive trigger: Iran's call receive is silent >= 800 ms
// while another sub's link receives: Iran sends XS on it, once per
// xsRxResend, and the exit moves its primary there.
func TestXS261RxSilent(t *testing.T) {
	setDarkcall(t)
	setXS261(t, true, true)
	n, iran, exit := darkswPairDC(t)
	ep := primaryOf(exit)
	ip := iranTwin(t, n, iran, ep)
	now := time.Now()
	iran.mu.Lock()
	live := iran.rrLiveSubsLocked()
	var o *msub
	for i, s := range live {
		s.dc.ok, s.dc.lastRd, s.dc.path = true, ctrlNow(), string(rune('a'+i))
		if s != ip {
			o = s
		}
	}
	ip.lastRecv.Store(monoNano(now)) // the peer's primary: read last
	if o != nil {
		o.lastRecv.Store(monoNano(now.Add(-5 * time.Second)))
	}
	iran.dc.rxAt = now.Add(-time.Second)
	r0 := xsStat.sentRx.Load()
	iran.xsRxSilentLocked(live, now, ctrlNow())
	iran.xsRxSilentLocked(live, now.Add(200*time.Millisecond), ctrlNow()) // within xsRxResend
	iran.mu.Unlock()
	if o == nil {
		t.Fatal("no second sub")
	}
	if got := xsStat.sentRx.Load() - r0; got != 1 {
		t.Fatalf("rx XS sent %d, want 1", got)
	}
	mWaitFor(t, "exit moves", 2*time.Second, func() bool {
		np := primaryOf(exit)
		return np != ep && iranTwin(t, n, iran, np) == o
	})
	closeAndWait(t, iran, exit)
}

// b258's cs224 fix under the key: a sub a switch left and a later switch
// chose again is live again (its dcLeft mark cleared); key off: b254.
func TestXS261ReChosenNotStale(t *testing.T) {
	for _, on := range []bool{true, false} {
		setXS261(t, on, false)
		setDarkcall(t)
		setDc254(t, true)
		_, iran, exit := darkswPairDC(t)
		f := newDcFake()
		darkcallReadSet(f.read)
		darkcallMarksSet(func(*msub) (uint32, uint32, bool) { return 0, 0, true })
		a := primaryOf(exit)
		f.set(a, "lossy")
		dcVisit(exit, 3)
		b := primaryOf(exit)
		if b == a {
			t.Fatal("no first switch")
		}
		f.set(a, "")
		f.set(b, "lossy")
		dcVisit(exit, 3)
		exit.mu.Lock()
		back := exit.primary.Load() == a
		left := a.dcLeft
		exit.mu.Unlock()
		if !back {
			t.Fatal("no switch back")
		}
		if left == on {
			t.Fatalf("xs261 %v: re-chosen sub dcLeft %v", on, left)
		}
		closeAndWait(t, iran, exit)
	}
}
