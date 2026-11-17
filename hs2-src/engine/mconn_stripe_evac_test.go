package engine

import (
	"testing"
	"time"
)

// evacConn: an Iran striped connection, a primary and n stripe subs whose
// keys are 0..n (the primary's key is 0).
func evacConn(t *testing.T, n int, low map[int]bool) (*mConn, []*msub) {
	t.Helper()
	oldOn, oldRs, oldLow, oldPeer := stripeEvacOn.Load(), stripeResend.Load(), stripeEvacLow, stripeEvacPeerOK
	t.Cleanup(func() {
		stripeEvacOn.Store(oldOn)
		stripeResend.Store(oldRs)
		stripeEvacLow, stripeEvacPeerOK = oldLow, oldPeer
	})
	stripeEvacPeerOK = func(*msub) bool { return true }
	stripeEvacOn.Store(true)
	stripeResend.Store(true)
	stripeEvacLow = func(k any) bool { i, ok := k.(int); return ok && low[i] }
	stripeEvacSec.Store(0)
	m, p := gateConn(n)
	m.iran = true
	m.mode = planeLight
	p.key = 0
	for i, s := range m.subs[1:] {
		s.key = i + 1
	}
	m.primary.Store(p)
	m.stUpNow = true
	return m, m.subs
}

func TestStripeEvacPicksLoweredStripeSub(t *testing.T) {
	m, subs := evacConn(t, 3, map[int]bool{2: true})
	now := time.Now()
	if s := m.stripeEvacLocked(now); s != subs[2] {
		t.Fatalf("evac %v, want the sub on the lowered path", s)
	}
	// one move per connection per stripeEvacGap
	if s := m.stripeEvacLocked(now.Add(stripeEvacGap / 2)); s != nil {
		t.Fatalf("second move inside the gap: %v", s)
	}
	if s := m.stripeEvacLocked(now.Add(stripeEvacGap)); s != subs[2] {
		t.Fatalf("after the gap: %v", s)
	}
}

func TestStripeEvacPrefersStripeOverPrimary(t *testing.T) {
	m, subs := evacConn(t, 2, map[int]bool{0: true, 1: true})
	if s := m.stripeEvacLocked(time.Now()); s != subs[1] {
		t.Fatalf("evac %v, want stripe sub 1 before the primary", s)
	}
	m2, subs2 := evacConn(t, 2, map[int]bool{0: true})
	if s := m2.stripeEvacLocked(time.Now()); s != subs2[0] {
		t.Fatalf("evac %v, want the primary (the only bad sub)", s)
	}
}

func TestStripeEvacNeverTheLastGoodOrOff(t *testing.T) {
	m, _ := evacConn(t, 2, map[int]bool{0: true, 1: true, 2: true})
	if s := m.stripeEvacLocked(time.Now()); s != nil {
		t.Fatalf("every sub bad: moved %v", s)
	}
	m, _ = evacConn(t, 2, map[int]bool{1: true})
	stripeEvacOn.Store(false)
	if s := m.stripeEvacLocked(time.Now()); s != nil {
		t.Fatalf("key off: moved %v", s)
	}
	if stripeEvacSkip(1) {
		t.Fatal("key off: pick skips a lowered link")
	}
	stripeEvacOn.Store(true)
	if !stripeEvacSkip(1) || stripeEvacSkip(0) {
		t.Fatal("key on: pick must skip exactly the lowered links")
	}
	for _, f := range []func(*mConn){
		func(m *mConn) { m.iran = false },
		func(m *mConn) { m.handshaking = true },
		func(m *mConn) { m.closing = true },
		func(m *mConn) { m.rawW.Store(true) },
		func(m *mConn) { stripeResend.Store(false) },
		func(m *mConn) { m.stUpNow = false }, // a download: only with HS2_MCONN_STRIPE_EVAC_DOWN and a close handshake
	} {
		m, _ := evacConn(t, 2, map[int]bool{1: true})
		f(m)
		if s := m.stripeEvacLocked(time.Now()); s != nil {
			t.Fatalf("guarded state moved %v", s)
		}
	}
}

