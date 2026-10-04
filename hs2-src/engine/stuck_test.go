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
	// Still unanswered: a ping goes out every 2 controlIntervals (each waits
	// that long for its pong), so the 4th leaves at ~21 s and, without the
	// cap, a 5th at ~27 s.
	time.Sleep(23 * time.Second)
	mu.Lock()
	n := got
	mu.Unlock()
	if n != ctrlPending {
		t.Fatalf("%d pings sent in 31 s while none was answered, want %d (ctrlPending)", n, ctrlPending)
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
	// Its own users wedged it (a full receive buffer, apps not reading): the
	// guard's business, not a stuck path — in the load test such a link was
	// taken for stuck 1.7 s after the guard had freed it.
	t.Run("wedged by its own users", func(t *testing.T) {
		r := newStuckRig(t, 3)
		f, ml := r.add()
		f.m.guard = &sessGuard{}
		r.step(f, 0)
		for i := 0; i < 5; i++ {
			waiting(f, stuckWait+2*time.Second)
			f.m.guard.parkedAt.Store(ctrlNow() - int64(time.Second)) // parked a moment ago
			r.step(f, 1<<10)
		}
		if ml.degraded {
			t.Fatalf("a link wedged by its own users was taken for stuck:\n%s", r.lg)
		}
		// once the guard has freed it and stuckWait has passed, the link is
		// judged again
		f.m.guard.parkedAt.Store(ctrlNow() - int64(stuckWait+time.Second))
		for i := 0; i < stuckStreak; i++ {
			waiting(f, stuckWait+2*time.Second)
			r.step(f, 1<<10)
		}
		if !ml.degraded {
			t.Fatal("still not judged long after the wedge")
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
	// The load test's squeeze (8 Mbit/s for every link): the other busy links
	// were just answered, but after a round trip of seconds — no evidence.
	t.Run("squeeze: the others answer, in seconds", func(t *testing.T) {
		r := newStuckRig(t, 3)
		for _, g := range r.good {
			g.m.rttMicros.Store(4_900_000)
		}
		f, ml := r.add()
		r.step(f, 0)
		for i := 0; i < 5; i++ {
			waiting(f, stuckWait+2*time.Second)
			r.step(f, 1<<10)
		}
		if ml.degraded {
			t.Fatalf("drained on answers that took 4.9 s:\n%s", r.lg)
		}
	})
	// Waits rise link by link in a squeeze: every link waiting now counts
	// as busy, its streak complete or not — here 4 of 7 busy links wait and
	// only 3 answer promptly: the path, not those links.
	t.Run("path slow: links still building their streak count", func(t *testing.T) {
		r := newStuckRig(t, 3)
		var early, late []*meteredFakeLink
		var mls []*managedLink
		for i := 0; i < 2; i++ {
			f, ml := r.add()
			early, mls = append(early, f), append(mls, ml)
		}
		for i := 0; i < 2; i++ {
			f, ml := r.add()
			late, mls = append(late, f), append(mls, ml)
		}
		r.step(nil, 0)
		for _, f := range early {
			waiting(f, stuckWait+2*time.Second)
		}
		r.step(nil, 0) // early: streak 1
		for _, f := range append(early, late...) {
			waiting(f, stuckWait+2*time.Second)
		}
		r.step(nil, 0) // early: streak 2; late: streak 1
		for i, ml := range mls {
			if ml.degraded {
				t.Fatalf("link %d drained while most busy links wait:\n%s", i, r.lg)
			}
		}
	})
	// Fewer than half the busy links answer promptly (the others answer, but
	// in seconds): the path or the other server — none is drained, and it is
	// said once.
	t.Run("path slow: few answer promptly", func(t *testing.T) {
		r := newStuckRig(t, 4)
		for _, g := range r.good[1:] {
			g.m.rttMicros.Store(4_900_000)
		}
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
				t.Fatalf("link %d drained while the path is slow:\n%s", i, r.lg)
			}
		}
		if r.lg.count("3 of 7 busy links have waited 6s+ for an answer and only 1 answer promptly") != 1 {
			t.Fatalf("the slow path is not said once:\n%s", r.lg)
		}
	})
	// A congested path with a short queue: the light links answer at once,
	// links whose users load a lot wait behind their own backlog while they
	// move their share (more than the links that answer) — their own load,
	// not a throttle. A link that waits while moving next to nothing is.
	t.Run("moves its share: waiting on its own load", func(t *testing.T) {
		r := newStuckRig(t, 0)
		var light, heavy, loaded []*meteredFakeLink
		var mls []*managedLink
		for i := 0; i < 20; i++ {
			f, _ := r.add()
			f.m.rttMicros.Store(150_000)
			light = append(light, f)
		}
		for i := 0; i < 6; i++ {
			f, ml := r.add()
			heavy, mls = append(heavy, f), append(mls, ml)
		}
		for i := 0; i < 5; i++ {
			f, ml := r.add()
			loaded, mls = append(loaded, f), append(mls, ml)
		}
		throttled, tml := r.add()
		tick := func() {
			for _, f := range light {
				f.download(8<<10, 0)
				f.m.ctrlAnsweredSent.Store(ctrlNow() - int64(150*time.Millisecond))
			}
			for _, f := range heavy {
				waiting(f, 10*time.Second)
				f.download(200<<10, 0)
			}
			for _, f := range loaded {
				waiting(f, 8*time.Second)
				f.download(30<<10, 0)
			}
			waiting(throttled, 8*time.Second)
			throttled.download(1<<10, 0)
			r.clk.Advance(healthTick)
			r.m.sampleHealth()
		}
		for i := 0; i < 5; i++ {
			tick()
		}
		for i, ml := range mls {
			if ml.degraded {
				t.Fatalf("link %d drained while it moves its share:\n%s", i, r.lg)
			}
		}
		if !tml.degraded {
			t.Fatalf("the link that waits while moving next to nothing is not drained:\n%s", r.lg)
		}
	})
	// Nothing received at all for suspectAfter: suspect, not stuck — it takes
	// no new users and serves again the moment anything arrives (or its TCP
	// gives up); its ping waiting on top of that changes nothing.
	t.Run("suspect: nothing received", func(t *testing.T) {
		r := newStuckRig(t, 3)
		f, ml := r.add()
		r.step(f, 4<<10)
		for i := 0; i < 6; i++ { // silent, its ping just sent
			waiting(f, time.Second)
			r.step(f, 0)
		}
		for i := 0; i < 4; i++ {
			waiting(f, stuckWait+2*time.Second)
			r.step(f, 0)
		}
		if !ml.suspect || ml.degraded {
			t.Fatalf("suspect %v, degraded %v:\n%s", ml.suspect, ml.degraded, r.lg)
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

// At most drainHeadroom stuck links drain at a time (the longest waits
// first): the rest follow once those are gone — a wave of verdicts, right or
// wrong, never takes more than the pool's spare room at once.
func TestStuckDrainsAtMostHeadroomAtOnce(t *testing.T) {
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
	wait := func() {
		for k, f := range fs {
			if f.Alive() {
				waiting(f, stuckWait+time.Duration(k)*time.Second)
			}
		}
	}
	for i := 0; i < stuckStreak; i++ {
		wait()
		r.step(nil, 0)
	}
	if n, cap := count(), drainHeadroom(32); n != cap {
		t.Fatalf("%d stuck links drained in one tick, want %d (drainHeadroom)", n, cap)
	}
	if !mls[5].degraded || mls[0].degraded {
		t.Fatal("the longest waits are not drained first")
	}
	wait()
	r.step(nil, 0)
	if n := count(); n != drainHeadroom(32) {
		t.Fatalf("%d of 6 drained while %d still drain", n, drainHeadroom(32))
	}
	for k, ml := range mls {
		if ml.degraded {
			fs[k].Close() // their drain is over
		}
	}
	wait()
	r.step(nil, 0)
	if n := count(); n != 6 {
		t.Fatalf("%d of 6 drained after the first ones closed", n)
	}
}

// Stuck links that are a good part of the busy ones — 4 of 10, the others
// answering at once — are still caught: they do not pass for a slow path.
func TestStuckMinorityIsCaught(t *testing.T) {
	r := newStuckRig(t, 6)
	var fs []*meteredFakeLink
	var mls []*managedLink
	for i := 0; i < 4; i++ {
		f, ml := r.add()
		fs, mls = append(fs, f), append(mls, ml)
	}
	r.step(nil, 0)
	for i := 0; i < 5; i++ {
		for _, f := range fs {
			waiting(f, stuckWait+2*time.Second)
			f.download(1<<10, 0)
		}
		r.step(nil, 0)
	}
	for i, ml := range mls {
		if !ml.degraded || !ml.stuck {
			t.Fatalf("stuck link %d of 4 (of 10 busy) not drained:\n%s", i, r.lg)
		}
	}
	if r.lg.count("answer promptly") != 0 {
		t.Fatalf("taken for a slow path:\n%s", r.lg)
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

// After a path-wide wait the links get stuckRecover to come back on their own
// (TCP backed off through it); one still waiting after that is judged.
func TestStuckWaitsOutRecoveryAfterMassWait(t *testing.T) {
	r := newStuckRig(t, 4)
	var fs []*meteredFakeLink
	var mls []*managedLink
	for i := 0; i < 3; i++ {
		f, ml := r.add()
		fs, mls = append(fs, f), append(mls, ml)
	}
	r.step(nil, 0)
	for _, g := range r.good { // the squeeze: the others answer, in seconds
		g.m.rttMicros.Store(4_900_000)
	}
	for i := 0; i < 2; i++ {
		for _, f := range fs {
			waiting(f, stuckWait+2*time.Second)
		}
		r.step(nil, 0)
	}
	// the squeeze ends: the others answer at once again, two come back, one
	// still waits (from before)
	for _, g := range r.good {
		g.m.rttMicros.Store(120_000)
	}
	for _, f := range fs[:2] {
		f.m.ctrlWait.Store(0)
		r.good = append(r.good, f)
	}
	for i := 0; i < 3; i++ {
		waiting(fs[2], stuckWait+20*time.Second)
		r.step(nil, 0)
	}
	if mls[2].degraded {
		t.Fatalf("drained %s after a path-wide wait, before it could recover:\n%s", 3*healthTick, r.lg)
	}
	r.clk.Advance(stuckRecover - 5*healthTick) // 28 s since the last slow tick
	waiting(fs[2], stuckWait+50*time.Second)
	r.step(nil, 0)
	if mls[2].degraded {
		t.Fatalf("drained before stuckRecover was over:\n%s", r.lg)
	}
	r.clk.Advance(healthTick)
	waiting(fs[2], stuckWait+50*time.Second)
	r.step(nil, 0)
	if !mls[2].degraded || mls[0].degraded || mls[1].degraded {
		t.Fatalf("after the recovery window: still-stuck drained=%v, recovered drained=%v/%v\n%s", mls[2].degraded, mls[0].degraded, mls[1].degraded, r.lg)
	}
}

// A pong that comes back while a later ping is still out shows that later
// ping's wait at once, not the answered one's until every pong is back: one
// late answer is one long wait, not a streak of them.
func TestControlWaitFollowsEachAnswer(t *testing.T) {
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
	firstAnswered := make(chan struct{})
	go func() { // the exit: the first ping answered 7 s late, the second 5 s, then at once
		st, err := srv.AcceptStream()
		if err != nil {
			return
		}
		var k [1]byte
		io.ReadFull(st, k[:])
		var mu sync.Mutex
		answer := func(p []byte) {
			pong := make([]byte, ctrlPongLen)
			copy(pong, p)
			mu.Lock()
			st.Write(pong)
			mu.Unlock()
		}
		for n := 0; ; n++ {
			p := make([]byte, ctrlPingLen)
			if _, err := io.ReadFull(st, p); err != nil {
				return
			}
			switch n {
			case 0:
				time.AfterFunc(7*time.Second, func() { answer(p); close(firstAnswered) })
			case 1:
				time.AfterFunc(5*time.Second, func() { answer(p) })
			default:
				answer(p)
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
	select {
	case <-firstAnswered:
	case <-time.After(20 * time.Second):
		t.Fatal("the first ping never came")
	}
	time.Sleep(1500 * time.Millisecond)
	if w := ctrlWaitOf(mtr); w >= 4*time.Second || w == 0 {
		t.Fatalf("1.5 s after the first answer the wait shows %s; want the second ping's, a few seconds", w)
	}
	cancel()
	<-done
}

// The longer the path was slow, the longer the links get to come back (TCP's
// retries spread out with it): as long as the slow spell, stuckRecover at
// least and stuckRecoverMax at most; a spell that pauses for less than its
// window is still one spell.
func TestStuckRecoveryGrowsWithTheSlowSpell(t *testing.T) {
	for _, tc := range []struct {
		name        string
		spell, want time.Duration
		pause       bool
	}{
		{"90 s", 90 * time.Second, 90 * time.Second, false},
		{"90 s with a pause", 90 * time.Second, 90 * time.Second, true},
		{"5 min", 5 * time.Minute, stuckRecoverMax, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newStuckRig(t, 4)
			f, ml := r.add()
			r.step(nil, 0)
			for _, g := range r.good { // slow for all: answers take seconds
				g.m.rttMicros.Store(4_900_000)
			}
			for el := time.Duration(0); el < tc.spell; el += healthTick {
				if tc.pause && el >= tc.spell/3 && el < tc.spell/2 {
					for _, g := range r.good {
						g.m.rttMicros.Store(120_000)
					}
				} else {
					for _, g := range r.good {
						g.m.rttMicros.Store(4_900_000)
					}
				}
				waiting(f, stuckWait+el)
				r.step(nil, 0)
			}
			if ml.degraded {
				t.Fatalf("drained during the slow spell:\n%s", r.lg)
			}
			for _, g := range r.good { // it ends; f still waits
				g.m.rttMicros.Store(120_000)
			}
			// the spell runs from its first slow tick to its last: one tick
			// short of tc.spell here
			for el := healthTick; el < min(tc.want, tc.spell-healthTick); el += healthTick {
				waiting(f, stuckWait+tc.spell+el)
				r.step(nil, 0)
				if ml.degraded {
					t.Fatalf("drained %s after a %s slow spell, want %s of recovery:\n%s", el, tc.spell, tc.want, r.lg)
				}
			}
			for i := 0; i < 2; i++ {
				waiting(f, stuckWait+tc.spell+tc.want)
				r.step(nil, 0)
			}
			if !ml.degraded {
				t.Fatalf("not drained %s after a %s slow spell:\n%s", tc.want+2*healthTick, tc.spell, r.lg)
			}
		})
	}
}

// The loss rule waits out a slow spell too: the retransmits of links getting
// over a squeeze say nothing about one link. A link still lossy after the
// recovery window is degraded on degradeStreak fresh samples.
func TestLossWaitsOutASlowSpell(t *testing.T) {
	r := newStuckRig(t, 4)
	f, ml := r.add()
	lossy := func() { f.download(200<<10, 60) } // ~140 packets, 60 resent
	r.step(nil, 0)
	for _, g := range r.good { // slow for all: answers take seconds
		g.m.rttMicros.Store(4_900_000)
	}
	for i := 0; i < 10; i++ { // a 20 s spell
		lossy()
		r.step(nil, 0)
	}
	for _, g := range r.good {
		g.m.rttMicros.Store(120_000)
	}
	for el := healthTick; el < stuckRecover; el += healthTick {
		lossy()
		r.step(nil, 0)
		if ml.degraded {
			t.Fatalf("degraded for loss %s after a slow spell:\n%s", el, r.lg)
		}
	}
	for i := 0; i < degradeStreak+1; i++ {
		lossy()
		r.step(nil, 0)
	}
	if !ml.degraded || ml.stuck {
		t.Fatalf("a link still lossy after the recovery window: degraded %v stuck %v\n%s", ml.degraded, ml.stuck, r.lg)
	}
	if r.lg.count("link 4 degraded (up-loss") != 1 {
		t.Fatalf("no loss line:\n%s", r.lg)
	}
}
