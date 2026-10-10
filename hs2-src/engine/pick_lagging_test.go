package engine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Each live signal makes a link lagging, and an answer clears the open wait.
func TestLaggingSignals(t *testing.T) {
	now := ctrlNow()
	ago := func(d time.Duration) int64 { return now - int64(d) }
	cases := []struct {
		name string
		set  func(ml *managedLink, mtr *linkMeter)
		want bool
	}{
		{"quiet link that answers", func(ml *managedLink, mtr *linkMeter) { mtr.rxAt.Store(ago(time.Second)) }, false},
		{"ping unanswered", func(ml *managedLink, mtr *linkMeter) { mtr.ctrlWait.Store(ago(lagWait + time.Second)) }, true},
		{"ping just sent", func(ml *managedLink, mtr *linkMeter) { mtr.ctrlWait.Store(ago(lagWait / 2)) }, false},
		{"writer stuck", func(ml *managedLink, mtr *linkMeter) { mtr.wrStart.Store(ago(lagWait + time.Second)) }, true},
		{"writer busy", func(ml *managedLink, mtr *linkMeter) { mtr.wrStart.Store(ago(lagWait / 2)) }, false},
		{"nothing heard", func(ml *managedLink, mtr *linkMeter) { mtr.rxAt.Store(ago(lagQuiet + time.Second)) }, true},
		{"open unanswered", func(ml *managedLink, mtr *linkMeter) {
			mtr.rxAt.Store(ago(3 * time.Second))
			mtr.openWait.Store(ago(lagOpenWait + time.Second))
		}, true},
		{"open answered", func(ml *managedLink, mtr *linkMeter) {
			mtr.openWait.Store(ago(lagOpenWait + time.Second))
			mtr.rxAt.Store(ago(lagOpenWait))
		}, false},
		{"far path, open within twice its round trip", func(ml *managedLink, mtr *linkMeter) {
			mtr.rttMicros.Store(uint64(1200 * time.Millisecond / time.Microsecond))
			mtr.rxAt.Store(ago(3 * time.Second))
			mtr.openWait.Store(ago(1500 * time.Millisecond))
		}, false},
		{"far path, open past lagWait", func(ml *managedLink, mtr *linkMeter) {
			mtr.rttMicros.Store(uint64(1200 * time.Millisecond / time.Microsecond))
			mtr.rxAt.Store(ago(3 * time.Second))
			mtr.openWait.Store(ago(lagWait + 100*time.Millisecond))
		}, true},
		{"slow open noted", func(ml *managedLink, mtr *linkMeter) { ml.openSlowTill = now + int64(time.Second) }, true},
		{"slow open over", func(ml *managedLink, mtr *linkMeter) { ml.openSlowTill = now - 1 }, false},
	}
	for _, c := range cases {
		mtr := &linkMeter{}
		ml := &managedLink{link: &fakeLink{alive: true}, mtr: mtr}
		c.set(ml, mtr)
		if got := ml.lagging(now); got != c.want {
			t.Errorf("%s: lagging = %v, want %v", c.name, got, c.want)
		}
	}
	// An answered open wait is cleared, so it cannot condemn the link later.
	mtr := &linkMeter{}
	ml := &managedLink{link: &fakeLink{alive: true}, mtr: mtr}
	mtr.openWait.Store(ago(lagOpenWait + time.Second))
	mtr.rxAt.Store(ago(lagOpenWait))
	ml.lagging(now)
	if w := mtr.openWait.Load(); w != 0 {
		t.Fatalf("answered open wait kept: %d", w)
	}
}

// A lagging link takes no new users while most serving links do not lag,
// however light it looks; when most of them lag (the path, not the link) the
// usual order applies again, so the new users spread instead of piling onto
// the few that happen not to lag.
func TestPickPassesOverLaggingLink(t *testing.T) {
	slow := func(ml *managedLink) { ml.openSlowTill = ctrlNow() + int64(time.Minute) }
	busy := func(ml *managedLink) { ml.flowing = 30; ml.users.Store(30) }
	m, ls := pickPool(slow, busy, busy)
	for i := 0; i < 20; i++ {
		l, rel, ok := m.Pick()
		if !ok || l == ls[0].link {
			t.Fatalf("pick %d: got the lagging link (ok %v)", i, ok)
		}
		rel()
		for _, ml := range ls {
			ml.picks, ml.pickHist = 0, [pickWindow - 1]int{}
		}
	}

	m, ls = pickPool(slow, func(ml *managedLink) { slow(ml); busy(ml) }, busy)
	got := map[Link]int{}
	for i := 0; i < 20; i++ {
		l, rel, ok := m.Pick()
		if !ok {
			t.Fatal("no link")
		}
		got[l]++
		rel()
		for _, ml := range ls {
			ml.picks, ml.pickHist = 0, [pickWindow - 1]int{}
		}
	}
	if got[ls[0].link] != 20 {
		t.Fatalf("most links lag: want the lightest link every time, got %v", got)
	}
}

