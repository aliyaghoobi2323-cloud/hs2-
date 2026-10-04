package engine

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// The control channel publishes how long its oldest unanswered ping has
// waited: it grows while the exit's answers are held up (they queue behind a
// stuck link's traffic), at most ctrlPending pings go out meanwhile, and once
// the answers arrive — late — the wait is gone and pinging goes on.
func TestControlWaitShowsUnansweredPing(t *testing.T) {
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
	release := make(chan struct{})
	var mu sync.Mutex
	got := 0
	go func() { // the exit: reads every ping, answers none until release
		st, err := srv.AcceptStream()
		if err != nil {
			return
		}
		var k [1]byte
		io.ReadFull(st, k[:])
		var held [][]byte
		pings := make(chan []byte)
		go func() {
			for {
				p := make([]byte, ctrlPingLen)
				if _, err := io.ReadFull(st, p); err != nil {
					close(pings)
					return
				}
				mu.Lock()
				got++
				mu.Unlock()
				pings <- p
			}
		}()
		answer := func(p []byte) {
			pong := make([]byte, ctrlPongLen)
			copy(pong, p)
			st.Write(pong)
		}
		released := false
		for {
			select {
			case p, ok := <-pings:
				if !ok {
					return
				}
				if released {
					answer(p)
				} else {
					held = append(held, p)
				}
			case <-release:
				release = nil
				released = true
				for _, p := range held {
					answer(p)
				}
				held = nil
			}
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { // busy: a ping on every tick
		for ctx.Err() == nil {
			mtr.rdBytes.Add(activeBytes)
			time.Sleep(100 * time.Millisecond)
		}
	}()
	done := make(chan struct{})
	go func() { openControl(ctx, &mtcpLink{sess: cli, mtr: mtr}, nil); close(done) }()

	time.Sleep(controlInterval + 5*time.Second) // first ping at ~3 s, unanswered
	if w := ctrlWaitOf(mtr); w < 4*time.Second {
		t.Fatalf("an unanswered ping 5 s old shows a wait of %s", w)
	}
	time.Sleep(2 * controlInterval) // more ticks, still unanswered
	mu.Lock()
	n := got
	mu.Unlock()
	if n > ctrlPending {
		t.Fatalf("%d pings sent while none was answered, want at most %d", n, ctrlPending)
	}
	close(release)
	within(t, 2*controlInterval+time.Second, "the late answers clear the wait", func() bool {
		return ctrlWaitOf(mtr) < 2*time.Second && mtr.peerSeen.Load()
	})
	mu.Lock()
	before := got
	mu.Unlock()
	time.Sleep(2*controlInterval + 500*time.Millisecond)
	mu.Lock()
	after := got
	mu.Unlock()
	if after <= before {
		t.Fatal("pinging stopped after the late answers")
	}
	cancel()
	<-done
	if ctrlWaitOf(mtr) != 0 {
		t.Fatal("a control channel that ended left a wait behind")
	}
}

// stuckRig is a pool of metered fake links on a fake clock: every link's
// control channel works (peerSeen), the healthy ones move a busy download
// each sample and answer at once.
type stuckRig struct {
	m    *LinkManager
	clk  *v2Clock
	lg   *v2Log
	good []*meteredFakeLink
	mls  []*managedLink
}

func newStuckRig(t *testing.T, good int) *stuckRig {
	t.Helper()
	m, clk, lg := newV2Manager(&fakeDialer{}, 1, 32, false)
	r := &stuckRig{m: m, clk: clk, lg: lg}
	for i := 0; i < good; i++ {
		f := newMeteredFake()
		f.m.peerSeen.Store(true)
		f.m.rttMicros.Store(120_000)
		r.good = append(r.good, f)
		ml := addManaged(m, f)
		ml.users.Store(5)
		r.mls = append(r.mls, ml)
	}
	return r
}

// add puts one more link in, with its control channel working.
func (r *stuckRig) add() (*meteredFakeLink, *managedLink) {
	f := newMeteredFake()
	f.m.peerSeen.Store(true)
	ml := addManaged(r.m, f)
	ml.users.Store(5)
	return f, ml
}

// waiting makes f's oldest control ping d old.
func waiting(f *meteredFakeLink, d time.Duration) { f.m.ctrlWait.Store(ctrlNow() - int64(d)) }

// step is one sample: the healthy links download and are answered, f moves
// `moved` bytes.
func (r *stuckRig) step(f *meteredFakeLink, moved uint64) {
	for _, g := range r.good {
		g.download(200<<10, 0)
		g.m.ctrlAnsweredSent.Store(ctrlNow() - int64(100*time.Millisecond)) // a ping sent 100 ms ago, answered
	}
	if f != nil {
		f.m.rdBytes.Add(moved)
	}
	r.clk.Advance(healthTick)
	r.m.sampleHealth()
}

// A link whose traffic has waited stuckWait for an answer while it moves a
// trickle is degraded as stuck after stuckStreak samples (not before), and
// the others are not.
func TestStuckLinkIsDegraded(t *testing.T) {
	r := newStuckRig(t, 3)
	f, ml := r.add()
	r.step(f, 4<<10) // seed
	waiting(f, stuckWait+2*time.Second)
	for i := 1; i < stuckStreak; i++ {
		r.step(f, 4<<10)
		if ml.degraded {
			t.Fatalf("degraded after %d sample(s), want %d", i, stuckStreak)
		}
	}
	r.step(f, 4<<10)
	if !ml.degraded || !ml.stuck {
		t.Fatalf("stuck link not degraded (streak %d):\n%s", ml.stuckStreak, r.lg)
	}
	for i, g := range r.mls {
		if g.degraded {
			t.Fatalf("healthy link %d degraded", i)
		}
	}
	if r.lg.count("stuck: its traffic has waited") != 1 || r.lg.count("the other links answer in ~120ms") != 1 {
		t.Fatalf("no stuck line:\n%s", r.lg)
	}
}

// Not stuck: a link that still moves enough for the loss rule to judge it
// (busy, however long its answers take); a wait shorter than stuckWait; and
// a pool in which every link waits (the path or the other server is down —
// moving users between links would not help).
func TestStuckNotFlagged(t *testing.T) {
	t.Run("moves enough", func(t *testing.T) {
		r := newStuckRig(t, 3)
		f, ml := r.add()
		r.step(f, activeBytes)
		waiting(f, 20*time.Second)
		for i := 0; i < 5; i++ {
			r.step(f, activeBytes)
		}
		if ml.degraded {
			t.Fatal("a link moving activeBytes a sample was taken for stuck")
		}
	})
	t.Run("short wait", func(t *testing.T) {
		r := newStuckRig(t, 3)
		f, ml := r.add()
		r.step(f, 0)
		waiting(f, stuckWait-time.Second)
		for i := 0; i < 5; i++ {
			r.step(f, 0)
		}
		if ml.degraded {
			t.Fatal("a wait under stuckWait was taken for stuck")
		}
	})
	// The path goes dark at O while this link has a ping out; another link's
	// pong lands just after that ping left, but its own ping went out before
	// it — so it proves nothing about the path after O.
	t.Run("outage: an answer that arrived later, to a ping sent before", func(t *testing.T) {
		r := newStuckRig(t, 3)
		f, ml := r.add()
		r.step(f, 0)
		sent := ctrlNow() - int64(stuckWait+2*time.Second)
		f.m.ctrlWait.Store(sent)
		for i := 0; i < 5; i++ {
			for _, g := range r.good {
				g.download(200<<10, 0)                                        // still busy (data in flight before O)
				g.m.ctrlAnsweredSent.Store(sent - int64(50*time.Millisecond)) // its ping left 50 ms before ours
			}
			r.clk.Advance(healthTick)
			r.m.sampleHealth()
		}
		if ml.degraded {
			t.Fatalf("taken for stuck on an answer to a ping sent before its own:\n%s", r.lg)
		}
	})
	// A path-wide squeeze: every busy link's ping waits behind its own
	// backlog; an idle link, with nothing queued, still answers at once —
	// it is no evidence that the path works.
	t.Run("squeeze: only an idle link answers", func(t *testing.T) {
		r := newStuckRig(t, 0)
		idle, _ := r.add()
		var fs []*meteredFakeLink
		var mls []*managedLink
		for i := 0; i < 5; i++ {
			f, ml := r.add()
			fs, mls = append(fs, f), append(mls, ml)
		}
		for i := 0; i < 5; i++ {
			idle.m.ctrlAnsweredSent.Store(ctrlNow())
			for _, f := range fs {
				waiting(f, stuckWait+2*time.Second)
				f.m.rdBytes.Add(20 << 10) // busy, under activeBytes
			}
			r.clk.Advance(healthTick)
			r.m.sampleHealth()
		}
		for i, ml := range mls {
			if ml.degraded {
				t.Fatalf("link %d drained in a squeeze, on an idle link's answer:\n%s", i, r.lg)
			}
		}
	})
	// The same in a small pool: two busy links wait (too few for the mass
	// rule), only an idle one answers — still no evidence.
	t.Run("squeeze in a small pool: only an idle link answers", func(t *testing.T) {
		r := newStuckRig(t, 0)
		idle, _ := r.add()
		var fs []*meteredFakeLink
		var mls []*managedLink
		for i := 0; i < 2; i++ {
			f, ml := r.add()
			fs, mls = append(fs, f), append(mls, ml)
		}
		for i := 0; i < 5; i++ {
			idle.m.ctrlAnsweredSent.Store(ctrlNow())
			for _, f := range fs {
				waiting(f, stuckWait+2*time.Second)
				f.m.rdBytes.Add(20 << 10)
			}
			r.clk.Advance(healthTick)
			r.m.sampleHealth()
		}
		for i, ml := range mls {
			if ml.degraded {
				t.Fatalf("link %d drained on an idle link's answer:\n%s", i, r.lg)
			}
		}
	})
	// More than a third of the busy links wait at once: the path or the other
	// server, not those links — none is drained, and it is said once.
	t.Run("mass: a third of the busy links at once", func(t *testing.T) {
		r := newStuckRig(t, 4)
		var mls []*managedLink
		var fs []*meteredFakeLink
		for i := 0; i < 3; i++ {
			f, ml := r.add()
			fs, mls = append(fs, f), append(mls, ml)
		}
		r.step(nil, 0)
		for i := 0; i < 4; i++ {
			for _, f := range fs {
				waiting(f, stuckWait+2*time.Second)
			}
			r.step(nil, 0)
		}
		for i, ml := range mls {
			if ml.degraded {
				t.Fatalf("link %d drained in a mass wait:\n%s", i, r.lg)
			}
		}
		if r.lg.count("3 of 7 busy links have waited 6s+ for an answer at once") != 1 {
			t.Fatalf("the mass wait is not said once:\n%s", r.lg)
		}
	})
	t.Run("every link waits", func(t *testing.T) {
		r := newStuckRig(t, 0)
		var fs []*meteredFakeLink
		var mls []*managedLink
		for i := 0; i < 4; i++ {
			f, ml := r.add()
			fs, mls = append(fs, f), append(mls, ml)
		}
		for i := 0; i < 5; i++ {
			for _, f := range fs {
				waiting(f, 10*time.Second)
				f.m.rdBytes.Add(1 << 10)
			}
			r.clk.Advance(healthTick)
			r.m.sampleHealth()
		}
		for i, ml := range mls {
			if ml.degraded {
				t.Fatalf("link %d degraded although no link answers:\n%s", i, r.lg)
			}
		}
	})
}

// A stuck link is drained without the maxDrain wait: right after it starts
// draining, its connections that moved no data for drainStall close, those
// still moving data stay.
func TestStuckLinkDrainsAtOnce(t *testing.T) {
	m, clk, lg := newV2Manager(nil, 1, 8, false)
	pl := newV2PipeLink(t)
	quiet, moving := pl.open(t), pl.open(t)
	now := clk.Now()
	pl.flowMu.Lock()
	quiet.lastActive = now.Add(-drainStall - time.Second)
	moving.lastActive = now
	pl.flowMu.Unlock()
	ml := addManaged(m, pl)
	m.mu.Lock()
	ml.degraded, ml.stuck, ml.draining, ml.drainSince = true, true, true, now
	ml.users.Store(2)
	m.mu.Unlock()
	m.heal(context.Background())
	within(t, time.Second, "the quiet connection on the stuck link is closed", func() bool { return quiet.done.Load() })
	time.Sleep(100 * time.Millisecond)
	if moving.done.Load() {
		t.Fatal("closed a connection that still moves data")
	}
	if lg.count("stuck — its connections that moved no data for 15s are closed now") != 1 {
		t.Fatalf("no drain line:\n%s", lg)
	}
	m.mu.RLock()
	in := len(m.links) == 1
	m.mu.RUnlock()
	if !in {
		t.Fatal("the stuck link was dropped while a user still moves data on it")
	}
}

// At most drainHeadroom stuck links are drained per tick (the longest waits
// first); the rest follow on the next ticks.
func TestStuckDrainsAtMostHeadroomPerTick(t *testing.T) {
	r := newStuckRig(t, 30) // max 32: headroom 4
	var fs []*meteredFakeLink
	var mls []*managedLink
	for i := 0; i < 6; i++ {
		f, ml := r.add()
		fs, mls = append(fs, f), append(mls, ml)
	}
	r.step(nil, 0)
	count := func() int {
		n := 0
		for _, ml := range mls {
			if ml.degraded {
				n++
			}
		}
		return n
	}
	for i := 0; i < stuckStreak; i++ {
		for k, f := range fs {
			waiting(f, stuckWait+time.Duration(k)*time.Second)
		}
		r.step(nil, 0)
	}
	if n, cap := count(), drainHeadroom(32); n != cap {
		t.Fatalf("%d stuck links drained in one tick, want %d (drainHeadroom)", n, cap)
	}
	if !mls[5].degraded || mls[0].degraded {
		t.Fatal("the longest waits are not drained first")
	}
	for k, f := range fs {
		waiting(f, stuckWait+time.Duration(k)*time.Second)
	}
	r.step(nil, 0)
	if n := count(); n != 6 {
		t.Fatalf("%d of 6 drained after the next tick", n)
	}
}

// The link's writer is stuck: pings time out writing, yet the control loop
// carries on and its wait grows (it does not end and forget it), at most
// ctrlPending pings are queued meanwhile, and each queued ping keeps its own
// sequence number (a timed-out write stays queued in smux, pointing at the
// buffer it was given).
func TestControlKeepsWaitingOnAStuckWriter(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time cadence")
	}
	a, b := net.Pipe()
	sc := &stuckConn{Conn: b, release: make(chan struct{})}
	mtr := &linkMeter{statsPoll: make(chan struct{}, 1)}
	cli, _, err := newSession(a, false, nil, mtr)
	if err != nil {
		t.Fatal(err)
	}
	srv, _, err := newSession(sc, true, nil, &linkMeter{})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	defer srv.Close()
	var mu sync.Mutex
	var seqs []uint64
	go func() { // the exit: answers every ping it gets
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
			mu.Lock()
			seqs = append(seqs, binary.BigEndian.Uint64(ping))
			mu.Unlock()
			copy(pong, ping)
			st.Write(pong)
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { // heavy: the fixed beat
		for ctx.Err() == nil {
			mtr.rdBytes.Add(activeBytes)
			time.Sleep(100 * time.Millisecond)
		}
	}()
	done := make(chan struct{})
	go func() { openControl(ctx, &mtcpLink{sess: cli, mtr: mtr}, nil); close(done) }()
	within(t, controlInterval+2*time.Second, "the first ping is answered", func() bool { return mtr.peerSeen.Load() })
	sc.stuck.Store(true) // the exit stops reading: the link's writer blocks
	mu.Lock()
	before := len(seqs)
	mu.Unlock()
	time.Sleep(10 * controlInterval)
	if w := ctrlWaitOf(mtr); w < stuckWait {
		t.Fatalf("after %s of a stuck writer the wait is %s (the loop ended, or forgot it)", 10*controlInterval, w)
	}
	select {
	case <-done:
		t.Fatal("the control loop ended on a write timeout")
	default:
	}
	close(sc.release) // the writer moves again: the queued pings go out
	time.Sleep(time.Second)
	mu.Lock()
	queued := append([]uint64(nil), seqs[before:]...)
	mu.Unlock()
	if len(queued) < 2 || len(queued) > ctrlPending+1 {
		t.Fatalf("%d pings were queued behind the stuck writer, want 2 to %d: %v", len(queued), ctrlPending+1, queued)
	}
	for i := 1; i < len(queued); i++ {
		if queued[i] <= queued[i-1] {
			t.Fatalf("queued pings lost their own sequence numbers: %v", queued)
		}
	}
	within(t, 2*controlInterval+time.Second, "the late answers clear the wait", func() bool { return ctrlWaitOf(mtr) < 2*time.Second })
}

// The pending list: a pong answers its ping and every earlier one, says when
// the ping it answers was sent, and the list holds at most ctrlPending.
func TestCtrlPendingList(t *testing.T) {
	var p ctrlPendingList
	for i := uint64(1); !p.full(); i++ {
		p.add(i, int64(i*100))
		if i > ctrlPending {
			t.Fatalf("the list took %d pings, want at most %d", i, ctrlPending)
		}
	}
	if len(p.items) != ctrlPending || p.oldest() != 100 {
		t.Fatalf("%d pending, oldest %d", len(p.items), p.oldest())
	}
	if at, ok := p.answer(2); !ok || at != 200 || p.oldest() != 300 {
		t.Fatalf("answer(2): at %d ok %v, oldest now %d; want 200 true 300", at, ok, p.oldest())
	}
	if _, ok := p.answer(2); ok {
		t.Fatal("a duplicate answer counted")
	}
	if at, ok := p.answer(99); ok || at != 0 || !p.empty() || p.oldest() != 0 {
		t.Fatalf("an answer past the list: at %d ok %v, empty %v", at, ok, p.empty())
	}
}
