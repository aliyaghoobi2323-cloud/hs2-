package engine

import (
	"testing"
	"time"
)

// stripeSteerManager: 8 free links on paths a, b, c, d (two each), the
// steering active on a fresh report that cuts path c.
func stripeSteerManager(t *testing.T) (*LinkManager, []*mcLink) {
	t.Helper()
	old := stripeK.Load()
	t.Cleanup(func() { stripeK.Store(old) })
	stripeK.Store(4)
	caps := uint8(capMConn | capMConnRaw)
	paths := []string{"a>x", "a>x", "b>x", "b>x", "c>x", "c>x", "d>x", "d>x"}
	ls := make([]*mcLink, len(paths))
	for i := range ls {
		ls[i] = newMCLink(caps)
		ls[i].mtr.path = paths[i]
	}
	m := mcManager(ls...)
	m.steer = newSteerState()
	m.steer.active = true
	m.steer.repCut = map[string]bool{"c>x": true}
	return m, ls
}

func withStripeSteer(t *testing.T, on bool) {
	t.Helper()
	old := lbStripeSteer
	t.Cleanup(func() { lbStripeSteer = old })
	lbStripeSteer = on
}

// HS2_LB_STRIPE_STEER: with the key on a stripe sub never goes to the cut
// path while a free link on another path can take it; off, the old spread
// (the cut path takes its share).
func TestStripeSteerAvoidsCutPath(t *testing.T) {
	for _, on := range []bool{false, true} {
		withStripeSteer(t, on)
		m, ls := stripeSteerManager(t)
		onCut := 0
		for i := 0; i < 400; i++ {
			l, rel, ok := m.pickStripe(nil)
			if !ok {
				t.Fatalf("on=%v: no stripe link", on)
			}
			if ls[linkIndex(ls, l)].mtr.path == "c>x" {
				onCut++
			}
			rel()
		}
		t.Logf("on=%v: %d of 400 stripe picks on the cut path", on, onCut)
		if on && onCut != 0 {
			t.Fatalf("key on: %d stripe picks on the cut path", onCut)
		}
		if !on && onCut == 0 {
			t.Fatal("key off: the cut path never picked (the test does not see the old spread)")
		}
	}
}

// Extra slots (one stripe holding several links): with the key on they fill
// the paths not cut first, then — the stripe never stalls for the steering —
// the cut path's links, under the usual keep-free rule.
func TestStripeSteerFallbackNoStall(t *testing.T) {
	withStripeSteer(t, true)
	m, ls := stripeSteerManager(t)
	var avoid []any
	var rels []func()
	t.Cleanup(func() {
		for _, r := range rels {
			r()
		}
	})
	before := stripeSteerStat.fallback.Load()
	var got []string
	for {
		l, rel, ok := m.pickStripe(avoid)
		if !ok {
			break
		}
		rels = append(rels, rel)
		avoid = append(avoid, l)
		got = append(got, ls[linkIndex(ls, l)].mtr.path)
	}
	// 8 links, keep-free 2 of them: 6 picks, the 6 links not on c
	if len(got) != 6 {
		t.Fatalf("picks %v, want 6 (8 free links, %d kept free)", got, stripeKeepFree)
	}
	for _, p := range got {
		if p == "c>x" {
			t.Fatalf("a cut-path link before every other path's: %v", got)
		}
	}
	// only the cut path left beside the kept-free rule: a set-aside link on
	// it (the pool's own for the stripe). st150: this connection already
	// has subs on paths not cut (avoid), so a cut sub would take nothing —
	// no pick, a link asked from the pool instead
	m.mu.Lock()
	for _, ml := range m.links {
		if ml.mtr.path == "c>x" {
			ml.stripeRes = m.now()
			break
		}
	}
	m.stripeWant = 0
	m.stSteerAskAt = time.Time{} // sd151: the last pick's ask is long past
	m.stSteerWantUntil = time.Time{}
	m.mu.Unlock()
	asked := stripeSteerStat.askDial.Load()
	if _, _, ok := m.pickStripe(avoid); ok {
		t.Fatal("a cut path's link taken while the connection has subs on paths not cut")
	}
	if stripeSteerStat.askDial.Load() == asked || m.stripeWant == 0 {
		t.Fatalf("no link asked from the pool (askDial %d, want %d)", stripeSteerStat.askDial.Load()-asked, m.stripeWant)
	}
	// another connection with no sub on a path not cut: the set-aside link
	// on the cut path is still taken (never a stall)
	l, rel, ok := m.pickStripe(nil)
	if !ok {
		t.Fatal("stalled: the set-aside link on the cut path was the only one")
	}
	rels = append(rels, rel)
	if ls[linkIndex(ls, l)].mtr.path != "c>x" {
		t.Fatalf("picked %s, want the cut path's set-aside link", ls[linkIndex(ls, l)].mtr.path)
	}
	if stripeSteerStat.fallback.Load() == before {
		t.Fatal("fallback not counted")
	}
}

