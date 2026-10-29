package engine

import (
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// R1 round 5's light QUIET link and the wait for a QUIET link
// (quietlight.go).

// setLight sets QuietLight and QuietWaitMs for one test.
func setLight(t *testing.T, light, waitMs int) {
	t.Helper()
	l, w := QuietLight.Get(), QuietWaitMs.Get()
	QuietLight.Set(light)
	QuietWaitMs.Set(waitMs)
	t.Cleanup(func() { QuietLight.Set(l); QuietWaitMs.Set(w) })
}

// TestQuietCountLight: the light link is one QUIET link more up to
// quietLightMax links, both ends alike (QuietTotal: what the exit dials),
// none past it.
func TestQuietCountLight(t *testing.T) {
	if QuietOff() {
		t.Skip("HS2_TUNE_QUIET=0")
	}
	defer func(x, n, f, m int) { QuietExtra, QuietN, QuietFew, QuietMany = x, n, f, m }(QuietExtra, QuietN, QuietFew, QuietMany)
	QuietExtra, QuietN, QuietFew, QuietMany = 1, 0, 1, 4
	for _, light := range []int{0, 1} {
		setLight(t, light, 0)
		for _, c := range []struct{ max, q int }{{1, 1 + light}, {4, 1 + light}, {5, 1}, {6, 4}, {8, 4}, {300, 4}} {
			if got := QuietCount(c.max); got != c.q {
				t.Fatalf("light %d, max_links %d: QUIET %d, want %d", light, c.max, got, c.q)
			}
			if QuietTotal(c.max) != c.max+c.q {
				t.Fatalf("light %d, max_links %d: total %d", light, c.max, QuietTotal(c.max))
			}
		}
	}
}

// TestLightRule: both ways light and steady over the full 2 s (a game, a
// probe) is light; a call, a one-way flow, an idle connection, a burst or
// half a window are not.
func TestLightRule(t *testing.T) {
	tick := int64(moveTick)
	run := func(n int, tx, rx func(i int) uint64) *moveRule {
		var r moveRule
		var cTx, cRx uint64
		now := int64(time.Hour)
		r.tick(0, 0, 0, now, true, true)
		for i := 0; i < n; i++ {
			now += tick
			cTx += tx(i)
			cRx += rx(i)
			r.tick(cTx, cRx, 0, now, true, true)
		}
		return &r
	}
	every := func(v uint64) func(int) uint64 { return func(int) uint64 { return v } }
	alt := func(v uint64) func(int) uint64 {
		return func(i int) uint64 {
			if i%2 == 0 {
				return v
			}
			return 0
		}
	}
	cases := []struct {
		name   string
		n      int
		tx, rx func(int) uint64
		want   bool
	}{
		{"game", 25, every(6 * 90), every(3 * 350), true},
		{"probe each 200 ms", 25, alt(32), alt(32), true},
		{"game, half a window", 10, every(6 * 90), every(3 * 350), false},
		{"call 1.5 Mbit/s", 25, every(18750), every(18750), false},
		{"one way", 25, every(6 * 90), every(0), false},
		{"idle", 25, every(0), every(0), false},
		{"a page each 5 s", 60, func(i int) uint64 {
			if i%50 == 0 {
				return 600
			}
			return 0
		}, func(i int) uint64 {
			if i%50 < 3 {
				return 64 << 10
			}
			return 0
		}, false},
	}
	for _, c := range cases {
		if got := run(c.n, c.tx, c.rx).lightRule(); got != c.want {
			t.Errorf("%s: light %v, want %v", c.name, got, c.want)
		}
	}
}

// lightRig is newQuietRig (one general QUIET link, one BULK) with a light
// QUIET link placed second, the exit's move hints on both QUIET links.
func lightRig(t *testing.T, rate float64, delay time.Duration) (*quietRig, *classPair) {
	t.Helper()
	angleCores(t)
	if MoveHintOff() {
		t.Skip("HS2_TUNE_MOVE_HINT=0")
	}
	setLight(t, 1, 0)
	r := newQuietRig(t, rate, delay, false)
	lu, ld := newAngleWire(rate, delay), newAngleWire(rate, delay)
	r.wires = append(r.wires, lu, ld)
	lp := newClassPair(t, lu, ld, classCaps(tlscarrier.CapQuiet))
	if lp.edge.caps&tlscarrier.CapLight == 0 {
		t.Fatalf("the QUIET link's caps %#x lack CapLight", lp.edge.caps)
	}
	r.q.place(lp.edge, "")
	go r.serve(lp)
	var gen, light *quietLink
	for _, ql := range r.q.quietLinks() {
		if ql.light.Load() {
			light = ql
		} else {
			gen = ql
		}
	}
	if gen == nil || light == nil || gen.link != r.quiet.edge || light.link != lp.edge {
		t.Fatalf("roles: general %v, light %v", gen, light)
	}
	go openHint(r.ctx, r.quiet.edge, r.q)
	go openHint(r.ctx, lp.edge, r.q)
	go runExitHints(r.ctx)
	waitQuiet(t, "the hint streams up", func() bool { return r.q.hinting(r.quiet.edge) && r.q.hinting(lp.edge) })
	return r, lp
}

// echoCheck reads st's echo of what pattern wrote, byte by byte in order,
// until it ends; got counts the bytes, bad the first one out of order.
func echoCheck(st io.Reader, got *atomic.Int64, bad *atomic.Int64) {
	b := make([]byte, 64<<10)
	var off int64
	for {
		n, err := st.Read(b)
		for i := 0; i < n; i++ {
			if b[i] != phaseByte(off+int64(i)) && bad.Load() < 0 {
				bad.Store(off + int64(i))
			}
		}
		off += int64(n)
		got.Store(off)
		if err != nil {
			return
		}
	}
}

// TestQuietLightGame: a game-like connection (100 small messages a second
// each way) opens on the general QUIET link, moves to the light one within
// a few seconds, then, sending in bulk, moves on to the BULK link (its
// group's second move); every echoed byte comes back in order throughout.
// A call-like one never moves to the light link.
func TestQuietLightGame(t *testing.T) {
	if testing.Short() {
		t.Skip("emulated links, ~10 s")
	}
	r, lp := lightRig(t, 4*angleRateBps, 10*time.Millisecond)
	st := r.open(9, qpEcho)
	gs := st.(*groupStream)
	var got, bad atomic.Int64
	bad.Store(-1)
	go echoCheck(st, &got, &bad)
	var sent int64
	var mu sync.Mutex
	write := func(n int) {
		mu.Lock()
		defer mu.Unlock()
		b := make([]byte, n)
		for i := range b {
			b[i] = phaseByte(sent + int64(i))
		}
		if _, err := st.Write(b); err != nil {
			t.Errorf("write: %v", err)
		}
		sent += int64(n)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tk := time.NewTicker(10 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				write(100)
			}
		}
	}()
	waitQuiet(t, "the game moved to the light link", func() bool { return r.q.movedLight.Load() == 1 })
	if gs.cur().link != lp.edge {
		t.Fatal("moved, but not on the light link")
	}
	n0 := got.Load()
	time.Sleep(500 * time.Millisecond)
	if got.Load() <= n0 {
		t.Fatal("no echo on the light link")
	}
	close(stop)
	<-done
	// now in bulk: the edge's own rule (a backlog up) moves it to BULK
	end := time.Now().Add(6 * time.Second)
	for r.q.moved.Load() == 0 && time.Now().Before(end) {
		write(32 << 10)
	}
	if r.q.moved.Load() != 1 || gs.cur().link != r.bulk.edge || gs.g.Moves() != 2 {
		t.Fatalf("after the bulk: moved to BULK %d, on bulk %v, group moves %d", r.q.moved.Load(), gs.cur().link == r.bulk.edge, gs.g.Moves())
	}
	write(1000)
	mu.Lock()
	want := sent
	mu.Unlock()
	waitQuiet(t, "every byte echoed", func() bool { return got.Load() >= want })
	if b := bad.Load(); b >= 0 {
		t.Fatalf("echo byte %d out of order", b)
	}
	st.Close()

	// a call-like one: never light
	c := r.open(10, qpEcho)
	go io.Copy(io.Discard, c)
	tk := time.NewTicker(20 * time.Millisecond)
	defer tk.Stop()
	frame := make([]byte, 3750)
	for i := 0; i < 150; i++ {
		<-tk.C
		if _, err := c.Write(frame); err != nil {
			t.Fatal(err)
		}
	}
	if r.q.movedLight.Load() != 1 || c.(*groupStream).cur().link != r.quiet.edge {
		t.Fatalf("a call moved to the light link (moves to it %d)", r.q.movedLight.Load())
	}
	c.Close()
}

