package engine

import (
	"runtime"
	"testing"
	"time"
)

// Tests of HS2_MCONN_STRIPE_UP (mconn_stripe_up.go).

// setStripeUp switches the key for one test.
func setStripeUp(t *testing.T, on bool) {
	t.Helper()
	old := stripeUp.Load()
	stripeUp.Store(on)
	t.Cleanup(func() { stripeUp.Store(old) })
}

// fixUpPathQ sets the upload path queue Iran's gate reads, for one test.
func fixUpPathQ(t *testing.T, q time.Duration) {
	t.Helper()
	p := stripeUpPathQ
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

// iranWire: what Iran wrote on every link (records included), and how many
// links carried more than 256 KiB of it.
func iranWire(n *tNet) (total int64, used int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, l := range n.links {
		w := l.iran.wrote.Load()
		total += w
		if w > 256<<10 {
			used++
		}
	}
	return
}

// upPair: a lite pair whose links each carry at most perLink bytes/s in the
// upload direction, striping up to k subs.
func upPair(t *testing.T, id uint64, perLink int64, k int, up bool) (*tNet, *mConn, *mConn) {
	t.Helper()
	setStripeRate(t, 200<<10)
	setLite(t, true)
	resetLiteBudget()
	n := &tNet{exit: newMConnExit(id)}
	n.hook = func(l *tLink) { l.iran.rate.Store(perLink) }
	iran, err := newMConnIran(id, n.open)
	if err != nil {
		t.Fatal(err)
	}
	iran.setStripe(k, nil)
	if up {
		iran.setStripeUp()
	}
	t.Cleanup(resetLiteBudget) // the next test starts with the budget it expects
	return n, iran, n.exit
}

func upload(t *testing.T, iran, exit *mConn, size int) time.Duration {
	t.Helper()
	data := randBytes(size, 88)
	t0 := time.Now()
	both(t,
		func() error { return xfer(iran, data, 32<<10, 0) },
		func() error { return readN(exit, data) },
	)
	return time.Since(t0)
}

// A heavy upload with the key on grows to K subs through Iran's gate (an
// empty path: the width opens one sub at a time), every byte arrives once
// and in order, and it runs several times faster than one link allows.
func TestMConnStripeUploadGrowsAndSpreads(t *testing.T) {
	before := runtime.NumGoroutine()
	setStripeUp(t, true)
	fixUpPathQ(t, 0)
	const perLink, size = 1 << 20, 12 << 20
	n, iran, exit := upPair(t, 61, perLink, 4, true)
	d := upload(t, iran, exit, size)
	if k := stripeSubs(iran); k != 3 {
		t.Fatalf("%d stripe subs, want 3 (K=4)", k)
	}
	if k := stripeSubs(exit); k != 3 {
		t.Fatalf("exit knows %d stripe subs, want 3", k)
	}
	total, used := iranWire(n)
	if used < 4 {
		t.Fatalf("upload used %d links, want 4", used)
	}
	if over := float64(total)/float64(size) - 1; over > 0.01 {
		t.Fatalf("wire %d bytes for %d: %.2f%% over (duplication?)", total, size, over*100)
	}
	if d > 9*time.Second {
		t.Fatalf("upload took %v, one link alone takes %v", d, time.Duration(size/perLink)*time.Second)
	}
	t.Logf("12 MiB up in %v over %d links", d.Round(time.Millisecond), used)
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// Without the key the same upload does not grow past two stripe subs: the
// growth rule waits for data received on the stripe subs (the old
// behaviour, kept with the key off) — only a sub that joined less than a
// sample ago reads as used.
func TestMConnStripeUploadOffNarrow(t *testing.T) {
	setStripeUp(t, false)
	_, iran, exit := upPair(t, 62, 1<<20, 4, false)
	upload(t, iran, exit, 8<<20)
	if k := stripeSubs(iran); k > 2 {
		t.Fatalf("%d stripe subs with the key off, want at most 2", k)
	}
	closeAndWait(t, iran, exit)
}

// Under a queue on the upload path Iran's gate holds the upload to the pair
// it had before the key (the primary and one stripe sub: the width's floor)
// and opens no stripe sub beyond the first.
func TestMConnStripeUploadGated(t *testing.T) {
	setStripeUp(t, true)
	old := stripeQTarget.Load()
	t.Cleanup(func() { stripeQTarget.Store(old) })
	stripeQTarget.Store(int64(3 * time.Millisecond))
	fixUpPathQ(t, 50*time.Millisecond)
	n, iran, exit := upPair(t, 63, 2<<20, 4, true)
	upload(t, iran, exit, 8<<20)
	if k := stripeSubs(iran); k > 1 {
		t.Fatalf("%d stripe subs under a queue, want at most 1", k)
	}
	if k := n.count(planeStripe); k > 1 {
		t.Fatalf("%d stripe opens under a queue, want at most 1", k)
	}
	closeAndWait(t, iran, exit)
}

// Iran's width: -1 (no gate) without the per-connection flag (the key off,
// or an older exit), the gate with it; the gate reads Iran's own path queue,
// not the exit's.
func TestStripeUpGateIran(t *testing.T) {
	old := stripeQTarget.Load()
	t.Cleanup(func() { stripeQTarget.Store(old) })
	stripeQTarget.Store(int64(10 * time.Millisecond))
	fixPathQ(t, 0)                     // the exit's queue: empty
	fixUpPathQ(t, 50*time.Millisecond) // the upload's: a queue
	m, p := gateConn(3)
	m.iran = true
	now := time.Now()
	if w := m.stripeWidthLocked(p, now); w != -1 {
		t.Fatalf("Iran without the upload flag: width %d, want -1", w)
	}
	m.stUp = true
	if w := m.stripeWidthLocked(p, now); w != 0 {
		t.Fatalf("Iran under an upload queue: width %d, want 0", w)
	}
	setPathQ(50 * time.Millisecond)
	p2 := stripeUpPathQ
	p2.mu.Lock()
	p2.q = 0
	p2.mu.Unlock()
	resetStripeGlobal()
	for i := 1; i <= 3; i++ {
		now = now.Add(stripeQGrow)
		if w := m.stripeWidthLocked(p, now); w != i {
			t.Fatalf("step %d: width %d, want %d (Iran reads the upload queue)", i, w, i)
		}
	}
}

// The capability (ext bit 2) is announced only with both keys on, never in
// the caps byte nor on bits 0/1 alone.
func TestStripeUpCap(t *testing.T) {
	withMConn(t, true)
	setLite(t, true)
	old := stripeK.Load()
	t.Cleanup(func() { stripeK.Store(old) })
	stripeK.Store(4)
	setStripeUp(t, false)
	if infoExt() != capExtMConnStripe {
		t.Fatalf("ext %#x with the key off, want only the stripe bit", infoExt())
	}
	setStripeUp(t, true)
	if infoExt() != capExtMConnStripe|capExtMConnStripeUp {
		t.Fatalf("ext %#x with the key on", infoExt())
	}
	stripeK.Store(0)
	if infoExt() != 0 {
		t.Fatalf("ext %#x without HS2_MCONN_STRIPE", infoExt())
	}
	if mconnCap() != capMConn|capMConnRaw {
		t.Fatalf("caps byte changed: %#x", mconnCap())
	}
	pi := &peerInfo{V2: true, Caps: capMConn | capMConnRaw, Ext: capExtMConnStripe | capExtMConnStripeUp}
	if !pi.mconnStripeUp() {
		t.Fatal("peer cap not read")
	}
	pi.Ext = capExtMConnStripe | 1<<1 // bit 1 is the UDP fast path's
	if pi.mconnStripeUp() {
		t.Fatal("ext bit 1 read as the upload stripe")
	}
	pi.Ext = capExtMConnStripeUp // without the stripe itself
	if pi.mconnStripeUp() {
		t.Fatal("upload stripe without the stripe")
	}
}

// Past the configured K the stripe grows only with the key on, only while
// each sub carries stripeSubRateHi, and never past stripeKMaxUp.
func TestStripeKBeyond(t *testing.T) {
	m := &mConn{}
	m.stK = 8
	hi := float64(stripeSubRateHi.Load())
	setStripeUp(t, false)
	if !m.stripeKCapLocked(7, 0) || m.stripeKCapLocked(8, 100*hi) {
		t.Fatal("key off: K is not the cap")
	}
	setStripeUp(t, true)
	if m.stripeKCapLocked(8, 7.9*hi) {
		t.Fatal("grew past K with subs under the high rate")
	}
	if !m.stripeKCapLocked(8, 8*hi) {
		t.Fatal("no growth past K at the high rate")
	}
	if m.stripeKCapLocked(stripeKMaxUp, 1000*hi) {
		t.Fatal("grew past stripeKMaxUp")
	}
	if stripeWantCap() != stripeKMaxUp {
		t.Fatal("the pool request cap did not follow")
	}
}

// The reorder bound grows on demand: an exit receiver takes a slot first
// (stripeReorder), then steps up to stripeReorderConnMax within the
// process's extra; everything goes back on release. Nothing without oooGrow.
func TestStripeReorderGrow(t *testing.T) {
	a0, x0 := stripeStat.rxActive.Load(), stripeReorderExtra.Load()
	var r mrecv
	if r.growOOOLocked() {
		t.Fatal("grew without oooGrow")
	}
	r.oooGrow = true
	if !r.growOOOLocked() || r.oooMax != stripeReorder || !r.oooSlot || stripeStat.rxActive.Load() != a0+1 {
		t.Fatalf("first growth: max %d slot %v", r.oooMax, r.oooSlot)
	}
	for r.growOOOLocked() {
	}
	if r.oooMax != stripeReorderConnMax {
		t.Fatalf("bound %d, want %d", r.oooMax, stripeReorderConnMax)
	}
	if got := stripeReorderExtra.Load() - x0; got != int64(stripeReorderConnMax-stripeReorder) {
		t.Fatalf("extra %d", got)
	}
	// the process's extra caps the sum
	var rs []*mrecv
	for i := 0; i < 4; i++ {
		x := &mrecv{oooGrow: true, oooMax: stripeReorder}
		for x.growOOOLocked() {
		}
		rs = append(rs, x)
	}
	if e := stripeReorderExtra.Load() - x0; e > stripeReorderExtraMax {
		t.Fatalf("process extra %d over %d", e, stripeReorderExtraMax)
	}
	r.releaseOOOLocked(0)
	for _, x := range rs {
		x.releaseOOOLocked(0)
	}
	if stripeStat.rxActive.Load() != a0 || stripeReorderExtra.Load() != x0 || r.oooMax != 0 {
		t.Fatalf("not given back: active %d extra %d max %d", stripeStat.rxActive.Load(), stripeReorderExtra.Load(), r.oooMax)
	}
}

// A striped upload whose sub stalls (loss recovery): the exit's receiver
// grows its reorder bound past mRecvBuf instead of holding every sub, and
// every byte arrives; the exit's slot goes back at the end.
func TestMConnStripeUploadStalledSub(t *testing.T) {
	setStripeUp(t, true)
	fixUpPathQ(t, 0)
	a0 := stripeStat.rxActive.Load()
	n, iran, exit := upPair(t, 64, 4<<20, 3, true)
	g0 := stripeStat.reorderGrow.Load()
	done, stalled := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stalled)
		for stripeSubs(iran) < 2 {
			select {
			case <-done:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
		time.Sleep(300 * time.Millisecond)
		n.link(1).iran.stall(1500 * time.Millisecond)
	}()
	upload(t, iran, exit, 24<<20)
	close(done)
	<-stalled
	if stripeStat.reorderGrow.Load() == g0 {
		t.Log("no reorder growth needed (the stalled sub held little)")
	}
	closeAndWait(t, iran, exit)
	if stripeStat.rxActive.Load() != a0 {
		t.Fatalf("exit slot not given back: %d, want %d", stripeStat.rxActive.Load(), a0)
	}
}

// A raw first sub (production lite) upgraded by an upload's first stripe sub
// keeps growing: the manager ticks although Iran receives nothing that would
// kick it (a regression: the upload stayed on two subs in the lab).
func TestMConnStripeUploadFromRaw(t *testing.T) {
	before := runtime.NumGoroutine()
	setStripeUp(t, true)
	fixUpPathQ(t, 0)
	setStripeRate(t, 200<<10)
	n, iran, exit := newRawPair(t)
	t.Cleanup(resetLiteBudget)
	n.hook = func(l *tLink) { l.iran.rate.Store(1 << 20) }
	n.link(0).iran.rate.Store(1 << 20)
	iran.setStripe(4, nil)
	iran.setStripeUp()
	d := upload(t, iran, exit, 12<<20)
	if iran.rawW.Load() {
		t.Fatal("not upgraded")
	}
	if k := stripeSubs(iran); k != 3 {
		t.Fatalf("%d stripe subs, want 3 (K=4)", k)
	}
	t.Logf("12 MiB up from raw in %v", d.Round(time.Millisecond))
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// Iran's upload gate reads the light users' links too (active, not busy):
// with no other busy link their median (or, with HS2_MCONN_STRIPE_UP_LQ=25,
// their lower quartile) is the path's queue; a link that
// acknowledged nothing (idle: a stale srtt) never counts; the exit's gate
// (light off) is unchanged. A link's socket is read through its stat func.
func TestStripeUpPathQLight(t *testing.T) {
	oldPct := stripeUpLightPct.Load()
	t.Cleanup(func() { stripeUpLightPct.Store(oldPct) })
	stripeUpLightPct.Store(50)
	{ // the median (default): 1, 6, 8 ms → 6 ms
		p := &stripeQ{links: map[*linkMeter]*stripeLinkQ{}, light: true}
		for _, q := range []time.Duration{time.Millisecond, 6 * time.Millisecond, 8 * time.Millisecond} {
			p.links[&linkMeter{}] = &stripeLinkQ{active: true, q: q}
		}
		p.at = time.Now()
		if q, _ := p.delay(p.at, nil); q != 6*time.Millisecond {
			t.Fatalf("q %v, want the light links' median 6ms", q)
		}
	}
	stripeUpLightPct.Store(25) // HS2_MCONN_STRIPE_UP_LQ=25
	p := &stripeQ{links: map[*linkMeter]*stripeLinkQ{}, light: true}
	own, a, b, c, idle, busy := &linkMeter{}, &linkMeter{}, &linkMeter{}, &linkMeter{}, &linkMeter{}, &linkMeter{}
	p.links[own] = &stripeLinkQ{busy: true, active: true, q: 50 * time.Millisecond}
	d := &linkMeter{}
	p.links[a] = &stripeLinkQ{active: true, q: 1 * time.Millisecond}
	p.links[b] = &stripeLinkQ{active: true, q: 1200 * time.Microsecond}
	p.links[c] = &stripeLinkQ{active: true, q: 8 * time.Millisecond}  // a sparse link: stale srtt
	p.links[d] = &stripeLinkQ{active: true, q: 15 * time.Millisecond} // one in loss recovery
	p.links[idle] = &stripeLinkQ{q: 90 * time.Millisecond}
	p.at = time.Now()
	if q, _ := p.delay(p.at, []*linkMeter{own}); q != 1200*time.Microsecond {
		t.Fatalf("q %v, want the light links' lower quartile 1.2ms (the noisy ones left out)", q)
	}
	p.links[busy] = &stripeLinkQ{busy: true, active: true, q: 20 * time.Millisecond}
	if q, _ := p.delay(p.at, []*linkMeter{own}); q != 20*time.Millisecond {
		t.Fatalf("q %v, want the larger of busy (20ms) and light (1.2ms)", q)
	}
	p.light = false
	if q, _ := p.delay(p.at, []*linkMeter{own, busy}); q != 0 {
		t.Fatalf("q %v with light off and no other busy link, want 0", q)
	}
	// the socket read: acked bytes decide active / busy
	var acked uint64 = 1000
	st := func() (tcpStat, bool) {
		return tcpStat{bytesAcked: acked, rttUs: 84000, minRttUs: 80000}, true
	}
	r := &stripeQ{links: map[*linkMeter]*stripeLinkQ{}, light: true}
	m := &linkMeter{}
	r.links[m] = &stripeLinkQ{stat: st}
	t0 := time.Now()
	r.delay(t0, nil) // the first look: nothing counts
	acked += 500     // a game's few packets
	if q, _ := r.delay(t0.Add(stripeQEvery), nil); q != 4*time.Millisecond {
		t.Fatalf("q %v, want 4ms from the light link's socket", q)
	}
	if l := r.links[m]; !l.active || l.busy {
		t.Fatalf("active %v busy %v, want a light link", l.active, l.busy)
	}
	if q, _ := r.delay(t0.Add(2*stripeQEvery), nil); q != 0 {
		t.Fatalf("q %v from a link that acknowledged nothing, want 0", q)
	}
}

// HS2_MCONN_STRIPE_UP_QMS sets Iran's upload target alone; the exit's
// download gate keeps HS2_MCONN_STRIPE_QMS's.
func TestStripeUpQTarget(t *testing.T) {
	old, oldUp := stripeQTarget.Load(), stripeUpQTarget.Load()
	t.Cleanup(func() { stripeQTarget.Store(old); stripeUpQTarget.Store(oldUp) })
	stripeQTarget.Store(int64(3 * time.Millisecond))
	stripeUpQTarget.Store(int64(8 * time.Millisecond))
	fixPathQ(t, 5*time.Millisecond)
	fixUpPathQ(t, 5*time.Millisecond)
	ir, p := gateConn(2)
	ir.iran, ir.stUp, ir.stW, ir.stWInit = true, true, 2, true
	ex, q := gateConn(2)
	ex.stW, ex.stWInit = 2, true
	now := time.Now()
	if w := ir.stripeWidthLocked(p, now); w != 2 {
		t.Fatalf("Iran width %d under 5 ms with an 8 ms target, want 2 (kept)", w)
	}
	if w := ex.stripeWidthLocked(q, now); w != 1 {
		t.Fatalf("exit width %d under 5 ms with a 3 ms target, want 1 (halved)", w)
	}
}
