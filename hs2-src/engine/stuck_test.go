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
		old := newMeteredFake() // busy, to an older exit: no control channel
		addManaged(r.m, old)
		var mls []*managedLink
		var fs []*meteredFakeLink
		for i := 0; i < 3; i++ {
			f, ml := r.add()
			fs, mls = append(fs, f), append(mls, ml)
		}
		old.download(200<<10, 0)
		r.step(nil, 0)
		for i := 0; i < 4; i++ {
			for _, f := range fs {
				waiting(f, stuckWait+2*time.Second)
			}
			old.download(200<<10, 0)
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
// retries spread out with it): as long as the spell was slow all told,
// stuckRecover at least and stuckRecoverMax at most; a pause shorter than
// the window does not end the spell, nor count in it.
func TestStuckRecoveryGrowsWithTheSlowSpell(t *testing.T) {
	for _, tc := range []struct {
		name        string
		spell, want time.Duration
		pause       bool
	}{
		{"90 s", 90 * time.Second, 90 * time.Second, false},
		{"90 s with a 16 s pause", 90 * time.Second, 74 * time.Second, true},
		{"5 min", 5 * time.Minute, stuckRecoverMax, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newStuckRig(t, 4)
			f, ml := r.add()
			w, _ := r.add() // waits through the spell too, then recovers
			r.step(nil, 0)
			slowFor := time.Duration(0)
			for el := time.Duration(0); el < tc.spell; el += healthTick {
				paused := tc.pause && el >= 30*time.Second && el < 46*time.Second
				for _, g := range r.good { // slow for all: answers take seconds
					g.m.rttMicros.Store(map[bool]uint64{true: 120_000, false: 4_900_000}[paused])
				}
				if paused {
					w.m.ctrlWait.Store(0)
				} else {
					waiting(w, stuckWait+time.Second)
					slowFor += healthTick
				}
				waiting(f, stuckWait+el)
				r.step(nil, 0)
			}
			if ml.degraded {
				t.Fatalf("drained during the slow spell:\n%s", r.lg)
			}
			if want := min(slowFor, stuckRecoverMax); want != tc.want {
				t.Fatalf("test setup: slow for %s, want %s", slowFor, tc.want)
			}
			for _, g := range r.good { // it ends; w comes back, f still waits
				g.m.rttMicros.Store(120_000)
			}
			w.m.ctrlWait.Store(0)
			r.good = append(r.good, w)
			for el := healthTick; el < tc.want; el += healthTick {
				waiting(f, stuckWait+tc.spell+el)
				r.step(nil, 0)
				if ml.degraded {
					t.Fatalf("drained %s after a spell slow for %s, want %s of recovery:\n%s", el, slowFor, tc.want, r.lg)
				}
			}
			for i := 0; i < 2; i++ {
				waiting(f, stuckWait+tc.spell+tc.want)
				r.step(nil, 0)
			}
			if !ml.degraded {
				t.Fatalf("not drained %s after a spell slow for %s:\n%s", tc.want+2*healthTick, slowFor, r.lg)
			}
		})
	}
}

// One-tick blips do not stretch the window: each adds a tick, not the time
// between them — 13 blips in 20 min (the review's case) leave the loss rule
// judging most of the time.
func TestStuckBlipsDoNotStretchTheWindow(t *testing.T) {
	r := newStuckRig(t, 4)
	var ws []*meteredFakeLink
	for i := 0; i < 5; i++ {
		f, _ := r.add()
		ws = append(ws, f)
	}
	lossy, lml := r.add()
	blip := func(k int) bool {
		switch k {
		case 0, 14, 28, 55, 109:
			return true
		}
		return k > 109 && (k-109)%59 == 0
	}
	calmTicks := 0
	for k := 0; k < 600; k++ { // 20 min
		for _, f := range ws {
			if blip(k) { // one tick: 5 links wait, 4 answer
				waiting(f, stuckWait+time.Second)
			} else {
				f.m.ctrlWait.Store(0)
			}
		}
		lossy.download(200<<10, 0)
		if k >= 120 { // lossy from 4 min on
			lossy.m.peerRetrans.Add(60)
		}
		r.m.mu.Lock()
		if r.m.stuckSlowAt.IsZero() || r.clk.Now().Add(healthTick).Sub(r.m.stuckSlowAt) >= r.m.stuckRecoverFor() {
			if k >= 120 {
				calmTicks++
			}
		}
		r.m.mu.Unlock()
		r.step(nil, 0)
	}
	if calmTicks < 300 || !lml.degraded {
		t.Fatalf("calm %d of 480 ticks after 4 min, lossy link degraded %v:\n%s", calmTicks, lml.degraded, r.lg)
	}
}

