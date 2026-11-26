package engine

import (
	"testing"
	"time"
)

func l480Keys(t *testing.T, stick, live bool) {
	t.Helper()
	o1, o2, o3 := l478FastOpen.Load(), l480Stick.Load(), l480LivePick.Load()
	t.Cleanup(func() { l478FastOpen.Store(o1); l480Stick.Store(o2); l480LivePick.Store(o3) })
	l478FastOpen.Store(true)
	l480Stick.Store(stick)
	l480LivePick.Store(live)
}

// l480 STICK: after l478's move, setPrimaryLocked with prefPrimary cleared
// keeps the live sub, and the duplicating send path queues nothing on the old
// dark sub; once the old link reads again the stick ends.
func TestL480StickHolds(t *testing.T) {
	silent, rd := l477Setup(t)
	for _, on := range []bool{false, true} {
		l480Keys(t, on, false)
		m, a, b := l477Conn(true)
		a.dcMtr = &linkMeter{}
		a.dcMtr.lastRd.Store(5)
		silent[a], rd[a] = time.Second, 7
		m.l477TailResLocked(nil, time.Now(), 0)
		if m.primary.Load() != b {
			t.Fatal("no l478 move")
		}
		m.prefPrimary = nil // another rule drops the preference
		m.setPrimaryLocked()
		if got := m.primary.Load(); on && got != b || !on && got != a {
			t.Fatalf("stick=%v: primary sub %d", on, got.idx)
		}
		aq := len(a.q)
		c := getChunkN(mRecData, mDataHdr+100)
		c.n = 100
		m.mu.Lock()
		err := m.sendLocked(c)
		m.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if on && len(a.q) != aq {
			t.Fatal("copy queued on the dark old primary")
		}
		if !on && len(a.q) == aq && m.primary.Load() == a {
			t.Fatal("key off: old sub got no copy")
		}
		if on {
			a.dcMtr.lastRd.Store(99) // its link reads again: the hold keeps the stick
			if m.l480SkipSubLocked() != a {
				t.Fatal("stick ended inside the hold")
			}
			oh := l480Hold
			l480Hold = 0
			if m.l480SkipSubLocked() != nil || m.l480Old != nil {
				t.Fatal("stick kept after the old link read and the hold")
			}
			l480Hold = oh
		}
	}
}

// l480 LIVEPICK: the mark is removed and alone changes nothing without the key.
func TestL480LiveMark(t *testing.T) {
	l480Keys(t, false, false)
	av := []any{1, 2}
	if got := l480WithLive(av); len(got) != 2 {
		t.Fatal("mark added with the key off")
	}
	l480LivePick.Store(true)
	got := l480WithLive(av)
	if len(got) != 3 || len(av) != 2 {
		t.Fatal("mark not added, or caller's slice changed")
	}
	out, ok := l480TakeLive(got)
	if !ok || len(out) != 2 || out[0] != 1 || out[1] != 2 {
		t.Fatalf("take: %v %v", out, ok)
	}
	if _, ok := l480TakeLive(av); ok {
		t.Fatal("mark found where none was")
	}
}

// l480: at a fresh sub's join, a sub silent past l478's bar (but under
// l477's 1 s) has its tail resent there with STICK on; not with it off.
func TestL480JoinEarly(t *testing.T) {
	l477Setup(t)
	h := func(s *msub, _ int64) bool { return s.idx == 0 }
	l478SilHook.Store(&h)
	t.Cleanup(func() { l478SilHook.Store(nil) })
	for _, on := range []bool{false, true} {
		l480Keys(t, on, false)
		m, a, b := l477Conn(true)
		a.dcMtr = &linkMeter{}
		a.dcMtr.lastRd.Store(1)
		a.dcMtr.pendSince.Store(2)
		m.l477TailResLocked(b, time.Now(), int64(700*time.Millisecond))
		if on != (len(b.q) > 0) {
			t.Fatalf("stick=%v: %d chunks resent on the joining sub", on, len(b.q))
		}
		if on && m.primary.Load() != b {
			t.Fatal("primary not moved to the joining sub")
		}
	}
}

