package engine

import (
	"testing"
	"time"
)

// r68fix2 (mconn_sub_resend.go): a short, quiet light connection whose
// second sub joined with a hole loses its first sub's link (an RST: what the
// link held is gone) right after both sides wrote on it. With
// HS2_MCONN_SUB_RESEND the unacked chunks are sent again on the live sub:
// both directions arrive byte-exact and the connection goes on. Key off (the
// rel68 rule): the connection is typically reset (a sub died holding the
// only copy of some bytes).
func TestMConnSubResendRst(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(map[bool]string{true: "resend", false: "off"}[on], func(t *testing.T) {
			ended, err := subResendRst(t, on)
			if on && (ended || err != nil) {
				t.Fatalf("resend on: ended=%v err=%v, want the connection alive and byte-exact", ended, err)
			}
			if !on {
				t.Logf("resend off (the old rule): ended=%v err=%v", ended, err)
			}
		})
	}
}

func subResendRst(t *testing.T, on bool) (ended bool, err error) {
	setCloseHS(t, true)
	oldR, oldS, oldK := stripeResend.Load(), mconnSubResend.Load(), keepStallOn.Load()
	stripeResend.Store(true)
	mconnSubResend.Store(on)
	keepStallOn.Store(true)
	t.Cleanup(func() { stripeResend.Store(oldR); mconnSubResend.Store(oldS); keepStallOn.Store(oldK) })
	r0 := subResendStat.resends.Load()
	n, iran, exit := newLitePair(t)
	// a little control-like traffic, then the second sub joins
	hello := randBytes(300, 41)
	both(t, func() error { return xfer(iran, hello, 300, 0) }, func() error { return readN(exit, hello) })
	mWaitFor(t, "two subs", mLiteAfter+4*time.Second, func() bool { return liveSubs(iran) == 2 && liveSubs(exit) == 2 })
	// the FIRST sub's link (the one without a hole: the second joined with
	// one, within mHoleSafe) swallows what is written from now on (its
	// buffers lost at the RST), then dies
	l := n.link(0)
	l.iran.drop.Store(true)
	l.exit.drop.Store(true)
	req, resp := randBytes(20000, 42), randBytes(30000, 43)
	if _, e := iran.Write(req); e != nil {
		t.Fatal(e)
	}
	if _, e := exit.Write(resp); e != nil {
		t.Fatal(e)
	}
	time.Sleep(100 * time.Millisecond)
	l.iran.Close()
	l.exit.Close()
	done := make(chan error, 2)
	go func() { done <- readN(exit, req) }()
	go func() { done <- readN(iran, resp) }()
	for i := 0; i < 2; i++ {
		select {
		case e := <-done:
			if e != nil && err == nil {
				err = e
			}
		case <-time.After(5 * time.Second):
			if err == nil {
				err = errTimeoutTest
			}
		}
	}
	ended = rstEnded(iran) || rstEnded(exit)
	if on && err == nil && !ended {
		more := randBytes(40000, 44)
		both(t, func() error { return xfer(iran, more, 2000, 0) }, func() error { return readN(exit, more) })
		if subResendStat.resends.Load() == r0 {
			t.Fatal("no sub resend ran")
		}
	}
	iran.Close()
	exit.Close()
	return ended, err
}

type testTimeoutErr struct{}

func (testTimeoutErr) Error() string { return "timeout waiting for the bytes" }

var errTimeoutTest error = testTimeoutErr{}

