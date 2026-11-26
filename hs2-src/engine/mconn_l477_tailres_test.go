package engine

import (
	"testing"
	"time"
)

// l477: a dark in-flight sub of a kept non-call lite conn resends only its
// tail (<= l477TailMax) once per dark episode on the live hedge sub, else on
// the fresh sub when it joins; never for a call; key off: nothing.
func l477Setup(t *testing.T) (silent map[*msub]time.Duration, rd map[*msub]int64) {
	t.Helper()
	cs223Keys(t)
	o1, o2, o3 := l477TailRes.Load(), mconnSubResend.Load(), keepStallOn.Load()
	t.Cleanup(func() { l477TailRes.Store(o1); mconnSubResend.Store(o2); keepStallOn.Store(o3) })
	l477TailRes.Store(true)
	mconnSubResend.Store(true)
	keepStallOn.Store(true)
	silent, rd = map[*msub]time.Duration{}, map[*msub]int64{}
	h := func(s *msub, _ int64) (time.Duration, int64) { return silent[s], rd[s] }
	l477SilForHook.Store(&h)
	t.Cleanup(func() { l477SilForHook.Store(nil) })
	return
}

// a: primary with 30 x 1000 B kept chunks; b: the hedge with none.
func l477Conn(iran bool) (*mConn, *msub, *msub) {
	now := time.Now()
	m := &mConn{lite: true, iran: iran, mode: planeLight, sendOff: 30000}
	a := &msub{m: m, idx: 0}
	b := &msub{m: m, idx: 1, missEnd: 1 << 40}
	for i := 0; i < 30; i++ {
		a.sent = append(a.sent, sentRec{c: cs223Chunk(uint64(i*1000), 1000), t: now})
	}
	m.subs = []*msub{a, b}
	m.primary.Store(a)
	return m, a, b
}

func l477Offs(s *msub) (lo, hi uint64, n int) {
	lo = 1 << 62
	for _, c := range s.q {
		lo, hi = min(lo, c.off), max(hi, c.off+uint64(c.n))
		n += c.n
	}
	return
}

func TestL477PrimaryDeadHedgeAlive(t *testing.T) {
	silent, rd := l477Setup(t)
	for _, iran := range []bool{true, false} {
		m, a, b := l477Conn(iran)
		now := time.Now()
		silent[a], rd[a] = 300*time.Millisecond, 7
		m.l477TailResLocked(nil, now, 0)
		if len(b.q) != 0 {
			t.Fatal("resent before max(400 ms, 3 srtt)")
		}
		silent[a] = time.Second
		e0 := l477Stat.eps.Load()
		m.l477TailResLocked(nil, now, 0)
		lo, hi, n := l477Offs(b)
		if lo != 14000 || hi != 30000 || n != 16000 || l477Stat.eps.Load() != e0+1 {
			t.Fatalf("iran=%v tail [%d,%d) %d B, want [14000,30000) 16000", iran, lo, hi, n)
		}
		q := len(b.q)
		m.l477TailResLocked(nil, now, 0) // same episode
		if len(b.q) != q {
			t.Fatal("resent twice in one episode")
		}
		// v2: a reply written on the dark sub after the tail went: only it, once
		a.sent = append(a.sent, sentRec{c: cs223Chunk(30000, 500), t: now})
		m.l477TailResLocked(nil, now, 0)
		if len(b.q) != q+1 || b.q[0].off != 30000 {
			t.Fatalf("iran=%v: new bytes in the episode: q %d -> %d", iran, q, len(b.q))
		}
		m.l477TailResLocked(nil, now, 0)
		if len(b.q) != q+1 {
			t.Fatal("new bytes resent twice")
		}
		// srtt 500 ms: needs 1.5 s
		m2, a2, b2 := l477Conn(iran)
		a2.subSrtt = 500 * time.Millisecond
		silent[a2], rd[a2] = time.Second, 3
		m2.l477TailResLocked(nil, now, 0)
		if len(b2.q) != 0 {
			t.Fatal("3 x srtt not honoured")
		}
	}
}

func TestL477FreshSub(t *testing.T) {
	silent, rd := l477Setup(t)
	m, a, b := l477Conn(true)
	now := time.Now()
	silent[a], rd[a] = time.Second, 5
	silent[b], rd[b] = time.Second, 9 // the hedge dark too, nothing in it
	n0 := l477Stat.noTgt.Load()
	m.l477TailResLocked(nil, now, 0)
	if len(b.q) != 0 || l477Stat.noTgt.Load() == n0 {
		t.Fatal("no live target: must wait")
	}
	nw := &msub{m: m, idx: 2, missEnd: 1 << 40}
	m.subs = append(m.subs, nw)
	j0 := l477Stat.join.Load()
	m.l477TailResLocked(nw, now, 0)
	if lo, hi, _ := l477Offs(nw); lo != 14000 || hi != 30000 || l477Stat.join.Load() != j0+1 {
		t.Fatalf("fresh sub tail [%d,%d)", lo, hi)
	}
	if len(b.q) != 0 {
		t.Fatal("the dark hedge got the tail")
	}
	// a new episode (a read on the link, then dark again) may resend once more
	rd[a] = 11
	nw2 := &msub{m: m, idx: 3, missEnd: 1 << 40}
	m.subs = append(m.subs, nw2)
	m.l477TailResLocked(nw2, now, 0)
	if len(nw2.q) == 0 {
		t.Fatal("new episode")
	}
}

func TestL477NoCallNoKey(t *testing.T) {
	silent, rd := l477Setup(t)
	now := time.Now()
	m, a, b := l477Conn(true)
	m.callLikeAt = now
	silent[a], rd[a] = time.Second, 5
	m.l477TailResLocked(nil, now, 0)
	if len(b.q) != 0 {
		t.Fatal("call conn resent")
	}
	m, a, b = l477Conn(true)
	silent[a], rd[a] = time.Second, 5
	l477TailRes.Store(false)
	m.l477TailResLocked(nil, now, 0)
	if len(b.q) != 0 {
		t.Fatal("key off")
	}
	l477TailRes.Store(true)
	m.stN = 2 // striped download
	m.l477TailResLocked(nil, now, 0)
	if len(b.q) != 0 {
		t.Fatal("striped conn")
	}
	// one chunk bigger than the bound: nothing
	m, a, b = l477Conn(true)
	a.sent = []sentRec{{c: cs223Chunk(0, 1000), t: now}, {c: cs223Chunk(1000, 20000), t: now}}
	silent[a], rd[a] = time.Second, 5
	m.l477TailResLocked(nil, now, 0)
	if len(b.q) != 0 {
		t.Fatal("over the bound")
	}
}

// Receiver of a lite conn: the tail's duplicate copy and the dead link's late
// copy are dropped by offset; the app reads every byte once.
func TestL477TailExact(t *testing.T) {
	m := newMConn(1477, false, nil)
	m.lite = true
	s := &msub{m: m}
	data := randBytes(30000, 7)
	put := func(o, e int) {
		c := getChunk(mRecData)
		copy(c.buf[:], data[o:e])
		m.deliver(s, uint64(o), c, e-o)
	}
	for o := 0; o < 14000; o += 1000 {
		put(o, o+1000) // before the fault
	}
	for o := 14000; o < 30000; o += 1000 {
		put(o, o+1000) // the tail resent on the hedge
	}
	for o := 12000; o < 30000; o += 1000 {
		put(o, o+1000) // the dead link's late TCP retransmission
	}
	m.recvFin(uint64(len(data)))
	if err := readAllCheck(m, data); err != nil {
		t.Fatal(err)
	}
	m.end(errMConnClosed, false)
}
