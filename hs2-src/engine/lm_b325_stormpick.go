package engine

import (
	"fmt"
	"math/rand/v2"
	"sync/atomic"
)

// HS2_B325_STORMPICK (b325, default OFF until a lab A/B): the user-connection
// fast pick (pickHeldFast, lk162) in a reconnect storm.
//
// Why (st322, lab2): at ~2,400 reopens in one second the Iran open path spent
// ~270 us of CPU per reopen in pickLockedFP: every picker scans all ~300
// links for the single global minimum, so all of them choose the SAME link,
// all but one lose its fpPicks compare-and-swap and rescan (up to
// fastPickTries, then the write-locked fallback). 36 % of each scan was
// mtcpLink.Alive's pointer chase (interface call -> mtcpLink -> smux
// Session -> dead).
//
// What:
//
//	(a) liveness is read from managedLink.sessDead, the smux session's own
//	    closed flag cached when the link joins the
//	    pool: the same atomic Alive() reads, one load instead of an
//	    interface call and two pointer hops. Links of other types keep
//	    Alive(). (mtcpLink.dead is never set; its Alive() is the session.)
//	(b) herd breaking: the scan keeps everything that decides WHICH links
//	    may be chosen — the 3 tiers (serving / retiring / any), the cut path
//	    last (cutNew), the burst cap and pressure, the steering path rank,
//	    light-before-heavy — as the "class" of the key, and finds the best
//	    class exactly as before. Inside that class it does not take the one
//	    least-loaded link (which every concurrent picker would also take) but
//	    the better of two links drawn uniformly at random from the class
//	    (power of two choices, reservoir-sampled in the same single pass, no
//	    allocation). Concurrent pickers then spread over the class and
//	    rarely race for one fpPicks.
//
// Only pickHeldFast uses it; the write-locked fallback (pickLocked), mconn
// sub / call / dup / rescue placement are unchanged.
var b325StormPick atomic.Bool

func init() { b325StormPick.Store(envBoolDefault("HS2_B325_STORMPICK", false)) }

var b325Stat struct {
	picks atomic.Uint64 // storm-pick scans
	p2c   atomic.Uint64 // scans whose best class had 2+ links (a real choice)
}

// sessDeadOf is the link's closed flag when it has one Alive() is exactly
// "not this flag" of; nil otherwise (the caller uses Alive()).
func sessDeadOf(l Link) *atomic.Bool {
	switch t := l.(type) {
	case *mtcpLink:
		if t != nil && t.sess != nil {
			return t.sess.DeadFlag()
		}
	}
	return nil
}

// aliveCheap is ml.link.Alive() without the interface call when it can be.
func (ml *managedLink) aliveCheap() bool {
	if p := ml.sessDead; p != nil {
		return !p.Load()
	}
	return ml.link.Alive()
}

// classLess orders the parts of a pick key that decide eligibility (all but
// load and users), as pickKey.less does.
func (a pickKey) classLess(b pickKey) bool {
	if a.cutNew != b.cutNew {
		return !a.cutNew
	}
	if a.pressed != b.pressed {
		return !a.pressed
	}
	if a.path != b.path {
		return a.path < b.path
	}
	if a.heavy != b.heavy {
		return !a.heavy
	}
	return false
}

// b325Rand is a choice in [0,n): pickRandHook in tests, math/rand/v2 else.
func b325Rand(n int) int {
	if h := pickRandHook.Load(); h != nil {
		return h.choose(n)
	}
	return rand.IntN(n)
}

// pickStormFP is pickLockedFP with (a) and (b) above. Same eligibility, same
// tiers, same class order; the pick inside the best class is the better of
// two random members. Caller holds m.mu (read or write).
func (m *LinkManager) pickStormFP(limit int) (*managedLink, int32) {
	b325Stat.picks.Add(1)
	drawn := m.steerDrawLocked(false, func(ml *managedLink) bool {
		return ml.aliveCheap() && m.infoReadyLocked(ml) && ml.serving() && (limit <= 0 || int(ml.users.Load()) < limit) && !stripeOn(ml)
	})
	for tier := 0; tier < 3; tier++ {
		var a, b *managedLink
		var afp, bfp int32
		var ka, kb pickKey
		n := 0
		for _, ml := range m.links {
			if !ml.aliveCheap() || !m.infoReadyLocked(ml) {
				continue
			}
			if limit > 0 && int(ml.users.Load()) >= limit || stripeGuardSkip(ml, limit) {
				continue
			}
			switch tier {
			case 0:
				if ml.retiring || ml.degraded || ml.draining || ml.suspect {
					continue
				}
			case 1:
				if ml.degraded || ml.draining || ml.suspect {
					continue
				}
			}
			fp := ml.fpPicks.Load()
			picks := ml.picks + int(fp)
			k := newPickKey(ml.pressed, ml.flowing, picks, picks+ml.histPicks(), int(ml.users.Load()), m.perLink)
			k.heavy = pickClass && (ml.heavy > 0 || stripeOn(ml))
			k.pressed = k.pressed || stripeGuardPressed(ml)
			k.path = m.steerRankLocked(ml, drawn)
			k.cutNew = m.steerCutNewLocked(ml)
			if stripeOn(ml) {
				k.path = stripeSteerRank
			}
			switch {
			case n == 0 || k.classLess(ka):
				a, afp, ka, b, n = ml, fp, k, nil, 1
			case ka.classLess(k):
				// a worse class: not a candidate
			default:
				n++
				if n == 2 {
					b, bfp, kb = ml, fp, k
				} else if j := b325Rand(n); j == 0 {
					a, afp, ka = ml, fp, k
				} else if j == 1 {
					b, bfp, kb = ml, fp, k
				}
			}
		}
		if a == nil {
			continue
		}
		chosen, chosenFP := a, afp
		if b != nil {
			b325Stat.p2c.Add(1)
			switch {
			case kb.less(ka):
				chosen, chosenFP = b, bfp
			case !ka.less(kb) && b325Rand(2) == 0:
				chosen, chosenFP = b, bfp
			}
		}
		if stripeLog {
			stripeNoteLight(chosen)
		}
		return chosen, chosenFP
	}
	return nil, 0
}

// b325Line: " fp: held=N plane=N retry=N fallback=N heldretry=N
// heldfallback=N" once the fast path ran, plus " b325: picks=N p2c=N" when
// the storm pick did.
func b325Line() string {
	s := ""
	h, p, f := fastPickStat.held.Load(), fastPickStat.plane.Load(), fastPickStat.fallback.Load()
	if h+p+f != 0 {
		s = fmt.Sprintf(" fp: held=%d plane=%d retry=%d fallback=%d heldretry=%d heldfallback=%d", h, p,
			fastPickStat.retry.Load(), f, fastPickStat.heldRetry.Load(), fastPickStat.heldFallback.Load())
	}
	if n := b325Stat.picks.Load(); n != 0 {
		s += fmt.Sprintf(" b325: picks=%d p2c=%d", n, b325Stat.p2c.Load())
	}
	return s
}