// A separate spell, after the last one's window has passed, starts afresh:
// one slow tick after a long spell gives stuckRecover, not that spell's.
func TestStuckSeparateSpellStartsAfresh(t *testing.T) {
	r := newStuckRig(t, 4)
	w1, _ := r.add()
	w2, _ := r.add()
	f, ml := r.add()
	slowTick := func(n int) {
		for i := 0; i < n; i++ {
			waiting(w1, stuckWait+time.Second)
			waiting(w2, stuckWait+time.Second)
			for _, g := range r.good {
				g.m.rttMicros.Store(4_900_000)
			}
			r.step(nil, 0)
		}
		w1.m.ctrlWait.Store(0)
		w2.m.ctrlWait.Store(0)
		for _, g := range r.good {
			g.m.rttMicros.Store(120_000)
		}
	}
	slowTick(45) // 90 s
	r.clk.Advance(3 * time.Minute)
	r.step(nil, 0)
	slowTick(1) // a new spell of one tick
	for el := healthTick; el < stuckRecover-healthTick; el += healthTick {
		waiting(f, stuckWait+el)
		r.step(nil, 0)
		if ml.degraded {
			t.Fatalf("drained %s after a one-tick spell:\n%s", el, r.lg)
		}
	}
	for i := 0; i < 3; i++ {
		waiting(f, stuckWait+stuckRecover)
		r.step(nil, 0)
	}
	if !ml.degraded {
		t.Fatalf("a one-tick spell after a long one held verdicts past stuckRecover:\n%s", r.lg)
	}
}

