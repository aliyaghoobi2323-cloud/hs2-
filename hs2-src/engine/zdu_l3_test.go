package engine

import (
	"sync/atomic"
	"testing"
)

// zduL3Set: nOld peerOld links then nCur current ones, all marked through
// their own atomic (as openL3 points them at the link meter).
func zduL3Set(lane bool, nOld, nCur int) (*l3Set, []*l3Link, []*l3Link) {
	s := &l3Set{}
	if lane {
		s.lane = newL3Lane()
	}
	var old, cur []*l3Link
	for i := 0; i < nOld+nCur; i++ {
		l := newL3Link(nil)
		l.zduOld = new(atomic.Bool)
		if i < nOld {
			l.zduOld.Store(true)
			old = append(old, l)
		} else {
			cur = append(cur, l)
		}
		s.add(l)
	}
	return s, old, cur
}

func setPeerOldGauge(t *testing.T, n int64) {
	t.Helper()
	zduGauge.peerOld.Add(n)
	zduL3Gen.Add(1)
	t.Cleanup(func() { zduGauge.peerOld.Add(-n); zduL3Gen.Add(1) })
}

// While a current-instance link lives, no new TUN packet goes on a peerOld
// link (plain rendezvous, lane-pinned interactive flows, ICMP); with no live
// current link the old ones carry the packets.
func TestZDUL3PickCurrent(t *testing.T) {
	for _, lane := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "lane"}[lane], func(t *testing.T) { zduL3PickCurrent(t, lane) })
	}
}

func zduL3PickCurrent(t *testing.T, lane bool) {
	{
		setZDU(t, false) // other tests may leave the global gauge above 0
		s, old, cur := zduL3Set(lane, 6, 3)
		isOld := map[*l3Link]bool{}
		for _, l := range old {
			isOld[l] = true
		}
		// before the marks count (key off): flows spread over all links,
		// and lane flows pin themselves to old links too
		pinnedOld := 0
		var pkts [][]byte
		for i := 0; i < 400; i++ {
			p := udpPacket(uint16(1000+i), 443, 100)
			if i%4 == 0 {
				p = icmpPacket(byte(i), byte(i>>8))
			}
			pkts = append(pkts, p)
			if f, _, _, _ := s.pickAll(p); isOld[f] {
				pinnedOld++
			}
		}
		if pinnedOld == 0 {
			t.Fatalf("lane %v: no flow on an old link before the marks", lane)
		}
		setZDU(t, true)
		setPeerOldGauge(t, int64(len(old)))
		for _, p := range pkts {
			if f, _, _, _ := s.pickAll(p); f == nil || isOld[f] {
				t.Fatalf("lane %v: packet on a peerOld link (%v) while current links live", lane, f)
			}
		}
		// current links die (the new process died before its marks cleared):
		// the old links are the fallback
		for _, l := range cur {
			l.dead.Store(true)
		}
		for _, p := range pkts {
			if f, _, _, _ := s.pickAll(p); f == nil || !isOld[f] {
				t.Fatalf("lane %v: no fallback to the old links (%v)", lane, f)
			}
		}
	}
}

// Key off: the marks and the gauge are ignored, the pick is the plain one.
func TestZDUL3KeyOff(t *testing.T) {
	setZDU(t, false)
	s, old, _ := zduL3Set(false, 6, 3)
	setPeerOldGauge(t, int64(len(old)))
	onOld := 0
	for i := 0; i < 300; i++ {
		p := udpPacket(uint16(2000+i), 443, 1200)
		f, _, _, _ := s.pickAll(p)
		if want := rendezvous(s.links, flowHash(p), false); f != want {
			t.Fatal("key off: pick differs from plain rendezvous")
		}
		if f.isZDUOld() {
			onOld++
		}
	}
	if onOld == 0 {
		t.Fatal("key off: old links never picked")
	}
	if l, cut := s.zduLinks(); cut || len(l) != len(s.links) {
		t.Fatal("key off: zduLinks filtered")
	}
	pk := udpPacket(7, 443, 100)
	if a := testing.AllocsPerRun(200, func() { s.pickAll(pk) }); a != 0 {
		t.Fatalf("key off: pickAll allocates %.1f", a)
	}
}
