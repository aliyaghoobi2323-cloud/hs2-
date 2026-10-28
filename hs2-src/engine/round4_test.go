package engine

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"
)

// R1 round 4: the placement by users (place.go), the QUIET cap
// (quietcap.go) and the realtime lane (rtlane.go). XShare's are in
// xshare_test.go.

// placeLinks is n placement candidates on links of their own.
func placeLinks(n int) []*placeCand {
	out := make([]*placeCand, n)
	for i := range out {
		out[i] = &placeCand{link: &mtcpLink{}}
	}
	return out
}

// placeOne places a connection of tag on one of cands and books it.
func placeOne(t *testing.T, b *placeBook, cands []*placeCand, tag uint32) *placeCand {
	t.Helper()
	c := b.choose(cands, tag)
	if c == nil {
		t.Fatal("no link chosen")
	}
	b.add(c.link, tag)
	return c
}

// linksOf is the candidates where tag has moved connections.
func linksOf(b *placeBook, cands []*placeCand, tag uint32) int {
	n := 0
	for _, c := range cands {
		if b.by[c.link][tag] > 0 {
			n++
		}
	}
	return n
}

// TestPlaceUsers: a lone heavy user spreads over every link (a policer on
// each connection wants that); eight heavy users that move together take a
// link each; a viewer then goes where one user is; in one_vs_16 the heavy
// user keeps off the one-connection user's link.
func TestPlaceUsers(t *testing.T) {
	defer func(v int) { PlaceUsers.Set(v) }(PlaceUsers.Get())
	PlaceUsers.Set(1) // every user held (the browser's case below sets its own)
	t.Run("lone", func(t *testing.T) {
		var b placeBook
		cands := placeLinks(8)
		for range 16 {
			placeOne(t, &b, cands, 1)
		}
		for i, c := range cands {
			if b.by[c.link][1] != 2 {
				t.Fatalf("link %d has %d of the lone user's 16 connections, not 2", i, b.by[c.link][1])
			}
		}
	})
	t.Run("eight", func(t *testing.T) {
		var b placeBook
		cands := placeLinks(8)
		due := map[uint32]int{}
		for u := uint32(1); u <= 8; u++ {
			due[u] = 16
		}
		b.setDue(due)
		for range 16 { // interleaved, as the mover meets them
			for u := uint32(1); u <= 8; u++ {
				placeOne(t, &b, cands, u)
			}
		}
		b.setDue(nil)
		for u := uint32(1); u <= 8; u++ {
			if n := linksOf(&b, cands, u); n != 1 {
				t.Fatalf("user %d on %d links, not 1", u, n)
			}
		}
		for i, c := range cands {
			if len(b.by[c.link]) != 1 {
				t.Fatalf("link %d has %d users", i, len(b.by[c.link]))
			}
		}
		// a viewer: a link with one user, and a second viewer another one
		v1 := placeOne(t, &b, cands, 100)
		v2 := placeOne(t, &b, cands, 101)
		if len(b.by[v1.link]) != 2 || len(b.by[v2.link]) != 2 || v1 == v2 {
			t.Fatalf("viewers on links of %d and %d users (same %v)", len(b.by[v1.link]), len(b.by[v2.link]), v1 == v2)
		}
	})
	t.Run("one_vs_16", func(t *testing.T) {
		var b placeBook
		cands := placeLinks(8)
		one := placeOne(t, &b, cands, 1)
		for range 16 {
			if c := placeOne(t, &b, cands, 2); c == one {
				t.Fatal("the heavy user went onto the one-connection user's link")
			}
		}
		if n := linksOf(&b, cands, 2); n != 4 {
			t.Fatalf("the heavy user on %d links, ceil(8/2) = 4 due", n)
		}
	})
	// a browser's six connections next to eight heavy users held a link
	// each: held to one link with every user held (PlaceUsers 1), spread
	// over the links with the fewest users with only heavy users held (2)
	for _, mode := range []int{1, 2} {
		t.Run(fmt.Sprintf("browser, mode %d", mode), func(t *testing.T) {
			defer func(v int) { PlaceUsers.Set(v) }(PlaceUsers.Get())
			PlaceUsers.Set(mode) // (restored to 1 for the cases after)
			var b placeBook
			cands := placeLinks(8)
			due := map[uint32]int{}
			for u := uint32(1); u <= 8; u++ {
				due[u] = 16
			}
			b.setDue(due)
			for range 16 {
				for u := uint32(1); u <= 8; u++ {
					placeOne(t, &b, cands, u)
				}
			}
			b.setDue(nil)
			for range 6 {
				placeOne(t, &b, cands, 50)
			}
			n := linksOf(&b, cands, 50)
			t.Logf("the browser on %d links", n)
			if mode == 1 && n != 1 || mode == 2 && n < 4 {
				t.Fatalf("mode %d: the browser's 6 connections on %d links", mode, n)
			}
			for u := uint32(1); u <= 8; u++ {
				if linksOf(&b, cands, u) != 1 {
					t.Fatalf("mode %d: heavy user %d on %d links", mode, u, linksOf(&b, cands, u))
				}
			}
		})
	}
	// mode 3's word: heavy from placeHeavy connections, moved or moving
	t.Run("heavy", func(t *testing.T) {
		var b placeBook
		cands := placeLinks(2)
		for range 6 {
			b.add(cands[0].link, 5)
		}
		b.add(cands[1].link, 6)
		b.setDue(map[uint32]int{5: 2})
		if !b.heavy(5) || b.heavy(6) {
			t.Fatalf("heavy: user 5 (6 moved, 2 moving) %v, user 6 (1) %v", b.heavy(5), b.heavy(6))
		}
		b.setDue(nil)
		if b.heavy(5) {
			t.Fatal("user 5 heavy with 6 connections")
		}
	})
	t.Run("drop", func(t *testing.T) {
		var b placeBook
		cands := placeLinks(2)
		c := placeOne(t, &b, cands, 1)
		b.drop(c.link, 1)
		b.drop(c.link, 1) // a second drop is harmless
		if len(b.by) != 0 {
			t.Fatalf("the book kept %v", b.by)
		}
		if b.line() == "" {
			t.Fatal("no line")
		}
	})
}

