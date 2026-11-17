package engine

import (
	"testing"
	"time"
)

// ou180 (prod audit #3): while a refused stripe pick waits for a link on a
// path not cut, other stripes' picks that find no link renew the shared
// stripeWant every few hundred ms (prod: stripe_pick_none 12139 in 1h48m).
// sd151 deduped the steering's ask against that want, so it never went out
// (3 sent, 4488 dropped in 3h46m) and the held connection retried every 8 s
// for the whole 2h45m cut without a link. Now the steering's ask is its own:
// one per stripeWantFor, one link on top of the shared want, and the link
// that answers it on a healthy path is set aside and taken by the held
// connection; a new link on the cut path is not set aside.
func TestStripeSteerAskSurvivesRenewedWant(t *testing.T) {
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
	setAsideCut := func() {
		for _, ml := range m.links {
			if ml.mtr.path == "c>x" && ml.stripeRes.IsZero() && !stripeOn(ml) {
				ml.stripeRes = now
				return
			}
		}
	}
	m.mu.Lock()
	setAsideCut()
	m.stripeWant, m.stSteerOut = 0, 0
	m.stSteerAskAt = time.Time{}
	m.stSteerWantUntil = time.Time{}
	m.mu.Unlock()
	asked, dropped := stripeSteerStat.askDial.Load(), stripeSteerStat.askSup.Load()
	const tries = 240 // 60 s, one refused pick each 250 ms
	maxWant := 0
	for i := 0; i < tries; i++ {
		m.mu.Lock()
		m.stripeWantRaiseLocked(now, 1) // another stripe's pick that found no link
		m.mu.Unlock()
		_, _, ok, held := m.pickStripeWhy(avoid)
		if ok || !held {
			t.Fatalf("try %d: ok=%v held=%v, want a refused pick", i, ok, held)
		}
		maxWant = max(maxWant, m.stripeWant)
		now = now.Add(250 * time.Millisecond)
		m.mu.Lock()
		if now.After(m.stripeWantUntil) {
			m.stripeWant, m.stSteerOut = 0, 0 // the health tick's lapse (stripeExtra)
		}
		setAsideCut()
		m.mu.Unlock()
	}
	a, d := stripeSteerStat.askDial.Load()-asked, stripeSteerStat.askSup.Load()-dropped
	t.Logf("%d refused picks over 60 s beside a renewed want: asked-dial %d, dropped %d, max stripeWant %d", tries, a, d, maxWant)
	if lo, hi := int64(60*time.Second/stripeWantFor)-1, int64(60*time.Second/stripeWantFor)+2; a < lo || a > hi {
		t.Fatalf("asked-dial %d over 60 s, want %d..%d (one per %v, not suppressed by the shared want)", a, lo, hi, stripeWantFor)
	}
	if maxWant != 2 {
		t.Fatalf("stripeWant peaked at %d, want 2 (the shared one + the steering's own)", maxWant)
	}

	// The pool answers: a new link on the cut path is not set aside, one on
	// a healthy path is, and the held connection takes it.
	m.mu.Lock()
	m.stripeWantRaiseLocked(now, 2)
	cut := newMCLink(capMConn | capMConnRaw)
	cut.mtr.path = "c>x"
	mc := m.newManaged(cut, 100, now)
	m.links = append(m.links, mc)
	wantAfterCut := m.stripeWant
	ok := newMCLink(capMConn | capMConnRaw)
	ok.mtr.path = "a>x"
	mo := m.newManaged(ok, 101, now)
	m.links = append(m.links, mo)
	m.mu.Unlock()
	if !mc.stripeRes.IsZero() || wantAfterCut != 2 {
		t.Fatalf("a new cut-path link set aside (res %v, want %d)", mc.stripeRes, wantAfterCut)
	}
	if mo.stripeRes.IsZero() {
		t.Fatal("the new healthy link was not set aside for the stripe")
	}
	l, rel, got, held := m.pickStripeWhy(avoid)
	if !got || held {
		t.Fatalf("the held connection did not get the new healthy link: ok=%v held=%v", got, held)
	}
	rels = append(rels, rel)
	if l != Link(ok) {
		t.Fatalf("picked %v, want the new healthy link", l)
	}
}
