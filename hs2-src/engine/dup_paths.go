package engine

import (
	"net"
	"os"
)

// Second copies on another path (HS2_DUP_PATHS=1, off by default).
//
// The adaptive redundancy sends a light connection's chunks over two subs
// (HS2_MCONN=lite: the second sub after a second) and an interactive TUN
// packet over two links (HS2_L3_DUP). With lb on, every link rides one path (a
// source IP, a destination IP). Two links on the same path share its limiter
// and queues, so their losses are correlated; links on different paths lose
// nearly independently. With the key on, the second copy goes, when it can, to
// a link on a different path from the first:
//
//   - mconn (pickPlaneLocked): a light sub picked while avoiding live links
//     (the first sub's) prefers, among the allowed links, one whose path is
//     not one of theirs and is healthy — the candidate itself passes the usual
//     tier checks, the balancer has not cooled its path (direct edge), and at
//     least half the path's live links are neither degraded, suspect nor
//     draining. Within those, the usual order. None: the usual pick.
//   - TUN (dupSecond): the same, over the live links whose peer reads
//     TypeDataDup (light first, as before); none on another path: as before.
//
// A link's path is the pair of IPs of its TCP connection as this side sees it
// (local, remote), set when its session starts: on the reverse edge, where
// Iran listens, that is the listening IP and the exit's source IP — still one
// key per path. One path only (or the key off, or lb off): no change.

// dupPathsOn: HS2_DUP_PATHS=1 (a var for tests).
var dupPathsOn = os.Getenv("HS2_DUP_PATHS") == "1"

// connPathKey is the path key of a connection: "local>remote" IPs, "" if
// either is unknown.
func connPathKey(c net.Conn) string {
	if c == nil {
		return ""
	}
	la, ra := c.LocalAddr(), c.RemoteAddr()
	if la == nil || ra == nil {
		return ""
	}
	lh, _, err1 := net.SplitHostPort(la.String())
	rh, _, err2 := net.SplitHostPort(ra.String())
	if err1 != nil || err2 != nil || lh == "" || rh == "" {
		return ""
	}
	return lh + ">" + rh
}

// mlPath is a managed link's path key ("" unknown).
func mlPath(ml *managedLink) string {
	if ml.mtr == nil {
		return ""
	}
	return ml.mtr.path
}

// countDupPath records where a second copy went: on another path than the
// first copy, or on the same one.
func countDupPath(other bool, l3 bool) {
	switch {
	case l3 && other:
		obs.l3DupPathOther.Add(1)
	case l3:
		obs.l3DupPathSame.Add(1)
	case other:
		obs.mconnPathOther.Add(1)
	default:
		obs.mconnPathSame.Add(1)
	}
}

// dupPathsLocked returns the paths of the live links in avoid (the ones a
// second sub must not share a path with) and the set of healthy paths, or nil
// maps when the preference does not apply (key off, no avoided live link with
// a known path, or no other path at all).
func (m *LinkManager) dupPathsLocked(avoid []any) (skip, healthy map[string]bool) {
	if !dupPathsOn || len(avoid) == 0 {
		return nil, nil
	}
	alive := map[string]int{}
	good := map[string]int{}
	for _, ml := range m.links {
		path := mlPath(ml)
		if path == "" || !ml.link.Alive() {
			continue
		}
		if avoided(ml.link, avoid) {
			if skip == nil {
				skip = map[string]bool{}
			}
			skip[path] = true
		}
		alive[path]++
		if !ml.degraded && !ml.suspect && !ml.draining {
			good[path]++
		}
	}
	if skip == nil {
		return nil, nil
	}
	for path, n := range alive {
		if skip[path] || good[path] == 0 || 2*good[path] < n {
			continue
		}
		if healthy == nil {
			healthy = map[string]bool{}
		}
		healthy[path] = true
	}
	return skip, healthy
}

// pathCooledLocked: the balancer (direct edge) has cooled ml's path.
func (m *LinkManager) pathCooledLocked(ml *managedLink) bool {
	return m.pathCooled != nil && m.pathCooled(ml.link)
}

// cooledLink reports whether link's path is cooling (its dials failed). A
// link the balancer does not hold reads false.
func (b *LB) cooledLink(link any) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	i, ok := b.held[link]
	return ok && !b.until[i].IsZero()
}

// dupSecondOtherPath is dupSecond's choice among the links on a path other
// than first's (nil if there is none, or first's path is unknown).
func dupSecondOtherPath(links []*l3Link, h uint32, first *l3Link) *l3Link {
	if first.path == "" {
		return nil
	}
	return dupSecondWhere(links, h, first, func(l *l3Link) bool { return l.path != "" && l.path != first.path })
}