// TestCapWindow: the cap fires once, past QuietCap within quietCapWin, and
// not with QuietCap 0.
func TestCapWindow(t *testing.T) {
	defer func(v int) { QuietCap.Set(v) }(QuietCap.Get())
	QuietCap.Set(64 << 10)
	ms := int64(time.Millisecond)
	var w capWindow
	if w.add(32<<10, 0) || w.add(32<<10, 100*ms) {
		t.Fatal("fired at the cap, not past it")
	}
	// a new window: the count starts again
	if w.add(32<<10, 700*ms) || w.add(32<<10, 800*ms) {
		t.Fatal("the old window counted")
	}
	if !w.add(1, 900*ms) {
		t.Fatal("did not fire past the cap")
	}
	if w.add(64<<10, 950*ms) {
		t.Fatal("fired twice")
	}
	QuietCap.Set(0)
	var z capWindow
	if z.add(1<<20, 0) {
		t.Fatal("fired with QuietCap 0")
	}
}

// TestHoldFor: the hold ends at the switch, at the group's end, or after
// QuietHoldMs.
func TestHoldFor(t *testing.T) {
	defer func(v int) { QuietHoldMs = v }(QuietHoldMs)
	QuietHoldMs = 50
	sw, done := make(chan struct{}), make(chan struct{})
	close(sw)
	if !holdFor(sw, done) {
		t.Fatal("a switch did not end the hold")
	}
	t0 := time.Now()
	if holdFor(make(chan struct{}), done) || time.Since(t0) < 40*time.Millisecond {
		t.Fatalf("the hold's end: %v", time.Since(t0))
	}
	close(done)
	if holdFor(make(chan struct{}), done) {
		t.Fatal("the group's end read as a switch")
	}
}

// capRig is hintRig with QuietCap at 64 KiB (and TwoWay off: the cap's own
// measure; with TwoWay a new group is pending for its first tick).
func capRig(t *testing.T) *quietRig {
	t.Helper()
	old, otw := QuietCap.Get(), TwoWay.Get()
	QuietCap.Set(64 << 10)
	TwoWay.Set(0)
	r := hintRig(t, 4*angleRateBps, 10*time.Millisecond)
	t.Cleanup(func() {
		quiesce(r) // (first: the rig's loops read the switches)
		QuietCap.Set(old)
		TwoWay.Set(otw)
	})
	return r
}

// quiesce ends a rig's context and waits for its loops (the mover, the
// exit's hint loop: a move tick each) to see it, so a test may set the
// package's switches again without a race.
func quiesce(r *quietRig) {
	r.cancel()
	time.Sleep(3 * moveTick)
}