// l480: a lite conn with 3 live subs (the fresh one joined) sends a chunk
// written after the move on the live new sub, not as a pair on the dark
// ones; key off: the pair path.
func TestL480NoPairOnDark(t *testing.T) {
	silent, rd := l477Setup(t)
	oc := mconnCall.Load()
	t.Cleanup(func() { mconnCall.Store(oc) })
	mconnCall.Store(true)
	ops := l480dPairSk.Load()
	t.Cleanup(func() { l480dPairSk.Store(ops) })
	for i, on := range []bool{false, true, true} {
		l480Keys(t, on, false)
		psk := i == 2 // the last pass: l480d PAIRSK
		l480dPairSk.Store(psk)
		m, a, b := l477Conn(true)
		h := &msub{m: m, idx: 2}
		m.subs = []*msub{a, h, b} // a: dark primary, h: dark hedge, b: live fresh sub
		a.dcMtr = &linkMeter{}
		a.dcMtr.lastRd.Store(5)
		silent[a], rd[a] = time.Second, 7
		m.l477TailResLocked(nil, time.Now(), 0)
		bq := len(b.q)
		p0, s0 := l480Stat.nopair.Load(), l480dStat.pairSk.Load()
		c := getChunkN(mRecData, mDataHdr+100)
		c.n = 100
		m.mu.Lock()
		err := m.sendLocked(c)
		m.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if on && !psk && (len(b.q) != bq+1 || l480Stat.nopair.Load() == p0) {
			t.Fatal("chunk not on the live sub, or the pair path ran")
		}
		if psk && (len(b.q) != bq+1 || l480dStat.pairSk.Load() == s0 || l480Stat.nopair.Load() != p0) {
			t.Fatal("PAIRSK: chunk not paired onto the live sub")
		}
		if psk && len(a.q) != 0 && a.q[len(a.q)-1] == c {
			t.Fatal("PAIRSK: chunk on the stuck old sub")
		}
	}
}

// l480b NEWPICK: a link silent while pending is silent; an idle one (no
// write unanswered) or a fresh read is not. The hedge mark adds the pool's
// silent links to the avoid list, unless no other link is left; key off:
// no mark.
func TestL480bNewPick(t *testing.T) {
	o := l480NewPick.Load()
	t.Cleanup(func() { l480NewPick.Store(o) })
	nowc := ctrlNow()
	sil := func(rdAgo, wrAgo, psAgo time.Duration) *linkMeter {
		m := &linkMeter{}
		m.lastRd.Store(nowc - int64(rdAgo))
		m.lastWr.Store(nowc - int64(wrAgo))
		m.pendSince.Store(nowc - int64(psAgo))
		return m
	}
	dead := sil(time.Second, 100*time.Millisecond, 900*time.Millisecond)
	idle := sil(time.Second, 2*time.Second, time.Second)      // last write answered
	fresh := sil(50*time.Millisecond, 10*time.Millisecond, 0) // pending but read just now
	if !l480LinkSilent(dead, nowc) || l480LinkSilent(idle, nowc) || l480LinkSilent(fresh, nowc) || l480LinkSilent(nil, nowc) {
		t.Fatal("silence verdicts")
	}
	l480NewPick.Store(false)
	if got := l480WithPend([]any{1}); len(got) != 1 {
		t.Fatal("mark added with the key off")
	}
	l480NewPick.Store(true)
	lm := NewLinkManager(nil, 4, 4, 50, nil)
	ls := []*fakeLink{{alive: true}, {alive: true}, {alive: true}}
	for i, mt := range []*linkMeter{dead, idle, fresh} {
		lm.links = append(lm.links, &managedLink{link: ls[i], id: i, mtr: mt})
	}
	av := l480WithPend([]any{avoidKey(ls[2])})
	got := lm.l480PendAvoid(av)
	if len(got) != 2 || got[0] != avoidKey(ls[2]) || got[1] != avoidKey(ls[0]) {
		t.Fatalf("avoid %v", got)
	}
	// only the silent link left outside avoid: unchanged (no mark)
	got = lm.l480PendAvoid(l480WithPend([]any{avoidKey(ls[1]), avoidKey(ls[2])}))
	if len(got) != 2 {
		t.Fatalf("avoid with no other link %v", got)
	}
	if got := lm.l480PendAvoid([]any{7}); len(got) != 1 {
		t.Fatal("no mark: changed")
	}
}
