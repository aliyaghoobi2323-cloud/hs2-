package engine

import (
	"time"
)

// sp176: HS2_LB_SPARE_EDGE (default on, exit side, reverse).
//
// spareForGap measured a path's share over every link the exit holds — the
// edge's retiring links included: links the edge retired (surplus, a lossy
// path's moved links) or born retiring (spares it did not need), which hold
// their slots for as long as their connections last (HS2_RETIRE_KEEP_IDLE
// keeps idle user connections on them). The edge admits a spare as serving
// only if its address is under its share of the SERVING links
// (arrivalWantedLocked). With the main tunnel at 77 up and 64 serving, a path
// at its share of the 64 was "2.1 links under" its share of the 77: every
// minute a spare was dialed, born retiring at the edge ("the pattern needs 63
// serving; it takes no connections"), closed 31 s later, and the exit asked
// again — nothing could ever move, because nothing was wrong.
//
// With the key on, spareForGap asks only for a spare the edge would serve, by
// the edge's own rule in the edge's frame: the path the dial goes to has fewer
// than lb2Keep links, or its edge address (the links of every path to that
// address summed, as revShrinkKeys groups them) plus the spare is within half
// a link of that address's share of target+1 links, the target being the
// pool's (the edge's serving count, as it last asked). The exit's link counts
// include retiring links, so this is never looser than the edge's check. A
// round asks for no more spares (HS2_LB_SPARE_PER_ROUND) than the edge would
// serve one after another.
//
// And whatever the frames say, a path whose earlier spare did not stay (its
// link count did not grow by the next ask) waits before the next one: 1, 2,
// 4 … 16 minutes, reset once the path gains a link or is no longer under.
var lbSpareEdge bool // loadEnvKeys

const gapBackMax = 16 * time.Minute

// gapBackoff is a path's spareForGap history.
type gapBackoff struct {
	at    time.Time // the last ask
	links int       // the path's links at that ask (before the spare)
	miss  int       // asks in a row whose spare did not stay
}

// setSpareWant installs the pool's target getter (the edge's serving count).
func (b *LB) setSpareWant(f func() int) {
	b.mu.Lock()
	b.spareWant = f
	b.mu.Unlock()
}

// edgeRoomLocked is how many spares dialed on path u the edge would serve,
// one after another: its arrival rule (arrivalWantedLocked) over n serving
// links (0: none). Caller holds b.mu.
func (b *LB) edgeRoomLocked(u int, eff []int, n int, now time.Time) int {
	if b.links[u] < lb2Keep {
		return lb2Keep - b.links[u]
	}
	R := b.paths[u].Remote
	c, wR, W := 0, 0, 0
	for j, p := range b.paths {
		if p.Remote == R {
			c += b.links[j]
		}
		if b.eligibleLocked(j, now) && eff[j] > 0 {
			W += eff[j]
			if p.Remote == R {
				wR += eff[j]
			}
		}
	}
	if W == 0 || wR == 0 {
		return 0
	}
	k := 0
	for k < 8 && float64(c+k+1) <= float64(n+k+1)*float64(wR)/float64(W)+0.5 {
		k++
	}
	return k
}

// gapWaitLocked reports whether path u's spare asks are backing off; it
// first books the outcome of the previous ask. Caller holds b.mu.
func (b *LB) gapWaitLocked(u int, now time.Time) bool {
	g := b.gapBack[b.paths[u].Name()]
	if g == nil {
		return false
	}
	if b.links[u] > g.links {
		g.miss = 0 // the last spare stayed: the share moved
		return false
	}
	if g.miss == 0 {
		return false
	}
	wait := min(time.Minute<<min(g.miss-1, 5), gapBackMax)
	return now.Sub(g.at) < wait
}

// gapAskedLocked books an ask for path u. Caller holds b.mu.
func (b *LB) gapAskedLocked(u int, now time.Time) {
	if b.gapBack == nil {
		b.gapBack = map[string]*gapBackoff{}
	}
	name := b.paths[u].Name()
	g := b.gapBack[name]
	if g == nil {
		g = &gapBackoff{}
		b.gapBack[name] = g
	}
	if !g.at.IsZero() && b.links[u] <= g.links {
		g.miss++
	} else {
		g.miss = 0
	}
	g.at, g.links = now, b.links[u]
}

// gapClearLocked drops the backoff of every path but keep (-1: all): they
// are no longer the path furthest under its share.
// Caller holds b.mu.
func (b *LB) gapClearLocked(keep int) {
	for k := range b.gapBack {
		if keep < 0 || k != b.paths[keep].Name() {
			delete(b.gapBack, k)
		}
	}
}
