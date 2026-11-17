package engine

import (
	"bytes"
	"io"
	"testing"
	"time"
)

// dl242/dl243 (HS2_DL243_RESCUE, exit): a young stripe sub whose first chunk
// its own link has not delivered after max(dl242Min, 2 x primary srtt) AND
// whose link is truly stuck (bytes_acked not moving, unacked > 0, or RTO
// loss) gets its lowest-offset outstanding chunks (<= dl243MaxBytes) queued
// at the front of the primary, then is held until it delivers. A sub whose
// link is slow but acking, one that delivers, one too fresh, one without a
// TCP view, the key off, and a third rescue on a conn within dl243ConnWin are
// left alone.
func TestDL242Rescue(t *testing.T) {
	old, oldK, oldMarks, oldSrtt, oldAfter, oldLink := stripeQTarget.Load(), dl242Rescue.Load(), subAckMarks, dl242Srtt, dl242After, dl243Link
	t.Cleanup(func() {
		stripeQTarget.Store(old)
		dl242Rescue.Store(oldK)
		subAckMarks = oldMarks
		dl242Srtt = oldSrtt
		dl242After = oldAfter
		dl243Link = oldLink
	})
	dl242After = func(time.Duration, func()) {}
	stripeQTarget.Store(0)
	withStripeSteer(t, false)
	m, p := gateConn(1)
	m.primary.Store(p)
	now := time.Now()
	for _, s := range m.subs {
		s.rw = exitCountedStream{c: &exitConn{mtr: &linkMeter{}}}
		s.lastPush = now
		s.lastRecv.Store(monoNano(now))
		s.kick = make(chan struct{}, 1)
	}
	y := m.subs[1]
	// the smux marks never move (dl242's coarse signal): only TCP decides
	subAckMarks = func(s *msub) (uint32, uint32, bool) { return 3000, 0, true }
	type tcpv struct {
		la linkAck
		q  uint64
		ok bool
	}
	links := map[*msub]*tcpv{}
	dl243Link = func(s *msub) (linkAck, uint64, bool) {
		if v := links[s]; v != nil {
			return v.la, v.q, v.ok
		}
		return linkAck{}, 0, false
	}
	srtt := 50 * time.Millisecond
	dl242Srtt = func(*msub) time.Duration { return srtt }
	mk := func(off uint64, n int) *mchunk {
		ch := getChunk(mRecData)
		ch.n = n
		ch.setOff(off)
		return ch
	}

	// key off: not armed
	dl242Rescue.Store(false)
	y.stJoinAt = now
	m.dl242ArmLocked(y)
	if y.dl242.armed {
		t.Fatal("key off: armed")
	}
	dl242Rescue.Store(true)
	m.dl242ArmLocked(y)
	if !y.dl242.armed {
		t.Fatalf("not armed: %+v", y.dl242)
	}

	// three kept unacked chunks (written out of order on purpose) + one queued
	y.sent = []sentRec{{c: mk(5000, 1000), t: now, w: 1000, marked: true}, {c: mk(3000, 1000), t: now, w: 2000, marked: true}, {c: mk(9000, 1000), t: now, w: 3000, marked: true}}
	q := mk(11000, 1000)
	y.q = []*mchunk{q}
	ly := &tcpv{la: linkAck{acked: 1000, unacked: 4}, q: 5000, ok: true}
	links[y] = ly
	// first look takes a0/q0; too fresh: no rescue
	if !m.dl242CheckLocked(y, now) || y.dl242.a0 != 1000 || y.dl242.q0 != 5000 {
		t.Fatalf("first look: %+v", y.dl242)
	}
	if !m.dl242CheckLocked(y, now.Add(dl242Min-time.Millisecond)) || len(p.q) != 0 {
		t.Fatal("rescued before dl242Min")
	}
	// 2 x srtt over dl242Min sets the wait
	srtt = 400 * time.Millisecond
	if !m.dl242CheckLocked(y, now.Add(500*time.Millisecond)) || len(p.q) != 0 {
		t.Fatal("rescued before 2 x srtt")
	}
	// slow but acking (bytes_acked moved at 700 ms): not stuck at 801 ms
	ly.la.acked = 2000
	m.dl242CheckLocked(y, now.Add(700*time.Millisecond))
	s0 := dl242Stat.slowOK.Load()
	if !m.dl242CheckLocked(y, now.Add(801*time.Millisecond)) || len(p.q) != 0 || dl242Stat.slowOK.Load() != s0+1 {
		t.Fatal("a moving link was rescued")
	}
	// unacked == 0 and no loss: nothing in flight, not stuck
	ly.la.unacked = 0
	if !m.dl242CheckLocked(y, now.Add(1600*time.Millisecond)) || len(p.q) != 0 {
		t.Fatal("rescued with nothing in flight")
	}
	ly.la.unacked = 4
	// stuck: no ack movement for 2 x srtt -> rescue in offset order
	refs0 := map[*mchunk]int32{}
	for _, r := range y.sent {
		refs0[r.c] = r.c.refs.Load()
	}
	refs0[q] = q.refs.Load()
	r0, f0 := dl242Stat.rescues.Load(), dl242Stat.fast.Load()
	resAt := now.Add(1600 * time.Millisecond)
	if !m.dl242CheckLocked(y, resAt) {
		t.Fatal("rescue: want a post look")
	}
	if dl242Stat.rescues.Load() != r0+1 || !y.dl242.hold || !y.dl242.post {
		t.Fatalf("no rescue: %+v", y.dl242)
	}
	want := []uint64{3000, 5000, 9000, 11000}
	if len(p.q) != len(want) {
		t.Fatalf("primary got %d chunks, want %d", len(p.q), len(want))
	}
	for i, ch := range p.q {
		if ch.off != want[i] {
			t.Fatalf("chunk %d off %d, want %d (offset order)", i, ch.off, want[i])
		}
		if ch.refs.Load() != refs0[ch]+1 {
			t.Fatalf("chunk %d refs %d, want %d (sub + primary)", i, ch.refs.Load(), refs0[ch]+1)
		}
	}
	if len(y.q) != 1 || len(y.sent) != 3 {
		t.Fatal("the young sub lost its own references")
	}
	// held: new chunks go to the primary only
	for i := 0; i < 10; i++ {
		ch := getChunk(mRecData)
		ch.n = 100
		if r := m.stripeSendLocked(ch); r != stripeSent {
			t.Fatalf("send %d: %d", i, r)
		}
		ch.unref()
	}
	if len(y.q) != 1 {
		t.Fatalf("held sub got %d new chunks", len(y.q)-1)
	}
	// the link delivers 100 ms after the rescue: a "fast" (false) trigger,
	// the hold ends
	ly.la.acked = 1000 + 5000
	if m.dl242CheckLocked(y, resAt.Add(100*time.Millisecond)) || y.dl242.hold || dl242Stat.fast.Load() != f0+1 {
		t.Fatalf("delivery after rescue: %+v", y.dl242)
	}
	p.q = nil

	// byte cap: only the hole (lowest offsets, <= dl243MaxBytes)
	big := &msub{m: m, stripe: true, idx: 5, done: make(chan struct{}), stJoinAt: now, kick: make(chan struct{}, 1)}
	m.subs = append(m.subs, big)
	big.dl242 = dl242Sub{armed: true}
	for i := 5; i >= 0; i-- {
		big.sent = append(big.sent, sentRec{c: mk(uint64(100000+i*(64<<10)), 64<<10), t: now, w: 1, marked: true})
	}
	links[big] = &tcpv{la: linkAck{acked: 0, unacked: 10, rtoLoss: true}, q: 1 << 20, ok: true}
	m.dl242CheckLocked(big, now)
	if !m.dl242CheckLocked(big, now.Add(time.Second)) || len(p.q) != 4 {
		t.Fatalf("byte cap: primary got %d chunks, want 4 (256 KiB)", len(p.q))
	}
	if p.q[0].off != 100000 || p.q[3].off != 100000+3*(64<<10) {
		t.Fatalf("byte cap: not the lowest offsets (%d..%d)", p.q[0].off, p.q[3].off)
	}
	p.q = nil

	// per-conn cap: a third rescue within dl243ConnWin is skipped
	c3 := &msub{m: m, stripe: true, idx: 6, done: make(chan struct{}), stJoinAt: now, kick: make(chan struct{}, 1)}
	m.subs = append(m.subs, c3)
	c3.dl242 = dl242Sub{armed: true}
	c3.sent = []sentRec{{c: mk(900000, 1000), t: now, w: 1, marked: true}}
	links[c3] = &tcpv{la: linkAck{unacked: 1, rtoLoss: true}, q: 1000, ok: true}
	cp0 := dl242Stat.capped.Load()
	m.dl242CheckLocked(c3, now)
	if m.dl242CheckLocked(c3, now.Add(time.Second)) || len(p.q) != 0 || dl242Stat.capped.Load() != cp0+1 {
		t.Fatal("third rescue in 10 s not capped")
	}
	// ... and allowed again after the window
	t3 := now.Add(dl243ConnWin + 2*time.Second)
	c3.dl242, c3.stJoinAt = dl242Sub{armed: true}, t3
	c3.sent[0].t = t3
	m.dl242CheckLocked(c3, t3)
	if !m.dl242CheckLocked(c3, t3.Add(time.Second)) || len(p.q) != 1 {
		t.Fatal("rescue after the window not allowed")
	}
	p.q = nil

	// a sub whose link delivers in time: no rescue, counted ok
	z := &msub{m: m, stripe: true, idx: 2, done: make(chan struct{}), stJoinAt: now}
	m.subs = append(m.subs, z)
	z.dl242 = dl242Sub{armed: true}
	z.sent = []sentRec{{c: mk(20000, 1000), t: now, w: 1000, marked: true}}
	lz := &tcpv{la: linkAck{acked: 500, unacked: 2}, q: 1000, ok: true}
	links[z] = lz
	m.dl242CheckLocked(z, now)
	lz.la.acked = 1500
	ok0 := dl242Stat.ok.Load()
	if m.dl242CheckLocked(z, now.Add(2*time.Second)) || dl242Stat.ok.Load() != ok0+1 || len(p.q) != 0 || z.dl242.hold {
		t.Fatal("a delivering sub was rescued")
	}
	// no TCP view: never rescued
	nv := &msub{m: m, stripe: true, idx: 7, done: make(chan struct{}), stJoinAt: now}
	nv.dl242 = dl242Sub{armed: true}
	nv.sent = []sentRec{{c: mk(70000, 1000), t: now, w: 1000, marked: true}}
	nt0 := dl242Stat.noTCP.Load()
	if m.dl242CheckLocked(nv, now) || dl242Stat.noTCP.Load() != nt0+1 || len(p.q) != 0 {
		t.Fatal("a sub without a TCP view: want no watch")
	}
	// past dl242Watch: never rescued
	v := &msub{m: m, stripe: true, idx: 3, done: make(chan struct{}), stJoinAt: now}
	v.dl242 = dl242Sub{armed: true}
	v.sent = []sentRec{{c: mk(30000, 1000), t: now, w: 1000, marked: true}}
	links[v] = &tcpv{la: linkAck{unacked: 1, rtoLoss: true}, q: 1000, ok: true}
	m.dl242CheckLocked(v, now)
	if m.dl242CheckLocked(v, now.Add(dl242Watch+time.Millisecond)) || len(p.q) != 0 {
		t.Fatal("rescued past dl242Watch")
	}
	// no proven target (primary dead, the only other sub unproven): skipped, retried
	m.dl243Res = [dl243ConnMax]time.Time{}
	p.dead = true
	u := &msub{m: m, stripe: true, idx: 4, done: make(chan struct{}), stJoinAt: now}
	u.dl242 = dl242Sub{armed: true}
	u.sent = []sentRec{{c: mk(40000, 1000), t: now, w: 1000, marked: true}}
	links[u] = &tcpv{la: linkAck{unacked: 1, rtoLoss: true}, q: 1000, ok: true}
	m.subs = []*msub{p, u}
	nt := dl242Stat.noTarget.Load()
	m.dl242CheckLocked(u, now)
	if !m.dl242CheckLocked(u, now.Add(time.Second)) || dl242Stat.noTarget.Load() != nt+1 {
		t.Fatal("no target: want a skip and another look")
	}
}

// The peer side of a rescue: the copy on the primary fills the hole, the
// young sub's late copy (and an overlapping one) is dropped, the reader gets
// every byte once. This is the rel49 reassembly an old Iran build has too.
func TestDL242DuplicateDelivery(t *testing.T) {
	const ck = 1000
	data := make([]byte, 6*ck)
	for i := range data {
		data[i] = byte(i * 7)
	}
	m := newMConn(0xd242, false, nil)
	s := &msub{m: m}
	put := func(o, n int) {
		c := getChunk(mRecData)
		copy(c.buf[:], data[o:o+n])
		m.deliver(s, uint64(o), c, n)
	}
	// primary: chunks 1, 2, 4, 5 (0 and 3 sit on the slow young sub)
	put(1*ck, ck)
	put(2*ck, ck)
	put(4*ck, ck)
	put(5*ck, ck)
	// rescue on the primary: 0 and 3
	put(0, ck)
	put(3*ck, ck)
	// the young sub finally delivers its copies, plus an overlapping span
	put(0, ck)
	put(3*ck, ck)
	put(ck/2, 2*ck)
	m.recvFin(uint64(len(data)))
	got, err := io.ReadAll(m)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("got %d bytes, want %d once, in order", len(got), len(data))
	}
}
