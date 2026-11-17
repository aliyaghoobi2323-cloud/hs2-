package engine

import (
	"testing"
	"time"
)

// b270: a txDark non-primary call sub queues on the live primary the chunks
// the primary lacks, at the txDark edge; a repeat within b270TxResEvery does
// nothing; a later repeat takes only chunks past the first resend; a txDark
// primary or an isDark sub (cs223's case) or the key off: nothing.
func TestB270TxDarkResend(t *testing.T) {
	cs223Keys(t)
	old := b270TxResOn.Load()
	t.Cleanup(func() { b270TxResOn.Store(old) })
	b270TxResOn.Store(true)
	m := &mConn{lite: true, callLikeAt: time.Now()}
	s := &msub{m: m, idx: 1}
	p := &msub{m: m, idx: 0, missEnd: 1 << 40}
	m.subs = []*msub{p, s}
	m.primary.Store(p)
	c0, c1, c2 := cs223Chunk(0, 1000), cs223Chunk(1000, 1000), cs223Chunk(2000, 1000)
	now := time.Now()
	s.sent = []sentRec{{c: c0, t: now}, {c: c1, t: now}}
	p.q = []*mchunk{c0} // p holds c0 already: only c1 is missing there

	// not txDark: nothing
	m.b270TxResendLocked(s, now, 0)
	if len(p.q) != 1 {
		t.Fatalf("not txDark: p.q %d", len(p.q))
	}
	s.dc.txDark = true
	e0 := b270Stat.edge.Load()
	m.b270TxResendLocked(s, now, 0)
	if len(p.q) != 2 || p.q[0] != c1 || b270Stat.edge.Load() != e0+1 {
		t.Fatalf("edge: p.q %d", len(p.q))
	}
	// a new chunk lands on s only; a repeat too soon does nothing
	s.sent = append(s.sent, sentRec{c: c2, t: now})
	p.q = nil // p wrote c0, c1 (missEnd 1<<40: delivery unknown)
	m.b270TxResendLocked(s, now.Add(100*time.Millisecond), 0)
	if len(p.q) != 0 {
		t.Fatalf("too soon: p.q %d", len(p.q))
	}
	// the repeat takes only c2 (c0, c1 were covered by the edge resend)
	m.b270TxResendLocked(s, now.Add(b270TxResEvery+time.Millisecond), 0)
	if len(p.q) != 1 || p.q[0] != c2 {
		t.Fatalf("repeat: p.q %d", len(p.q))
	}
	// isDark: cs223's dark resend owns it
	p.q = nil
	s.b270End = 0
	s.dc.isDark = true
	m.b270TxResendLocked(s, now.Add(time.Second), 0)
	if len(p.q) != 0 {
		t.Fatal("isDark sub: b270 must not act")
	}
	s.dc.isDark = false
	// a txDark primary is switched, not resent
	m.primary.Store(s)
	m.b270TxResendLocked(s, now.Add(2*time.Second), 0)
	if len(p.q) != 0 {
		t.Fatal("txDark primary: b270 must not act")
	}
	m.primary.Store(p)
	// the only other sub is txDark too: nowhere to resend
	p.dc.txDark = true
	m.b270TxResendLocked(s, now.Add(3*time.Second), 0)
	if len(p.q) != 0 {
		t.Fatal("txDark target")
	}
	p.dc.txDark = false
	// key off
	b270TxResOn.Store(false)
	m.b270TxResendLocked(s, now.Add(4*time.Second), 0)
	if len(p.q) != 0 {
		t.Fatal("key off")
	}
	// not a call (callKeepLocked false): nothing
	b270TxResOn.Store(true)
	m.callLikeAt = time.Time{}
	m.b270TxResendLocked(s, now.Add(5*time.Second), 0)
	if len(p.q) != 0 {
		t.Fatal("not a call")
	}
}

// b270: a non-primary call sub whose link has received nothing for
// darkcallAfter while the connection's receive advances (the peer->us
// direction of that link is dead) resends like a txDark one; a receive
// that is itself stalled (the whole conn silent) does not trigger it.
func TestB270RxDeadResend(t *testing.T) {
	cs223Keys(t)
	old := b270TxResOn.Load()
	t.Cleanup(func() { b270TxResOn.Store(old) })
	b270TxResOn.Store(true)
	m := &mConn{lite: true, callLikeAt: time.Now()}
	s := &msub{m: m, idx: 1}
	p := &msub{m: m, idx: 0, missEnd: 1 << 40}
	m.subs = []*msub{p, s}
	m.primary.Store(p)
	now := time.Now()
	nowc := monoNano(now)
	c0 := cs223Chunk(0, 1000)
	s.sent = []sentRec{{c: c0, t: now}}
	s.dc.ok, s.dc.lastRd = true, nowc-int64(darkcallAfter())-1
	m.dc.rxAt = now.Add(-darkcallAfter()) // the conn's receive is stalled too: not half dead
	m.b270TxResendLocked(s, now, nowc)
	if len(p.q) != 0 {
		t.Fatalf("conn silent: p.q %d", len(p.q))
	}
	m.dc.rxAt = now // receiving over p
	r0 := b270Stat.rxEdge.Load()
	m.b270TxResendLocked(s, now, nowc)
	if len(p.q) != 1 || p.q[0] != c0 || b270Stat.rxEdge.Load() != r0+1 {
		t.Fatalf("rx dead: p.q %d", len(p.q))
	}
	s.dc.lastRd = nowc // the link receives again
	m.b270TxResendLocked(s, now.Add(time.Second), nowc)
	if s.b270Trig {
		t.Fatal("trigger must clear when the link receives")
	}
}
