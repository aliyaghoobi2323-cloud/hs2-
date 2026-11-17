package engine

import (
	"strings"
	"testing"
)

// dl188: HS2_MCONN_STRIPE_DL_COPIES reaches the raw upgrade's open, and only
// a download under stripeDlCopiesMax subs (fair share: never a big stripe's
// growth); uploads keep their own SHARE_COPIES rule; the key off changes
// nothing.
func TestStripeDlCopiesHintFair(t *testing.T) {
	oldC, oldD, oldM := stripeShareCopies.Load(), stripeDlCopies.Load(), stripeDlCopiesMax.Load()
	t.Cleanup(func() { stripeShareCopies.Store(oldC); stripeDlCopies.Store(oldD); stripeDlCopiesMax.Store(oldM) })
	stripeShareCopies.Store(true)
	stripeDlCopiesMax.Store(4)
	m := &mConn{iran: true}
	raw := func() bool { return stripeCopyUp(m.stripeRawCopyHintLocked(nil)) }
	grow := func() bool { return stripeCopyUp(m.stripeCopyUpHintLocked(nil)) }

	stripeDlCopies.Store(false)
	if raw() || grow() {
		t.Fatal("key off: a download's open has the copies hint")
	}
	m.stGrowUp = true
	if !grow() {
		t.Fatal("key off: an upload's growth lost its SHARE_COPIES hint")
	}
	if raw() {
		t.Fatal("key off: the raw upgrade got the hint")
	}
	m.stGrowUp = false

	stripeDlCopies.Store(true)
	if !raw() || !grow() {
		t.Fatal("key on: a fresh download's open has no copies hint")
	}
	for i := 0; i < 3; i++ { // 1 (first) + 3 stripe subs = 4: at the bound
		m.subs = append(m.subs, &msub{m: m, stripe: true})
	}
	if raw() || grow() {
		t.Fatal("key on: a download at stripeDlCopiesMax subs still takes copies' links")
	}
	stripeDlCopiesMax.Store(0)
	if !grow() {
		t.Fatal("MAX=0 (no bound): no hint")
	}
	stripeDlCopiesMax.Store(4)
	m.subs = m.subs[:2]
	if !grow() {
		t.Fatal("under the bound again: no hint")
	}
	stripeShareCopies.Store(false)
	if raw() || grow() {
		t.Fatal("SHARE_COPIES off: hint")
	}
	stripeShareCopies.Store(true)
	m.iran = false
	if raw() || grow() {
		t.Fatal("exit side: hint")
	}
}

// The pick-none histogram names every refusing condition of a link, not
// only the first.
func TestStripePickWhyMask(t *testing.T) {
	if got := pwMaskName(pwCopies | pwHeavy | pwLight); got != "heavy+copies+light" {
		t.Fatalf("mask name %q", got)
	}
	if got := pwMaskName(pwLight); got != "ok(light)" {
		t.Fatalf("light only: %q", got)
	}
	if got := pwMaskName(0); got != "ok" {
		t.Fatalf("none: %q", got)
	}
	oldK, oldC := stripeK.Load(), stripeShareCopies.Load()
	t.Cleanup(func() { stripeK.Store(oldK); stripeShareCopies.Store(oldC) })
	stripeK.Store(8)
	stripeShareCopies.Store(true)
	caps := uint8(capMConn | capMConnRaw)
	ls := []*mcLink{newMCLink(caps), newMCLink(caps), newMCLink(caps)}
	m := mcManager(ls...)
	m.links[0].heavy = 1
	m.links[1].pressed = true
	m.links[1].heavy = 1
	m.links[2].users.Store(3)
	m.mu.Lock()
	s := m.stripePickWhyLocked([]any{avoidKey(ls[2]), stripeRawHint{}}, m.exitInfo.Load())
	m.mu.Unlock()
	for _, w := range []string{"raw=true", "copyhint=false", "heavy=1", "heavy+pressed=1", "avoid+light=1"} {
		if !strings.Contains(s, w) {
			t.Fatalf("pickwhy %q: no %q", s, w)
		}
	}
}
