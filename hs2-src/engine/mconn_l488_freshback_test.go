package engine

import (
	"net"
	"testing"
	"time"
)

// l488 FRESHBACK: a read after a pending silence of l478SilMin marks the link
// recovering; it stays so while TCP_INFO shows backoff (or ca_state Loss),
// clears at backoff 0, expires at l488RecMax (l488RecNoTCP without a
// socket); a short gap or an idle link is never marked; the hedge avoid list
// and the silent-link set take recovering links, unless nothing else is
// left; key off: nothing changes.
func TestL488FreshBack(t *testing.T) {
	o, on, oh := l488FreshBack.Load(), l480NewPick.Load(), l488TCPInfoBackoff
	t.Cleanup(func() { l488FreshBack.Store(o); l480NewPick.Store(on); l488TCPInfoBackoff = oh })
	l480NewPick.Store(true)
	nowc := ctrlNow()
	mk := func(rdAgo, wrAgo, psAgo time.Duration) *linkMeter {
		m := &linkMeter{}
		m.lastRd.Store(nowc - int64(rdAgo))
		m.lastWr.Store(nowc - int64(wrAgo))
		m.pendSince.Store(nowc - int64(psAgo))
		return m
	}
	read := func(m *linkMeter) { l488OnRead(m, nowc); m.lastRd.Store(nowc) }

	// key off: never marked
	l488FreshBack.Store(false)
	k := mk(2*time.Second, time.Second, time.Second)
	read(k)
	if k.l488Rec.Load() != 0 || l488Recovering(k, nowc) {
		t.Fatal("marked with the key off")
	}
	l488FreshBack.Store(true)
	back := uint8(3)
	ca := uint8(4)
	l488TCPInfoBackoff = func(*net.TCPConn) (uint8, uint8, bool) { return back, ca, true }

	rec := mk(2*time.Second, time.Second, 1500*time.Millisecond) // pending 1.5 s, then a read
	short := mk(300*time.Millisecond, 100*time.Millisecond, 200*time.Millisecond)
	idle := mk(2*time.Second, 3*time.Second, 3*time.Second) // nothing unanswered
	for _, m := range []*linkMeter{rec, short, idle} {
		read(m)
	}
	if !l488Recovering(rec, nowc) || l488Recovering(short, nowc) || l488Recovering(idle, nowc) || l488Recovering(nil, nowc) {
		t.Fatal("recovery verdicts")
	}
	// it reads now, so l480's silence test alone lets it through; l488Avoid does not
	if l480LinkSilent(rec, nowc) || !l488Avoid(rec, nowc) || l488Avoid(short, nowc) {
		t.Fatal("avoid verdicts")
	}

	// the hedge's avoid list takes it; with no other link it is unchanged
	lm := NewLinkManager(nil, 4, 4, 50, nil)
	ls := []*fakeLink{{alive: true}, {alive: true}}
	lm.links = append(lm.links, &managedLink{link: ls[0], id: 0, mtr: rec}, &managedLink{link: ls[1], id: 1, mtr: short})
	got := lm.l480PendAvoid(l480WithPend(nil))
	if len(got) != 1 || got[0] != avoidKey(ls[0]) {
		t.Fatalf("hedge avoid %v", got)
	}
	if got := lm.l480PendAvoid(l480WithPend([]any{avoidKey(ls[1])})); len(got) != 1 {
		t.Fatalf("no other link: %v", got)
	}
	if s := lm.l480SilentLinks(nil); len(s) != 1 || s[0] != Link(ls[0]) {
		t.Fatalf("silent links %v", s)
	}

	// backoff 0 but still in Loss: recovering; backoff 0 and Open: cleared
	back = 0
	if !l488Recovering(rec, nowc) {
		t.Fatal("cleared while in Loss")
	}
	ca = 0
	if l488Recovering(rec, nowc) || rec.l488Rec.Load() != 0 {
		t.Fatal("not cleared at backoff 0")
	}

	// expiry: still in backoff, but past l488RecMax
	back, ca = 2, 0
	rec2 := mk(2*time.Second, time.Second, time.Second)
	read(rec2)
	if !l488Recovering(rec2, nowc) || l488Recovering(rec2, nowc+int64(l488RecMax)) {
		t.Fatal("expiry")
	}
	// no socket to ask: time only (l488RecNoTCP)
	l488TCPInfoBackoff = nil
	rec3 := mk(2*time.Second, time.Second, time.Second)
	read(rec3)
	if !l488Recovering(rec3, nowc) || l488Recovering(rec3, nowc+int64(l488RecNoTCP)) {
		t.Fatal("no-socket recovery")
	}
}
