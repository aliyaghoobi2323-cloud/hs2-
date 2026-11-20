package engine

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// dl188 diagnostics (HS2_MCONN_STRIPE_LOG=1 only): for a stripe pick that
// found no link, every serving link's FULL set of conditions that keep the
// shared pick (pickStripeSharedWhyLocked) off it — not only the first one the
// pick tests — as a histogram of condition combinations, so the binding
// constraint shows (the dl186 "skip[copies=209]" counted the first refusing
// rule only).
//
//	stripe pickwhy raw=true copyhint=false: copies=150 copies+heavy=12 heavy+stripe=30 ...
//
// The combination names: heavy (a download rides it), stripe (a stripe sub
// holds it), info, pressed, avoid (this connection's own), copies (lite's
// copy-only subs ride it and the open has no copies hint), call, lane
// (SHARE_LANE), cut (a cut path while the steering wants none), light (a
// light user's primary sub rides it: informational, the shared pick allows
// it). "ok": a link the shared pick could take.
const (
	pwHeavy = 1 << iota
	pwStripe
	pwInfo
	pwPressed
	pwAvoid
	pwCopies
	pwCall
	pwLane
	pwCut
	pwLight
	pwN = iota
)

var pwNames = [pwN]string{"heavy", "stripe", "info", "pressed", "avoid", "copies", "call", "lane", "cut", "light"}

// stripeRawHint (in a stripe open's avoid list, stripeLog only): the open is
// a raw connection's upgrade (mconn_raw.go).
type stripeRawHint struct{}

func stripeRawOf(avoid []any) bool {
	for _, a := range avoid {
		if _, ok := a.(stripeRawHint); ok {
			return true
		}
	}
	return false
}

// pwMaskName: "copies+light", "ok" for 0.
func pwMaskName(mask int) string {
	if mask&^pwLight == 0 {
		if mask&pwLight != 0 {
			return "ok(light)"
		}
		return "ok"
	}
	var p []string
	for i := 0; i < pwN; i++ {
		if mask&(1<<i) != 0 {
			p = append(p, pwNames[i])
		}
	}
	return strings.Join(p, "+")
}

// stripePickWhyLocked (under m.mu): the histogram line for a pick that found
// no link.
func (m *LinkManager) stripePickWhyLocked(avoid []any, pool *peerInfo) string {
	shareCopies := stripeShareCopies.Load() && stripeCopyUp(avoid)
	held := make(map[*managedLink]int)
	stripeHeld.mu.Lock()
	for ml, h := range stripeHeld.n {
		held[ml] = h
	}
	stripeHeld.mu.Unlock()
	now := ctrlNow()
	tn := time.Now()
	hist := map[int]int{}
	for _, ml := range m.links {
		if !ml.up() {
			continue
		}
		mask := 0
		if ml.heavy > 0 {
			mask |= pwHeavy
		}
		if held[ml] > 0 {
			mask |= pwStripe
		}
		if m.gateInfo && ml.mtr != nil && !ml.mtr.infoDone.Load() || !linkMConn(ml, pool) {
			mask |= pwInfo
		}
		if ml.pressed {
			mask |= pwPressed
		}
		if avoided(ml.link, avoid) {
			mask |= pwAvoid
		}
		c := stripeLinkCopies(ml.link)
		if c > 0 && !shareCopies {
			mask |= pwCopies
		}
		if (stripeCallFree || shareCopies) && callLinkOn(ml.mtr, tn) {
			mask |= pwCall
		}
		if stripeShareLane && laneAtOf(ml) != 0 && now-laneAtOf(ml) < int64(stripeLaneQuiet) {
			mask |= pwLane
		}
		if m.stSteerNoCut && m.stripeCutLocked(ml) {
			mask |= pwCut
		}
		if int(ml.users.Load())-held[ml]-c > 0 {
			mask |= pwLight
		}
		hist[mask]++
	}
	type kv struct {
		k string
		n int
	}
	var l []kv
	for k, n := range hist {
		l = append(l, kv{pwMaskName(k), n})
	}
	sort.Slice(l, func(i, j int) bool { return l[i].n > l[j].n || l[i].n == l[j].n && l[i].k < l[j].k })
	var b strings.Builder
	fmt.Fprintf(&b, "raw=%v copyhint=%v dlshare=%v:", stripeRawOf(avoid), stripeCopyUp(avoid), stripeDlShareOf(avoid))
	for _, e := range l {
		fmt.Fprintf(&b, " %s=%d", e.k, e.n)
	}
	return b.String()
}
