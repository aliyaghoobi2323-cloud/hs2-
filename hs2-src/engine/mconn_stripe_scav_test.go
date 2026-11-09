package engine

import (
	"sync/atomic"
	"testing"
	"time"
)

// Tests of the stripe as a scavenger (mconn_stripe_scav.go) and of its own
// links (mconn_stripe_pool.go).

// fixPathQ sets the path queue the gate reads, for one test.
func fixPathQ(t *testing.T, q time.Duration) {
	t.Helper()
	p := stripePathQ
	p.mu.Lock()
	p.fixed, p.q = true, q
	p.mu.Unlock()
	resetStripeGlobal()
	t.Cleanup(resetStripeGlobal)
	t.Cleanup(func() {
		p.mu.Lock()
		p.fixed, p.q = false, 0
		p.mu.Unlock()
	})
}

func resetStripeGlobal() {
	g := &stripeGlobal
	g.mu.Lock()
	g.growAt, g.cutAt = time.Time{}, time.Time{}
	g.mu.Unlock()
}

func setPathQ(q time.Duration) {
	p := stripePathQ
	p.mu.Lock()
	p.q = q
	p.mu.Unlock()
}

// gateConn: an exit connection with a primary and n stripe subs (no I/O).
func gateConn(n int) (*mConn, *msub) {
	m := &mConn{}
	p := &msub{m: m, done: make(chan struct{})}
	m.subs = append(m.subs, p)
	for i := 0; i < n; i++ {
		m.subs = append(m.subs, &msub{m: m, stripe: true, idx: i + 1, done: make(chan struct{})})
	}
	m.stN = n
	return m, p
}

// The width starts at 0 (the primary alone), grows by one sub per
// stripeQGrow while the path's queue is under half the target, halves at
// once above it, and never exceeds the live stripe subs.
func TestStripeGateWidth(t *testing.T) {
	old := stripeQTarget.Load()
	t.Cleanup(func() { stripeQTarget.Store(old) })
	stripeQTarget.Store(int64(10 * time.Millisecond))
	fixPathQ(t, time.Millisecond)
	m, p := gateConn(6)
	now := time.Now()
	if w := m.stripeWidthLocked(p, now); w != 0 {
		t.Fatalf("first width %d, want 0", w)
	}
	for i := 1; i <= 8; i++ {
		now = now.Add(stripeQGrow)
		m.stripeWidthLocked(p, now)
	}
	if m.stW != 6 {
		t.Fatalf("width %d after 8 s of no queue, want 6 (the live subs)", m.stW)
	}
	setPathQ(12 * time.Millisecond)
	now = now.Add(stripeQEvery)
	if w := m.stripeWidthLocked(p, now); w != 3 {
		t.Fatalf("width %d after a queue, want 3 (halved)", w)
	}
	now = now.Add(stripeQEvery)
	if w := m.stripeWidthLocked(p, now); w != 1 {
		t.Fatalf("width %d after a second tick of queue, want 1", w)
	}
	// between half the target and the target: held
	setPathQ(7 * time.Millisecond)
	now = now.Add(5 * stripeQGrow)
	if w := m.stripeWidthLocked(p, now); w != 1 {
		t.Fatalf("width %d in the hold band, want 1", w)
	}
	// after a cut no stripe of the process grows for stripeCutHold, and the
	// process grows by one sub per stripeQGrow however many stripes there are
	setPathQ(12 * time.Millisecond)
	now = now.Add(stripeQEvery)
	m.stripeWidthLocked(p, now) // 1 -> 0
	setPathQ(time.Millisecond)
	m2, p2 := gateConn(6)
	m2.stripeWidthLocked(p2, now)
	for i := 0; i < int(stripeCutHold/stripeQGrow)-1; i++ {
		now = now.Add(stripeQGrow)
		m.stripeWidthLocked(p, now)
		m2.stripeWidthLocked(p2, now)
	}
	if m.stW != 0 || m2.stW != 0 {
		t.Fatalf("widths %d %d inside the hold after a cut, want 0 0", m.stW, m2.stW)
	}
	for i := 0; i < 4; i++ {
		now = now.Add(stripeQGrow)
		m.stripeWidthLocked(p, now)
		m2.stripeWidthLocked(p2, now)
	}
	if m.stW+m2.stW != 4 {
		t.Fatalf("widths %d+%d after 4 growth steps, want 4 in all", m.stW, m2.stW)
	}
	// the rank: only the first w stripe subs (in join order) may take chunks
	if r := m.stripeRankLocked(m.subs[1], p); r != 0 {
		t.Fatalf("rank of the first stripe sub %d", r)
	}
	if r := m.stripeRankLocked(m.subs[3], p); r != 2 {
		t.Fatalf("rank of the third stripe sub %d", r)
	}
	// Iran and the key at 0: no gate
	stripeQTarget.Store(0)
	if w := m.stripeWidthLocked(p, now.Add(time.Second)); w != -1 {
		t.Fatalf("gate off: width %d, want -1", w)
	}
	stripeQTarget.Store(int64(10 * time.Millisecond))
	m.iran = true
	if w := m.stripeWidthLocked(p, now.Add(2*time.Second)); w != -1 {
		t.Fatalf("Iran: width %d, want -1", w)
	}
}