// The key needs a fresh report: inactive steering moves nothing.
func TestStripeSteerInactive(t *testing.T) {
	withStripeSteer(t, true)
	m, _ := stripeSteerManager(t)
	m.steer.active = false
	for _, ml := range m.links {
		if m.stripeCutLocked(ml) {
			t.Fatal("cut without a fresh report")
		}
	}
}

// Only cut-path links can take the sub, and the keep-free rule sends it to
// the shared pick: the key still finds the shared link it found without the
// key (no stall).
func TestStripeSteerSharedOnlyCut(t *testing.T) {
	oldS := stripeShare
	t.Cleanup(func() { stripeShare = oldS })
	stripeShare = true
	var got []int
	for _, on := range []bool{false, true} {
		withStripeSteer(t, on)
		old := stripeK.Load()
		t.Cleanup(func() { stripeK.Store(old) })
		stripeK.Store(4)
		caps := uint8(capMConn | capMConnRaw)
		// link 0: free, empty, cut path (one free link: the keep-free rule
		// sends the sub to the shared pick); 1-4: light users, cut path; 5: a
		// download on path a (no candidate)
		paths := []string{"c>x", "c>x", "c>x", "c>x", "c>x", "a>x"}
		ls := make([]*mcLink, len(paths))
		for i := range ls {
			ls[i] = newMCLink(caps)
			ls[i].mtr.path = paths[i]
		}
		m := mcManager(ls...)
		m.steer = newSteerState()
		m.steer.active = true
		m.steer.repCut = map[string]bool{"c>x": true}
		for i := 1; i <= 4; i++ {
			m.links[i].users.Store(3)
			m.links[i].flowing = 2
		}
		m.links[1].flowing = 1
		m.links[5].heavy = 1
		l, rel, ok := m.pickStripe(nil)
		if !ok {
			t.Fatalf("on=%v: stalled with only a cut path's links", on)
		}
		got = append(got, linkIndex(ls, l))
		rel()
	}
	if got[0] != got[1] {
		t.Fatalf("picked link %d with the key off, %d with it on: want the same", got[0], got[1])
	}
}

// The Iran sender puts nothing on a cut path's sub while a sub on a path not
// cut is in the stripe; with every sub cut it still sends (never a stall).
func TestStripeSteerSender(t *testing.T) {
	caps := uint8(capMConn | capMConnRaw)
	for _, on := range []bool{false, true} {
		withStripeSteer(t, on)
		m, p := gateConn(3)
		m.iran = true
		m.primary.Store(p)
		for _, s := range m.subs {
			s.key = Link(newMCLink(caps))
		}
		cutSub := m.subs[2]
		m.subMeterOf(cutSub).steerCut.Store(true)
		for i := 0; i < 40; i++ {
			c := getChunk(mRecData)
			c.n = 100
			if r := m.stripeSendLocked(c); r != stripeSent {
				t.Fatalf("on=%v chunk %d: %d, want sent", on, i, r)
			}
			c.unref()
		}
		t.Logf("on=%v: cut sub got %d of 40", on, len(cutSub.q))
		if on && len(cutSub.q) != 0 {
			t.Fatalf("key on: the cut sub got %d chunks", len(cutSub.q))
		}
		if !on && len(cutSub.q) == 0 {
			t.Fatal("key off: the cut sub got nothing (the test does not see the old spread)")
		}
		if on {
			for _, s := range m.subs {
				m.subMeterOf(s).steerCut.Store(true)
			}
			c := getChunk(mRecData)
			c.n = 100
			if r := m.stripeSendLocked(c); r != stripeSent {
				t.Fatalf("every sub cut: %d, want sent", r)
			}
			c.unref()
		}
	}
}

