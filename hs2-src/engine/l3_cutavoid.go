package engine

import "sync/atomic"

// Interactive TUN flows off cut paths (HS2_LB_RESCUE_CUT_L3=1, both sides,
// with HS2_L3_LANE; stuck59).
//
// The lane (l3_lane.go) keeps an interactive flow (ICMP, small packets) on
// its link as long as the link is alive and carries no download, and picks
// it by rendezvous over every live link: a ping flow that hashed onto a link
// of a path the balancer has since cut stays there until the link dies, and
// its HS2_L3_DUP copy is again a rendezvous pick, just as likely on a cut
// path (lab3 scenario P, three of four paths cut: ping >120 ms 4-17% of the
// samples). With the key on, each side's balancer publishes the paths its
// judge holds cut (never all of them: nowhere better), and:
//   - a lane flow on a link of a cut path is moved at once, to the
//     rendezvous pick among links off cut paths (all links when none);
//   - the dup's second copy prefers a link off cut paths as well.
//
// Nothing is closed and no stream touched: a TUN packet has no connection
// state, the next one simply goes over the new link.
var l3CutAvoid bool // loadEnvKeys: HS2_LB_RESCUE_CUT_L3

// l3CutSet holds the cut path keys (as mtr.path) of ONE tunnel's balancer:
// each TUN link set (l3Set, one per tunnel) owns one, its links point at it,
// and that tunnel's judge (the exit's LB, the edge's LinkManager steer)
// publishes into it, so two tunnels in one process never read each other's
// cut paths (stuck60; it was one process-wide set).
type l3CutSet struct {
	p atomic.Pointer[map[string]bool]
}

// any reports whether some path is held cut (nil set: none).
func (c *l3CutSet) any() bool {
	if c == nil {
		return false
	}
	p := c.p.Load()
	return p != nil && len(*p) > 0
}

// has reports whether path is held cut (nil set: no).
func (c *l3CutSet) has(path string) bool {
	if c == nil {
		return false
	}
	p := c.p.Load()
	return p != nil && (*p)[path]
}

// publishCutPaths records the judge's cut paths (nil when none or all) in
// its tunnel's set, j.cutOut (nil: this tunnel has no TUN; nothing to do).
func (j *q2Judge) publishCutPaths() {
	if !l3CutAvoid || j.cutOut == nil {
		return
	}
	var cut map[string]bool
	all := true
	for k, q := range j.p {
		if !q.have {
			continue
		}
		if !q.cut {
			all = false
			continue
		}
		if cut == nil {
			cut = map[string]bool{}
		}
		cut[k] = true
	}
	if all {
		cut = nil
	}
	old := j.cutOut.p.Load()
	if old == nil && cut == nil {
		return
	}
	if old == nil || len(*old) != len(cut) {
		j.log("lb l3 cut-avoid: %d cut path(s) kept off the lane; lane flows moved off so far %d, dup copies placed off cut paths %d", len(cut), l3CutMoved.Load(), l3CutDupOff.Load())
	}
	j.cutOut.p.Store(&cut)
}

// l3OnCut reports whether l rides a path its own tunnel holds cut.
func l3OnCut(l *l3Link) bool {
	if !l3CutAvoid || l == nil || l.path == "" {
		return false
	}
	return l.cut.has(l.path)
}

// l3CutMoved: lane flows moved off a cut path; l3CutDupOff: second copies
// placed off cut paths.
var l3CutMoved, l3CutDupOff atomic.Int64

// rendezvousOffCut is rendezvous over the links off cut paths, else nil.
func rendezvousOffCut(links []*l3Link, h uint32, lightOnly bool) *l3Link {
	var best *l3Link
	var bestW uint32
	for _, l := range links {
		if !l.Alive() || lightOnly && l.isHeavy() || l3OnCut(l) {
			continue
		}
		if w := mix32(h ^ l.id); best == nil || w > bestW {
			best, bestW = l, w
		}
	}
	return best
}

// setL3Cut gives the exit's balancer its tunnel's cut path set.
func (b *LB) setL3Cut(c *l3CutSet) {
	b.mu.Lock()
	b.l3Cut = c
	b.mu.Unlock()
}

// setL3Cut gives the edge's steer judge its tunnel's cut path set.
func (m *LinkManager) setL3Cut(c *l3CutSet) {
	m.mu.Lock()
	m.l3Cut = c
	m.mu.Unlock()
}