// With no exit link tracked there is nothing to measure the path with: no
// gate (the unit tests' pipes, and the edge).
func TestStripeGateUnmeasured(t *testing.T) {
	m, p := gateConn(2)
	now := time.Now()
	if w := m.stripeWidthLocked(p, now); w != -1 {
		t.Fatalf("width %d with no link tracked, want -1", w)
	}
	// and still between two measurements (a regression: it read 0 there)
	if w := m.stripeWidthLocked(p, now.Add(stripeQEvery/2)); w != -1 {
		t.Fatalf("width %d between measurements, want -1", w)
	}
}

// Under a queue the sender puts every chunk on the primary: width 0 is the
// connection without the stripe.
func TestStripeGateSendsOnPrimary(t *testing.T) {
	old := stripeQTarget.Load()
	t.Cleanup(func() { stripeQTarget.Store(old) })
	stripeQTarget.Store(int64(10 * time.Millisecond))
	fixPathQ(t, 50*time.Millisecond)
	m, p := gateConn(3)
	m.lite = true
	m.primary.Store(p)
	for i := 0; i < 20; i++ {
		c := getChunk(mRecData)
		c.n = 100
		if r := m.stripeSendLocked(c); r != stripeSent {
			t.Fatalf("chunk %d: %d, want sent", i, r)
		}
		c.unref()
	}
	if len(p.q) != 20 {
		t.Fatalf("primary has %d chunks, want all 20", len(p.q))
	}
	for _, s := range m.subs[1:] {
		if len(s.q) != 0 {
			t.Fatalf("stripe sub %d got %d chunks under a queue", s.idx, len(s.q))
		}
	}
}

// A link the stripe asked for is set aside when it comes (held: heavy for
// light picks and the TUN lane), taken first by the next stripe pick, and
// goes back if nobody takes it; the pool's target counts the stripe's links
// on top.
func TestStripePoolSetAside(t *testing.T) {
	noStripeShare(t) // HS2_MCONN_STRIPE_SHARE in the environment would let it take a busy link
	old := stripeK.Load()
	t.Cleanup(func() { stripeK.Store(old) })
	stripeK.Store(4)
	caps := uint8(capMConn | capMConnRaw)
	ls := []*mcLink{newMCLink(caps), newMCLink(caps), newMCLink(caps)}
	m := mcManager(ls...)
	now := time.Now()
	m.clock = func() time.Time { return now }
	for _, ml := range m.links {
		ml.users.Store(1) // every link has a light user
	}
	if _, _, ok := m.pickStripe(nil); ok {
		t.Fatal("stripe took a link a user rides")
	}
	if m.stripeWant != 1 {
		t.Fatalf("want %d, want 1", m.stripeWant)
	}
	if x := m.stripeExtra(); x != 1 {
		t.Fatalf("extra %d, want 1 (the link asked for)", x)
	}
	nl := newMCLink(caps)
	m.mu.Lock()
	ml := m.newManaged(nl, 9, now)
	m.links = append(m.links, ml)
	m.mu.Unlock()
	if ml.stripeRes.IsZero() || !stripeOn(ml) || nl.mtr.heavyN.Load() < 1 {
		t.Fatal("the new link was not set aside (held, heavy)")
	}
	if m.stripeWant != 0 {
		t.Fatalf("want %d after the link came, want 0", m.stripeWant)
	}
	m.mu.Lock()
	k := m.planeKeyLocked(planeLight, ml)
	m.mu.Unlock()
	if !k.pk.heavy {
		t.Fatal("a set-aside link is not heavy for light subs")
	}
	if x := m.stripeExtra(); x != 1 {
		t.Fatalf("extra %d, want 1 (the set-aside link)", x)
	}
	l, rel, ok := m.pickStripe(nil)
	if !ok || l != Link(nl) {
		t.Fatalf("picked %v ok=%v, want the set-aside link", l, ok)
	}
	if !ml.stripeRes.IsZero() || !stripeOn(ml) {
		t.Fatal("the hold did not pass to the sub")
	}
	rel()
	if stripeOn(ml) {
		t.Fatal("release left the link held")
	}
	// a set-aside link nobody takes goes back after stripeResFor
	m.mu.Lock()
	m.stripeAskLocked(now)
	ml2 := m.newManaged(newMCLink(caps), 10, now)
	m.links = append(m.links, ml2)
	m.mu.Unlock()
	if !stripeOn(ml2) {
		t.Fatal("second link not set aside")
	}
	now = now.Add(stripeResFor + time.Second)
	if x := m.stripeExtra(); x != 0 || stripeOn(ml2) || !ml2.stripeRes.IsZero() {
		t.Fatalf("lapsed set-aside: extra %d, held %v", x, stripeOn(ml2))
	}
	// a request lapses too
	m.mu.Lock()
	m.stripeAskLocked(now)
	m.mu.Unlock()
	now = now.Add(stripeWantFor + time.Second)
	if x := m.stripeExtra(); x != 0 {
		t.Fatalf("lapsed request still counted: %d", x)
	}
}