// The loss rule waits out a slow spell too: the retransmits of links getting
// over a squeeze say nothing about one link. Not even on the spell's first
// tick, with two bad samples already counted; after the recovery window it
// takes degradeStreak fresh samples.
func TestLossWaitsOutASlowSpell(t *testing.T) {
	r := newStuckRig(t, 4)
	f, ml := r.add()
	w1, _ := r.add()
	w2, _ := r.add()
	lossy := func() { f.download(200<<10, 60) } // ~140 packets, 60 resent
	lossy()
	r.step(nil, 0) // the baseline
	lossy()
	r.step(nil, 0)
	lossy()
	r.step(nil, 0)            // two bad samples before the spell
	for i := 0; i < 10; i++ { // a 20 s spell: the others answer in seconds, two wait
		for _, g := range r.good {
			g.m.rttMicros.Store(4_900_000)
		}
		waiting(w1, stuckWait+time.Second)
		waiting(w2, stuckWait+time.Second)
		lossy()
		r.step(nil, 0)
		if ml.degraded {
			t.Fatalf("degraded for loss on slow tick %d:\n%s", i+1, r.lg)
		}
	}
	for _, g := range r.good {
		g.m.rttMicros.Store(120_000)
	}
	w1.m.ctrlWait.Store(0)
	w2.m.ctrlWait.Store(0)
	for el := healthTick; el < stuckRecover; el += healthTick {
		lossy()
		r.step(nil, 0)
		if ml.degraded {
			t.Fatalf("degraded for loss %s after a slow spell:\n%s", el, r.lg)
		}
	}
	for i := 0; i < degradeStreak-1; i++ { // the streak starts afresh
		lossy()
		r.step(nil, 0)
	}
	if ml.degraded {
		t.Fatalf("degraded on %d fresh samples after the window, want %d:\n%s", degradeStreak-1, degradeStreak, r.lg)
	}
	for i := 0; i < 2; i++ {
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

// prompt makes f a busy link answered at once; backlogged makes it a heavy
// one with 2.5 s of its own backlog (pongs take 2.5 s, no ping waits 6 s).
func prompt(f *meteredFakeLink, bytes uint64) {
	f.download(bytes, 0)
	f.m.rttMicros.Store(120_000)
	f.m.ctrlWait.Store(0)
	f.m.ctrlAnsweredSent.Store(ctrlNow() - int64(100*time.Millisecond))
}

func backlogged(f *meteredFakeLink, bytes, rt uint64) {
	f.download(bytes, rt)
	f.m.rttMicros.Store(2_500_000)
	waiting(f, time.Second)
	f.m.ctrlAnsweredSent.Store(ctrlNow() - int64(2600*time.Millisecond))
}

// Links slow to answer are not a slow path: neither lossy links whose own
// pongs take seconds (at night, two of three busy links) nor heavy links
// waiting behind their own backlog hold off the loss rule or the stuck rule —
// only links waiting stuckWait while moving little count against the path.
func TestSlowAnswersAreNotASlowPath(t *testing.T) {
	t.Run("lossy links at night", func(t *testing.T) {
		r := newStuckRig(t, 0)
		light, _ := r.add()
		var lossy []*meteredFakeLink
		var lml []*managedLink
		for i := 0; i < 2; i++ {
			f, ml := r.add()
			lossy, lml = append(lossy, f), append(lml, ml)
		}
		for i := 0; i < 10; i++ {
			prompt(light, 16<<10)
			for _, f := range lossy {
				backlogged(f, 200<<10, 60) // 43% resent, pongs in 2.5 s
			}
			r.step(nil, 0)
		}
		if !lml[0].degraded || !lml[1].degraded {
			t.Fatalf("lossy links not degraded:\n%s", r.lg)
		}
	})
	t.Run("slow answers do not tip the balance", func(t *testing.T) {
		r := newStuckRig(t, 0)
		var light, slowly, thr []*meteredFakeLink
		var tml []*managedLink
		for i := 0; i < 3; i++ {
			f, _ := r.add()
			light = append(light, f)
		}
		for i := 0; i < 4; i++ {
			f, _ := r.add()
			slowly = append(slowly, f)
		}
		for i := 0; i < 2; i++ {
			f, ml := r.add()
			thr, tml = append(thr, f), append(tml, ml)
		}
		for i := 0; i < 4; i++ {
			for _, f := range light {
				prompt(f, 50<<10)
			}
			for _, f := range slowly { // busy, answering in 2.5 s
				backlogged(f, 50<<10, 0)
			}
			for _, f := range thr {
				waiting(f, 8*time.Second)
				f.download(1<<10, 0)
			}
			r.step(nil, 0)
		}
		if !tml[0].degraded || !tml[1].degraded {
			t.Fatalf("two throttled links, three prompt, four slow to answer: taken for a slow path:\n%s", r.lg)
		}
	})
	t.Run("heavy links outnumber light ones", func(t *testing.T) {
		r := newStuckRig(t, 0)
		var light, heavy []*meteredFakeLink
		for i := 0; i < 10; i++ {
			f, _ := r.add()
			light = append(light, f)
		}
		for i := 0; i < 12; i++ {
			f, _ := r.add()
			heavy = append(heavy, f)
		}
		thr, tml := r.add()
		lossy, lml := r.add()
		for i := 0; i < 10; i++ {
			for _, f := range light {
				prompt(f, 50<<10)
			}
			for _, f := range heavy {
				backlogged(f, 200<<10, 0)
			}
			waiting(thr, 8*time.Second)
			thr.download(1<<10, 0)
			prompt(lossy, 200<<10)
			lossy.m.peerRetrans.Add(60)
			r.step(nil, 0)
		}
		if !tml.degraded || !tml.stuck || !lml.degraded || lml.stuck {
			t.Fatalf("throttled degraded=%v stuck=%v, lossy degraded=%v:\n%s", tml.degraded, tml.stuck, lml.degraded, r.lg)
		}
		if r.lg.count("answer promptly") != 0 {
			t.Fatalf("taken for a slow path:\n%s", r.lg)
		}
	})
}

// The share is half the median of what the links answering promptly move: a
// waiting link moving between half the median and half the largest is its
// share's worth (not drained), one between half the smallest and half the
// median is throttled (drained).
func TestStuckShareIsHalfTheMedian(t *testing.T) {
	r := newStuckRig(t, 0)
	var good []*meteredFakeLink
	for i := 0; i < 3; i++ {
		f, _ := r.add()
		good = append(good, f)
	}
	a, aml := r.add()
	b, bml := r.add()
	for i := 0; i < 4; i++ {
		for k, f := range good {
			prompt(f, uint64(100+20*k)<<10) // 100, 120, 140 KB: median 120
		}
		waiting(a, 8*time.Second)
		a.download(65<<10, 0) // over half the median, under half the largest
		waiting(b, 8*time.Second)
		b.download(55<<10, 0) // under half the median, over half the smallest
		r.step(nil, 0)
	}
	if aml.degraded || !bml.degraded {
		t.Fatalf("65 KB drained %v (want no), 55 KB drained %v (want yes):\n%s", aml.degraded, bml.degraded, r.lg)
	}
}

// Under stuckMoveFloor a waiting link is throttled whatever the others move:
// light links at night (12 KB a tick each) do not let one moving 7 KB with
// its users waiting 8 s pass for one moving its share.
func TestStuckFloorCatchesAThrottleAtNight(t *testing.T) {
	r := newStuckRig(t, 0)
	var good []*meteredFakeLink
	for i := 0; i < 6; i++ {
		f, _ := r.add()
		good = append(good, f)
	}
	thr, tml := r.add()
	for i := 0; i < 4; i++ {
		for _, f := range good {
			prompt(f, 12<<10)
		}
		waiting(thr, 8*time.Second)
		thr.download(7<<10, 0)
		r.step(nil, 0)
	}
	if !tml.degraded || !tml.stuck {
		t.Fatalf("a link moving 7 KB a tick while its users wait 8 s is not drained:\n%s", r.lg)
	}
}

// What counts against the path is links waiting stuckWait while moving
// little, two at least: heavy links waiting behind their own backlog do not,
// nor does a single waiting link.
func TestSlowPathNeedsWaitingLightLinks(t *testing.T) {
	t.Run("heavy links waiting on their own load", func(t *testing.T) {
		r := newStuckRig(t, 0)
		var light, heavy []*meteredFakeLink
		for i := 0; i < 4; i++ {
			f, _ := r.add()
			light = append(light, f)
		}
		for i := 0; i < 6; i++ {
			f, _ := r.add()
			heavy = append(heavy, f)
		}
		thr, tml := r.add()
		for i := 0; i < 4; i++ {
			for _, f := range light {
				prompt(f, 50<<10)
			}
			for _, f := range heavy {
				waiting(f, 8*time.Second)
				f.download(200<<10, 0)
			}
			waiting(thr, 8*time.Second)
			thr.download(1<<10, 0)
			r.step(nil, 0)
		}
		if !tml.degraded {
			t.Fatalf("heavy links waiting on their own load passed for a slow path:\n%s", r.lg)
		}
	})
	t.Run("one waiting link", func(t *testing.T) {
		r := newStuckRig(t, 0)
		var lossy []*meteredFakeLink
		var lml []*managedLink
		for i := 0; i < 2; i++ {
			f, ml := r.add()
			lossy, lml = append(lossy, f), append(lml, ml)
		}
		thr, _ := r.add()
		for i := 0; i < 5; i++ {
			for _, f := range lossy {
				backlogged(f, 200<<10, 60)
			}
			waiting(thr, 8*time.Second)
			r.step(nil, 0)
		}
		if !lml[0].degraded || !lml[1].degraded {
			t.Fatalf("one waiting link passed for a slow path and held off the loss rule:\n%s", r.lg)
		}
	})
}

// A congested path: links wait while the ones that answer promptly take
// several times their usual time. Light links answer within stuckPrompt
// through a squeeze and outnumber the heavy ones backing off, so the count
// alone called it a minority of stuck links — 33 drained in the load test
// (300 links, 190 → 60 Mbit/s). Their time against the usual says it is the
// path: none is drained and the line says so. With the others at their
// usual time the same waiting links are caught.
func TestStuckCongestedPathIsSlow(t *testing.T) {
	for _, c := range []struct {
		name       string
		usual, rtt uint64 // µs, the links that answer promptly: before, then
		drained    bool
	}{
		{"answers at 9x the usual", 120_000, 1_100_000, false},
		{"answers at the usual", 120_000, 120_000, true},
		{"a fast path at 7x, under the floor", 20_000, 150_000, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newStuckRig(t, 12)
			for _, g := range r.good {
				g.m.rttMicros.Store(c.usual)
			}
			for i := 0; i < 40; i++ { // 80 s at the usual time
				r.step(nil, 0)
			}
			var wml []*managedLink
			var ws []*meteredFakeLink
			for i := 0; i < 4; i++ {
				f, ml := r.add()
				ws, wml = append(ws, f), append(wml, ml)
			}
			for i := 0; i < 6; i++ {
				for _, g := range r.good {
					g.m.rttMicros.Store(c.rtt)
				}
				for _, f := range ws {
					waiting(f, stuckWait+2*time.Second)
					f.m.rdBytes.Add(4 << 10)
				}
				r.step(nil, 0)
			}
			n := 0
			for _, ml := range wml {
				if ml.degraded {
					n++
				}
			}
			if c.drained && n != len(wml) || !c.drained && n != 0 {
				t.Fatalf("%d of %d waiting links drained, want all: %v\n%s", n, len(wml), c.drained, r.lg)
			}
			if got := r.lg.count("the path is congested"); c.drained && got != 0 || !c.drained && got != 1 {
				t.Fatalf("%d congestion lines:\n%s", got, r.lg)
			}
		})
	}
}

// The usual time is the lowest per-minute value of the last stuckBaseMins
// minutes: a congested spell does not raise it while the calm minutes before
// it are in the window, and it forgets them after.
func TestRTTFloorWindow(t *testing.T) {
	var f rttFloor
	t0 := time.Unix(1000, 0)
	f.note(t0, 120*time.Millisecond)
	f.note(t0.Add(30*time.Second), 150*time.Millisecond)
	if b := f.base(); b != 120*time.Millisecond {
		t.Fatalf("base %v, want 120ms", b)
	}
	for k := 1; k < stuckBaseMins; k++ { // congested minutes
		f.note(t0.Add(time.Duration(k)*time.Minute), 1100*time.Millisecond)
	}
	if b := f.base(); b != 120*time.Millisecond {
		t.Fatalf("base %v during the spell, want the calm 120ms", b)
	}
	f.note(t0.Add(time.Duration(stuckBaseMins)*time.Minute), 1100*time.Millisecond)
	if b := f.base(); b != 1100*time.Millisecond {
		t.Fatalf("base %v once the calm minute left the window, want 1.1s", b)
	}
	f.note(t0.Add(time.Hour), 90*time.Millisecond) // after a long gap
	if b := f.base(); b != 90*time.Millisecond {
		t.Fatalf("base %v after an hour without samples, want 90ms", b)
	}
}