// TestQuietCapMovesDownload: with the cap a download leaves the QUIET link
// once it passed 64 KiB there (the hint at once, its next write held for
// the move), long before the 300 ms of backlog the hint alone waits for,
// and the QUIET link carried less than 128 KiB of it; the bytes arrive.
func TestQuietCapMovesDownload(t *testing.T) {
	angleCores(t)
	r := capRig(t)
	st := r.open(7, qpDown)
	var got int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		b := make([]byte, 64<<10)
		for got < 2<<20 {
			n, err := st.Read(b)
			got += int64(n)
			if err != nil {
				return
			}
		}
	}()
	t0 := time.Now()
	waitQuiet(t, "the download moved on the cap", func() bool { return r.q.moved.Load() == 1 })
	moved := time.Since(t0)
	id := st.(*groupStream).g.ID()
	exitGroups.mu.Lock()
	eg := exitGroups.m[id]
	exitGroups.mu.Unlock()
	if eg == nil {
		t.Fatal("the exit's group is gone")
	}
	onQuiet := eg.Main().TxBytes()
	t.Logf("moved %v after it opened; %d B written on the QUIET link", moved, onQuiet)
	if moved > 250*time.Millisecond {
		t.Fatalf("the cap moved it after %v", moved)
	}
	if onQuiet >= 128<<10 {
		t.Fatalf("%d B on the QUIET link, the cap is 64 KiB", onQuiet)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("2 MiB did not arrive after the move")
	}
	if got < 2<<20 {
		t.Fatalf("read %d B", got)
	}
	st.Close()
}

// TestQuietCapMovesUpload: an upload with a backlog passes the cap at the
// edge and moves at once.
func TestQuietCapMovesUpload(t *testing.T) {
	angleCores(t)
	r := capRig(t)
	st := r.open(8, qpUp)
	b := make([]byte, 32<<10)
	t0 := time.Now()
	go func() {
		for {
			if _, err := st.Write(b); err != nil {
				return
			}
		}
	}()
	waitQuiet(t, "the upload moved on the cap", func() bool { return r.q.moved.Load() == 1 })
	if d := time.Since(t0); d > 250*time.Millisecond {
		t.Fatalf("the cap moved the upload after %v", d)
	}
	gs := st.(*groupStream)
	if n := gs.g.Main().TxBytes(); n >= 128<<10 {
		t.Fatalf("%d B of the upload on the QUIET link", n)
	}
	n0 := r.upCount(qpUp).Load()
	time.Sleep(300 * time.Millisecond)
	if r.upCount(qpUp).Load() <= n0 {
		t.Fatal("nothing arrived after the move")
	}
	st.Close()
}

// TestQuietCapCallStays: a 1.5 Mbit/s call (TestHintCallStays's) never
// passes the cap: its frames are small writes, a key frame of 18.75 KB
// each 2 s is far under 64 KiB in any 500 ms.
func TestQuietCapCallStays(t *testing.T) {
	angleCores(t)
	if testing.Short() {
		t.Skip("emulated links, 4 s")
	}
	r := capRig(t)
	st := r.open(9, qpEcho)
	go io.Copy(io.Discard, st)
	stop := time.After(4 * time.Second)
	tk := time.NewTicker(20 * time.Millisecond)
	defer tk.Stop()
	frame := make([]byte, 3750)
	key := make([]byte, 5*3750)
	for i := 0; ; i++ {
		select {
		case <-stop:
			if r.q.moved.Load() != 0 {
				t.Fatalf("a 1.5 Mbit/s call moved (%d)", r.q.moved.Load())
			}
			st.Close()
			return
		case <-tk.C:
		}
		p := frame
		if i%100 == 0 {
			p = key
		}
		binary.BigEndian.PutUint32(p, uint32(i))
		if _, err := st.Write(p); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			t.Fatal(err)
		}
	}
}

// ---- the two-way rule ----