// st150: the exit's download sender, its own judge's cut marks: nothing on a
// cut path's sub while a sub on a path not cut is in the stripe; every sub
// cut: it still sends.
func TestStripeSteerExitSender(t *testing.T) {
	old := stripeQTarget.Load()
	t.Cleanup(func() { stripeQTarget.Store(old) })
	stripeQTarget.Store(0) // no scavenger width: every sub may take chunks
	for _, on := range []bool{false, true} {
		withStripeSteer(t, on)
		m, p := gateConn(3)
		m.primary.Store(p)
		now := time.Now()
		for _, s := range m.subs {
			s.rw = exitCountedStream{c: &exitConn{mtr: &linkMeter{}}}
			s.lastPush = now
			s.lastRecv.Store(monoNano(now))
		}
		cutSub := m.subs[2]
		m.subMeterOf(cutSub).steerCut.Store(true)
		for i := 0; i < 40; i++ {
			c := getChunk(mRecData)
			c.n = 100
			if r := m.stripeSendLocked(c); r != stripeSent {
				t.Fatalf("on=%v chunk %d: %d, want sent", on, i, r)
			}
			c.unref()
		}
		if on && len(cutSub.q) != 0 {
			t.Fatalf("key on: the exit's cut sub got %d chunks", len(cutSub.q))
		}
		if !on && len(cutSub.q) == 0 {
			t.Fatal("key off: the cut sub got nothing (the test does not see the old spread)")
		}
		if on {
			for _, s := range m.subs {
				m.subMeterOf(s).steerCut.Store(true)
			}
			c := getChunk(mRecData)
			c.n = 100
			if r := m.stripeSendLocked(c); r != stripeSent {
				t.Fatalf("every sub cut: %d, want sent", r)
			}
			c.unref()
		}
	}
}

// st150: a cut path's sub takes no slot of the scavenger's width — the sub
// not cut behind it moves up.
func TestStripeSteerRankSkipsCut(t *testing.T) {
	withStripeSteer(t, true)
	m, p := gateConn(3)
	for _, s := range m.subs {
		s.rw = exitCountedStream{c: &exitConn{mtr: &linkMeter{}}}
	}
	m.subMeterOf(m.subs[1]).steerCut.Store(true)
	if r := m.stripeRankLocked(m.subs[2], p); r != 0 {
		t.Fatalf("rank behind a cut sub %d, want 0", r)
	}
	withStripeSteer(t, false)
	if r := m.stripeRankLocked(m.subs[2], p); r != 1 {
		t.Fatalf("key off: rank %d, want 1", r)
	}
}

// st150: the exit's judge marks the held links of its cut paths; none when
// every judged path is cut, none with the key off.
func TestStripeSteerExitMark(t *testing.T) {
	mk := func() (*LB, *linkMeter, *linkMeter) {
		b := &LB{paths: []LBPath{{Local: "k1", Remote: "i"}, {Local: "k2", Remote: "i"}}, held: map[any]int{}, hooks: map[any]lbHooks{}}
		b.judge = newQ2Judge(lb2Floor, "kharej", "Iran", nil)
		a, c := &linkMeter{}, &linkMeter{}
		b.held["L1"], b.hooks["L1"] = 0, lbHooks{mtr: a}
		b.held["L2"], b.hooks["L2"] = 1, lbHooks{mtr: c}
		q1, q2 := b.judge.path("k1>i"), b.judge.path("k2>i")
		q1.have, q1.cut = true, true
		q2.have = true
		return b, a, c
	}
	withStripeSteer(t, true)
	b, a, c := mk()
	b.stripeSteerMarkLocked()
	if !a.steerCut.Load() || c.steerCut.Load() {
		t.Fatalf("marks %v %v, want true false", a.steerCut.Load(), c.steerCut.Load())
	}
	b.judge.p["k2>i"].cut = true // every path cut: nowhere better
	b.stripeSteerMarkLocked()
	if a.steerCut.Load() || c.steerCut.Load() {
		t.Fatal("every path cut: links still marked")
	}
	withStripeSteer(t, false)
	b, a, _ = mk()
	b.stripeSteerMarkLocked()
	if a.steerCut.Load() {
		t.Fatal("key off: link marked")
	}
}

// st150: a cut primary takes no slot of the width: at width 0 (the primary
// alone) the first sub not cut takes the chunks instead of the cut primary.
func TestStripeSteerCutPrimaryWidth(t *testing.T) {
	withStripeSteer(t, true)
	m, p := gateConn(2)
	m.primary.Store(p)
	now := time.Now()
	for _, s := range m.subs {
		s.rw = exitCountedStream{c: &exitConn{mtr: &linkMeter{}}}
		s.lastPush = now
		s.lastRecv.Store(monoNano(now))
	}
	m.subMeterOf(p).steerCut.Store(true)
	old := stripeQTarget.Load()
	t.Cleanup(func() { stripeQTarget.Store(old) })
	stripeQTarget.Store(int64(10 * time.Millisecond))
	fixPathQ(t, time.Second) // a full path queue: the width stays 0
	if w := m.stripeWidthLocked(p, now); w != 0 {
		t.Skipf("width %d, the test needs 0", w)
	}
	for i := 0; i < 10; i++ {
		c := getChunk(mRecData)
		c.n = 100
		if r := m.stripeSendLocked(c); r != stripeSent {
			t.Fatalf("chunk %d: %d, want sent", i, r)
		}
		c.unref()
	}
	if len(p.q) != 0 || len(m.subs[1].q) == 0 {
		t.Fatalf("cut primary got %d, first sub %d", len(p.q), len(m.subs[1].q))
	}
}

