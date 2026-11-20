package engine

import (
	"encoding/binary"
	"math/rand"
	"testing"
)

// d421: two processes of the other server send TypeDataDup at once (a ZDU
// handover: both read the shared TUN), each numbering from its own start.

// d421Frames is streams' ids (each sent as two copies), interleaved at random,
// a copy at most lag frames after its twin.
func d421Frames(r *rand.Rand, starts []uint32, n, lag int) (ids []uint32, from []int) {
	type pend struct {
		id  uint32
		src int
		at  int
	}
	next := append([]uint32(nil), starts...)
	var q []pend
	for len(ids) < 4*n || len(q) > 0 {
		if len(ids) < 4*n && (len(q) == 0 || r.Intn(2) == 0) {
			s := r.Intn(len(starts))
			id := next[s] & l3DupIDMask
			next[s]++
			ids, from = append(ids, id), append(from, s)
			q = append(q, pend{id, s, len(ids) + r.Intn(lag+1)})
			continue
		}
		k := r.Intn(len(q))
		if len(ids) >= 4*n {
			k = 0
		}
		p := q[k]
		q = append(q[:k], q[k+1:]...)
		ids, from = append(ids, p.id|l3DupSecondBit), append(from, p.src)
	}
	return
}

// d421Check feeds frames to accept and wants every (source, id) passed once.
func d421Check(t *testing.T, name string, ids []uint32, from []int, accept func(src int, payload []byte) bool) {
	t.Helper()
	seen := map[[2]uint32]int{}
	for i, raw := range ids {
		p := make([]byte, l3DupHdr+1)
		binary.BigEndian.PutUint32(p, raw)
		if accept(from[i], p) {
			seen[[2]uint32{uint32(from[i]), raw & l3DupIDMask}]++
		}
	}
	want := 0
	for i := range ids {
		if ids[i]&l3DupSecondBit == 0 {
			want++
		}
	}
	dup, lost := 0, want-len(seen)
	for _, c := range seen {
		if c > 1 {
			dup += c - 1
		}
	}
	if dup != 0 || lost != 0 {
		t.Fatalf("%s: %d ids: %d passed twice, %d lost", name, want, dup, lost)
	}
}

// Old peer (no instance id): two interleaved streams far apart, ids wrapping.
func TestD421TwoStreamsNoInst(t *testing.T) {
	r := rand.New(rand.NewSource(421))
	for _, starts := range [][]uint32{
		{0x12345678, 0x6abcdef0},
		{l3DupIDMask - 300, 0x40000000},                // A wraps
		{l3DupIDMask - 300, l3DupIDMask - 100 - 1<<29}, // both near/through the wrap... B far
	} {
		ids, from := d421Frames(r, starts, 20000, 40)
		var d l3Dedup
		d421Check(t, "l3Dedup", ids, from, func(_ int, p []byte) bool { return d.accept(p) != nil })
		var rx l3Rx
		d421Check(t, "l3Rx zero inst", ids, from, func(_ int, p []byte) bool { return rx.accept(zduInst{}, p) != nil })
	}
}

// With instance ids: even overlapping id ranges of two processes dedupe apart
// (one window per process), and a third process (a restart) evicts the least
// recently used one.
func TestD421PerInstance(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	in := []zduInst{{Start: 10, Rand: 1}, {Start: 20, Rand: 2}, {Start: 30, Rand: 3}}
	for _, starts := range [][]uint32{{1000, 1100}, {l3DupIDMask - 50, l3DupIDMask - 20}, {5, 0x50000000}} {
		ids, from := d421Frames(r, starts, 20000, 40)
		var rx l3Rx
		d421Check(t, "two insts", ids, from, func(s int, p []byte) bool { return rx.accept(in[s], p) != nil })
	}
	// three senders round robin (old, new, and links whose inst is unknown yet)
	ids, from := d421Frames(r, []uint32{100, 0x30000000, 0x60000000}, 20000, 40)
	var rx l3Rx
	all := []zduInst{in[0], in[1], {}}
	d421Check(t, "three slots", ids, from, func(s int, p []byte) bool { return rx.accept(all[s], p) != nil })
}

// The fix is needed: the old single window let second copies through when
// two streams interleave (s417's 123 ICMP DUPs), the new one does not.
func TestD421OldWindowFailed(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	ids, from := d421Frames(r, []uint32{0x12345678, 0x6abcdef0}, 2000, 6)
	var w l3Win // the pre-d421 behaviour: one window, cleared on a far id
	passed := 0
	for _, raw := range ids {
		id := raw & l3DupIDMask
		if !w.near(id) {
			w.reset(id)
		}
		if w.mark(id) {
			passed++
		}
	}
	if passed <= 4000 {
		t.Fatalf("single window passed %d of 4000 ids: test not sensitive", passed)
	}
	var d l3Dedup
	d421Check(t, "two windows", ids, from, func(_ int, p []byte) bool { return d.accept(p) != nil })
}

// Sender: the second copy goes over a link of the first's peer process; with
// no other such link there is no second copy; an old peer (no inst) is as
// before.
func TestD421SecondSameInst(t *testing.T) {
	a, b := zduInst{Start: 1, Rand: 9}, zduInst{Start: 2, Rand: 9}
	var links []*l3Link
	for i := 0; i < 16; i++ {
		in := a
		if i%2 == 1 {
			in = b
		}
		links = append(links, instLink(uint32(i*7919+1), in))
	}
	for h := uint32(0); h < 2000; h++ {
		first := links[h%16]
		sec := dupSecond(links, h, first)
		if sec == nil || sec.peerInst() != first.peerInst() {
			t.Fatalf("h %d: first inst %v second %v", h, first.peerInst(), sec)
		}
	}
	// one link of b only: no second copy for it
	one := []*l3Link{links[0], links[2], links[1]}
	if s := dupSecond(one, 5, links[1]); s != nil {
		t.Fatalf("lone b link got second %v", s.peerInst())
	}
	// old peers (no inst): any link, as before
	var old []*l3Link
	cross := 0
	for i := 0; i < 8; i++ {
		old = append(old, instLink(uint32(i*31+3), zduInst{}))
	}
	for h := uint32(0); h < 200; h++ {
		if s := dupSecond(old, h, old[0]); s == nil {
			t.Fatal("old peers: no second")
		} else if s != old[1] {
			cross++
		}
	}
	if cross == 0 {
		t.Fatal("old peers: second never varied")
	}
	// pickAll end to end: both copies to one inst
	s := dupSet(links...)
	for i := 0; i < 500; i++ {
		f, sec, _, _ := s.pickAll(icmpPacket(byte(i), byte(i>>8)))
		if f == nil || sec == nil || f.peerInst() != sec.peerInst() {
			t.Fatalf("pickAll %d: %v / %v", i, f, sec)
		}
	}
}
