package engine

import (
	"testing"
	"time"
)

// qualRig is a pool of metered fake links on a fake clock whose sockets
// report a round trip (TCP_INFO) the test sets per tick.
type qualRig struct {
	m   *LinkManager
	clk *v2Clock
	lg  *v2Log
	fs  []*meteredFakeLink
	mls []*managedLink
}

func newQualRig(n int) *qualRig {
	m, clk, lg := newV2Manager(&fakeDialer{}, 1, 32, false)
	r := &qualRig{m: m, clk: clk, lg: lg}
	for range n {
		f := newMeteredFake()
		r.fs = append(r.fs, f)
		r.mls = append(r.mls, addManaged(m, f))
	}
	return r
}

// tick sets each link's smoothed round trip (ms) and runs one sample in
// which every link sends a few segments (timed: its deviation counts).
func (r *qualRig) tick(rttMs ...int) {
	for i, f := range r.fs {
		f.mu.Lock()
		f.ts.rtt = uint32(rttMs[i] * 1000)
		f.ts.segsOut += qualTimedSegs
		f.mu.Unlock()
		f.m.wrBytes.Add(qualTimedSegs * 100)
	}
	r.clk.Advance(healthTick)
	r.m.sampleHealth()
}

func (r *qualRig) poor() []bool {
	r.m.mu.RLock()
	defer r.m.mu.RUnlock()
	var p []bool
	for _, ml := range r.mls {
		p = append(p, ml.poor)
	}
	return p
}

