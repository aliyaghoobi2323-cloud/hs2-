package engine

import (
	"encoding/binary"
	"sync/atomic"
	"testing"
	"time"
)

// icmpPacket is an 84-byte IPv4 echo request to 10.77.<a>.<b>.
func icmpPacket(a, b byte) []byte {
	p := make([]byte, 84)
	p[0] = 0x45
	p[9] = 1
	copy(p[12:16], []byte{10, 77, 0, 1})
	copy(p[16:20], []byte{10, 77, a, b})
	return p
}

// udpPacket is an IPv4/UDP packet of size bytes for the given ports.
func udpPacket(sport, dport uint16, size int) []byte {
	p := make([]byte, size)
	p[0] = 0x45
	p[9] = 17
	copy(p[12:16], []byte{10, 77, 0, 1})
	copy(p[16:20], []byte{10, 77, 0, 2})
	binary.BigEndian.PutUint16(p[20:], sport)
	binary.BigEndian.PutUint16(p[22:], dport)
	return p
}

// laneLink is a link with its own heavy counter.
func laneLink(id uint32, heavy int32) *l3Link {
	l := &l3Link{id: id * 0x9e3779b9, heavy: new(atomic.Int32)}
	l.heavy.Store(heavy)
	return l
}

// laneSet is a set with the lane on and a test clock.
func laneSet(clock *int64, links ...*l3Link) *l3Set {
	s := &l3Set{lane: newL3Lane()}
	s.lane.now = func() int64 { return *clock }
	for _, l := range links {
		s.add(l)
	}
	return s
}

// plainSet is the same links without the lane (the old pick).
func plainSet(links ...*l3Link) *l3Set {
	s := &l3Set{}
	for _, l := range links {
		s.add(l)
	}
	return s
}

// With one heavy and one light link every ping goes to the light one; the
// plain pick (the old logic) puts about half of them on the heavy one.
func TestL3LanePingsAvoidHeavyLink(t *testing.T) {
	heavy, light := laneLink(1, 3), laneLink(2, 0)
	var clock int64 = 1
	s := laneSet(&clock, heavy, light)
	old := plainSet(heavy, light)
	oldOnHeavy := 0
	for i := 0; i < 2000; i++ {
		p := icmpPacket(byte(i>>8), byte(i))
		for k := 0; k < 3; k++ {
			if got := s.pick(p); got != light {
				t.Fatalf("ping flow %d packet %d on the heavy link", i, k)
			}
		}
		if old.pick(p) == heavy {
			oldOnHeavy++
		}
	}
	if oldOnHeavy < 500 {
		t.Fatalf("plain pick put only %d of 2000 pings on the heavy link: the test does not bite", oldOnHeavy)
	}
}

// Small UDP packets (a game, DNS) ride the lane too.
func TestL3LaneSmallUDPAvoidsHeavyLink(t *testing.T) {
	heavy, light := laneLink(1, 1), laneLink(2, 0)
	var clock int64 = 1
	s := laneSet(&clock, heavy, light)
	for i := 0; i < 2000; i++ {
		p := udpPacket(uint16(10000+i), 27015, 120)
		for k := 0; k < 4; k++ {
			if s.pick(p) != light {
				t.Fatalf("small udp flow %d on the heavy link", i)
			}
		}
	}
}

// A bulk flow (full-size packets) keeps the plain pick, packet after packet.
func TestL3LaneBulkFlowKeepsPlainPick(t *testing.T) {
	links := []*l3Link{laneLink(1, 2), laneLink(2, 0), laneLink(3, 0), laneLink(4, 5)}
	var clock int64 = 1
	s, old := laneSet(&clock, links...), plainSet(links...)
	for i := 0; i < 2000; i++ {
		p := udpPacket(uint16(i), 443, 1400)
		want := old.pick(p)
		for k := 0; k < 20; k++ {
			if got := s.pick(p); got != want {
				t.Fatalf("bulk flow %d packet %d moved off its plain pick", i, k)
			}
			clock += int64(time.Second)
		}
	}
}

// With the key off the set has no lane and picks exactly as before.
func TestL3LaneOffUnchanged(t *testing.T) {
	defer func(v bool) { l3LaneOn = v }(l3LaneOn)
	l3LaneOn = false
	s := newL3Set()
	if s.lane != nil {
		t.Fatal("lane set up with HS2_L3_LANE off")
	}
	links := []*l3Link{laneLink(1, 3), laneLink(2, 0), laneLink(3, 1)}
	for _, l := range links {
		s.add(l)
	}
	for i := 0; i < 3000; i++ {
		for _, p := range [][]byte{icmpPacket(byte(i>>8), byte(i)), udpPacket(uint16(i), 53, 80), tcpPacket(uint16(i), 80)} {
			if got, want := s.pick(p), rendezvous(links, flowHash(p), false); got != want {
				t.Fatalf("flow %d: key off picked a different link", i)
			}
		}
	}
	l3LaneOn = true
	if newL3Set().lane == nil {
		t.Fatal("no lane with HS2_L3_LANE on")
	}
}

