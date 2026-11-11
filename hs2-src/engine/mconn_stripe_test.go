package engine

import (
	"runtime"
	"testing"
	"time"
)

// Tests of HS2_MCONN_STRIPE (mconn_stripe.go).

// setStripeRate sets the per-sub growth rate for a test (restored after it).
func setStripeRate(t *testing.T, bps int64) {
	t.Helper()
	old, oldHot := stripeSubRate.Load(), stripeHotTicks.Load()
	stripeSubRate.Store(bps)
	stripeHotTicks.Store(1) // grow at the first sample (TestStripeHot checks the wait)
	t.Cleanup(func() { stripeSubRate.Store(old); stripeHotTicks.Store(oldHot) })
}

// rateLinks holds every link's exit-side writes (the download direction) to
// bps from the moment it opens.
func rateLinks(n *tNet, bps int64) {
	n.hook = func(l *tLink) { l.exit.rate.Store(bps) }
}

func stripeSubs(m *mConn) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.liveStripeLocked()
}

// wireBytes: what the exit wrote on every link (records included).
func wireBytes(n *tNet) (total int64, used int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, l := range n.links {
		w := l.exit.wrote.Load()
		total += w
		if w > 256<<10 {
			used++
		}
	}
	return
}

// stripeDownload runs one heavy download on a pair whose links each carry at most
// perLink bytes/s and returns how long it took.
func stripeDownload(t *testing.T, n *tNet, iran, exit *mConn, size int) time.Duration {
	t.Helper()
	data := randBytes(size, 77)
	t0 := time.Now()
	both(t,
		func() error { return xfer(exit, data, 32<<10, 0) },
		func() error { return readN(iran, data) },
	)
	return time.Since(t0)
}