// TestTwoWayState: a call's steady other way is pending in its first second
// and steady after it; a download's request (a few hundred bytes) and a
// stream that misses ticks are not; past RTMax neither.
func TestTwoWayState(t *testing.T) {
	ring := func(v ...uint32) []uint32 { return v }
	call := ring(18750, 18750, 18750, 18750, 18750, 18750, 18750, 18750, 18750, 18750)
	bursts := ring(0, 0, 40000, 0, 0, 0, 0, 60000, 0, 0) // a call's frames held back
	none := make([]uint32, 10)
	cases := []struct {
		name        string
		user, other []uint32
		seen        int
		want        int
	}{
		{"call 0.3 s", call, call, 3, twoWayPending},
		{"call 1 s", call, call, 10, twoWayYes},
		{"call 1 s, its download held back", call, bursts, 10, twoWayYes},
		{"call, two ticks missed", ring(18750, 0, 18750, 18750, 18750, 18750, 18750, 18750, 0, 18750), call, 10, twoWayYes},
		{"call held back, four ticks missed", ring(0, 0, 37500, 18750, 0, 18750, 56250, 0, 18750, 18750), call, 10, twoWayYes},
		{"call held back 0.4 s in", ring(0, 0, 0, 0, 0, 0, 0, 37500, 0, 18750), none, 4, twoWayPending},
		{"request", ring(300, 0, 0, 0, 0, 0, 0, 0, 0, 0), call, 3, twoWayNo},
		{"request, later", ring(0, 0, 0, 0, 0, 0, 0, 0, 0, 300), call, 10, twoWayNo},
		{"every other tick", ring(4000, 0, 4000, 0, 4000, 0, 4000, 0, 4000, 0), call, 10, twoWayNo},
		{"past RTMax: an upload", ring(60000, 60000, 60000, 60000, 60000, 60000, 60000, 60000, 60000, 60000), none, 10, twoWayNo},
		{"past RTMax 0.3 s in", ring(0, 0, 0, 0, 0, 0, 0, 60000, 60000, 60000), none, 3, twoWayNo},
		{"a slow upload, nothing back", call, none, 10, twoWayNo},
		{"nothing seen", call, call, 0, twoWayPending},
	}
	for _, c := range cases {
		if got := twoWayState(c.user, c.other, len(c.user)-1, c.seen); got != c.want {
			t.Fatalf("%s: %d, want %d", c.name, got, c.want)
		}
	}
	// rings of their own lengths (the edge's are moveSlots long)
	long := make([]uint32, moveSlots)
	for i := range long {
		long[i] = 18750
	}
	if got := twoWayState(long, long, 3, 20); got != twoWayYes {
		t.Fatalf("a call on the edge's rings: %d", got)
	}
}

// twoWayRig is hintRig with TwoWay set to on.
func twoWayRig(t *testing.T, on bool) *quietRig {
	t.Helper()
	old := TwoWay.Get()
	TwoWay.Set(0)
	if on {
		TwoWay.Set(1)
	}
	r := hintRig(t, 4*angleRateBps, 10*time.Millisecond)
	t.Cleanup(func() {
		quiesce(r)
		TwoWay.Set(old)
	})
	return r
}

// callOn sends a 1.5 Mbit/s call (3750 B each 20 ms, a 19 KB key frame each
// 2 s) on st for d.
func callOn(t *testing.T, st stream, d time.Duration) {
	t.Helper()
	stop := time.After(d)
	tk := time.NewTicker(20 * time.Millisecond)
	defer tk.Stop()
	frame := make([]byte, 3750)
	key := make([]byte, 5*3750)
	for i := 0; ; i++ {
		select {
		case <-stop:
			return
		case <-tk.C:
		}
		p := frame
		if i%100 == 0 {
			p = key
		}
		if _, err := st.Write(p); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			t.Fatal(err)
		}
	}
}

// TestTwoWayKeepsCall: a connection whose download direction is backlogged
// (the exit writes for good: a call held back on a full QUIET link looks
// so) while its user sends a steady call stream: the exit's hint moves it
// within 3 s; with TwoWay it stays on QUIET; and a plain download still
// moves with TwoWay on.
func TestTwoWayKeepsCall(t *testing.T) {
	angleCores(t)
	if testing.Short() {
		t.Skip("emulated links, 9 s")
	}
	for _, on := range []bool{false, true} {
		r := twoWayRig(t, on)
		k0 := twoWayKept.Load()
		st := r.open(10, qpDuplex)
		go io.Copy(io.Discard, st)
		callOn(t, st, 3*time.Second)
		moved, kept := r.q.moved.Load(), twoWayKept.Load()-k0
		t.Logf("TwoWay %v: moved %d, kept %d", on, moved, kept)
		switch {
		case !on && moved != 1:
			t.Fatalf("TwoWay off: the backlogged connection moved %d times", moved)
		case on && (moved != 0 || kept == 0):
			t.Fatalf("TwoWay on: moved %d, kept %d", moved, kept)
		}
		st.Close()
		quiesce(r)
	}
	r := twoWayRig(t, true)
	st := r.open(11, qpDown)
	go io.Copy(io.Discard, st)
	t0 := time.Now()
	waitQuiet(t, "a download with TwoWay on", func() bool { return r.q.moved.Load() == 1 })
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("a download moved after %v with TwoWay on", d)
	}
	st.Close()
}

// ---- the realtime lane ----

// rtClassifier is a flow classifier ticked with per-tick bytes.
func rtClassifier(perTick []uint64) (*flowClassifier, bool) {
	var c flowClassifier
	var f flowStats
	confirmed := false
	for i, b := range perTick {
		f.tx.Add(b)
		_, _, confirmed = c.Tick(&f, time.Duration(i+1)*classTick)
	}
	return &c, confirmed
}

