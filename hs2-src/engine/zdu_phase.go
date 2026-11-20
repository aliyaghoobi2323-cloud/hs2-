package engine

import (
	"math/rand/v2"
	"sync/atomic"
	"time"
)

// ZDU review issue 3 (proposals/zdu350.md Phase 1, Kharej flow step 2): the
// Iran pool moves new connections to a new exit generation in steps, not at
// its first link.
//
// With links of two peer generations alive (newest: N links; older: O links)
// and the pool's own target W (O when not set):
//
//   - hold (N < need = max(1, min(ceil(O/2), W))): the OLDER generation stays
//     current; the new links are marked like peerOld (not serving, outside the
//     counts and the L3 picker), so nothing lands on a new process that has
//     only a few links (S9 review: every new user, heavy ones included, piled
//     onto 1-2 new links: light behind heavy);
//   - mix (need <= N < full = max(1, min(O, W))): no link is marked; both
//     generations serve. The plain picker spreads by load over all serving
//     links, and a new mconn connection picks its generation with probability
//     N/(N+O) (zduMix.pick), so new conns follow the ratio of new to old links;
//   - full (N >= full; or the mix lasted zduMixMax and N >= 3/4 of full; or
//     it lasted 2 x zduMixMax): the newest generation is current and the
//     older links are peerOld, as before. Review L4: the old cap went full at
//     30 s with N = ceil(O/2), so every new conn landed on about half the old
//     link count while the pool still grew (a hot spot at 4x scale).
//
// A dead new process takes its links with it: N falls to 0 and the older
// generation is current again on the next scan (unchanged revert).
//
// HS2_ZDU off or one generation: the scan is the old "newest wins".

// zduMix: both generations take new conns, by link ratio.
type zduMix struct {
	newI, oldI zduInst
	newN, oldN int
}

func (x *zduMix) pick() zduInst {
	if rand.IntN(x.newN+x.oldN) < x.newN {
		zduStat.mixNew.Add(1)
		return x.newI
	}
	zduStat.mixOld.Add(1)
	return x.oldI
}

// zduMixMax bounds the mix phase (a new pool that never reaches full).
var zduMixMax = 30 * time.Second

// zduPhaseOn: the stepped shift (tests of the pin semantics turn it off to
// keep their one-link-flips setups).
var zduPhaseOn atomic.Bool

func init() { zduPhaseOn.Store(true) }

type zduPhase int

const (
	zduPhaseNone zduPhase = iota // one generation (or none)
	zduPhaseHold
	zduPhaseMix
	zduPhaseFull
)

// zduPhaseOf is the phase for N new and O older links, target w, a mix that
// started mixAge ago (0: not started).
func zduPhaseOf(n, o, w int, mixAge time.Duration) zduPhase {
	if n == 0 || o == 0 {
		return zduPhaseNone
	}
	if w <= 0 {
		w = o
	}
	need := max(1, min((o+1)/2, w))
	full := max(1, min(o, w))
	switch {
	case n < need:
		return zduPhaseHold
	case n >= full:
		return zduPhaseFull
	case mixAge > 2*zduMixMax:
		return zduPhaseFull
	case mixAge > zduMixMax && 4*n >= 3*full:
		return zduPhaseFull
	}
	return zduPhaseMix
}

func (c *zduCurCache) fresh() bool {
	return c.mix == nil || monoNano(time.Now()) < c.until
}

// zduPhaseLocked scans the live links. Caller holds m.mu (read or write).
func (m *LinkManager) zduPhaseLocked() *zduCurCache {
	var newest zduInst
	for _, ml := range m.links {
		if !ml.link.Alive() {
			continue
		}
		if in := linkZDUInst(ml.link); in.known() && in.newer(newest) {
			newest = in
		}
	}
	c := &zduCurCache{inst: newest}
	if !newest.known() || !zduPhaseOn.Load() {
		return c
	}
	var prev zduInst
	n, o := 0, 0
	for _, ml := range m.links {
		if !ml.link.Alive() {
			continue
		}
		in := linkZDUInst(ml.link)
		switch {
		case !in.known():
		case in == newest:
			n++
		default:
			o++
			if in.newer(prev) {
				prev = in
			}
		}
	}
	// zdu389 (S9c): once the newest generation was full it stays current
	// for the rest of this handover. The phase was computed from link counts
	// alone, so one new link fewer than the old count (a link recycled, the
	// storm) re-opened a fresh 30 s mix (or a hold) and sent new conns back
	// to the draining exit: 592 of them after the S9c storm.
	if f := m.zdu.full.Load(); f != nil && *f == newest {
		m.zdu.mixAt.Store(0)
		return c
	}
	now := monoNano(time.Now())
	var age time.Duration
	if at := m.zdu.mixAt.Load(); at != 0 {
		age = time.Duration(now - at)
	}
	switch zduPhaseOf(n, o, m.Target(), age) {
	case zduPhaseHold:
		c.inst = prev
		m.zdu.mixAt.Store(0)
		zduStat.held.Add(1)
	case zduPhaseMix:
		if age == 0 {
			m.zdu.mixAt.Store(now)
			age = 0
		}
		c.mix = &zduMix{newI: newest, oldI: prev, newN: n, oldN: o}
		lim := zduMixMax // re-checked at zduMixMax, then at 2 x zduMixMax
		if age >= zduMixMax {
			lim = 2 * zduMixMax
		}
		c.until = now + int64(lim-age) + 1
	default:
		m.zdu.mixAt.Store(0)
		if o > 0 && n > 0 {
			f := newest
			m.zdu.full.Store(&f)
		}
	}
	return c
}

// zduMixNow is the pool's mix (nil: not mixing).
func (m *LinkManager) zduMixNow() *zduMix {
	if c := m.zdu.cache.Load(); c != nil {
		return c.mix
	}
	return nil
}

// zduMixingLocked: refresh marks nothing while mixing. Caller holds m.mu.
func (m *LinkManager) zduMixingLocked() bool {
	m.zduCurrentLocked()
	return m.zduMixNow() != nil
}
