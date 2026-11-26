package engine

import (
	"testing"
	"time"
)

// TestL480dCallPend: the call opens' avoid gets the pending-silence mark once,
// none with the key off, and pickPlaneCallSkip's l480PendAvoid then leaves
// out the silent link (and only it).
func TestL480dCallPend(t *testing.T) {
	o := l480dCallPendOn.Load()
	t.Cleanup(func() { l480dCallPendOn.Store(o) })
	l480dCallPendOn.Store(false)
	if got := l480dCallPend([]any{1}); len(got) != 1 {
		t.Fatal("mark added with the key off")
	}
	l480dCallPendOn.Store(true)
	got := l480dCallPend([]any{1})
	if len(got) != 2 {
		t.Fatalf("no mark: %v", got)
	}
	if again := l480dCallPend(got); len(again) != 2 {
		t.Fatalf("mark added twice: %v", again)
	}
	nowc := ctrlNow()
	mk := func(rdAgo, wrAgo, psAgo time.Duration) *linkMeter {
		m := &linkMeter{}
		m.lastRd.Store(nowc - int64(rdAgo))
		m.lastWr.Store(nowc - int64(wrAgo))
		m.pendSince.Store(nowc - int64(psAgo))
		return m
	}
	lm := NewLinkManager(nil, 4, 4, 50, nil)
	ls := []*fakeLink{{alive: true}, {alive: true}, {alive: true}}
	for i, mt := range []*linkMeter{mk(time.Second, 100*time.Millisecond, 900*time.Millisecond), mk(time.Second, 2*time.Second, time.Second), mk(50*time.Millisecond, 10*time.Millisecond, 0)} {
		lm.links = append(lm.links, &managedLink{link: ls[i], id: i, mtr: mt})
	}
	av := lm.l480PendAvoid(l480dCallPend([]any{avoidKey(ls[2])}))
	if len(av) != 2 || av[0] != avoidKey(ls[2]) || av[1] != avoidKey(ls[0]) {
		t.Fatalf("avoid %v", av)
	}
	// y2D panic: rrStart's avoid holds a rescueJoinMark (a map inside)
	jm := rescueJoinMark{paths: map[string]bool{"a>b": true}}
	av = lm.l480PendAvoid(l480dCallPend([]any{jm, avoidKey(ls[2])}))
	if len(av) != 3 || av[2] != avoidKey(ls[0]) {
		t.Fatalf("avoid with a join mark %v", av)
	}
	if got := lm.l480LiveAvoid(l480WithLive([]any{jm})); len(got) < 1 {
		t.Fatalf("live avoid with a join mark %v", got)
	}
}

// TestL480dCallLive: the call opens get LIVEPICK's mark once (none with the
// key off), and l480LiveAvoid then leaves out an idle link that read nothing
// within l480LiveFor (a blackholed link with nothing pending).
func TestL480dCallLive(t *testing.T) {
	o, ol, ok485 := l480dCallLiveOn.Load(), l480LivePick.Load(), l485CallOpen.Load()
	t.Cleanup(func() { l480dCallLiveOn.Store(o); l480LivePick.Store(ol); l485CallOpen.Store(ok485) })
	l485CallOpen.Store(false) // l485 CALLOPEN marks every call open whatever this key says
	l480LivePick.Store(true)
	l480dCallLiveOn.Store(false)
	if got := l480dCallLive([]any{1}); len(got) != 1 {
		t.Fatal("mark added with the key off")
	}
	l480dCallLiveOn.Store(true)
	got := l480dCallLive([]any{1})
	if len(got) != 2 || len(l480dCallLive(got)) != 2 {
		t.Fatalf("mark: %v", got)
	}
	nowc := ctrlNow()
	mk := func(rdAgo time.Duration) *linkMeter {
		m := &linkMeter{}
		m.lastRd.Store(nowc - int64(rdAgo))
		m.lastWr.Store(nowc - int64(rdAgo) - 1)
		return m
	}
	lm := NewLinkManager(nil, 4, 4, 50, nil)
	ls := []*fakeLink{{alive: true}, {alive: true}}
	for i, mt := range []*linkMeter{mk(3 * time.Second), mk(10 * time.Millisecond)} {
		lm.links = append(lm.links, &managedLink{link: ls[i], id: i, mtr: mt})
	}
	jm := rescueJoinMark{paths: map[string]bool{"a>b": true}}
	av := lm.l480LiveAvoid(l480dCallLive([]any{jm}))
	if len(av) != 2 || av[1] != avoidKey(ls[0]) {
		t.Fatalf("avoid %v", av)
	}
}
