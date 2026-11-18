package engine

import (
	"fmt"
	"sync/atomic"
)

// b313: HS2_B313_RJMEMO (default on; =0: rs145's pickRescueJoinLocked as
// before).
//
// After b312 the Iran-side LinkManager.mu wait at 3x load is the rescue
// joins' placement: b312 par T{N,A} mutex profiles put 3.5-4.8k s per 20 s
// window on the Unlock of pickPlaneCall's rs145 branch (rescueStart /
// rrStart -> pickRescueJoinLocked), the writers queueing on each other in
// bursts (rescueVisitRaw starts one per conn). That branch runs under the
// write lock up to four pickPlaneTiers (two pickCallRescueLocked tiers for
// each of "other Iran IP" and "any IP"); each runs the steer draw plus up to
// three tier scans over every link, and every scan re-evaluates the same
// per-link filter: mlPath, the rs145 path map, linkFresh, pathHost + the
// host map, rescueLowKey (a sync.Map), stripeOn and linkCopies — up to 16
// evaluations per link per join.
//
// With the key on the filter is evaluated once per link per join (one pass,
// bits kept on the link under the write lock: the same inputs read at one
// instant instead of at up to 16), and a pickPlaneTiers whose filter no link
// passes is not run at all (it could only return nil). The links chosen and
// the rs145 counters are the same; only the time under the lock shrinks.

var b313RJMemo atomic.Bool

func init() { b313RJMemo.Store(envBoolDefault("HS2_B313_RJMEMO", true)) }

var b313Stat struct {
	joins   atomic.Uint64 // memo passes (rescue-join picks)
	skipped atomic.Uint64 // pickPlaneTiers runs skipped: no link passes the filter
}

// b313Line: " b313rj: joins=N skipped=N" on the mem line once a memo pass ran.
func b313Line() string {
	n := b313Stat.joins.Load()
	if n == 0 && b313Fast.placed.Load()+b313Fast.none.Load() == 0 {
		return ""
	}
	return fmt.Sprintf(" b313rj: joins=%d skipped=%d fast=%d none=%d retry=%d fallback=%d", n, b313Stat.skipped.Load(),
		b313Fast.placed.Load(), b313Fast.none.Load(), b313Fast.retry.Load(), b313Fast.fallback.Load())
}

const (
	rjBase  uint8 = 1 << iota // not lowered, off the left paths, fresh
	rjHost                    // ... and on another Iran IP than the left paths
	rjLight                   // pickCallRescueLocked's first-tier light test
)

// rjOutcome: which rs145 pass found the link.
const (
	rjNone = iota
	rjFresh
	rjFreshIP
)

// rjPickCore is rs145's pick (pickRescueJoinLocked's order: other Iran IP
// first, pickCallRescueLocked's light tier before its any tier) with the
// fast-path count of the chosen link. memo: m.mu is held for writing and the
// per-link bits are kept on the links; otherwise (read lock) nothing is
// written and only the prepass that skips empty scans is shared.
func (m *LinkManager) rjPickCore(avoid []any, pool *peerInfo, mk *rescueJoinMark, memo bool) (*managedLink, int32, int) {
	now := ctrlNow()
	hosts := map[string]bool{}
	for p := range mk.paths {
		hosts[pathHost(p)] = true
	}
	bitsOf := func(ml *managedLink) uint8 {
		p := mlPath(ml)
		if rescueLowKey(ml.link) || mk.paths[p] || !linkFresh(ml.mtr, now) {
			return 0
		}
		b := rjBase
		if !hosts[pathHost(p)] {
			b |= rjHost
		}
		if ml.heavy == 0 && !stripeOn(ml) && !ml.degraded && !ml.draining && !ml.suspect && linkCopies(ml.link) == 0 {
			b |= rjLight
		}
		return b
	}
	var ep uint64
	if memo {
		m.rjEp++
		ep = m.rjEp
	}
	var cnt [8]int // by bits
	for _, ml := range m.links {
		b := bitsOf(ml)
		if memo {
			ml.rjEp, ml.rjBits = ep, b
		}
		cnt[b]++
	}
	try := func(need uint8) (*managedLink, int32) {
		n := 0
		for b, c := range cnt {
			if uint8(b)&need == need {
				n += c
			}
		}
		if n == 0 {
			b313Stat.skipped.Add(1)
			return nil, 0
		}
		ok := func(ml *managedLink) bool { return bitsOf(ml)&need == need }
		if memo {
			ok = func(ml *managedLink) bool { return ml.rjEp == ep && ml.rjBits&need == need }
		}
		return m.pickPlaneTiersFP(planeLight, avoid, pool, ok)
	}
	for _, step := range [...]struct {
		need uint8
		out  int
	}{{rjBase | rjHost | rjLight, rjFresh}, {rjBase | rjHost, rjFresh}, {rjBase | rjLight, rjFreshIP}, {rjBase, rjFreshIP}} {
		if ml, fp := try(step.need); ml != nil {
			return ml, fp, step.out
		}
	}
	return nil, 0, rjNone
}

func rjCount(out int) {
	switch out {
	case rjFresh:
		rs145Stat.pickFresh.Add(1)
	case rjFreshIP:
		rs145Stat.pickFreshIP.Add(1)
	default:
		rs145Stat.pickOld.Add(1)
	}
}

// pickRescueJoinMemoLocked is pickRescueJoinLocked with the filter memoised
// (m.mu write-locked; the order of picks is the original's).
func (m *LinkManager) pickRescueJoinMemoLocked(avoid []any, pool *peerInfo, mk *rescueJoinMark) *managedLink {
	b313Stat.joins.Add(1)
	ml, _, out := m.rjPickCore(avoid, pool, mk, true)
	rjCount(out)
	return ml
}

// HS2_B313_RJFAST (default on since the b313 A/B on lab1, 2026-11-18; needs HS2_LM_FASTPICK): the
// rescue join is placed like lk162's fast picks, under LinkManager.mu's READ
// lock with the fpPicks compare-and-swap (placeFast), retried fastPickTries
// times and then left to the write-locked path. Why: the b312/b305 profiles
// show the write-locked rescue joins' wait is a convoy (each writer waits
// for the readers to drain and for a wake-up while holding RWMutex.w; the
// pick itself is ~20 us), so making them readers removes the queue rather
// than shortening each hold. The pick is rjPickCore (the same order); the
// placement is counted in fpPicks and folded at the next sample, as lk162.
var b313RJFast atomic.Bool

func init() { b313RJFast.Store(envBoolDefault("HS2_B313_RJFAST", true)) }

var b313Fast struct {
	placed   atomic.Uint64 // placed under the read lock
	none     atomic.Uint64 // no candidate (the caller goes on as after a nil pick)
	retry    atomic.Uint64 // lost CAS races
	fallback atomic.Uint64 // fastPickTries lost: write-locked path
}

// rescueJoinFast: done=false sends the caller to the write-locked pick.
func (m *LinkManager) rescueJoinFast(avoid []any, pool *peerInfo, mk *rescueJoinMark) (l Link, release func(), ok, done bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for try := 0; try < fastPickTries; try++ {
		ml, fp, out := m.rjPickCore(avoid, pool, mk, false)
		if ml == nil {
			rjCount(out)
			b313Fast.none.Add(1)
			return nil, nil, false, true
		}
		if m.placeFast(ml, fp) {
			rjCount(out)
			b313Fast.placed.Add(1)
			return ml.link, m.releaseFor(ml), true, true
		}
		b313Fast.retry.Add(1)
	}
	b313Fast.fallback.Add(1)
	return nil, nil, false, false
}
