package engine

import "sync/atomic"

// The dup's second copy walks every other-path link in rank order
// (HS2_B309_DUPSCAN, default on; both sides act on their own sends).
//
// ab308 (lab3, b303 R1A t=330): a 5 s data-dark on the SECOND copy's path P4
// put both copies on the first's path P6 for the whole dark (86 echoes). The
// second's link went stuck, the re-pick (dupSecond, cut-aware) landed on a P6
// link, and b278SecondMoved tried only the single top-ranked other-path link
// (light, then any): that link was on the dark P4 path (stuck or cut), so the
// copy stayed on P6 per packet until the dark ended.
//
// With the key on, wherever b278 leaves the second copy on the first's path,
// on a stuck link or on a cut path, b309Scan takes the best-ranked link
// (rendezvous mix32(h^id), the dupSecond rank) on another path that is
// alive, reads TypeDataDup, is not stuck, not on a cut path and not on a
// path the flow avoids — light first, then heavy. The flow also avoids, for
// b308Hold, the path of a kept second link it had to leave stuck or cut
// (the per-link stuck verdict lets that path's idle links rank as healthy).
// Nothing valid: the old pick stands, so the copy is never dropped.
//
// Cost: no allocation; one pass per tier over the links, and the stuck read
// (TCP_INFO, 50 ms cache) only for a candidate that outranks the best valid
// one so far (~ln n reads in rank order). It runs only on a re-pick that
// went wrong, never on a kept-link hit. Key off: exactly b308.
var b309On atomic.Bool

func init() { b309On.Store(envBoolDefault("HS2_B309_DUPSCAN", true)) }

// b309Held: second-copy paths put on hold; b309Moved: second copies the scan
// moved to a valid other-path link.
var b309Held, b309Moved atomic.Int64

// b309Scan is the best-ranked valid link on a path other than first's,
// avoidA's and avoidB's ("" avoids nothing), light before heavy; nil if none.
func b309Scan(links []*l3Link, h uint32, first *l3Link, avoidA, avoidB string, now int64) *l3Link {
	fp := ""
	if first != nil {
		fp = first.path
	}
	for tier := 0; tier < 2; tier++ {
		var best *l3Link
		var bestW uint32
		for _, l := range links {
			if l == first || l.path == "" || l.path == fp || l.path == avoidA || l.path == avoidB ||
				l.isHeavy() != (tier == 1) {
				continue
			}
			w := mix32(h ^ l.id)
			if best != nil && w <= bestW {
				continue
			}
			if !l.Alive() || !l.canDup() || l3OnCut(l) || l.txStuck(now) {
				continue
			}
			best, bestW = l, w
		}
		if best != nil {
			return best
		}
	}
	return nil
}

// b309Bad: second is no use as the second copy while a valid link elsewhere
// may exist (none, on first's path, on an avoided or cut path, or stuck).
func b309Bad(first, second *l3Link, avoidA, avoidB string, now int64) bool {
	if second == nil {
		return true
	}
	if second.path != "" && (first != nil && second.path == first.path ||
		second.path == avoidA || second.path == avoidB) {
		return true
	}
	return l3OnCut(second) || second.txStuck(now)
}

// b309Fix returns second, or the scan's link when second is bad and the scan
// finds one.
func b309Fix(links []*l3Link, h uint32, first, second *l3Link, avoidA, avoidB string, now int64) *l3Link {
	if !b309On.Load() || !b278On.Load() || first == nil || first.path == "" ||
		!b309Bad(first, second, avoidA, avoidB, now) {
		return second
	}
	if l := b309Scan(links, h, first, avoidA, avoidB, now); l != nil {
		if l != second {
			b309Moved.Add(1)
		}
		return l
	}
	return second
}

// b309Avoid notes the path of the kept second link the flow is leaving when
// that link is stuck or on a cut path, and returns the path
// the second copy avoids now ("" none).
func (k *l3DupKeep) b309Avoid(first *l3Link, now int64) string {
	if !b309On.Load() {
		return ""
	}
	// txStuck, not the cached flag: a kept pick that expired (b303Refresh)
	// may not have read the socket for up to 50 ms (ab309 R2B: left stuck,
	// no hold, back on an idle link of the dark path 1 s later).
	if l := k.link; l != nil && l.path != "" && (l3OnCut(l) || l.txStuck(now)) &&
		(k.avoid2 != l.path || now >= k.avoid2Until) {
		k.avoid2, k.avoid2Until = l.path, now+b308Hold
		b309Held.Add(1)
	}
	if k.avoid2 == "" || now >= k.avoid2Until || k.avoid2 == first.path {
		k.avoid2 = ""
		return ""
	}
	return k.avoid2
}
