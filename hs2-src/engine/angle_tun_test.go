package engine

import (
	"context"
	"encoding/binary"
	"net/netip"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

// Angle A's TUN path: the pinned flow table, the per-flow queues and their
// bounds, and a ping in the latency lane of the link's scheduler.

// A flow keeps the link its first packet picked while that link lives,
// whatever links come (the hash alone would move a share of them to a new
// link); only the flows of a link that dies move.
func TestAngleTunPinned(t *testing.T) {
	angleCores(t)
	var s l3Set
	links := []*l3Link{{id: 0x9e3779b9}, {id: 0x3c6ef372}, {id: 0xdaa66d2b}}
	s.add(links[0])
	s.add(links[1])
	const flows = 3000
	first := make([]*l3Link, flows)
	for i := range first {
		first[i] = s.linkFor(uint32(i) * 2654435761)
	}
	s.add(links[2])
	moved, hashMoves := 0, 0
	for i := range first {
		h := uint32(i) * 2654435761
		if s.linkFor(h) != first[i] {
			moved++
		}
		if s.pickFlow(h) != first[i] {
			hashMoves++
		}
	}
	if moved != 0 {
		t.Fatalf("%d of %d pinned flows moved to a new link", moved, flows)
	}
	if hashMoves < flows/6 {
		t.Fatalf("the hash alone moved only %d of %d flows: the test shows nothing", hashMoves, flows)
	}
	victim := links[0]
	victim.dead.Store(true)
	for i := range first {
		h := uint32(i) * 2654435761
		l := s.linkFor(h)
		switch {
		case l == victim:
			t.Fatal("a flow kept a dead link")
		case first[i] != victim && l != first[i]:
			t.Fatalf("flow %d moved off a live link", i)
		}
	}
	// a pin idle past l3PinIdle goes at the next sweep
	s.sweepPins(ctrlNow()+int64(l3PinIdle)+1, l3PinIdle)
	if len(s.pins) != 0 {
		t.Fatalf("%d idle pins kept", len(s.pins))
	}
}

func ipv4(a, b, c, d byte) netip.Addr { return netip.AddrFrom4([4]byte{a, b, c, d}) }

// udpPacketN is an IPv4/UDP packet of n bytes from port sport of the user
// at 10.99.0.<user>.
func udpPacketN(user byte, sport uint16, n int) []byte {
	p := make([]byte, n)
	p[0] = 0x45
	binary.BigEndian.PutUint16(p[2:], uint16(n))
	p[9] = 17
	copy(p[12:16], []byte{10, 99, 0, user})
	copy(p[16:20], []byte{10, 77, 0, 2})
	binary.BigEndian.PutUint16(p[20:], sport)
	binary.BigEndian.PutUint16(p[22:], 5001)
	return p
}

// echoPacket is an ICMP echo request of the user at 10.99.0.<user>, its
// payload the time it was made.
func echoPacket(user byte, seq uint16) []byte {
	p := make([]byte, 64)
	p[0] = 0x45
	binary.BigEndian.PutUint16(p[2:], 64)
	p[9] = 1
	copy(p[12:16], []byte{10, 99, 0, user})
	copy(p[16:20], []byte{10, 77, 0, 2})
	p[20] = 8
	binary.BigEndian.PutUint16(p[26:], seq)
	binary.BigEndian.PutUint64(p[28:], uint64(time.Now().UnixNano()))
	return p
}

// tunPush queues pkt as pumpTun would.
func tunPush(sq *tunSched, pkt []byte) {
	b := append([]byte(nil), pkt...)
	flow, user, _ := flowKey(b, false)
	sq.push(&b, UserTag(user, classV6Prefix), flow, isEcho(b))
}

// The per-flow queues' bounds: a flow past 64 loses its oldest packets, a
// user past 1024 those of its longest flow, a link past tunLinkPkts its
// fattest user's; a packet that waited past 20 ms is dropped when its frame
// is built; a ping's frame is one packet in the latency lane, a bulk flow's
// up to the link's bulk frame; and what is queued counts with its user's
// memory until it leaves.
func TestAngleTunFlowQueues(t *testing.T) {
	angleCores(t)
	l := newAngleLink(t, angleRateBps, angleDelay)
	st, err := l.edge.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	sq := newTunSched(st)
	var pool sync.Pool
	var drops atomic.Uint64
	sq.attach(&pool, &drops)
	// one flow
	for i := 0; i < 70; i++ {
		tunPush(sq, udpPacketN(1, 1000, 500))
	}
	if f := sq.flows[flowHash(udpPacketN(1, 1000, 500))]; f == nil || f.n() != tunFlowPkts || drops.Load() != 70-tunFlowPkts {
		t.Fatalf("flow holds %d, drops %d: want %d and %d", f.n(), drops.Load(), tunFlowPkts, 70-tunFlowPkts)
	}
	// one user's flows
	for i := 0; i < 20; i++ {
		for k := 0; k < 60; k++ {
			tunPush(sq, udpPacketN(2, uint16(2000+i), 500))
		}
	}
	u2 := sq.users[UserTag(ipv4(10, 99, 0, 2), classV6Prefix)]
	if u2.pkts != tunUserPkts {
		t.Fatalf("user holds %d, want %d", u2.pkts, tunUserPkts)
	}
	for f := range u2.flows {
		if f.n() < 45 {
			t.Fatalf("a flow of the user was cut to %d: the longest should lose first", f.n())
		}
	}
	// the link's bound: users 3.. until past it; the fattest loses
	d0 := drops.Load()
	for u := byte(3); u < 10; u++ {
		for i := 0; i < 16; i++ {
			for k := 0; k < 40; k++ {
				tunPush(sq, udpPacketN(u, uint16(3000+i), 200))
			}
		}
	}
	if sq.pkts > tunLinkPkts {
		t.Fatalf("link holds %d, want <= %d", sq.pkts, tunLinkPkts)
	}
	if drops.Load() == d0 || u2.pkts >= tunUserPkts {
		t.Fatalf("past the link's bound the fattest user (%d queued) lost nothing", u2.pkts)
	}
	tag1 := UserTag(ipv4(10, 99, 0, 1), classV6Prefix)
	if n := userMems.acct(tag1).n.Load(); n != int64(tunFlowPkts*500) {
		t.Fatalf("user 1's memory %d, want %d", n, tunFlowPkts*500)
	}
	// a ping: one packet a frame, the latency lane
	sq.close()
	sq = newTunSched(st)
	sq.attach(&pool, &drops)
	tunPush(sq, echoPacket(1, 1))
	tunPush(sq, echoPacket(1, 2))
	f, b, lane := sq.next() // both, in one latency-lane frame
	if f == nil || lane != 1 || len(b) != 2*(15+64) {
		t.Fatalf("ping frame: lane %d, %d bytes", lane, len(b))
	}
	// a bulk flow (judged bulk by C1): frames up to the bulk frame
	for i := 0; i < 40; i++ {
		tunPush(sq, udpPacketN(4, 4000, 1400))
	}
	g := sq.flows[flowHash(udpPacketN(4, 4000, 1400))]
	g.class = ClassBulk
	f, b, lane = sq.next()
	if want := max(1, l.edge.BulkFrame()/1415) * 1415; f != g || lane != 2 || len(b) != want {
		t.Fatalf("bulk frame: lane %d, %d bytes, want %d (bulk frame %d)", lane, len(b), want, l.edge.BulkFrame())
	}
	// stale: dropped as its frame is built
	sq.close()
	sq = newTunSched(st)
	sq.attach(&pool, &drops)
	tunPush(sq, udpPacketN(5, 5000, 300))
	time.Sleep(tunFlowWait + 5*time.Millisecond)
	d0 = drops.Load()
	if f, _, _ := sq.next(); f != nil || drops.Load() != d0+1 {
		t.Fatalf("a stale packet went out (or was not counted)")
	}
	sq.close()
	for _, u := range []byte{1, 2, 3, 4, 5} {
		if n := userMems.acct(UserTag(ipv4(10, 99, 0, u), classV6Prefix)).n.Load(); n != 0 {
			t.Fatalf("user %d's memory %d after the link closed", u, n)
		}
	}
}

// Over user_mem_share under pressure, a user's new TUN packets are turned
// away (counted), another user's are not, and the status counts the user at
// its cap.
func TestAngleTunUserMemShare(t *testing.T) {
	angleCores(t)
	defer func(s, p int) { UserMemShare, UserMemPool = s, p }(UserMemShare, UserMemPool)
	UserMemShare, UserMemPool = 8<<10, 0 // pressure: any byte queued
	l := newAngleLink(t, angleRateBps, angleDelay)
	st, err := l.edge.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	sq := newTunSched(st)
	var pool sync.Pool
	var drops atomic.Uint64
	sq.attach(&pool, &drops)
	defer sq.close()
	for i := 0; i < 30; i++ {
		tunPush(sq, udpPacketN(21, uint16(6000+i%4), 1000))
	}
	u := sq.users[UserTag(ipv4(10, 99, 0, 21), classV6Prefix)]
	if u.pkts > 9 || drops.Load() < 21 {
		t.Fatalf("user over its share kept %d packets, %d dropped", u.pkts, drops.Load())
	}
	if userMems.atCap() < 1 {
		t.Fatal("no user at its memory cap")
	}
	tunPush(sq, udpPacketN(22, 7000, 1000))
	if v := sq.users[UserTag(ipv4(10, 99, 0, 22), classV6Prefix)]; v == nil || v.pkts != 1 {
		t.Fatal("another user's first packet was turned away")
	}
}

// paceTun is a TUN device that hands pumpTun what is put in it.
type paceTun struct {
	in  chan []byte
	got func([]byte)
}

func (p *paceTun) MTU() int { return 1500 }
func (p *paceTun) Read(b []byte) (int, error) {
	return copy(b, <-p.in), nil
}
func (p *paceTun) Write(b []byte) (int, error) {
	if p.got != nil {
		p.got(b)
	}
	return len(b), nil
}

// A ping on the TUN is light: next to a TUN bulk flow past the link's rate
// and two user uploads on the same link, its one-way delay stays within
// the path's and 10 ms (§5.1), while the bulk flow loses what does not
// fit. Since T16 an hs2 link is a QUIET and a BULK connection, so
// "quiet_bulk" is the gate: the TUN's flows and the uploads open on the
// QUIET connection, the bulk flow and the uploads move once to the BULK
// one, the ping stays (an echo never moves), the bottleneck as fq_codel
// serves a sparse flow next to backlogged ones. "one_session" keeps one
// smux session (a peer without QUIET links: level 1, or HS2_TUNE_QUIET=0):
// the ping goes in the latency lane of the link's scheduler ahead of the
// bulk flow, but waits besides behind the edge socket's unsent bytes,
// which C3 holds to its cap (T12's default: 9.3 ms at 10 Mbit/s), so its
// bound is the path's, 10 ms and that cap.
func TestAngleTunPingAheadOfBulk(t *testing.T) {
	angleCores(t)
	if testing.Short() {
		t.Skip("emulated links")
	}
	t.Run("quiet_bulk", quietTunPing)
	t.Run("one_session", oneSessionTunPing)
}

// tunPingRx is the exit's TUN of tunPing: the pings' one-way delays, the
// bulk flow's bytes, and what of each came over a BULK connection.
type tunPingRx struct {
	mu                 sync.Mutex
	oneWay             []time.Duration
	bulkIn, bulkOnBulk atomic.Uint64
	pings, pingsOnBulk atomic.Uint64
}

// dev is a TUN device for the exit's link of class bulk (or not).
func (x *tunPingRx) dev(bulk bool) *paceTun {
	return &paceTun{in: make(chan []byte), got: func(b []byte) {
		if len(b) >= 36 && b[9] == 1 {
			d := time.Duration(time.Now().UnixNano() - int64(binary.BigEndian.Uint64(b[28:])))
			x.mu.Lock()
			x.oneWay = append(x.oneWay, d)
			x.mu.Unlock()
			x.pings.Add(1)
			if bulk {
				x.pingsOnBulk.Add(1)
			}
			return
		}
		x.bulkIn.Add(uint64(len(b)))
		if bulk {
			x.bulkOnBulk.Add(uint64(len(b)))
		}
	}}
}

// reset starts the pings' count anew.
func (x *tunPingRx) reset() {
	x.mu.Lock()
	x.oneWay = nil
	x.mu.Unlock()
	x.pings.Store(0)
	x.pingsOnBulk.Store(0)
}

// p99 is the pings' 99th percentile one-way delay so far, its median and
// their count.
func (x *tunPingRx) p99() (p99, p50 time.Duration, n int) {
	x.mu.Lock()
	r := append([]time.Duration(nil), x.oneWay...)
	x.mu.Unlock()
	if len(r) == 0 {
		return 0, 0, 0
	}
	sort.Slice(r, func(i, j int) bool { return r[i] < r[j] })
	return r[(len(r)*99+99)/100-1], r[len(r)/2], len(r)
}

// tunPingLoad feeds the edge's TUN the bulk flow (1400 B packets at ~12
// Mbit/s, past the link's 10) until ctx ends.
func tunPingLoad(ctx context.Context, edgeTun *paceTun) {
	tk := time.NewTicker(time.Millisecond)
	defer tk.Stop()
	for ctx.Err() == nil {
		<-tk.C
		select {
		case edgeTun.in <- udpPacketN(31, 9000, 1400):
		case <-ctx.Done():
			return
		}
	}
}

// tunPings sends tunPingN pings, one each 20 ms, from the edge's TUN (200:
// the p99 is the second worst, not the worst, so one hiccup of the host
// under the emulated links is not the verdict).
func tunPings(edgeTun *paceTun) {
	for i := 0; i < tunPingN; i++ {
		edgeTun.in <- echoPacket(32, uint16(i))
		time.Sleep(20 * time.Millisecond)
	}
}

const tunPingN = 200

func quietTunPing(t *testing.T) {
	r := newQuietRig(t, angleRateBps, angleDelay, false)
	x := &tunPingRx{}
	exitSet := &l3Set{toUser: true}
	r.onL3 = func(p *classPair, st *smux.Stream) {
		pl := newStreamL3Link(st, func() bool { return false }, nil)
		pl.class = l3ClassOf(p == r.quiet, p == r.bulk) // as serveStream: replies by arrival
		exitSet.add(pl)
		exitSet.serveLink(r.ctx, pl, x.dev(p == r.bulk))
		exitSet.remove(pl)
	}
	edgeSet := &l3Set{}
	edgeTun := &paceTun{in: make(chan []byte, 1024)}
	openL3(r.ctx, r.quiet.edge, edgeSet, edgeTun, t.Logf) // as RunIran: one per link
	openL3(r.ctx, r.bulk.edge, edgeSet, edgeTun, t.Logf)
	edgeSet.mu.RLock()
	n := len(edgeSet.links)
	var bulkLink *l3Link
	for _, l := range edgeSet.links {
		if l.class == l3Bulk {
			bulkLink = l
		}
	}
	edgeSet.mu.RUnlock()
	if n != 2 || bulkLink == nil || bulkLink.sq == nil {
		t.Fatalf("the edge's TUN links: %d, a BULK one with per-flow queues %v", n, bulkLink != nil && bulkLink.sq != nil)
	}
	go edgeSet.pumpTun(r.ctx, edgeTun)
	for i := 0; i < 2; i++ {
		st := r.open(31, qpUp)
		go func() {
			defer st.Close()
			b := make([]byte, 32<<10)
			for r.ctx.Err() == nil {
				if _, err := st.Write(b); err != nil {
					return
				}
			}
		}()
	}
	go tunPingLoad(r.ctx, edgeTun)
	deadline := time.Now().Add(5 * time.Second)
	for (edgeSet.moves.Load() < 1 || r.q.moved.Load() < 2) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if m := edgeSet.moves.Load(); m != 1 {
		t.Fatalf("%d TUN flows moved to the BULK link, want the bulk one", m)
	}
	if m := r.q.moved.Load(); m != 2 {
		t.Fatalf("%d of the 2 uploads moved to the BULK link (%s)", m, r.q.line())
	}
	time.Sleep(time.Second) // the moves settle (the QUIET link's backlog drains)
	bulk0, onBulk0 := x.bulkIn.Load(), x.bulkOnBulk.Load()
	x.reset()
	tunPings(edgeTun)
	p99, p50, k := x.p99()
	bulkIn, onBulk := x.bulkIn.Load()-bulk0, x.bulkOnBulk.Load()-onBulk0
	ld, bd := bulkLink.sq.dropsLine()
	bound := angleDelay + 10*time.Millisecond
	t.Logf("ping one-way p50 %v p99 %v (%d, %d over BULK); TUN bulk in %.2f Mbit/s (%.0f%% over BULK); BULK drops light/bulk %d/%d; bound %v; %s",
		p50, p99, k, x.pingsOnBulk.Load(), float64(bulkIn)*8/1e6/4, 100*float64(onBulk)/float64(max(bulkIn, 1)), ld, bd, bound, r.q.line())
	if k < tunPingN*9/10 {
		t.Fatalf("only %d of %d pings arrived", k, tunPingN)
	}
	if x.pingsOnBulk.Load() != 0 {
		t.Fatalf("%d pings came over the BULK link", x.pingsOnBulk.Load())
	}
	if p99 > bound {
		t.Fatalf("ping p99 one-way %v, want <= %v", p99, bound)
	}
	if onBulk < bulkIn*9/10 {
		t.Fatalf("the bulk flow's packets came %d of %d B over the BULK link", onBulk, bulkIn)
	}
	if bd == 0 {
		t.Fatal("the bulk flow past the link's rate lost nothing on the BULK link")
	}
}

func oneSessionTunPing(t *testing.T) {
	l := newAngleLink(t, angleRateBps, angleDelay)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	x := &tunPingRx{}
	exitSet := &l3Set{toUser: true}
	l.onL3 = func(st *smux.Stream) {
		pl := newStreamL3Link(st, func() bool { return false }, nil)
		exitSet.add(pl)
		exitSet.serveLink(ctx, pl, x.dev(false))
	}
	st, err := l.edge.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write([]byte{kindL3}); err != nil {
		t.Fatal(err)
	}
	edgeSet := &l3Set{}
	pl := newStreamL3Link(st, func() bool { return false }, nil)
	if pl.sq == nil {
		t.Fatal("no per-flow queues on a link with C2")
	}
	edgeSet.add(pl)
	edgeTun := &paceTun{in: make(chan []byte, 1024)}
	go edgeSet.serveLink(ctx, pl, edgeTun)
	go edgeSet.pumpTun(ctx, edgeTun)
	l.upload(ctx, 31, 2)
	go tunPingLoad(ctx, edgeTun)
	time.Sleep(2 * time.Second)
	x.reset()
	bulk0 := x.bulkIn.Load()
	tunPings(edgeTun)
	p99, p50, k := x.p99()
	ld, bd := pl.sq.dropsLine()
	bound := angleDelay + 10*time.Millisecond + angleHostQ()
	t.Logf("ping one-way p50 %v p99 %v (%d); TUN bulk in %.2f Mbit/s; drops light/bulk %d/%d; bound %v",
		p50, p99, k, float64(x.bulkIn.Load()-bulk0)*8/1e6/4, ld, bd, bound)
	if k < tunPingN*9/10 {
		t.Fatalf("only %d of %d pings arrived", k, tunPingN)
	}
	if p99 > bound {
		t.Fatalf("ping p99 one-way %v, want <= %v (the path's, 10 ms and C3's cap %v)", p99, bound, angleHostQ())
	}
	if bd == 0 {
		t.Fatal("the bulk flow past the link's rate lost nothing")
	}
}