func TestStripeEvacRateBound(t *testing.T) {
	old := stripeEvacPerSec
	t.Cleanup(func() { stripeEvacPerSec = old })
	stripeEvacPerSec = 2
	now := time.Unix(1000, 0)
	got := 0
	for range 5 {
		m, _ := evacConn(t, 2, map[int]bool{1: true})
		stripeEvacSec.Store(now.Unix())
		stripeEvacSecN.Store(int64(got))
		if m.stripeEvacLocked(now) != nil {
			got++
		}
	}
	if got != 2 {
		t.Fatalf("moves in one second %d, want 2", got)
	}
}

func TestStripeEvacRawGrow(t *testing.T) {
	oldRaw := stripeEvacRaw.Load()
	t.Cleanup(func() { stripeEvacRaw.Store(oldRaw) })
	mk := func() *mConn {
		m, subs := evacConn(t, 0, map[int]bool{0: true})
		m.raw1 = subs[0]
		m.rawW.Store(true)
		return m
	}
	now := time.Now()
	stripeEvacRaw.Store(false)
	if mk().stripeEvacRawGrowLocked(1, 10<<20, now) {
		t.Fatal("raw key off: grew")
	}
	stripeEvacRaw.Store(true)
	m := mk()
	if !m.stripeEvacRawGrowLocked(1, 10<<20, now) {
		t.Fatal("raw heavy on a lowered path: no growth")
	}
	if m.stripeEvacRawGrowLocked(1, 10<<20, now.Add(stripeEvacGap/2)) {
		t.Fatal("grew twice inside the gap")
	}
	if mk().stripeEvacRawGrowLocked(1, stripeEvacRawRate/2, now) {
		t.Fatal("a light raw connection grew")
	}
	m = mk()
	m.raw1.key = 7 // a good path
	if m.stripeEvacRawGrowLocked(1, 10<<20, now) {
		t.Fatal("raw sub on a good path grew")
	}
	if mk().stripeEvacRawGrowLocked(2, 10<<20, now) {
		t.Fatal("an already striped connection took the raw rule")
	}
}

// An exit that does not announce the resend (r31, or RESEND=0) is never
// closed a sub on purpose; the announcement follows HS2_MCONN_STRIPE_RESEND.
func TestStripeEvacNeedsPeerResend(t *testing.T) {
	m, _ := evacConn(t, 2, map[int]bool{1: true})
	stripeEvacPeerOK = func(*msub) bool { return false }
	if s := m.stripeEvacLocked(time.Now()); s != nil {
		t.Fatalf("moved %v toward an exit without the resend", s)
	}
	stripeEvacPeerOK = oldPeerOK
	if stripeEvacPeerOK(&msub{key: 1}) {
		t.Fatal("a sub without a link passed the peer check")
	}
	oldOn, oldLite, oldK, oldRs := mconnOn.Load(), mconnLite.Load(), stripeK.Load(), stripeResend.Load()
	t.Cleanup(func() { mconnOn.Store(oldOn); mconnLite.Store(oldLite); stripeK.Store(oldK); stripeResend.Store(oldRs) })
	mconnOn.Store(true)
	mconnLite.Store(true)
	stripeK.Store(4)
	stripeResend.Store(true)
	if infoExt()&capExtMConnResend == 0 {
		t.Fatal("resend on: not announced")
	}
	stripeResend.Store(false)
	if infoExt()&capExtMConnResend != 0 {
		t.Fatal("resend off: announced")
	}
}

var oldPeerOK = stripeEvacPeerOK

// evacHSSub: a sub whose exit reads CLOSE-REQ.
type evacHSSub struct{ mSub }

func (evacHSSub) mconnEvacHS() bool { return true }