// A heavy download on a lite connection grows to K stripe subs, every byte
// arrives once and in order, and it runs several times faster than one
// link allows. Nothing is duplicated: the wire carries the payload plus
// record headers only.
func TestMConnStripeGrowsAndSpreads(t *testing.T) {
	before := runtime.NumGoroutine()
	setStripeRate(t, 200<<10)
	const perLink, size = 1 << 20, 12 << 20
	setLite(t, true)
	resetLiteBudget()
	n := &tNet{exit: newMConnExit(31)}
	rateLinks(n, perLink)
	iran, err := newMConnIran(31, n.open)
	if err != nil {
		t.Fatal(err)
	}
	iran.setStripe(4, nil)
	exit := n.exit
	d := stripeDownload(t, n, iran, exit, size)
	if k := stripeSubs(iran); k != 3 {
		t.Fatalf("%d stripe subs, want 3 (K=4)", k)
	}
	if n.count(planeStripe) != 3 {
		t.Fatalf("%d stripe opens, want 3", n.count(planeStripe))
	}
	total, used := wireBytes(n)
	if used < 4 {
		t.Fatalf("download used %d links, want 4", used)
	}
	if over := float64(total)/float64(size) - 1; over > 0.01 {
		t.Fatalf("wire %d bytes for %d: %.2f%% over (duplication?)", total, size, over*100)
	}
	// one link alone: 12 s; ramping to four takes ~3 s
	if d > 9*time.Second {
		t.Fatalf("download took %v, one link alone takes %v", d, time.Duration(size/perLink)*time.Second)
	}
	t.Logf("12 MiB in %v over %d links", d.Round(time.Millisecond), used)
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// With the key off (stK 0) the same download stays on its one sub.
func TestMConnStripeOffStaysSingle(t *testing.T) {
	setStripeRate(t, 200<<10)
	setLite(t, true)
	resetLiteBudget()
	n := &tNet{exit: newMConnExit(32)}
	rateLinks(n, 4<<20)
	iran, err := newMConnIran(32, n.open)
	if err != nil {
		t.Fatal(err)
	}
	stripeDownload(t, n, iran, n.exit, 10<<20)
	if n.count(planeStripe) != 0 || stripeSubs(iran) != 0 {
		t.Fatal("stripe subs opened with the key off")
	}
	closeAndWait(t, iran, n.exit)
}

// A raw first sub (production lite) is upgraded by the first stripe sub:
// the bytes before the boundary went raw, the rest is striped, all intact
// both ways.
func TestMConnStripeFromRaw(t *testing.T) {
	before := runtime.NumGoroutine()
	setStripeRate(t, 200<<10)
	n, iran, exit := newRawPair(t)
	n.hook = func(l *tLink) { l.exit.rate.Store(1 << 20) }
	n.link(0).exit.rate.Store(1 << 20)
	iran.setStripe(3, nil)
	down, up := randBytes(8<<20, 41), randBytes(256<<10, 42)
	both(t,
		func() error { return xfer(exit, down, 32<<10, 0) },
		func() error { return readN(iran, down) },
		func() error { return xfer(iran, up, 4<<10, 10*time.Millisecond) },
		func() error { return readN(exit, up) },
	)
	// both sides write records now (a raw reader switches only at its next
	// Read past the boundary, so rawR may still be set here)
	if iran.rawW.Load() || exit.rawW.Load() {
		t.Fatal("not upgraded")
	}
	if k := stripeSubs(iran); k != 2 {
		t.Fatalf("%d stripe subs, want 2 (K=3)", k)
	}
	if k := stripeSubs(exit); k != 2 {
		t.Fatalf("exit knows %d stripe subs, want 2", k)
	}
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// A stripe sub whose link stalls for a while (loss recovery) only delays:
// the sender stops giving it chunks once its queue is full, the receiver
// holds the rest in its bounded reorder buffer, and every byte arrives.
func TestMConnStripeStalledSub(t *testing.T) {
	setStripeRate(t, 200<<10)
	setLite(t, true)
	resetLiteBudget()
	n := &tNet{exit: newMConnExit(33)}
	rateLinks(n, 2<<20)
	iran, err := newMConnIran(33, n.open)
	if err != nil {
		t.Fatal(err)
	}
	iran.setStripe(3, nil)
	data := randBytes(16<<20, 9)
	go func() {
		mWaitFor(t, "stripe", 10*time.Second, func() bool { return stripeSubs(iran) == 2 })
		n.link(1).exit.stall(700 * time.Millisecond)
	}()
	both(t,
		func() error { return xfer(n.exit, data, 32<<10, 0) },
		func() error { return readN(iran, data) },
	)
	iran.r.mu.Lock()
	lim := iran.r.oooMax
	iran.r.mu.Unlock()
	if lim != stripeReorder {
		t.Fatalf("reorder bound %d, want %d", lim, stripeReorder)
	}
	closeAndWait(t, iran, n.exit)
}

// The slot of a striping connection is given back when it ends.
func TestMConnStripeSlots(t *testing.T) {
	setStripeRate(t, 200<<10)
	setLite(t, true)
	a0 := stripeStat.active.Load()
	n := &tNet{exit: newMConnExit(34)}
	rateLinks(n, 2<<20)
	iran, err := newMConnIran(34, n.open)
	if err != nil {
		t.Fatal(err)
	}
	iran.setStripe(2, nil)
	stripeDownload(t, n, iran, n.exit, 6<<20)
	if stripeStat.active.Load() != a0+1 {
		t.Fatalf("active %d, want %d", stripeStat.active.Load(), a0+1)
	}
	closeAndWait(t, iran, n.exit)
	if stripeStat.active.Load() != a0 {
		t.Fatalf("slot not given back: %d", stripeStat.active.Load())
	}
}

func TestParseStripe(t *testing.T) {
	for v, want := range map[string]int{"": 0, "0": 0, "off": 0, "1": 0, "x": 0, "on": 8, "2": 2, "6": 6, "99": stripeKMax} {
		if got := parseStripe(v); got != want {
			t.Errorf("parseStripe(%q) = %d, want %d", v, got, want)
		}
	}
}

// The capability (ext bit 0) is announced only with lite and the key on, and
// never in the caps byte.
func TestStripeCap(t *testing.T) {
	withMConn(t, true)
	setLite(t, true)
	old := stripeK.Load()
	t.Cleanup(func() { stripeK.Store(old) })
	stripeK.Store(0)
	if infoExt()&capExtMConnStripe != 0 {
		t.Fatal("cap announced with the key off")
	}
	stripeK.Store(4)
	if infoExt()&capExtMConnStripe == 0 {
		t.Fatal("cap not announced with the key on")
	}
	if mconnCap() != capMConn|capMConnRaw {
		t.Fatalf("caps byte changed: %#x", mconnCap())
	}
	setLite(t, false)
	if infoExt()&capExtMConnStripe != 0 {
		t.Fatal("cap announced without lite")
	}
	pi := &peerInfo{V2: true, Caps: capMConn | capMConnRaw, Ext: capExtMConnStripe}
	if !pi.mconnStripe() {
		t.Fatal("peer cap not read")
	}
	pi.Ext = 0
	if pi.mconnStripe() {
		t.Fatal("older peer read as striping")
	}
	pi.Caps |= 1 << 7 // bit 7 of the caps byte is another feature's
	if pi.mconnStripe() {
		t.Fatal("caps bit 7 read as the stripe")
	}
}

// pickStripe takes only links without traffic, prefers one with no user,
// leaves stripeKeepFree free links, and a picked link counts as heavy for
// light picks at once.
func TestPickStripe(t *testing.T) {
	noStripeShare(t) // HS2_MCONN_STRIPE_SHARE in the environment would let it take a busy link
	old := stripeK.Load()
	t.Cleanup(func() { stripeK.Store(old) })
	stripeK.Store(4)
	caps := uint8(capMConn | capMConnRaw)
	ls := []*mcLink{newMCLink(caps), newMCLink(caps), newMCLink(caps), newMCLink(caps), newMCLink(caps), newMCLink(caps)}
	m := mcManager(ls...)
	m.links[0].heavy = 1   // a download
	m.links[1].flowing = 1 // a call
	m.links[2].users.Store(2)
	m.links[5].users.Store(1)
	// free: 2, 3, 4, 5 → two may go (two stay free), and only the links no
	// user rides (3, 4): a light user's sub never shares a stripe link
	l, rel, ok := m.pickStripe(nil)
	if !ok || (l != Link(ls[3]) && l != Link(ls[4])) {
		t.Fatalf("picked %v ok=%v, want an idle free link", l, ok)
	}
	if hn := ls[3].mtr.heavyN.Load() + ls[4].mtr.heavyN.Load(); hn != 1 {
		t.Fatalf("the picked link is not heavy for the TUN lane at once (heavyN sum %d)", hn)
	}
	l2, rel2, ok := m.pickStripe([]any{l})
	if !ok || l2 == l || (l2 != Link(ls[3]) && l2 != Link(ls[4])) {
		t.Fatalf("second pick %v ok=%v, want the other idle link", l2, ok)
	}
	if _, _, ok := m.pickStripe([]any{l, l2}); ok {
		t.Fatal("took a link a user rides, or one of the last free links")
	}
	rel2()
	var held *managedLink
	for _, ml := range m.links {
		if ml.link == l {
			held = ml
		}
	}
	if !stripeOn(held) {
		t.Fatal("picked link not held")
	}
	m.mu.Lock()
	k := m.planeKeyLocked(planeLight, held)
	m.mu.Unlock()
	if !k.pk.heavy {
		t.Fatal("a stripe link does not count as heavy for light subs")
	}
	rel()
	if stripeOn(held) {
		t.Fatal("release did not drop the hold")
	}
}

// Only a long download stripes: the first stripe sub waits for
// stripeHotTicks samples in a row at the growth rate; a dip starts over.
func TestStripeHot(t *testing.T) {
	old := stripeSubRate.Load()
	t.Cleanup(func() { stripeSubRate.Store(old) })
	stripeSubRate.Store(100)
	m := &mConn{iran: true}
	m.stK = 4
	m.stActive = true // no slot bookkeeping here
	now := time.Now()
	var sent uint64
	tick := func(bytes uint64) bool {
		now = now.Add(mLitePoll)
		sent += bytes
		return m.stripeTickLocked(now, sent, 0)
	}
	tick(0) // the first sample
	hot := int(stripeHotTicks.Load())
	for i := 1; i < hot; i++ {
		if tick(1000) {
			t.Fatalf("grew after %d hot samples, want %d", i, hot)
		}
	}
	tick(0) // a dip
	for i := 1; i < hot; i++ {
		if tick(1000) {
			t.Fatalf("grew after a dip and %d hot samples", i)
		}
	}
	if !tick(1000) {
		t.Fatalf("no growth after %d hot samples", hot)
	}
}