// Links already tried for this connection are never picked again, and a pick
// that excludes every link finds none.
func TestPickExceptSkipsTried(t *testing.T) {
	m, ls := pickPool(nil, nil)
	l, rel, ok := m.pickExcept([]Link{ls[0].link}, false)
	if !ok || l != ls[1].link {
		t.Fatalf("got %v (ok %v), want the untried link", l, ok)
	}
	rel()
	if _, _, ok := m.pickExcept([]Link{ls[0].link, ls[1].link}, false); ok {
		t.Fatal("picked a link the connection was tried on")
	}
}

// openStubStream records its header and whether it was closed.
type openStubStream struct {
	mu      sync.Mutex
	hdr     []byte
	closed  atomic.Bool
	writeGo chan struct{} // nil: writes return at once
}

func (s *openStubStream) Read([]byte) (int, error) { select {} }
func (s *openStubStream) Write(p []byte) (int, error) {
	if s.writeGo != nil {
		<-s.writeGo
	}
	s.mu.Lock()
	s.hdr = append(s.hdr, p...)
	s.mu.Unlock()
	return len(p), nil
}
func (s *openStubStream) Close() error { s.closed.Store(true); return nil }

// openStubLink opens openStubStreams; while gate is non-nil OpenStream waits on
// it (a SYN queued behind a writer that does not move).
type openStubLink struct {
	gate   chan struct{}
	stream *openStubStream
	opens  atomic.Int32
}

func (l *openStubLink) OpenStream() (stream, error) {
	l.opens.Add(1)
	if l.gate != nil {
		<-l.gate
	}
	return l.stream, nil
}
func (l *openStubLink) Active() int32 { return 0 }
func (l *openStubLink) Alive() bool   { return true }
func (l *openStubLink) Close() error  { return nil }

// slowNoted reports whether link i is noted slow (noteOpenSlow), read under
// the pool lock like the picker reads it.
func slowNoted(m *LinkManager, i int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.links[i].openSlowTill > ctrlNow()
}

func withOpenTimeout(t *testing.T, d time.Duration) {
	old := openTimeout
	openTimeout = d
	t.Cleanup(func() { openTimeout = old })
}

// A new connection whose stream cannot even open on its link (the SYN waits
// behind a stuck writer) is opened on another link after openTimeout; the
// stuck link turns lagging, its late stream is closed, and both links' slots
// come back right.
func TestOpenStreamMovesOffStuckLink(t *testing.T) {
	withOpenTimeout(t, 100*time.Millisecond)
	stuck := &openStubLink{gate: make(chan struct{}), stream: &openStubStream{}}
	good := &openStubLink{stream: &openStubStream{}}
	m := NewLinkManager(nil, 1, 32, 8, nil)
	m.links = []*managedLink{
		{link: stuck, id: 0},
		{link: good, id: 1, flowing: 10}, // heavier: the first pick goes to stuck
	}
	start := time.Now()
	st, release, ok := openStream(context.Background(), m, false, 0)
	took := time.Since(start)
	if !ok || st != good.stream {
		t.Fatalf("got %v (ok %v), want the good link's stream", st, ok)
	}
	if took < 100*time.Millisecond || took > time.Second {
		t.Fatalf("open took %s, want about openTimeout", took)
	}
	if !slowNoted(m, 0) {
		t.Fatal("the stuck link was not noted as slow")
	}
	if n := m.links[0].users.Load(); n != 0 {
		t.Fatalf("stuck link keeps %d users, want 0", n)
	}
	close(stuck.gate) // its SYN goes out at last
	deadline := time.Now().Add(2 * time.Second)
	for !stuck.stream.closed.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !stuck.stream.closed.Load() {
		t.Fatal("the late stream on the stuck link was not closed")
	}
	if len(stuck.stream.hdr) != 0 {
		t.Fatal("the late stream sent a header: the exit would dial the panel for nothing")
	}
	release()
	if n := m.links[1].users.Load(); n != 0 || m.users.Load() != 0 {
		t.Fatalf("users not released: link %d pool %d", n, m.users.Load())
	}
}

// Once a stream is open (its SYN went out), a header that waits is not tried
// on another link too — the exit would dial the panel for each copy — but the
// link is still noted slow for the connections after it.
func TestOpenStreamWaitsForHeaderWithoutDoubling(t *testing.T) {
	withOpenTimeout(t, 100*time.Millisecond)
	slowHdr := &openStubLink{stream: &openStubStream{writeGo: make(chan struct{})}}
	other := &openStubLink{stream: &openStubStream{}}
	m := NewLinkManager(nil, 1, 32, 8, nil)
	m.links = []*managedLink{
		{link: slowHdr, id: 0},
		{link: other, id: 1, flowing: 10},
	}
	done := make(chan struct{})
	var st stream
	var ok bool
	go func() {
		st, _, ok = openStream(context.Background(), m, false, 0)
		close(done)
	}()
	time.Sleep(350 * time.Millisecond)
	if n := other.opens.Load(); n != 0 {
		t.Fatalf("a waiting header was doubled on another link (%d opens)", n)
	}
	if !slowNoted(m, 0) {
		t.Fatal("the link keeping the header waiting was not noted as slow")
	}
	close(slowHdr.stream.writeGo)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("open did not finish once the header went out")
	}
	if !ok || st != slowHdr.stream {
		t.Fatalf("got %v (ok %v), want the stream that was waiting", st, ok)
	}
}

