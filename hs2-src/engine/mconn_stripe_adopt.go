package engine

import (
	"os"
	"sync/atomic"
	"time"
)

// HS2_MCONN_STRIPE_ADOPT_BACK=1 (diag60, Iran, with HS2_MCONN_STRIPE; off by
// default): a link the stripe asked the pool for may be a RETIRING link the
// pool brings back into service, not only a new one.
//
// Why (production, tunnel 1, 5000 users, 03:05 Tehran): the stripe pick
// takes only a link no user rides (stripeAlone) or one set aside for it; a
// link is set aside only when it is NEW (newManaged -> stripeAdoptLocked).
// With 70-75 serving and 15-25 retiring links, the stripe's ask (and
// HS2_MCONN_STRIPE_FAST_RESERVE's standing reserve) raised the target, and
// reconcile met it by bringing back the retiring link with the MOST open
// connections ("link(s) 62,92 back in service" during the pick-none bursts):
// never new, never empty, never set aside. "set aside" was logged 0 times
// and "set-aside=true" picked 0 times in 10 single uploads; 8 of the 10 never
// got a stripe sub and ran raw on one link (505-1400 Mbit/s, one collapse to
// 200). With the key: while the stripe wants links, the retiring links no
// user rides (nothing flowing, no download) come back first and are set
// aside for it, exactly as a new link would be. Without such a link nothing
// changes.
var stripeAdoptBack atomic.Bool

func init() { stripeAdoptBack.Store(os.Getenv("HS2_MCONN_STRIPE_ADOPT_BACK") == "1") }

// stripeAdoptBackStat: retiring links set aside for the stripe (diagnostics).
var stripeAdoptBackStat atomic.Uint64

// stripeWantsLocked (under m.mu): the stripe still wants links now.
func (m *LinkManager) stripeWantsLocked(now time.Time) bool {
	return stripeAdoptBack.Load() && stripeK.Load() > 0 && stripeAlone && m.stripeWant > 0 && !now.After(m.stripeWantUntil)
}

// stripeIdleBack: a retiring link the stripe may take as its own.
func stripeIdleBack(ml *managedLink) bool {
	return ml.link.Alive() && ml.heavy == 0 && ml.flowing == 0 && ml.users.Load() == 0 && !stripeOn(ml)
}

// stripeBackOrderLocked (reconcile, under m.mu): while the stripe wants
// links, the idle retiring links first (stable: the usual order otherwise).
func (m *LinkManager) stripeBackOrderLocked(retiring []*managedLink, now time.Time) {
	if !m.stripeWantsLocked(now) {
		return
	}
	idle := make([]*managedLink, 0, len(retiring))
	rest := make([]*managedLink, 0, len(retiring))
	for _, ml := range retiring {
		if stripeIdleBack(ml) {
			idle = append(idle, ml)
		} else {
			rest = append(rest, ml)
		}
	}
	copy(retiring, idle)
	copy(retiring[len(idle):], rest)
}

// stripeBackAdoptLocked (reconcile, under m.mu, ml just came back into
// service): set it aside for the stripe when it wants one and ml is idle.
func (m *LinkManager) stripeBackAdoptLocked(ml *managedLink, now time.Time) {
	if !m.stripeWantsLocked(now) || !stripeIdleBack(ml) {
		return
	}
	m.stripeAdoptLocked(ml, now)
	stripeAdoptBackStat.Add(1)
}