// With the key off nothing is set aside and the target is the autopilot's.
func TestStripePoolOff(t *testing.T) {
	old := stripeK.Load()
	t.Cleanup(func() { stripeK.Store(old) })
	stripeK.Store(0)
	m := mcManager(newMCLink(capMConn | capMConnRaw))
	m.mu.Lock()
	m.stripeWant, m.stripeWantUntil = 3, time.Now().Add(time.Minute)
	ml := m.newManaged(newMCLink(capMConn|capMConnRaw), 5, time.Now())
	m.mu.Unlock()
	if !ml.stripeRes.IsZero() || stripeOn(ml) {
		t.Fatal("a link set aside with the key off")
	}
	if x := m.stripeExtra(); x != 0 {
		t.Fatalf("extra %d with the key off", x)
	}
}

// The exit keeps a stripe sub's link heavy for its TUN lane while the sub
// lives, used or not, and lets go when it closes.
func TestStripeExitLaneHeavy(t *testing.T) {
	mtr := &linkMeter{}
	tr := newExitTraffic(nil)
	mtr.stripeN.Add(1)
	tr.heavy[mtr] = 0
	tr.storeHeavy()
	if mtr.heavyN.Load() != 1 {
		t.Fatalf("heavyN %d with a stripe sub, want 1", mtr.heavyN.Load())
	}
	tr.storeHeavy() // gone from this tick's counts: still the stripe
	if mtr.heavyN.Load() != 1 {
		t.Fatalf("heavyN %d after a quiet tick, want 1", mtr.heavyN.Load())
	}
	mtr.stripeN.Add(-1)
	tr.heavy[mtr] = 0
	tr.storeHeavy()
	if mtr.heavyN.Load() != 0 {
		t.Fatalf("heavyN %d after the sub closed, want 0", mtr.heavyN.Load())
	}
}

// The path's queue is the median of the busy links that do not carry the
// asking connection's subs; idle links and its own do not count.
func TestStripePathQOthers(t *testing.T) {
	p := &stripeQ{links: map[*linkMeter]*stripeLinkQ{}}
	own, a, b, c, idle := &linkMeter{}, &linkMeter{}, &linkMeter{}, &linkMeter{}, &linkMeter{}
	p.links[own] = &stripeLinkQ{busy: true, q: 50 * time.Millisecond}
	p.links[a] = &stripeLinkQ{busy: true, q: 2 * time.Millisecond}
	p.links[b] = &stripeLinkQ{busy: true, q: 4 * time.Millisecond}
	p.links[c] = &stripeLinkQ{busy: true, q: 9 * time.Millisecond}
	p.links[idle] = &stripeLinkQ{q: 90 * time.Millisecond}
	p.at = time.Now() // measured just now: no socket read
	q, ok := p.delay(p.at, []*linkMeter{own})
	if !ok || q != 4*time.Millisecond {
		t.Fatalf("q %v ok=%v, want the others' median 4ms", q, ok)
	}
	q, _ = p.delay(p.at, []*linkMeter{own, a, b, c})
	if q != 0 {
		t.Fatalf("q %v with no other busy link, want 0 (nobody to yield to)", q)
	}
	if _, ok := (&stripeQ{links: map[*linkMeter]*stripeLinkQ{}}).delay(time.Now(), nil); ok {
		t.Fatal("measured with no link tracked")
	}
}

