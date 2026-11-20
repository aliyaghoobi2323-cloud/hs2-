package engine

import (
	"testing"
	"time"
)

func TestZDUDrainModeKeyOff(t *testing.T) {
	setZDU(t, false)
	t.Cleanup(func() { zduDraining.Store(false) })
	if ZDUEnterDrain() || ZDUDraining() {
		t.Fatal("key off: drain mode entered")
	}
	setZDU(t, true)
	if !ZDUEnterDrain() || !ZDUDraining() {
		t.Fatal("key on: drain mode not entered")
	}
}

// A draining old exit holds no slot target: a slot under the target retires,
// a spare too, and a poolctl target is ignored.
func TestZDUDrainExitSlots(t *testing.T) {
	setZDU(t, true)
	t.Cleanup(func() { zduDraining.Store(false) })
	p := &exitPool{want: 8, min: 1, max: 32, peers: &linkPeers{}}
	cancelled := 0
	a := &exitSlot{id: 1, cancel: func() { cancelled++ }}
	b := &exitSlot{id: 2, cancel: func() { cancelled++ }, spare: true}
	p.slots = []*exitSlot{a, b}
	if p.retireIfOver(a) || p.retireIfOver(b) {
		t.Fatal("not draining: a slot under the target retired")
	}
	ZDUEnterDrain()
	if !p.retireIfOver(a) || !p.retireIfOver(b) || len(p.slots) != 0 || cancelled != 2 {
		t.Fatalf("draining: slots kept (%d left, %d cancelled)", len(p.slots), cancelled)
	}
	p.setTarget(20)
	if p.want != 8 {
		t.Fatalf("draining: poolctl target taken (want %d)", p.want)
	}
}

// A draining old exit starts no LB spare (addSpares / addSparePast), and a
// draining shaper gives its slot up once and never installs again.
func TestZDUDrainNoSpares(t *testing.T) {
	setZDU(t, true)
	t.Cleanup(func() { zduDraining.Store(false) })
	p := &exitPool{want: 8, min: 1, max: 32, peers: &linkPeers{}}
	ZDUEnterDrain()
	if n := p.addSpares(2); n != 0 || len(p.slots) != 0 {
		t.Fatalf("draining: %d spares started (%d slots)", n, len(p.slots))
	}
	if n := p.addSparePast(); n != 0 || len(p.slots) != 0 {
		t.Fatalf("draining: %d past-cap spares started", n)
	}
	s := newQShaperOn("", func(string, ...any) {})
	s.zduRelease() // nothing placed: no tc call, no lock
	if s.placed() {
		t.Fatal("released shaper reports a class")
	}
}

// Review M1: right after the commit no TUN packet has been seen yet; that is
// not "quiet": the grace runs from the commit (fails before the fix).
func TestZDUL3QuietFromCommit(t *testing.T) {
	setZDU(t, true)
	t.Cleanup(func() { zduDraining.Store(false); zduL3RxAt.Store(0) })
	zduL3RxAt.Store(0)
	ZDUEnterDrain()
	now := time.Now()
	if q, _ := ZDUL3Quiet(now, 5*time.Second); q {
		t.Fatal("L3 quiet at the commit before the grace ran")
	}
	if q, _ := ZDUL3Quiet(now.Add(6*time.Second), 5*time.Second); !q {
		t.Fatal("L3 not quiet after the grace with no packet")
	}
}
