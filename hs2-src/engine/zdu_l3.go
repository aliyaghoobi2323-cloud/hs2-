package engine

import "sync/atomic"

// ZDU S7 (proposals/zdu350.md §7): the L3 picker keeps new TUN packets off
// peerOld links (S3b for L3).
//
// During a Kharej handover Iran's pool holds the old exit's links next to the
// new exit's (zdu_counts.go marks the old ones). Those links close when the
// old exit leaves, and a TUN packet queued on one at that instant is lost
// (khok2: 1 ICMP at the old exit). So while at least one live link of the
// current instance is in the set, pickAll picks only among the current links;
// peerOld links stay a fallback for when no current link exists (the new
// process died in probation and the marks have not cleared yet, or a pool of
// only old links). A lane flow pinned to a peerOld link is unpinned.
//
// HS2_ZDU off: zduRefreshLocked never marks, zduGauge.peerOld stays 0 and
// zduLinks returns s.links after two atomic loads; the pick is unchanged.

// zduL3Gen is bumped whenever a meter's zduOld mark changes.
var zduL3Gen atomic.Uint64

// l3ZDUCur is an l3Set's current-instance link list for (gen, zgen).
type l3ZDUCur struct {
	gen, zgen uint64
	cur       []*l3Link // nil: no peerOld link in the set
}

func (l *l3Link) isZDUOld() bool { return l.zduOld != nil && l.zduOld.Load() }

// zduLinks returns the links pickAll chooses from and whether peerOld links
// were left out. Caller holds s.mu (read).
func (s *l3Set) zduLinks() ([]*l3Link, bool) {
	if !zduOn.Load() || (zduGauge.peerOld.Load() == 0 && zduExitMarked.Load() == 0) {
		return s.links, false
	}
	g, zg := s.gen.Load(), zduL3Gen.Load()
	c := s.zc.Load()
	if c == nil || c.gen != g || c.zgen != zg {
		c = &l3ZDUCur{gen: g, zgen: zg}
		old := 0
		for _, l := range s.links {
			if l.isZDUOld() {
				old++
			}
		}
		if old > 0 && old < len(s.links) {
			c.cur = make([]*l3Link, 0, len(s.links)-old)
			for _, l := range s.links {
				if !l.isZDUOld() {
					c.cur = append(c.cur, l)
				}
			}
		}
		s.zc.Store(c)
	}
	if c.cur == nil {
		return s.links, false
	}
	for _, l := range c.cur {
		if l.Alive() {
			return c.cur, true
		}
	}
	return s.links, false // no live current link: the old ones are the fallback
}

// zduUnstick drops a lane flow's pin to a peerOld link (pumpTun only, as the
// lane's flow table).
func (s *l3Set) zduUnstick(h uint32) {
	if e := &s.lane.flows[h&(l3LaneSlots-1)]; e.used && e.key == h && e.link != nil && e.link.isZDUOld() {
		e.link = nil
	}
}