// The TUN lane leaves a link a stripe takes at once, even inside
// l3LaneMoveGap (a stripe fills the link); a download alone still waits.
func TestL3LaneLeavesStripeLinkAtOnce(t *testing.T) {
	a, b := laneLink(1, 0), laneLink(2, 0)
	a.stripe, b.stripe = new(atomic.Int32), new(atomic.Int32)
	var clock int64 = 1
	s := laneSet(&clock, a)
	p := flowPreferring(t, []*l3Link{a, b}, b)
	if s.pick(p) != a {
		t.Fatal("first pick not on the only link")
	}
	s.add(b)
	// b turns heavy and the flow moves there (a is light again later)
	clock += int64(10 * time.Second)
	a.heavy.Store(1)
	if s.pick(p) != b {
		t.Fatal("did not leave a heavy link")
	}
	a.heavy.Store(0)
	b.heavy.Store(1) // a download: stays inside the gap
	if s.pick(p) != b {
		t.Fatal("left a download's link inside the gap")
	}
	b.stripe.Store(1) // a stripe: leaves now
	if s.pick(p) != a {
		t.Fatal("stayed on a stripe link inside the gap")
	}
}

// A raw connection striped while the gate holds the width at 0 (a queue on
// the path), and then while it opens and closes: every byte arrives both
// ways, nothing resets.
func TestMConnStripeGatedFromRaw(t *testing.T) {
	for _, flap := range []bool{false, true} {
		t.Run(map[bool]string{false: "held", true: "flapping"}[flap], func(t *testing.T) {
			setStripeRate(t, 200<<10)
			old := stripeQTarget.Load()
			t.Cleanup(func() { stripeQTarget.Store(old) })
			stripeQTarget.Store(int64(3 * time.Millisecond))
			fixPathQ(t, 50*time.Millisecond)
			n, iran, exit := newRawPair(t)
			n.hook = func(l *tLink) { l.exit.rate.Store(1 << 20) }
			n.link(0).exit.rate.Store(1 << 20)
			iran.setStripe(4)
			stop := make(chan struct{})
			defer close(stop)
			if flap {
				go func() {
					q := time.Duration(0)
					for {
						select {
						case <-stop:
							return
						case <-time.After(150 * time.Millisecond):
						}
						q = 50*time.Millisecond - q
						setPathQ(q)
					}
				}()
			}
			down, up := randBytes(6<<20, 51), randBytes(256<<10, 52)
			both(t,
				func() error { return xfer(exit, down, 32<<10, 0) },
				func() error { return readN(iran, down) },
				func() error { return xfer(iran, up, 4<<10, 10*time.Millisecond) },
				func() error { return readN(exit, up) },
			)
			closeAndWait(t, iran, exit)
		})
	}
}

