package engine

import "time"

// The stripe's own links (HS2_MCONN_STRIPE, the Iran side's pool). A stripe
// sub must not take a link light users would get: taking the pool's free
// links squeezed the light connections (games, calls, reels, the TUN's
// pings) onto the few left, beside each other's bursts and the downloads
// (the busy lab: game p99 160 → 190-230 ms even with the stripe moving
// little). So:
//
//   - a stripe sub goes only to a link no user rides (pickStripe);
//   - when there is none, the stripe asks the pool for one (stripeWant): the
//     next new link is set aside for it (stripeAdoptLocked) — held, heavy for
//     every light pick and for the TUN lane — until a stripe sub takes it or
//     stripeResFor passes;
//   - the pool's target is the autopilot's plus the links the stripe holds,
//     has set aside and still wants (stripeExtra), up to max_links: the light
//     users keep the pool they would have without the stripe.

// stripeSteerRank: a stripe link's place for HS2_LB_STEER (pickKey.path):
// after every path's other links, the cut and turned-off ones included —
// the light-path preference never sends a user onto a stripe.
const stripeSteerRank = 4

const (
	stripeWantFor = 10 * time.Second // a request lapses unless renewed
	stripeResFor  = 10 * time.Second // a set-aside link nobody took goes back
	stripeWantMax = stripeKMax       // links asked for at once (process)
)

// stripeAdoptLocked (under m.mu, a new link): set it aside for the stripe
// if one was asked for.
func (m *LinkManager) stripeAdoptLocked(ml *managedLink, now time.Time) {
	if stripeK.Load() == 0 || !stripeAlone || m.stripeWant <= 0 {
		return
	}
	if now.After(m.stripeWantUntil) {
		m.stripeWant = 0
		return
	}
	m.stripeWant--
	ml.stripeRes = now
	stripeHold(ml, 1)
	if ml.mtr != nil && ml.mtr.heavyN.Load() < 1 {
		ml.mtr.heavyN.Store(1)
	}
	if stripeLog {
		m.log("stripe: link %d set aside (%d more wanted)", ml.id, m.stripeWant)
	}
}

// stripeAskLocked (under m.mu): the stripe found no link; ask for one.
func (m *LinkManager) stripeAskLocked(now time.Time) {
	m.stripeWant = min(m.stripeWant+1, stripeWantMax)
	m.stripeWantUntil = now.Add(stripeWantFor)
}

// stripeExtra: links on top of the autopilot's target — held by stripe subs,
// set aside, or still wanted. Lapsed reservations are released here (each
// health tick).
func (m *LinkManager) stripeExtra() int {
	if stripeK.Load() == 0 || !stripeAlone {
		return 0
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, ml := range m.links {
		if !ml.stripeRes.IsZero() && (now.Sub(ml.stripeRes) > stripeResFor || !ml.link.Alive()) {
			ml.stripeRes = time.Time{}
			stripeHold(ml, -1)
		}
		if ml.link.Alive() && stripeOn(ml) {
			n++
		}
	}
	if now.After(m.stripeWantUntil) {
		m.stripeWant = 0
	}
	return n + m.stripeWant
}
