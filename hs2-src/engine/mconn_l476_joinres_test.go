package engine

import (
	"testing"
	"time"
)

// l476: a fresh sub joining a kept non-call lite conn (either side) whose
// subs are dark while pending takes their stranded chunks at once (lab3
// l476 trace: the crowd request written just before the fault stayed on the
// two dead subs while b449's fresh sub carried only new bytes); a sub that is
// not silent, a bulk join, the key off: nothing.
func TestL476JoinResend(t *testing.T) {
	cs223Keys(t)
	o1, o2, o3 := l476JoinRes.Load(), mconnSubResend.Load(), keepStallOn.Load()
	t.Cleanup(func() { l476JoinRes.Store(o1); mconnSubResend.Store(o2); keepStallOn.Store(o3) })
	l476JoinRes.Store(true)
	mconnSubResend.Store(true)
	keepStallOn.Store(true)
	now := time.Now()
	var silent map[*msub]bool
	h := func(s *msub, _ int64) bool { return silent[s] }
	cs226SilentHook.Store(&h)
	t.Cleanup(func() { cs226SilentHook.Store(nil) })
	mk := func(iran bool) (*mConn, *msub, *msub, []*mchunk) {
		m := &mConn{lite: true, iran: iran, mode: planeLight, sendOff: 3000}
		a := &msub{m: m, idx: 0}
		b := &msub{m: m, idx: 1}
		cs := []*mchunk{cs223Chunk(0, 1000), cs223Chunk(1000, 1000), cs223Chunk(2000, 1000)}
		a.sent = []sentRec{{c: cs[0], t: now}, {c: cs[1], t: now}}
		b.sent = []sentRec{{c: cs[1], t: now}, {c: cs[2], t: now}}
		m.subs = []*msub{a, b}
		m.primary.Store(a)
		silent = map[*msub]bool{a: true, b: true}
		return m, a, b, cs
	}
	join := func(m *mConn) *msub {
		nw := &msub{m: m, idx: len(m.subs), missEnd: 1 << 40}
		nw.lastRecv.Store(monoNano(now))
		m.subs = append(m.subs, nw)
		m.l476JoinResLocked(nw, now)
		return nw
	}
	for _, iran := range []bool{true, false} {
		m, _, _, cs := mk(iran)
		if m.callKeepLocked() || !m.l476KeepLocked() {
			t.Fatal("want a sub-resend kept non-call conn")
		}
		nw := join(m)
		got := map[uint64]bool{}
		for _, c := range nw.q {
			got[c.off] = true
		}
		if len(got) != 3 || !got[cs[0].off] || !got[cs[1].off] || !got[cs[2].off] {
			t.Fatalf("iran=%v: stranded chunks on the joining sub: %d distinct (q %d)", iran, len(got), len(nw.q))
		}
		n0 := len(nw.q)
		if nw2 := join(m); len(nw2.q) != 0 || len(nw.q) != n0 {
			t.Fatalf("iran=%v repeat: %d", iran, len(nw2.q))
		}
	}
	// one sub not silent: only the silent one resends
	m, a, _, _ := mk(true)
	silent[a] = false
	if nw := join(m); len(nw.q) != 2 {
		t.Fatalf("live sub resent: q %d", len(nw.q))
	}
	// bulk join, key off: nothing
	m, _, _, _ = mk(true)
	nw := &msub{m: m, idx: 2, missEnd: 1 << 40}
	nw.bulk.Store(true)
	m.subs = append(m.subs, nw)
	m.l476JoinResLocked(nw, now)
	if len(nw.q) != 0 {
		t.Fatal("bulk join")
	}
	m, _, _, _ = mk(true)
	l476JoinRes.Store(false)
	if nw = join(m); len(nw.q) != 0 || m.l476KeepLocked() {
		t.Fatal("key off")
	}
	// key off: b270ResendLocked still refuses a non-call conn
	m, a, _, _ = mk(true)
	l476JoinRes.Store(true)
	nw = &msub{m: m, idx: 2, missEnd: 1 << 40}
	if n := m.b270ResendLocked(a, nw, false, now); n != 0 {
		t.Fatal("b270ResendLocked must keep its call gate")
	}
}

// HS2_L476_JOINPRIM: the joining sub becomes the primary only when every
// other live sub is dark (Iran cs226Silent; Kharej silent for rc15fSilent
// after a receive within b449DarkRecent).
func TestL476JoinPrimary(t *testing.T) {
	cs223Keys(t)
	o1, o2, o3, o4 := l476JoinRes.Load(), mconnSubResend.Load(), keepStallOn.Load(), l476JoinPrim.Load()
	t.Cleanup(func() { l476JoinRes.Store(o1); mconnSubResend.Store(o2); keepStallOn.Store(o3); l476JoinPrim.Store(o4) })
	l476JoinRes.Store(true)
	mconnSubResend.Store(true)
	keepStallOn.Store(true)
	l476JoinPrim.Store(true)
	now := time.Now()
	var silent map[*msub]bool
	h := func(s *msub, _ int64) bool { return silent[s] }
	cs226SilentHook.Store(&h)
	t.Cleanup(func() { cs226SilentHook.Store(nil) })
	run := func(iran bool, rxAgo [2]time.Duration, sil [2]bool) bool {
		m := &mConn{lite: true, iran: iran, mode: planeLight, sendOff: 1000}
		a, b := &msub{m: m, idx: 0}, &msub{m: m, idx: 1}
		a.lastRecv.Store(monoNano(now.Add(-rxAgo[0])))
		b.lastRecv.Store(monoNano(now.Add(-rxAgo[1])))
		m.subs = []*msub{a, b}
		m.setPrimaryLocked()
		silent = map[*msub]bool{a: sil[0], b: sil[1]}
		nw := &msub{m: m, idx: 2, missEnd: 1 << 40}
		nw.lastRecv.Store(monoNano(now))
		m.subs = append(m.subs, nw)
		m.l476JoinResLocked(nw, now)
		return m.primary.Load() == nw
	}
	s := time.Second
	if !run(true, [2]time.Duration{s, s}, [2]bool{true, true}) {
		t.Fatal("iran: both dark, want the joining sub primary")
	}
	if run(true, [2]time.Duration{s, s}, [2]bool{true, false}) {
		t.Fatal("iran: one sub live")
	}
	if !run(false, [2]time.Duration{s, s}, [2]bool{}) {
		t.Fatal("kharej: both silent 1 s, want the joining sub primary")
	}
	if run(false, [2]time.Duration{s, 100 * time.Millisecond}, [2]bool{}) {
		t.Fatal("kharej: one sub heard 100 ms ago")
	}
	if run(false, [2]time.Duration{time.Minute, time.Minute}, [2]bool{}) {
		t.Fatal("kharej: idle conn (nothing heard for a minute)")
	}
	l476JoinPrim.Store(false)
	if run(true, [2]time.Duration{s, s}, [2]bool{true, true}) {
		t.Fatal("key off")
	}
}