// HS2_MCONN_STRIPE_EVAC_DOWN: a download moves a lowered stripe sub through
// the close handshake (CLOSE-REQ, the sub retired, nothing closed), never
// the primary, never a sub whose exit does not read CLOSE-REQ, never with
// the key off.
func TestStripeEvacDownCloseReq(t *testing.T) {
	old := stripeEvacDown.Load()
	t.Cleanup(func() { stripeEvacDown.Store(old) })
	stripeEvacDown.Store(true)
	m, subs := evacConn(t, 2, map[int]bool{2: true})
	m.stUpNow = false
	subs[2].rw = evacHSSub{}
	if s := m.stripeEvacLocked(time.Now()); s != nil {
		t.Fatalf("download: sub %v handed to subDied, want the close handshake", s)
	}
	b := subs[2]
	if !b.retired || !b.closeHS || len(b.q) != 1 || b.q[0].kind != mRecCloseReq {
		t.Fatalf("bad sub: retired=%v closeHS=%v q=%d", b.retired, b.closeHS, len(b.q))
	}
	if subs[1].retired || subs[0].retired {
		t.Fatal("a good sub was retired")
	}
	// the exit does not read CLOSE-REQ: no move
	m, subs = evacConn(t, 2, map[int]bool{2: true})
	m.stUpNow = false
	if s := m.stripeEvacLocked(time.Now()); s != nil || subs[2].retired {
		t.Fatalf("no close handshake on the link: moved %v", s)
	}
	// the primary is never moved by a download
	m, subs = evacConn(t, 2, map[int]bool{0: true})
	m.stUpNow = false
	subs[0].rw = evacHSSub{}
	if s := m.stripeEvacLocked(time.Now()); s != nil || subs[0].retired {
		t.Fatalf("download moved the primary")
	}
	// key off: a download is never moved
	stripeEvacDown.Store(false)
	m, subs = evacConn(t, 2, map[int]bool{2: true})
	m.stUpNow = false
	subs[2].rw = evacHSSub{}
	if s := m.stripeEvacLocked(time.Now()); s != nil || subs[2].retired {
		t.Fatalf("key off: download moved")
	}
}

// The exit, reading CLOSE-REQ on a stripe sub, queues what the sub holds or
// wrote recently once more at the front of the live sub with the shortest
// queue, in offset order, and keeps the sub's own copies (they drain before
// CLOSE-ACK, whose end is unchanged).
func TestStripeCloseResendExit(t *testing.T) {
	old := stripeResend.Load()
	t.Cleanup(func() { stripeResend.Store(old) })
	stripeResend.Store(true)
	m, p := gateConn(2)
	s, o := m.subs[1], m.subs[2]
	mk := func(off uint64) *mchunk {
		c := getChunk(mRecData)
		c.n = 1000
		c.setOff(off)
		return c
	}
	now := time.Now()
	m.mu.Lock()
	m.keepSentLocked(s, now, mk(2000), mk(0))
	s.push(mk(4000))
	p.push(mk(1000))
	p.push(mk(3000)) // the primary's queue is longer: o takes the chunks
	end := s.txEnd
	if n := m.onCloseReqStripeForTest(s); n != 3 {
		m.mu.Unlock()
		t.Fatalf("queued again %d chunks, want 3", n)
	}
	m.mu.Unlock()
	if len(o.q) != 3 || o.q[0].off != 0 || o.q[1].off != 2000 || o.q[2].off != 4000 {
		t.Fatalf("target queue %d chunks", len(o.q))
	}
	if len(s.q) != 1 || len(s.sent) != 2 || s.txEnd != end {
		t.Fatalf("bad sub changed: q=%d sent=%d txEnd=%d (was %d)", len(s.q), len(s.sent), s.txEnd, end)
	}
	if o.q[2].copies != 2 {
		t.Fatalf("copies %d, want 2", o.q[2].copies)
	}
	// Iran never does this; nor does a sub that does not stripe
	m.iran = true
	if m.stripeCloseResendLocked(s) != 0 {
		t.Fatal("Iran queued again")
	}
	m.iran = false
	p.stripe = false
	if m.stripeCloseResendLocked(p) != 0 {
		t.Fatal("non-stripe sub queued again")
	}
}

func (m *mConn) onCloseReqStripeForTest(s *msub) int { return m.stripeCloseResendLocked(s) }