// No light link: an interactive flow gets the plain pick.
func TestL3LaneNoLightLinkFallsBack(t *testing.T) {
	links := []*l3Link{laneLink(1, 1), laneLink(2, 4), laneLink(3, 2)}
	var clock int64 = 1
	s, old := laneSet(&clock, links...), plainSet(links...)
	for i := 0; i < 2000; i++ {
		p := icmpPacket(byte(i>>8), byte(i))
		if got, want := s.pick(p), old.pick(p); got != want {
			t.Fatalf("ping flow %d: %p, plain pick %p", i, got, want)
		}
	}
	// no link at all: nothing, as before
	if laneSet(&clock).pick(icmpPacket(0, 1)) != nil {
		t.Fatal("picked a link from an empty set")
	}
}

// flowPreferring finds a ping whose rendezvous over links is want.
func flowPreferring(t *testing.T, links []*l3Link, want *l3Link) []byte {
	for i := 0; i < 1<<16; i++ {
		p := icmpPacket(byte(i>>8), byte(i))
		if rendezvous(links, flowHash(p), false) == want {
			return p
		}
	}
	t.Fatal("no such flow")
	return nil
}

// A flow stays on its link while that stays light (a new link it would hash
// to does not move it), moves when its link turns heavy — at most once per
// l3LaneMoveGap — and leaves a dead link at once.
func TestL3LaneMovesOnlyWhenHeavyAndRateLimited(t *testing.T) {
	a, b := laneLink(1, 0), laneLink(2, 0)
	var clock int64 = 1
	s := laneSet(&clock, a)
	p := flowPreferring(t, []*l3Link{a, b}, b)
	if s.pick(p) != a {
		t.Fatal("first pick not on the only link")
	}
	s.add(b) // b now wins the hash, but a is light: stay
	clock += int64(10 * time.Second)
	if s.pick(p) != a {
		t.Fatal("moved off a light link")
	}
	a.heavy.Store(1) // a turns heavy: the gap since the first pick is over
	if s.pick(p) != b {
		t.Fatal("did not leave a heavy link")
	}
	b.heavy.Store(1)
	a.heavy.Store(0)
	clock += int64(l3LaneMoveGap) - 1
	for k := 0; k < 10; k++ {
		if s.pick(p) != b {
			t.Fatal("moved again inside l3LaneMoveGap")
		}
	}
	clock++
	if s.pick(p) != a {
		t.Fatal("did not move after l3LaneMoveGap")
	}
	a.dead.Store(true) // a dies right after the move: leave it now
	if s.pick(p) != b {
		t.Fatal("stayed on a dead link")
	}
}

// A flow that starts small and turns bulk leaves the lane for its plain pick
// (after a few packets), and an ICMP flow is interactive at any size.
func TestL3LaneClassHysteresis(t *testing.T) {
	heavy, light := laneLink(1, 1), laneLink(2, 0)
	var clock int64 = 1
	s := laneSet(&clock, heavy, light)
	// a flow whose plain pick is the heavy link
	var sport uint16
	for sport = 1; rendezvous(s.links, flowHash(udpPacket(sport, 9, 100)), false) != heavy; sport++ {
	}
	if s.pick(udpPacket(sport, 9, 100)) != light {
		t.Fatal("small flow not on the lane")
	}
	// avg 100 → 1400 in steps of 1/8: still interactive until it reaches 600
	got := s.pick(udpPacket(sport, 9, 1400))
	if got != light {
		t.Fatal("left the lane on one big packet")
	}
	for k := 0; k < 20; k++ {
		got = s.pick(udpPacket(sport, 9, 1400))
	}
	if got != heavy {
		t.Fatal("bulk flow still on the lane")
	}
	// and back below l3SmallEnter: on the lane again
	for k := 0; k < 40; k++ {
		got = s.pick(udpPacket(sport, 9, 60))
	}
	if got != light {
		t.Fatal("small again but not on the lane")
	}
	big := make([]byte, 1400)
	copy(big, icmpPacket(0, 9))
	for k := 0; k < 30; k++ {
		if s.pick(big) != light {
			t.Fatal("big ping off the lane")
		}
	}
}

// The exit counts heavy connections per link for its own TUN lane, and
// clears the count once they stop.
func TestExitTrafficStoresHeavyPerLink(t *testing.T) {
	tr := newExitTraffic(nil)
	m1, m2 := &linkMeter{}, &linkMeter{}
	c := tr.open(m1)
	tr.open(m2) // idle
	now := time.Unix(1000, 0)
	tr.sample(now)
	for i := 0; i < 5; i++ {
		c.bytes.Add(4 << 20) // 2 MiB/s
		now = now.Add(healthTick)
		tr.sample(now)
	}
	if m1.heavyN.Load() != 1 || m2.heavyN.Load() != 0 {
		t.Fatalf("heavy = %d/%d, want 1/0", m1.heavyN.Load(), m2.heavyN.Load())
	}
	tr.done(c)
	now = now.Add(healthTick)
	tr.sample(now)
	if m1.heavyN.Load() != 0 {
		t.Fatalf("heavy = %d after the connection closed", m1.heavyN.Load())
	}
}
