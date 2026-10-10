package engine

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// A new stream's answer is given twice the link's bare round trip (rttMin),
// at least lagAnswerFloor, never more than before (quietWait); the quiet test
// keeps the longer wait whatever the bare round trip.
func TestOpenAnswerWaitFollowsBareRoundTrip(t *testing.T) {
	ms := func(d time.Duration) uint64 { return uint64(d / time.Microsecond) }
	cases := []struct {
		name        string
		last, bare  time.Duration // rttMicros, rttMin (0: not known yet)
		want, quiet time.Duration
	}{
		{"no round trip yet", 0, 0, lagOpenWait, lagOpenWait},
		{"near path", 50 * time.Millisecond, 50 * time.Millisecond, lagAnswerFloor, lagOpenWait},
		{"near path behind its own queue", 400 * time.Millisecond, 50 * time.Millisecond, lagAnswerFloor, lagOpenWait},
		{"far path", 300 * time.Millisecond, 300 * time.Millisecond, 600 * time.Millisecond, lagOpenWait},
		{"farther path", 800 * time.Millisecond, 700 * time.Millisecond, 1400 * time.Millisecond, 1600 * time.Millisecond},
		{"very far path", 1500 * time.Millisecond, 1500 * time.Millisecond, lagWait, lagWait},
	}
	for _, c := range cases {
		mtr := &linkMeter{}
		mtr.rttMicros.Store(ms(c.last))
		mtr.rttMin.Store(ms(c.bare))
		if got := time.Duration(openAnswerWait(mtr)); got != c.want {
			t.Errorf("%s: openAnswerWait = %s, want %s", c.name, got, c.want)
		}
		if got := time.Duration(quietWait(mtr)); got != c.quiet {
			t.Errorf("%s: quietWait = %s, want %s", c.name, got, c.quiet)
		}
	}

	// On a near path a new stream left unanswered past the floor makes the
	// link lag; within it, not yet.
	now := ctrlNow()
	for _, c := range []struct {
		waited time.Duration
		want   bool
	}{{lagAnswerFloor - 50*time.Millisecond, false}, {lagAnswerFloor + 50*time.Millisecond, true}} {
		mtr := &linkMeter{}
		mtr.rttMicros.Store(ms(50 * time.Millisecond))
		mtr.rttMin.Store(ms(50 * time.Millisecond))
		mtr.rxAt.Store(now - int64(time.Second))
		mtr.openWait.Store(now - int64(c.waited))
		ml := &managedLink{link: &fakeLink{alive: true}, mtr: mtr}
		if got := ml.lagging(now); got != c.want {
			t.Errorf("near path, open unanswered for %s: lagging = %v, want %v", c.waited, got, c.want)
		}
	}
}

func TestMinNonZero(t *testing.T) {
	for _, c := range []struct {
		v    []uint64
		want uint64
	}{{nil, 0}, {[]uint64{0, 0}, 0}, {[]uint64{0, 7, 3, 0, 9}, 3}, {[]uint64{5}, 5}} {
		if got := minNonZero(c.v); got != c.want {
			t.Errorf("minNonZero(%v) = %d, want %d", c.v, got, c.want)
		}
	}
}

// The control loop keeps the shortest of the last round trips: one pong that
// waited behind a queue raises the last round trip, not the bare one.
func TestControlTracksBareRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time cadence")
	}
	a, b := net.Pipe()
	mtr := &linkMeter{statsPoll: make(chan struct{}, 1)}
	cli, _, err := newSession(a, false, nil, mtr)
	if err != nil {
		t.Fatal(err)
	}
	srv, _, err := newSession(b, true, nil, &linkMeter{})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	defer srv.Close()
	var slow atomic.Bool
	go func() { // the exit: answers every ping, late while slow
		st, err := srv.AcceptStream()
		if err != nil {
			return
		}
		var k [1]byte
		io.ReadFull(st, k[:])
		ping, pong := make([]byte, ctrlPingLen), make([]byte, ctrlPongLen)
		for {
			if _, err := io.ReadFull(st, ping); err != nil {
				return
			}
			if slow.Load() {
				time.Sleep(300 * time.Millisecond)
			}
			copy(pong, ping)
			st.Write(pong)
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { // busy: a ping every controlInterval
		for ctx.Err() == nil {
			mtr.rdBytes.Add(activeBytes)
			time.Sleep(100 * time.Millisecond)
		}
	}()
	go openControl(ctx, &mtcpLink{sess: cli, mtr: mtr}, nil)
	within(t, controlInterval+2*time.Second, "the first pong", func() bool { return mtr.rttMin.Load() != 0 })
	bare := mtr.rttMin.Load()
	if bare > uint64(100*time.Millisecond/time.Microsecond) {
		t.Fatalf("bare round trip on a pipe: %d µs", bare)
	}
	slow.Store(true)
	within(t, 2*controlInterval+2*time.Second, "a late pong", func() bool {
		return mtr.rttMicros.Load() >= uint64(300*time.Millisecond/time.Microsecond)
	})
	if got := mtr.rttMin.Load(); got > bare {
		t.Fatalf("one late pong raised the bare round trip: %d µs, was %d µs", got, bare)
	}
}

// A socket write that had to wait for the network marks when it went
// through (linkMeter.sentAt).
func TestSentAtMarksWritesThatWaited(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	m := &linkMeter{}
	c := &meteredConn{Conn: a, m: m}
	go func() {
		time.Sleep(20 * time.Millisecond) // the network takes it a while later
		io.ReadFull(b, make([]byte, 4))
	}()
	before := ctrlNow()
	if _, err := c.Write([]byte("abcd")); err != nil {
		t.Fatal(err)
	}
	if s := m.sentAt.Load(); s < before+int64(10*time.Millisecond) || s > ctrlNow() {
		t.Fatalf("sentAt %d, want the write's end (%d..%d)", s, before, ctrlNow())
	}
}

// A link busy with an upload of its own reads next to nothing, and a new
// stream there waits behind the upload — but its writes go through: it is
// slow, not gone, and a hello does not leave it. Once its writes stop going
// through too, it is gone.
func TestRescueLeavesLinkWithUpload(t *testing.T) {
	withRescueTimes(t, 50*time.Millisecond, 10*time.Millisecond)
	{
		m, ls := rsPool(t, 2)
		m.links[1].flowing = 10
		m.links[0].mtr.rxAt.Store(ctrlNow() - int64(3*time.Second)) // silent, not yet for lagQuiet
		stop := make(chan struct{})
		go func() { // the upload's writes go through, one after another
			for {
				m.links[0].mtr.sentAt.Store(ctrlNow())
				select {
				case <-stop:
					return
				case <-time.After(20 * time.Millisecond):
				}
			}
		}()
		app, edge := tcpPair(t)
		first, _, ended := startRelay(t, m, edge)
		app.Write(clientHello(t))
		lag(m, 0)
		time.Sleep(400 * time.Millisecond)
		if mv := m.rescue.moves.Load(); mv != 0 || len(ls[1].opened()) != 0 || first.closed() {
			t.Fatalf("moves %d, opens on link 1 %d, closed %v", mv, len(ls[1].opened()), first.closed())
		}
		close(stop)
		waitUntil(t, "the move", 3*time.Second, func() bool { return m.rescue.moves.Load() == 1 })
		app.Close()
		<-ended
	}
}