// A lone serving link that lags is no sign the path is slow: a healthy
// retiring link takes the new user instead.
func TestPickLoneLaggingServingYieldsToRetiring(t *testing.T) {
	m, ls := pickPool(
		func(ml *managedLink) { ml.openSlowTill = ctrlNow() + int64(time.Minute) },
		func(ml *managedLink) { ml.retiring = true },
	)
	l, rel, ok := m.Pick()
	if !ok || l != ls[1].link {
		t.Fatalf("got %v (ok %v), want the healthy retiring link", l, ok)
	}
	rel()
}

// During the refill hold the links that do not lag may all be full (the hold's
// cap); the lagging one must not take the connection then — the hold queues it.
func TestPickHoldSkipsLaggingLinkWhenOthersFull(t *testing.T) {
	slow := func(ml *managedLink) { ml.openSlowTill = ctrlNow() + int64(time.Minute) }
	full := func(ml *managedLink) { ml.users.Store(2) }
	m, ls := pickPool(full, full, slow)
	m.users.Store(4)
	m.perLink = 2
	m.target.Store(3)
	m.hold.on = true
	if l, rel, ok, _ := m.pickHeld(); ok {
		rel()
		if l == ls[2].link {
			t.Fatal("the refill hold placed the connection on the lagging link")
		}
	}
	if l, rel, ok := m.pickExcept(nil, true); ok {
		rel()
		if l == ls[2].link {
			t.Fatal("a held try picked the lagging link")
		}
	}
}

// failOpenLink fails every open at once (a link that died).
type failOpenLink struct{ opens atomic.Int32 }

func (l *failOpenLink) OpenStream() (stream, error) {
	l.opens.Add(1)
	return nil, errors.New("link died")
}
func (l *failOpenLink) Active() int32 { return 0 }
func (l *failOpenLink) Alive() bool   { return true }
func (l *failOpenLink) Close() error  { return nil }

// When the try on a second link fails while the first still waits for its SYN,
// the third link is tried at once, not a whole openTimeout later.
func TestOpenStreamNextLinkRightAfterFailure(t *testing.T) {
	withOpenTimeout(t, 200*time.Millisecond)
	stuck := &openStubLink{gate: make(chan struct{}), stream: &openStubStream{}}
	defer close(stuck.gate)
	dead := &failOpenLink{}
	good := &openStubLink{stream: &openStubStream{}}
	m := NewLinkManager(nil, 1, 32, 8, nil)
	m.links = []*managedLink{
		{link: stuck, id: 0},
		{link: dead, id: 1, flowing: 5},
		{link: good, id: 2, flowing: 10},
	}
	start := time.Now()
	st, release, ok := openStream(context.Background(), m, false, 0)
	took := time.Since(start)
	if !ok || st != good.stream {
		t.Fatalf("got %v (ok %v), want the good link's stream", st, ok)
	}
	release()
	if took > 300*time.Millisecond {
		t.Fatalf("open took %s with openTimeout 200ms: the third link waited a timeout after the second failed", took)
	}
}

// Once any try's stream is open its header is on the way: no further link is
// tried, even when the newest try is the one still waiting for its SYN.
func TestOpenStreamNoMoreTriesOnceOneOpened(t *testing.T) {
	withOpenTimeout(t, 100*time.Millisecond)
	a := &openStubLink{gate: make(chan struct{}), stream: &openStubStream{writeGo: make(chan struct{})}}
	b := &openStubLink{gate: make(chan struct{}), stream: &openStubStream{}}
	c := &openStubLink{stream: &openStubStream{}}
	m := NewLinkManager(nil, 1, 32, 8, nil)
	m.links = []*managedLink{{link: a, id: 0}, {link: b, id: 1, flowing: 5}, {link: c, id: 2, flowing: 10}}
	go func() {
		time.Sleep(140 * time.Millisecond) // A's SYN goes out after B was tried
		close(a.gate)
		time.Sleep(260 * time.Millisecond) // past B's own timeout
		close(a.stream.writeGo)
	}()
	st, release, ok := openStream(context.Background(), m, false, 0)
	if !ok || st != a.stream {
		t.Fatalf("got %v (ok %v), want the stream whose header was on its way", st, ok)
	}
	release()
	if n := c.opens.Load(); n != 0 {
		t.Fatalf("a third link was tried although a stream was open (%d opens)", n)
	}
	close(b.gate) // B's late stream is closed, never used
	deadline := time.Now().Add(2 * time.Second)
	for !b.stream.closed.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !b.stream.closed.Load() || len(b.stream.hdr) != 0 {
		t.Fatalf("B's late stream: closed %v, header %d bytes", b.stream.closed.Load(), len(b.stream.hdr))
	}
}
