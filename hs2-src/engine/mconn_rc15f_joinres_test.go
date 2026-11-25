package engine

import (
	"testing"
	"time"
)

// rc15f: on Kharej a fresh sub joining a call conn whose two subs both went
// receive-silent takes their stranded chunks at once (b474 s02-TB call 5:
// they stayed on the dead subs until Iran's CLOSE-REQ crossed 9 s later);
// a live sub (heard within rc15fSilent), Iran, a non-call, the key off: nothing.
func TestRC15FJoinResend(t *testing.T) {
	cs223Keys(t)
	old := rc15fJoinRes.Load()
	t.Cleanup(func() { rc15fJoinRes.Store(old) })
	rc15fJoinRes.Store(true)
	now := time.Now()
	mk := func(iran bool) (*mConn, *msub, *msub, []*mchunk) {
		m := &mConn{lite: true, iran: iran, callLikeAt: now, mode: planeLight}
		a := &msub{m: m, idx: 0}
		b := &msub{m: m, idx: 1}
		cs := []*mchunk{cs223Chunk(0, 1000), cs223Chunk(1000, 1000), cs223Chunk(2000, 1000)}
		a.sent = []sentRec{{c: cs[0], t: now}, {c: cs[1], t: now}}
		b.sent = []sentRec{{c: cs[1], t: now}, {c: cs[2], t: now}}
		dead := monoNano(now.Add(-time.Second))
		a.lastRecv.Store(dead)
		b.lastRecv.Store(dead)
		m.subs = []*msub{a, b}
		m.primary.Store(a)
		return m, a, b, cs
	}
	join := func(m *mConn) *msub {
		nw := &msub{m: m, idx: 2, missEnd: 1 << 40}
		nw.lastRecv.Store(monoNano(now))
		m.subs = append(m.subs, nw)
		m.rc15fJoinResLocked(nw, now)
		return nw
	}
	m, _, _, cs := mk(false)
	nw := join(m)
	got := map[uint64]bool{}
	for _, c := range nw.q {
		got[c.off] = true
	}
	if len(got) != 3 || !got[cs[0].off] || !got[cs[1].off] || !got[cs[2].off] {
		t.Fatalf("stranded chunks on the joining sub: %d distinct (q %d)", len(got), len(nw.q))
	}
	// a later join takes nothing already covered
	n0 := len(nw.q)
	nw2 := join(m)
	if len(nw2.q) != 0 || len(nw.q) != n0 {
		t.Fatalf("repeat: %d", len(nw2.q))
	}
	// one sub still heard: only the silent one resends
	m, a, _, _ := mk(false)
	a.lastRecv.Store(monoNano(now))
	nw = join(m)
	if len(nw.q) != 2 {
		t.Fatalf("live sub resent: q %d", len(nw.q))
	}
	m, _, _, _ = mk(true)
	if nw = join(m); len(nw.q) != 0 {
		t.Fatal("Iran must not act")
	}
	m, _, _, _ = mk(false)
	m.callLikeAt = time.Time{}
	if nw = join(m); len(nw.q) != 0 {
		t.Fatal("not a call")
	}
	m, _, _, _ = mk(false)
	rc15fJoinRes.Store(false)
	if nw = join(m); len(nw.q) != 0 {
		t.Fatal("key off")
	}
}

// rc15f through addSubLocked (fails without the hook: the joining sub
// carried only new bytes and the stranded ones waited for the CLOSE-REQ).
func TestRC15FJoinViaAddSub(t *testing.T) {
	cs223Keys(t)
	old := rc15fJoinRes.Load()
	t.Cleanup(func() { rc15fJoinRes.Store(old) })
	rc15fJoinRes.Store(true)
	now := time.Now()
	m := &mConn{lite: true, callLikeAt: now, mode: planeLight}
	m.sendCond.L = &m.mu
	m.sendOff = 1000 // the join marks missEnd = sendOff: it lacks [0, 1000)
	a := &msub{m: m, idx: 0}
	c0 := cs223Chunk(0, 1000)
	a.sent = []sentRec{{c: c0, t: now}}
	a.lastRecv.Store(monoNano(now.Add(-time.Second)))
	m.subs = []*msub{a}
	m.nsubs = 1
	m.primary.Store(a)
	m.mu.Lock()
	nw := m.addSubLocked(nil, nil, false)
	m.mu.Unlock()
	if len(nw.q) != 1 || nw.q[0] != c0 {
		t.Fatalf("joining sub q %d", len(nw.q))
	}
}