// The relative gate (HS2_MCONN_STRIPE_REL): the target is over the path's
// learned noise floor, and a cut holds only the connection that saw it.
func TestStripeGateRelative(t *testing.T) {
	oldRel, oldT := stripeRel, stripeQTarget.Load()
	t.Cleanup(func() {
		stripeRel = oldRel
		stripeQTarget.Store(oldT)
		p := stripePathQ
		p.mu.Lock()
		p.hist, p.histI, p.bq = nil, 0, 0
		p.mu.Unlock()
	})
	stripeRel = true
	stripeQTarget.Store(int64(3 * time.Millisecond))
	fixPathQ(t, 3*time.Millisecond)
	p := stripePathQ
	// the floor: the lower decile of the path queue, capped
	p.mu.Lock()
	for i := 0; i < stripeBaseN; i++ {
		p.noteBaseLocked(time.Duration(1+i%4) * time.Millisecond) // 1..4 ms noise
	}
	if p.bq != time.Millisecond {
		t.Fatalf("base %v, want 1ms", p.bq)
	}
	for i := 0; i < stripeBaseN; i++ {
		p.noteBaseLocked(50 * time.Millisecond)
	}
	if p.bq != stripeBaseMax {
		t.Fatalf("base %v under a standing queue, want the cap %v", p.bq, stripeBaseMax)
	}
	p.bq = 2 * time.Millisecond
	p.mu.Unlock()
	// 3 ms over a 2 ms floor: 1 ms of queue, under half the target: grows
	m, s := gateConn(6)
	m2, s2 := gateConn(6)
	now := time.Now()
	m.stripeWidthLocked(s, now)
	m2.stripeWidthLocked(s2, now)
	for i := 0; i < 4; i++ {
		now = now.Add(stripeQGrow)
		m.stripeWidthLocked(s, now)
		m2.stripeWidthLocked(s2, now)
	}
	if m.stW+m2.stW < 4 {
		t.Fatalf("widths %d+%d over the floor's noise, want growth", m.stW, m2.stW)
	}
	// 6 ms over a 2 ms floor: a real queue, both halve
	w1, w2 := m.stW, m2.stW
	setPathQ(6 * time.Millisecond)
	now = now.Add(stripeQEvery)
	m.stripeWidthLocked(s, now)
	if m.stW != w1/2 {
		t.Fatalf("width %d after a real queue, want %d", m.stW, w1/2)
	}
	// m2 (no cut of its own) grows again once the bucket refills; m holds
	setPathQ(3 * time.Millisecond)
	for i := 0; i < 2; i++ {
		now = now.Add(stripeQGrow)
		m.stripeWidthLocked(s, now)
		m2.stripeWidthLocked(s2, now)
	}
	if m.stW != w1/2 || m2.stW <= w2 {
		t.Fatalf("widths %d %d after one cut, want %d (held) and > %d", m.stW, m2.stW, w1/2, w2)
	}
}

// HS2_MCONN_STRIPE_BASE_MAX takes fractions of a ms; Iran's upload gate has
// its own cap (HS2_MCONN_STRIPE_UP_BASE_MAX).
func TestStripeBaseMaxCaps(t *testing.T) {
	t.Setenv("HS2_X_MS", "2.5")
	if d := envMsDefault("HS2_X_MS", time.Second); d != 2500*time.Microsecond {
		t.Fatalf("2.5 ms read as %v", d)
	}
	t.Setenv("HS2_X_MS", "junk")
	if d := envMsDefault("HS2_X_MS", time.Second); d != time.Second {
		t.Fatalf("junk read as %v, want the default", d)
	}
	oldB, oldU := stripeBaseMax, stripeUpBaseMax
	t.Cleanup(func() { stripeBaseMax, stripeUpBaseMax = oldB, oldU })
	stripeBaseMax, stripeUpBaseMax = 4*time.Millisecond, 1500*time.Microsecond
	down := &stripeQ{links: map[*linkMeter]*stripeLinkQ{}}
	up := &stripeQ{links: map[*linkMeter]*stripeLinkQ{}, light: true}
	for i := 0; i < stripeBaseN; i++ {
		down.noteBaseLocked(10 * time.Millisecond)
		up.noteBaseLocked(10 * time.Millisecond)
	}
	if down.bq != 4*time.Millisecond || up.bq != 1500*time.Microsecond {
		t.Fatalf("caps: exit %v (want 4ms), Iran upload %v (want 1.5ms)", down.bq, up.bq)
	}
}

// HS2_MCONN_STRIPE_BASE_MAX_MULTI: the download baseline is capped lower only
// while two or more stripes gate at once.
func TestStripeBaseMaxMulti(t *testing.T) {
	oldM := stripeBaseMaxMulti
	t.Cleanup(func() {
		stripeBaseMaxMulti = oldM
		stripeGlobal.mu.Lock()
		stripeGlobal.awin, stripeGlobal.act, stripeGlobal.actPrior = 0, 0, 0
		stripeGlobal.mu.Unlock()
	})
	stripeBaseMaxMulti = time.Millisecond
	p := &stripeQ{links: map[*linkMeter]*stripeLinkQ{}, bq: 3 * time.Millisecond}
	now := time.Now()
	a, b := &mConn{}, &mConn{}
	a.stripeNoteActive(now)
	if got := p.base(); got != 3*time.Millisecond {
		t.Fatalf("one stripe: base %v, want 3ms", got)
	}
	a.stripeNoteActive(now)
	b.stripeNoteActive(now)
	if got := p.base(); got != time.Millisecond {
		t.Fatalf("two stripes: base %v, want the 1ms cap", got)
	}
	up := &stripeQ{links: map[*linkMeter]*stripeLinkQ{}, bq: 3 * time.Millisecond, light: true}
	if got := up.base(); got != 3*time.Millisecond {
		t.Fatalf("upload gate: base %v, want unchanged", got)
	}
}