func eqBools(a []bool, b ...bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A link whose round trip stays far above its siblings' turns poor after
// qualOn ticks; the siblings do not. One bad link of two is told apart
// (the lower median), two of four as well; three of four, or all of them,
// is the path: none is poor.
func TestQualityDelayAgainstThePool(t *testing.T) {
	cases := []struct {
		name string
		rtt  []int
		want []bool
	}{
		{"one of four", []int{860, 60, 61, 62}, []bool{true, false, false, false}},
		{"one of two", []int{60, 860}, []bool{false, true}},
		{"two of four", []int{860, 900, 60, 62}, []bool{true, true, false, false}},
		{"three of four", []int{860, 900, 870, 62}, []bool{false, false, false, false}},
		{"all", []int{860, 900, 870, 880}, []bool{false, false, false, false}},
		{"under the margin", []int{200, 60, 61, 62}, []bool{false, false, false, false}},
		{"far path, under twice its round trip", []int{450, 250, 260, 255}, []bool{false, false, false, false}},
	}
	for _, c := range cases {
		r := newQualRig(len(c.rtt))
		for range 1 + qualSamplesMin + qualOn - 1 {
			r.tick(c.rtt...)
		}
		if got := r.poor(); !eqBools(got, c.want...) {
			t.Errorf("%s: poor = %v, want %v\n%s", c.name, got, c.want, r.lg)
		}
	}
	// Not before qualOn ticks of evidence (the first tick only sets the
	// baselines, the next qualSamplesMin make the first verdict).
	r := newQualRig(4)
	for range 1 + qualSamplesMin + qualOn - 2 {
		r.tick(860, 60, 61, 62)
	}
	if r.poor()[0] {
		t.Fatal("poor before qualOn ticks of it")
	}
	r.tick(860, 60, 61, 62)
	if !r.poor()[0] || r.lg.count("link 0: round trip 860ms at best where the other links take 61ms") != 1 {
		t.Fatalf("not poor after qualOn ticks: %v\n%s", r.poor(), r.lg)
	}
}

// A link's own load is not held against it: carrying downloads of its own
// next to links that carry little, its round trip may double and swing to
// seconds and it resends what its rate costs, and none of it makes it
// poor. Delay spikes on a quiet link are held against it, and so are a
// busy link's swings when the other links move as much and do not swing.
func TestQualityOwnLoadVersusSpikes(t *testing.T) {
	r := newQualRig(4)
	in := make([]uint32, 4)
	for i := range 15 {
		own := 140 // its own queue
		if i%3 == 0 {
			own = 2000 // a swing, as in loss recovery behind its own downloads
		}
		if i%5 == 0 {
			own = 70 // BBR drains it now and then
		}
		r.fs[0].m.rdBytes.Add(4 << 20) // busy
		qualTraffic(r.fs[0], &in[0], 3000, 300)
		for k := 1; k < 4; k++ {
			qualTraffic(r.fs[k], &in[k], 40, 0)
		}
		r.tick(own, 60, 62, 61)
	}
	if r.poor()[0] {
		t.Fatalf("a busy link's own queue and resends made it poor\n%s", r.lg)
	}
	r = newQualRig(4)
	for i := range 12 {
		spike := 60
		if i%3 == 0 {
			spike = 1600 // a delay spike every few seconds
		}
		r.tick(spike, 60, 62, 61)
	}
	if !r.poor()[0] || r.lg.count("round trip up to 1600ms where") == 0 {
		t.Fatalf("spikes of 1.6 s on a quiet link not seen: %v\n%s", r.poor(), r.lg)
	}
	// Spikes the socket only sees smoothed: its round trip deviates by
	// 300 ms where the others' deviate by 15.
	r = newQualRig(4)
	for i, f := range r.fs {
		f.mu.Lock()
		f.ts.rttVar = 15_000
		if i == 0 {
			f.ts.rttVar = 300_000
		}
		f.mu.Unlock()
	}
	for range 6 {
		r.tick(150, 60, 62, 61)
	}
	if !r.poor()[0] || r.lg.count("round trip up to 1350ms where the other links stay under 121ms") != 1 {
		t.Fatalf("a link deviating by 300 ms not seen: %v\n%s", r.poor(), r.lg)
	}
	r = newQualRig(4)
	for i := range 12 {
		spike := 150
		if i%3 == 0 {
			spike = 1600
		}
		for _, f := range r.fs {
			f.m.rdBytes.Add(4 << 20) // every link busy
		}
		r.tick(spike, 150, 140, 160)
	}
	if !r.poor()[0] || r.lg.count("round trip up to 1600ms where") == 0 {
		t.Fatalf("spikes of 1.6 s on a busy link among busy links not seen: %v\n%s", r.poor(), r.lg)
	}
}

// The floor is the least round trip the socket or the control channel saw
// in the window: a fresh control round trip counts (an idle link's smoothed
// round trip follows a change slowly), an old one does not count twice.
func TestQualityNotesControlRoundTrips(t *testing.T) {
	r := newQualRig(1)
	f, ml := r.fs[0], r.mls[0]
	r.tick(60) // the first sample only sets the baselines
	f.m.rttMicros.Store(900_000)
	f.pong(int64(healthTick), 0)
	r.tick(60)
	if lo, hi := ml.q.lo[0], ml.q.hi[0]; lo != 60_000 || hi != 900_000 {
		t.Fatalf("lo/hi %d/%d, want the socket's 60 ms and the pong's 900 ms", lo, hi)
	}
	r.tick(60) // no pong since
	if hi := ml.q.hi[1]; hi != 60_000 {
		t.Fatalf("the old pong counted again: hi %d", hi)
	}
	f.m.rttMicros.Store(40_000)
	f.pong(int64(healthTick), 0)
	r.tick(60)
	if lo := ml.q.lo[2]; lo != 40_000 {
		t.Fatalf("lo %d, want the pong's 40 ms", lo)
	}
	// The kernel's deviation counts in a tick that sent enough to be timed
	// (what a lost segment would wait: srtt + 4 × rttvar); in one that sent
	// nothing the socket's figures are old, and only a pong counts.
	f.mu.Lock()
	f.ts.rttVar = 300_000
	f.mu.Unlock()
	r.tick(60)
	if hi := ml.q.hi[3]; hi != 1_260_000 {
		t.Fatalf("hi %d, want 60 ms + 4 × 300 ms", hi)
	}
	r.clk.Advance(healthTick)
	r.m.sampleHealth() // nothing sent, no pong
	if lo, hi := ml.q.lo[4], ml.q.hi[4]; lo != 60_000 || hi != 0 {
		t.Fatalf("idle tick: lo/hi %d/%d, want the socket's 60 ms and none", lo, hi)
	}
}

// qualTraffic moves one tick of traffic on f: segs data segments each way,
// resent of them resent each way (both counters as the kernel and the
// exit's pong would say them; in: the segments received so far).
func qualTraffic(f *meteredFakeLink, in *uint32, segs, resent uint32) {
	*in += segs
	f.m.wrBytes.Add(uint64(segs) * 600)
	f.m.rdBytes.Add(uint64(segs) * 600)
	f.retrans.Add(uint64(resent))
	f.m.peerRetrans.Add(uint64(resent))
	f.pong(int64(healthTick), *in)
	f.mu.Lock()
	f.ts.segsOut += segs
	f.mu.Unlock()
}

// A quiet link resending a few percent — far under activeBytes, so the
// loss rule never judges it — turns poor once its resends add up; its
// siblings, resending nothing on the same light traffic, do not. When every
// link resends as much it is the path.
func TestQualityLowVolumeLoss(t *testing.T) {
	run := func(resent []uint32) *qualRig {
		r := newQualRig(len(resent))
		in := make([]uint32, len(resent))
		for range 20 {
			for i, f := range r.fs {
				qualTraffic(f, &in[i], 40, resent[i])
			}
			r.tick(60, 60, 60, 60)
		}
		return r
	}
	r := run([]uint32{2, 0, 0, 0}) // 5% of 40 segments a tick, each way
	if !eqBools(r.poor(), true, false, false, false) || r.lg.count("link 0: resends 4.4% of what it sends where the other links resend 0.0%") != 1 {
		t.Fatalf("a quiet lossy link: poor %v\n%s", r.poor(), r.lg)
	}
	if r = run([]uint32{2, 2, 2, 2}); !eqBools(r.poor(), false, false, false, false) {
		t.Fatalf("path-wide loss: poor %v\n%s", r.poor(), r.lg)
	}
	if r = run([]uint32{2, 2, 0, 0}); !eqBools(r.poor(), true, true, false, false) {
		t.Fatalf("two of four lossy: poor %v\n%s", r.poor(), r.lg)
	}
}

// A link resending a few percent while busy, among links as busy that
// resend none, is poor too (the loss rule judges 12% and over).
func TestQualityBusyLoss(t *testing.T) {
	r := newQualRig(4)
	in := make([]uint32, 4)
	for range 10 {
		for k, f := range r.fs {
			resent := uint32(0)
			if k == 0 {
				resent = 100 // 3%
			}
			f.m.rdBytes.Add(4 << 20)
			qualTraffic(f, &in[k], 3000, resent)
		}
		r.tick(60, 60, 60, 60)
	}
	if !eqBools(r.poor(), true, false, false, false) || r.lg.count("link 0: resends 3.3% of what it sends where the other links resend 0.0%") != 1 {
		t.Fatalf("a busy lossy link among busy ones: poor %v\n%s", r.poor(), r.lg)
	}
}

// A link poor for its resends turns back once its traffic has been clean
// for a while (qualCleanTau), not once the resends decayed over a minute
// have been diluted (2-3 minutes with its users still on it): a link busy
// with downloads that resent 10% for a minute, clean since. A link still
// resending stays poor. One left idle (a keepalive a tick) keeps its
// verdict, since nothing says it is clean, but its busy minute ages out
// within a few minutes (it took nearly seven).
func TestQualityLossClearsOnCleanTraffic(t *testing.T) {
	run := func(after func(*meteredFakeLink, *uint32)) (r *qualRig, back int, tick func(after func(*meteredFakeLink, *uint32))) {
		r = newQualRig(4)
		in := make([]uint32, 4)
		tick = func(after func(*meteredFakeLink, *uint32)) {
			for k, f := range r.fs {
				if k == 0 {
					after(f, &in[k])
				} else {
					f.m.rdBytes.Add(3000 * 600)
					qualTraffic(f, &in[k], 3000, 0)
				}
				f.mu.Lock()
				f.ts.rtt = 60_000
				f.mu.Unlock()
			}
			r.clk.Advance(healthTick)
			r.m.sampleHealth()
		}
		for range 30 { // a minute of 10% on link 0
			tick(func(f *meteredFakeLink, in *uint32) { f.m.rdBytes.Add(3000 * 600); qualTraffic(f, in, 3000, 300) })
		}
		if !r.poor()[0] {
			t.Fatalf("not poor\n%s", r.lg)
		}
		for n := 1; n <= 300; n++ {
			if tick(after); !r.poor()[0] {
				return r, n, tick
			}
		}
		return r, 0, tick
	}
	clean := func(f *meteredFakeLink, in *uint32) { f.m.rdBytes.Add(3000 * 600); qualTraffic(f, in, 3000, 0) }
	r, n, tick := run(clean)
	if d := time.Duration(n) * healthTick; n == 0 || d > qualHold+30*time.Second {
		t.Fatalf("clean and busy: back after %d ticks, want within qualHold and 30 s\n%s", n, r.lg)
	}
	for range 30 {
		if tick(clean); r.poor()[0] {
			t.Fatalf("poor again on clean traffic (its older resends counted again)\n%s", r.lg)
		}
	}
	if r, n, _ = run(func(f *meteredFakeLink, in *uint32) { f.m.rdBytes.Add(3000 * 600); qualTraffic(f, in, 3000, 90) }); n != 0 {
		t.Fatalf("still resending 3%%: back after %d ticks\n%s", n, r.lg)
	}
	keepalive := func(f *meteredFakeLink, _ *uint32) {
		f.m.rdBytes.Add(60)
		f.m.wrBytes.Add(60)
		f.mu.Lock()
		f.ts.segsOut++
		f.mu.Unlock()
	}
	if r, n, tick = run(keepalive); n == 0 || time.Duration(n)*healthTick < 2*time.Minute || time.Duration(n)*healthTick > 4*time.Minute {
		t.Fatalf("idle: back after %d ticks, want 2-4 minutes\n%s", n, r.lg)
	}
	// Back, its first new users' clean traffic does not turn it poor again
	// on what is left of the old resends.
	for range 15 {
		if tick(func(f *meteredFakeLink, in *uint32) { qualTraffic(f, in, 100, 0) }); r.poor()[0] {
			t.Fatalf("poor again on clean traffic after the idle spell\n%s", r.lg)
		}
	}
}

// While the path is slow for most links, or getting over it (stuck.go),
// no link turns poor: links back off unevenly through a squeeze.
func TestQualityNotWhileThePathIsSlow(t *testing.T) {
	r := newQualRig(4)
	r.m.mu.Lock()
	r.m.stuckSlowAt = r.clk.Now() // a slow spell just ended
	r.m.mu.Unlock()
	for range 1 + qualSamplesMin + qualOn {
		r.tick(860, 60, 61, 62)
	}
	if r.poor()[0] {
		t.Fatalf("poor while the path recovers\n%s", r.lg)
	}
	for range time.Duration(stuckRecover) / healthTick {
		r.tick(860, 60, 61, 62)
	}
	if !r.poor()[0] {
		t.Fatalf("not poor once the path is calm\n%s", r.lg)
	}
}

// A poor link stays poor for qualHold at least, and turns back only after
// qualOff ticks clear of half the margins: a link hovering at the margin
// does not flap.
func TestQualityHysteresis(t *testing.T) {
	r := newQualRig(4)
	for range 1 + qualSamplesMin + qualOn - 1 {
		r.tick(860, 60, 61, 62)
	}
	if !r.poor()[0] {
		t.Fatal("not poor")
	}
	// The path is fixed: the floor drops at once, but it stays poor for
	// qualHold.
	n := 0
	for r.poor()[0] {
		r.tick(60, 60, 61, 62)
		if n++; n > 50 {
			t.Fatal("never back")
		}
	}
	if d := time.Duration(n) * healthTick; d < qualHold || d > qualHold+time.Duration(qualOff)*healthTick {
		t.Fatalf("back after %s, want qualHold (%s) and up to qualOff ticks more", d, qualHold)
	}
	if r.lg.count("link 0: on par with the other links again") != 1 {
		t.Fatalf("no log line:\n%s", r.lg)
	}
	// Hovering between half the margin and the margin: neither poor again,
	// nor, once poor, back.
	r = newQualRig(4)
	for range 20 {
		r.tick(160, 60, 61, 62) // 100 ms over: past half of qualDelayAbs, under all of it
	}
	if r.poor()[0] {
		t.Fatal("poor under the margin")
	}
	for range qualOn {
		r.tick(860, 60, 61, 62)
	}
	for range 10 { // the 860 ms tick ages out of the window after qualTicks
		r.tick(160, 60, 61, 62)
	}
	for range 20 {
		r.tick(160, 60, 61, 62)
	}
	if !r.poor()[0] {
		t.Fatal("back while still past half the margin")
	}
}

// poorPool is pickPool with the sampler's count of poor and lagging serving
// links (judgeQuality) taken from the states.
func poorPool(states ...func(*managedLink)) (*LinkManager, []*managedLink) {
	m, ls := pickPool(states...)
	for _, ml := range ls {
		if ml.serving() {
			m.qualServing++
			if ml.poor {
				m.qualPoor++
			} else if ml.lagging(ctrlNow()) {
				m.qualLag++
			}
		}
	}
	return m, ls
}

// A poor link takes no new user while a serving link that is not poor (nor
// lagging) has room, however light it is; when more than half the serving
// links are poor, the usual order applies; when the others are full (the
// refill cap) or lag, the poor one is still used.
func TestPickPassesOverPoorLink(t *testing.T) {
	poor := func(ml *managedLink) { ml.poor = true }
	busy := func(ml *managedLink) { ml.flowing = 30; ml.users.Store(30); ml.pressed = true }
	light := func(ml *managedLink) { ml.flowing = 2; ml.users.Store(2) }
	for at := range 3 { // wherever it sits in the pool
		states := []func(*managedLink){busy, light, busy}
		states[at] = poor
		m, ls := poorPool(states...)
		for i := 0; i < 20; i++ {
			l, rel, ok := m.Pick()
			if !ok || l == ls[at].link {
				t.Fatalf("poor link %d of 3, pick %d: got it (ok %v)", at, i, ok)
			}
			rel()
		}
	}

	m, ls := poorPool(poor, func(ml *managedLink) { poor(ml); busy(ml) }, busy)
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
		t.Fatalf("most links poor: want the lightest link every time, got %v", got)
	}

	// The majority is of the serving links: a poor link that is retiring
	// counts for neither side, and one poor link of two serving is passed
	// over.
	m, ls = poorPool(poor, func(ml *managedLink) { poor(ml); ml.retiring = true }, busy)
	for i := 0; i < 20; i++ {
		l, rel, ok := m.Pick()
		if !ok || l != ls[2].link {
			t.Fatalf("pick %d: want the one serving link that is not poor (ok %v)", i, ok)
		}
		rel()
	}

	// The refill cap: the links that are not poor are full.
	m, ls = poorPool(poor, busy, busy)
	m.mu.Lock()
	if c := m.pickLocked(10); c != ls[0] {
		t.Fatalf("others full: got %v, want the poor link", c)
	}
	m.mu.Unlock()

	// The other one lags: a poor link beats a lagging one.
	slow := func(ml *managedLink) { ml.openSlowTill = ctrlNow() + int64(time.Minute) }
	m, ls = poorPool(poor, slow)
	for i := 0; i < 20; i++ {
		l, rel, ok := m.Pick()
		if !ok || l != ls[0].link {
			t.Fatalf("pick %d: got the lagging link (ok %v)", i, ok)
		}
		rel()
	}

	// Two of four lag and one of the other two is poor: passing over the
	// poor one too would leave one link of four for every new user (the
	// burst cap no longer spreads them: a poor link ranks after a pressed
	// one). The two that do not lag share them, as they would without the
	// poor verdict.
	m, ls = poorPool(poor, slow, slow, nil)
	got = map[Link]int{}
	for i := 0; i < 20; i++ {
		l, rel, ok := m.Pick()
		if !ok {
			t.Fatal("no link")
		}
		got[l]++
		rel()
		if i%5 == 4 { // a sample every 5 new users
			for _, ml := range ls {
				copy(ml.pickHist[1:], ml.pickHist[:len(ml.pickHist)-1])
				ml.pickHist[0], ml.picks = ml.picks, 0
			}
		}
	}
	if got[ls[0].link] < 5 || got[ls[3].link] < 5 || got[ls[1].link]+got[ls[2].link] > 0 {
		t.Fatalf("two lagging, one poor of four: want the two that do not lag to share, got %v", got)
	}
}