// r68fix3: the live sub is dark (its link swallows everything and it read
// nothing for over subResendFresh) when the first sub's link dies: no resend
// onto it (that was a long stall in the r68fix2 lab, B1), the old rule
// applies (a fast reset).
func TestMConnSubResendDarkTarget(t *testing.T) {
	setCloseHS(t, true)
	oldR, oldS, oldK := stripeResend.Load(), mconnSubResend.Load(), keepStallOn.Load()
	stripeResend.Store(true)
	mconnSubResend.Store(true)
	keepStallOn.Store(true)
	t.Cleanup(func() { stripeResend.Store(oldR); mconnSubResend.Store(oldS); keepStallOn.Store(oldK) })
	// the r68fix3 rule alone: b449d's target wait (Iran opens a new sub and
	// the conn goes on) is TestB449TgtWait* (mconn_b449_tgtwait_test.go)
	oldW := b449TgtWait.Load()
	b449TgtWait.Store(0)
	t.Cleanup(func() { b449TgtWait.Store(oldW) })
	r0, n0 := subResendStat.resends.Load(), subResendStat.noTarget.Load()
	o0 := subResendStat.noTgtLost.Load() + subResendStat.noTgtSurv.Load()
	n, iran, exit := newLitePair(t)
	hello := randBytes(300, 51)
	both(t, func() error { return xfer(iran, hello, 300, 0) }, func() error { return readN(exit, hello) })
	mWaitFor(t, "two subs", mLiteAfter+4*time.Second, func() bool { return liveSubs(iran) == 2 && liveSubs(exit) == 2 })
	// both links go dark; the second stays dark (no read) past subResendFresh
	for i := 0; i < 2; i++ {
		l := n.link(i)
		l.iran.drop.Store(true)
		l.exit.drop.Store(true)
	}
	if _, e := iran.Write(randBytes(20000, 52)); e != nil {
		t.Fatal(e)
	}
	time.Sleep(subResendFresh + 300*time.Millisecond)
	l := n.link(0)
	l.iran.Close()
	l.exit.Close()
	mWaitFor(t, "the first sub gone", 3*time.Second, func() bool { return liveSubs(iran) < 2 || rstEnded(iran) })
	time.Sleep(200 * time.Millisecond)
	if subResendStat.resends.Load() != r0 {
		t.Fatalf("a resend onto the dark sub ran (resends %d -> %d)", r0, subResendStat.resends.Load())
	}
	t.Logf("no fresh target: %d -> %d (lost %d survived %d), iran ended=%v", n0, subResendStat.noTarget.Load(),
		subResendStat.noTgtLost.Load(), subResendStat.noTgtSurv.Load(), rstEnded(iran))
	// b271 Q5: every no-target death got an outcome
	if o := subResendStat.noTgtLost.Load() + subResendStat.noTgtSurv.Load() - o0; o != subResendStat.noTarget.Load()-n0 {
		t.Fatalf("no-target outcomes %d, no-target deaths %d", o, subResendStat.noTarget.Load()-n0)
	}
	iran.Close()
	exit.Close()
}

// r68fix3: a connection over the scope keeps nothing new.
func TestMConnSubResendScope(t *testing.T) {
	oldR, oldS, oldK, oldSc := stripeResend.Load(), mconnSubResend.Load(), keepStallOn.Load(), subResendScope
	stripeResend.Store(true)
	mconnSubResend.Store(true)
	keepStallOn.Store(true)
	subResendScope = 64 << 10
	t.Cleanup(func() {
		stripeResend.Store(oldR)
		mconnSubResend.Store(oldS)
		keepStallOn.Store(oldK)
		subResendScope = oldSc
	})
	m := &mConn{mode: planeLight}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.subKeepLocked() {
		t.Fatal("a fresh light connection should keep")
	}
	m.sendOff = 64<<10 + 1
	s0 := subResendStat.scoped.Load()
	if m.subKeepLocked() || m.subKeepLocked() {
		t.Fatal("over the scope: kept")
	}
	if subResendStat.scoped.Load() != s0+1 {
		t.Fatal("the scope count should move once")
	}
}

// r68fix4: a sub's gap (a chunk written without a kept copy) heals once that
// chunk is older than max(subKeepAgeMin, subKeepAgeK x srtt) on a link that
// is not in an RTO loss; not before, and not while the link is in one.
func TestMConnSubResendGapHeals(t *testing.T) {
	oldL := subLinkAck
	la := linkAck{srtt: 100 * time.Millisecond}
	subLinkAck = func(*msub) (linkAck, bool) { return la, true }
	t.Cleanup(func() { subLinkAck = oldL })
	m := &mConn{mode: planeLight}
	x := &msub{m: m}
	m.subs = []*msub{x}
	now := time.Now()
	x.lastRecv.Store(monoNano(now))
	m.mu.Lock()
	defer m.mu.Unlock()
	g0, h0 := subResendStat.gapped.Load(), subResendStat.healed.Load()
	subGapLocked(x, now.Add(-200*time.Millisecond))
	subGapLocked(x, now.Add(-250*time.Millisecond)) // older: the newest stays
	if subResendStat.gapped.Load() != g0+1 || !m.subGapOpenLocked() {
		t.Fatal("one gap should be open")
	}
	m.subAgeTrimLocked(now, true)
	if x.subGapT.IsZero() {
		t.Fatal("healed at 200 ms (age 300 ms)")
	}
	if x.subSrtt != la.srtt || subKeepAgeOf(x) != subKeepAgeMin {
		t.Fatalf("srtt %v age %v", x.subSrtt, subKeepAgeOf(x))
	}
	la.rtoLoss = true
	m.subAgeTrimLocked(now.Add(time.Second), true)
	if x.subGapT.IsZero() {
		t.Fatal("healed during an RTO loss")
	}
	la.rtoLoss = false
	m.subAgeTrimLocked(now.Add(time.Second), true)
	if !x.subGapT.IsZero() || subResendStat.healed.Load() != h0+1 || m.subGapOpenLocked() {
		t.Fatal("the gap should heal: 1.2 s old, link fine")
	}
	la.srtt = 400 * time.Millisecond
	subGapLocked(x, now) // the tick looks only at a sub with something to trim
	m.subAgeTrimLocked(now, true)
	if subKeepAgeOf(x) != 1200*time.Millisecond {
		t.Fatalf("age %v, want 3 x 400 ms", subKeepAgeOf(x))
	}
}
