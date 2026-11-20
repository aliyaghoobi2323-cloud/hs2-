package engine

import (
	"sync/atomic"
	"testing"
	"time"
)

// zdu389 S9c: the second copy of an interactive flow (HS2_DUP / b278 / b303)
// was picked from every link, so a marked older-generation link kept getting
// TypeDataDup frames and the old process's drain end (no TUN packet from a
// link for 5 s) never came before its deadline. With the marks counting,
// neither copy goes on an older link while a current link lives, a kept
// second link that became older is dropped at once, and with the key off the
// pick is the plain one over all links.
func TestZDUL3DupSecondCurrent(t *testing.T) {
	for _, keys := range [][2]bool{{true, true}, {true, false}, {false, false}} {
		zduL3DupSecond(t, keys[0], keys[1])
	}
}

func zduL3DupSecond(t *testing.T, b278, b303 bool) {
	b303Keys(t, b278, b303)
	setZDU(t, false)
	clock := int64(time.Hour)
	links := b303Links(24, 6)
	old := map[*l3Link]bool{}
	for i, l := range links {
		l.zduOld = new(atomic.Bool)
		if i < 16 { // 16 older links, 8 current
			l.zduOld.Store(true)
			old[l] = true
		}
	}
	s := laneSet(&clock, links...)
	s.dup = true
	pkt := func(i int) []byte { return icmpPacket(byte(i), byte(i>>8)) }
	// key off (no marks counted): second copies use the old links too
	onOld := 0
	for i := 0; i < 300; i++ {
		if _, sec, _, _ := s.pickAll(pkt(i)); old[sec] {
			onOld++
		}
	}
	if onOld == 0 {
		t.Fatalf("b278 %v b303 %v: key off: no second copy on an older link (test not sensitive)", b278, b303)
	}
	setZDU(t, true)
	setPeerOldGauge(t, int64(len(old)))
	for i := 0; i < 300; i++ {
		f, sec, _, _ := s.pickAll(pkt(i))
		if old[f] || old[sec] {
			t.Fatalf("b278 %v b303 %v: packet %d: first %v (old %v) second %v (old %v) while current links live", b278, b303, i, f, old[f], sec, old[sec])
		}
	}
	if !b303 {
		return // nothing is kept without b303
	}
	// a kept second link marked older afterwards is not reused
	e := &s.lane.flows[7&(l3LaneSlots-1)]
	*e = l3Flow{key: 7, used: true, inter: true}
	var cur []*l3Link
	for _, l := range links {
		if !old[l] {
			cur = append(cur, l)
		}
	}
	first := cur[0]
	k := s.dupSecondFrom(cur, e, 7, first, clock)
	if k == nil || old[k] {
		t.Fatalf("b278 %v b303 %v: no current second link (%v)", b278, b303, k)
	}
	k.zduOld.Store(true)
	zduL3Gen.Add(1)
	t.Cleanup(func() { k.zduOld.Store(false) })
	var cur2 []*l3Link // what zduLinks returns now
	for _, l := range cur {
		if l != k {
			cur2 = append(cur2, l)
		}
	}
	if got := s.dupSecondFrom(cur2, e, 7, first, clock); got == k {
		t.Fatalf("b278 %v b303 %v: kept second link reused after it was marked older", b278, b303)
	}
}
