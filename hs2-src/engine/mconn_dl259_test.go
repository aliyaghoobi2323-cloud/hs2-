package engine

import (
	"testing"
	"time"
)

func TestDL259PathQ(t *testing.T) {
	a1, a2, b1, o := &linkMeter{path: "k>a"}, &linkMeter{path: "k>a"}, &linkMeter{path: "k>b"}, &linkMeter{path: "k>b"}
	p := &stripeQ{links: map[*linkMeter]*stripeLinkQ{
		a1: {busy: true, q: 8 * time.Millisecond},
		a2: {busy: true, q: 6 * time.Millisecond},
		b1: {busy: true, q: 500 * time.Microsecond},
		o:  {busy: true, q: 50 * time.Millisecond}, // own: left out
	}}
	qs := p.pathQ([]*linkMeter{o})
	if qs["k>a"] != 8*time.Millisecond || qs["k>b"] != 500*time.Microsecond {
		t.Fatalf("per-path median: %v", qs)
	}
	old := dl259On.Load()
	dl259On.Store(true)
	defer dl259On.Store(old)
	pri := &msub{}
	sa, sb := &msub{stripe: true}, &msub{stripe: true}
	m := &mConn{}
	m.subs = []*msub{pri, sa, sb}
	meters := map[*msub]*linkMeter{sa: a1, sb: b1}
	dl259TestMeter = func(s *msub) *linkMeter { return meters[s] }
	defer func() { dl259TestMeter = nil }()
	now := time.Now()
	g := m.dl259GateLocked(p, []*linkMeter{o}, pri, 0, 3*time.Millisecond, now)
	if g != 500*time.Microsecond {
		t.Fatalf("gate on uncut paths: %v", g)
	}
	if !m.dl259SubCutLocked(sa) || m.dl259SubCutLocked(sb) {
		t.Fatal("path a must be cut, b not")
	}
	// a's queue drains: still held for stripeCutHold, then open
	p.links[a1].q, p.links[a2].q = time.Millisecond, time.Millisecond
	p.at = now // a new measurement (b264 L4: the per-path cache is per p.at)
	m.dl259GateLocked(p, []*linkMeter{o}, pri, 0, 3*time.Millisecond, now.Add(time.Second))
	if !m.dl259SubCutLocked(sa) {
		t.Fatal("cut must hold")
	}
	m.dl259GateLocked(p, []*linkMeter{o}, pri, 0, 3*time.Millisecond, now.Add(stripeCutHold+time.Millisecond))
	if m.dl259SubCutLocked(sa) {
		t.Fatal("cut must open under half the target after the hold")
	}
}