// TestQuietWaitOpen: with the general QUIET link dead a moment ago, a new
// connection waits (QuietWaitMs) and opens on the QUIET link that comes up
// meanwhile; without the wait it opens on the BULK link at once (the red
// team's R2-12: it would stay there for good).
func TestQuietWaitOpen(t *testing.T) {
	angleCores(t)
	for _, waitMs := range []int{1500, 0} {
		setLight(t, 0, waitMs)
		r := newQuietRig(t, 4*angleRateBps, 10*time.Millisecond, false)
		r.quiet.edge.Close()
		if r.quiet.edge.Alive() {
			t.Fatal("the closed QUIET link is alive")
		}
		nu, nd := newAngleWire(4*angleRateBps, 10*time.Millisecond), newAngleWire(4*angleRateBps, 10*time.Millisecond)
		r.wires = append(r.wires, nu, nd)
		np := newClassPair(t, nu, nd, classCaps(tlscarrier.CapQuiet))
		go r.serve(np)
		go func() {
			time.Sleep(300 * time.Millisecond)
			r.q.place(np.edge, "")
		}()
		t0 := time.Now()
		st := r.open(11, qpEcho)
		took := time.Since(t0)
		gs, isGroup := st.(*groupStream)
		if waitMs > 0 {
			if !isGroup || gs.cur().link != np.edge || r.q.waitedOK.Load() != 1 {
				t.Fatalf("wait %d ms: opened on the new QUIET link %v (waited %d, got %d) after %v", waitMs, isGroup && gs.cur().link == np.edge, r.q.waited.Load(), r.q.waitedOK.Load(), took)
			}
		} else if isGroup || r.q.fallback.Load() != 1 || took > 250*time.Millisecond {
			t.Fatalf("no wait: a group %v, fallback %d, after %v", isGroup, r.q.fallback.Load(), took)
		}
		msg := []byte("after the wait")
		if _, err := st.Write(msg); err != nil {
			t.Fatal(err)
		}
		b := make([]byte, len(msg))
		if _, err := io.ReadFull(st, b); err != nil || string(b) != string(msg) {
			t.Fatalf("echo: %v %q", err, b)
		}
		st.Close()
		r.cancel()
	}
}