// st150: no primary (the exit before its first sub, or after it died): the
// steering's width rule must not touch it (a lab panic).
func TestStripeSteerNoPrimary(t *testing.T) {
	withStripeSteer(t, true)
	m, _ := gateConn(2)
	m.primary.Store(nil)
	now := time.Now()
	for _, s := range m.subs[1:] {
		s.rw = exitCountedStream{c: &exitConn{mtr: &linkMeter{}}}
		s.lastPush = now
		s.lastRecv.Store(monoNano(now))
	}
	if m.stripeSubCutLocked(nil) {
		t.Fatal("nil sub cut")
	}
	c := getChunk(mRecData)
	c.n = 100
	m.stripeSendLocked(c) // must not panic
	c.unref()
}

// sd151: the refused pick (a cut path's link only, the connection has subs
// on paths not cut) retried every 250 ms for a minute — the lab2 soak's
// spin. Before, every try asked the pool (240 asks, stripeWant climbing to
// its cap); now one ask while it is out (renewed as it lapses), the rest
// dropped, and the pool's want stays at one link.
func TestStripeSteerAskDeduped(t *testing.T) {
	withStripeSteer(t, true)
	m, _ := stripeSteerManager(t)
	now := time.Unix(1000, 0)
	m.clock = func() time.Time { return now }
	var avoid []any
	var rels []func()
	t.Cleanup(func() {
		for _, r := range rels {
			r()
		}
	})
	for {
		l, rel, ok := m.pickStripe(avoid)
		if !ok {
			break
		}
		rels = append(rels, rel)
		avoid = append(avoid, l)
	}
	m.mu.Lock()
	for _, ml := range m.links {
		if ml.mtr.path == "c>x" {
			ml.stripeRes = now
			break
		}
	}
	m.stripeWant = 0
	m.stSteerAskAt = time.Time{}
	m.stSteerWantUntil = time.Time{}
	m.mu.Unlock()
	asked, dropped := stripeSteerStat.askDial.Load(), stripeSteerStat.askSup.Load()
	const tries = 240
	maxWant := 0
	for i := 0; i < tries; i++ {
		_, _, ok, held := m.pickStripeWhy(avoid)
		if ok || !held {
			t.Fatalf("try %d: ok=%v held=%v, want a refused pick", i, ok, held)
		}
		maxWant = max(maxWant, m.stripeWant)
		now = now.Add(250 * time.Millisecond)
		m.mu.Lock()
		if now.After(m.stripeWantUntil) {
			m.stripeWant = 0 // the health tick's lapse (stripeExtra)
		}
		for _, ml := range m.links {
			if ml.mtr.path == "c>x" && ml.stripeRes.IsZero() && !stripeOn(ml) {
				ml.stripeRes = now // the pool sets a cut path's link aside again
				break
			}
		}
		m.mu.Unlock()
	}
	a, d := stripeSteerStat.askDial.Load()-asked, stripeSteerStat.askSup.Load()-dropped
	t.Logf("%d refused picks over 60 s: asked-dial %d, dropped %d, max stripeWant %d", tries, a, d, maxWant)
	if a < 1 {
		t.Fatal("the pool never asked (the refused pick must still ask)")
	}
	if a > int64(60*time.Second/stripeWantFor)+2 {
		t.Fatalf("asked-dial %d over 60 s: not deduped (one each %v at most)", a, stripeWantFor)
	}
	if a+d != tries {
		t.Fatalf("asked %d + dropped %d != %d refused picks", a, d, tries)
	}
	if maxWant > 1 {
		t.Fatalf("stripeWant reached %d: the refused picks inflate the pool's target", maxWant)
	}
	// a pick that finds a link on a path not cut is not held
	m.mu.Lock()
	m.steer.repCut = map[string]bool{}
	m.mu.Unlock()
	if _, rel, ok, held := m.pickStripeWhy(avoid); !ok || held {
		t.Fatalf("cut lifted: ok=%v held=%v", ok, held)
	} else {
		rel()
	}
}

// sd151: the connection's wait after a refused stripe open grows 2 → 4 →
// 8 s (plus jitter) and stays there; a joined sub resets it (openStripe).
func TestStripeSteerHoldBackoff(t *testing.T) {
	m := &mConn{}
	now := time.Unix(1000, 0)
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second}
	for i, w := range want {
		d := m.stripeSteerHoldLocked(now).Sub(now)
		if d < w || d >= w+stripeSteerHoldJitt {
			t.Fatalf("hold %d: %v, want %v (+ jitter)", i, d, w)
		}
	}
}