// TestRTAllowance: a call (18.75 KB a tick, a key frame each 2 s) and a
// confirmed light flow get a realtime allowance; a video segment's burst, a
// steady flow past RTMax, a short flow and a one-tick blip do not.
func TestRTAllowance(t *testing.T) {
	call := make([]uint64, 40)
	for i := range call {
		call[i] = 18750
		if i%20 == 0 {
			call[i] += 4 * 3750
		}
	}
	light := make([]uint64, 40)
	for i := range light {
		light[i] = 600
	}
	seg := make([]uint64, 40)
	for i := 0; i < 8; i++ {
		seg[20+i] = 60 << 10 // 480 KiB in 0.8 s, then nothing
	}
	fast := make([]uint64, 40)
	for i := range fast {
		fast[i] = 60 << 10 // ~5 Mbit/s steady
	}
	short := make([]uint64, 10)
	for i := range short {
		short[i] = 18750
	}
	cases := []struct {
		name string
		b    []uint64
		ok   bool
	}{{"call", call, true}, {"light", light, true}, {"segment", seg, false}, {"past RTMax", fast, false}, {"1 s old", short, false}}
	for _, c := range cases {
		cl, conf := rtClassifier(c.b)
		a := rtAllowance(cl, conf)
		t.Logf("%s: allowance %d B a tick (confirmed light %v)", c.name, a, conf)
		if (a > 0) != c.ok {
			t.Fatalf("%s: allowance %d", c.name, a)
		}
	}
	cl, conf := rtClassifier(call)
	if a := rtAllowance(cl, conf); a < 2*18750 {
		t.Fatalf("the call's allowance %d is under twice its tick", a)
	}
}

// TestSchedRTLane: a steady flow in LaneRT next to a burst of the latency
// lane and bulk: its frames wait no more than the frame in progress, items
// of every flow keep their order, and a user's realtime credit holds it to
// RTMax (what it pushes past goes as usual).
func TestSchedRTLane(t *testing.T) {
	m := newSchedSim(t, 10, 5)
	for range 4 {
		m.add(simFlow{user: 1, kind: simBacklog, lane: LaneBulk})
	}
	for u := uint32(10); u < 30; u++ { // twenty new flows' first bytes
		m.add(simFlow{user: u, kind: simRandom, lane: LaneLatency, size: 2000, gap: 60 * time.Millisecond})
	}
	call := m.add(simFlow{user: 2, kind: simPaced, lane: LaneRTSteady, size: 3750, rate: 1.5e6 / 8})
	game := m.add(simFlow{user: 3, kind: simPaced, lane: LaneRT, size: 300, rate: 30 * 300})
	m.run(5 * time.Second)
	worst := map[*simFlow]time.Duration{}
	n := map[*simFlow]int{}
	for _, s := range m.served {
		if (s.f == call || s.f == game) && s.at > time.Second {
			worst[s.f] = max(worst[s.f], s.wait)
			n[s.f]++
		}
	}
	bound := m.dur(BulkFrameSize(m.r)) + m.dur(2000)
	t.Logf("call frames %d, worst wait %v; game frames %d, worst wait %v (a bulk frame and a lane frame %v)", n[call], worst[call], n[game], worst[game], bound)
	if n[call] == 0 || worst[call] > bound {
		t.Fatalf("the steady realtime lane: %d frames, worst wait %v", n[call], worst[call])
	}
	// the light realtime lane is ahead of the steady one: a game frame
	// waits for the frame in progress, never behind a call frame queued
	if n[game] == 0 || worst[game] > m.dur(max(BulkFrameSize(m.r), 3750)) {
		t.Fatalf("the light realtime lane: %d frames, worst wait %v", n[game], worst[game])
	}
	// the credit: a user pushing realtime at twice RTMax
	m2 := newSchedSim(t, 100, 6)
	m2.add(simFlow{user: 3, kind: simBacklog, lane: LaneBulk})
	m2.add(simFlow{user: 4, kind: simPaced, lane: LaneRTSteady, size: 4000, rate: 2 * float64(RTMax) / 8})
	m2.run(5 * time.Second)
	before := m2.s.Stats().Served[LaneRT]
	rate := float64(before) / 5
	t.Logf("a user pushing realtime at 2×RTMax: %.2f Mbit/s in the realtime lane (RTMax %.1f)", rate*8/1e6, float64(RTMax)/1e6)
	if rate*8 > 1.1*float64(RTMax)+float64(rtBurst*8)/5 {
		t.Fatalf("the realtime lane took %.2f Mbit/s of one user", rate*8/1e6)
	}
}
