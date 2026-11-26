package engine

import (
	"testing"
	"time"
)

// l480cMLink: a fake link with a meter (l480LinkSilent reads it).
type l480cMLink struct {
	fakeLink
	m *linkMeter
}

func (l *l480cMLink) meter() *linkMeter         { return l.m }
func (l *l480cMLink) tcpStats() (tcpStat, bool) { return tcpStat{}, false }

// TestL480cNewPickAll: the first sub on a silent link moves in one pick that
// leaves out every silent link (l480b's 3 tries could keep landing on other
// silent links); with only silent links left the first pick is kept.
func TestL480cNewPickAll(t *testing.T) {
	o := l480NewPick.Load()
	t.Cleanup(func() { l480NewPick.Store(o) })
	l480NewPick.Store(true)
	nowc := ctrlNow()
	met := func(rdAgo, wrAgo, psAgo time.Duration) *linkMeter {
		m := &linkMeter{}
		m.lastRd.Store(nowc - int64(rdAgo))
		m.lastWr.Store(nowc - int64(wrAgo))
		m.pendSince.Store(nowc - int64(psAgo))
		return m
	}
	silent := func() *linkMeter { return met(time.Second, 100*time.Millisecond, 900*time.Millisecond) }
	lm := NewLinkManager(nil, 8, 8, 50, nil)
	var ls []*l480cMLink
	add := func(m *linkMeter, users int32) {
		l := &l480cMLink{fakeLink{alive: true}, m}
		ml := &managedLink{link: l, id: len(ls), mtr: m}
		ml.users.Store(users)
		lm.links = append(lm.links, ml)
		ls = append(ls, l)
	}
	// four silent links, lightly loaded (the load pick prefers them), and one
	// idle link with more users
	for range 4 {
		add(silent(), 0)
	}
	add(met(time.Second, 2*time.Second, time.Second), 5)
	if got := lm.l480SilentLinks([]Link{ls[0]}); len(got) != 3 {
		t.Fatalf("silent links %d, want 3", len(got))
	}
	e := &mconnEdge{lm: lm}
	rel0 := 0
	l, _ := e.l480FirstPick(ls[0], func() { rel0++ })
	if l != Link(ls[4]) || rel0 != 1 {
		t.Fatalf("first pick moved to %v (rel %d), want the idle link", l, rel0)
	}
	// the idle link goes silent too: nothing else, the first pick is kept
	ls[4].m = silent()
	lm.links[4].mtr = ls[4].m
	l, _ = e.l480FirstPick(ls[1], func() { rel0++ })
	if l != Link(ls[1]) || rel0 != 1 {
		t.Fatalf("with only silent links: %v rel %d", l, rel0)
	}
	// key off: unchanged
	l480NewPick.Store(false)
	if l, _ = e.l480FirstPick(ls[2], func() {}); l != Link(ls[2]) {
		t.Fatal("key off moved the pick")
	}
}
