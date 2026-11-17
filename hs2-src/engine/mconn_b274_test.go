package engine

import (
	"testing"
	"time"
)

// b274: a lite conn that was call-like within callFreeTTL wants its second
// sub back at once (callUp set: the call budget decides); an old call, a
// never-call conn, the key off or HS2_MCONN_CALL off: no.
func TestB274RepairWant(t *testing.T) {
	old, oldCall := b274RepairOn.Load(), mconnCall.Load()
	t.Cleanup(func() { b274RepairOn.Store(old); mconnCall.Store(oldCall) })
	b274RepairOn.Store(true)
	mconnCall.Store(true)
	now := time.Now()

	m := &mConn{lite: true, callLikeAt: now.Add(-time.Second)}
	a0 := b274Stat.repair.Load()
	if !m.b274RepairWantLocked(now) || !m.callUp || b274Stat.repair.Load() != a0+1 {
		t.Fatal("recent call: want the second sub with callUp")
	}
	if (&mConn{lite: true}).b274RepairWantLocked(now) {
		t.Fatal("never call-like: no")
	}
	if (&mConn{lite: true, callLikeAt: now.Add(-b274RecentTTL)}).b274RepairWantLocked(now) {
		t.Fatal("call-like too long ago: no")
	}
	if (&mConn{callLikeAt: now}).b274RepairWantLocked(now) {
		t.Fatal("not lite: no")
	}
	b274RepairOn.Store(false)
	if (&mConn{lite: true, callLikeAt: now}).b274RepairWantLocked(now) {
		t.Fatal("key off: no")
	}
	b274RepairOn.Store(true)
	mconnCall.Store(false)
	if (&mConn{lite: true, callLikeAt: now}).b274RepairWantLocked(now) {
		t.Fatal("HS2_MCONN_CALL off: no")
	}
}

// b274: a b270 resend leaves out s's sent chunks older than b270Recent
// (they went out before the link died) but keeps the recent ones.
func TestB274TxResRecentOnly(t *testing.T) {
	cs223Keys(t)
	old := b270TxResOn.Load()
	t.Cleanup(func() { b270TxResOn.Store(old) })
	b270TxResOn.Store(true)
	oldR := b270Recent
	t.Cleanup(func() { b270Recent = oldR })
	b270Recent = 3 * time.Second
	m := &mConn{lite: true, callLikeAt: time.Now()}
	s := &msub{m: m, idx: 1}
	p := &msub{m: m, idx: 0, missEnd: 1 << 40}
	m.subs = []*msub{p, s}
	m.primary.Store(p)
	now := time.Now()
	c0, c1 := cs223Chunk(0, 1000), cs223Chunk(1000, 1000)
	s.sent = []sentRec{{c: c0, t: now.Add(-b270Recent - time.Second)}, {c: c1, t: now}}
	s.dc.txDark = true
	m.b270TxResendLocked(s, now, 0)
	if len(p.q) != 1 || p.q[0] != c1 {
		t.Fatalf("only the recent chunk: p.q %d", len(p.q))
	}
	if len(s.sent) != 2 {
		t.Fatalf("s.sent must stay whole: %d", len(s.sent))
	}
}
