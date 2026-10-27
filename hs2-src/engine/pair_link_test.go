package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
	"github.com/xtaci/smux"
)

// Round 2, candidate β1 (decisions T16 to T18): an hs2 link is a pair of
// TCP connections, QUIET and BULK, under one smux session.

// The probe on the same link as 4 downloads and 2 uploads (§5.1's (d)) on a
// dropping bottleneck (fq_codel's: the flow with a standing queue loses 2%
// of its packets, each a round trip late with all behind it): on a pair the
// probe rides QUIET, which has no standing queue and loses nothing, and
// stays within RTT + 20 ms; on one connection it is behind every hole of the
// bulk's. (A FIFO bottleneck is the rig's: this pipe serves the sockets'
// unsent bytes, so its FIFO would queue QUIET behind BULK's host backlog,
// which C3 keeps on the host and the real path never sees, and it models
// no α, which holds the real FIFO's queue.)
func TestPairLinkProbeLossy(t *testing.T) {
	angleCores(t)
	if testing.Short() {
		t.Skip("emulated link, 40 s")
	}
	run := func(pair bool, o ppOpts) (p99 time.Duration, n int, down, up float64, lost []int) {
		caps := pairCaps()
		if !pair {
			caps &^= tlscarrier.CapPair
		}
		l := newAngleLinkPipe(t, angleRateBps, angleDelay, caps, pair, o)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var dn atomic.Uint64
		l.download(ctx, 7, 4, &dn)
		upc := l.upload(ctx, 7, 2)
		p := l.probe(ctx, 7)
		time.Sleep(2 * time.Second)
		p.on()
		r := angleRate(10*time.Second, &dn, upc)
		p99, n = p.pct(0.99)
		_, lost, _ = l.pdown.stats()
		return p99, n, r[0], r[1], lost
	}
	lossy := ppOpts{loss: 0.02, lossQ: 4 << 10, seed: 5}
	pp, pn, pd, pu, pl := run(true, lossy)
	sp, sn, sd, su, sl := run(false, lossy)
	t.Logf("fq, 2%% drops: pair probe p99 %v (%d), down %.2f up %.2f Mbit/s, lost QUIET/BULK %v; one connection p99 %v (%d), down %.2f up %.2f, lost %v",
		pp, pn, pd*8/1e6, pu*8/1e6, pl, sp, sn, sd*8/1e6, su*8/1e6, sl)
	if pn < 40 || sn < 20 {
		t.Fatalf("only %d / %d probes", pn, sn)
	}
	if pl[0] != 0 {
		t.Fatalf("QUIET lost %d packets: it has no standing queue", pl[0])
	}
	if pp > angleBound {
		t.Fatalf("pair: probe p99 %v, want <= %v (RTT %v + 20 ms)", pp, angleBound, angleRTT)
	}
	if sp <= pp {
		t.Fatalf("one connection's probe p99 %v no worse than the pair's %v: the drops did not show", sp, pp)
	}
	for _, x := range []float64{pd, pu} {
		if x < 0.9*angleRateBps {
			t.Fatalf("a direction carried %.2f Mbit/s of 10", x*8/1e6)
		}
	}
}

// Streams move once their sending is bulk (§3.5: 64 KiB over 2 s and a
// backlog held for 200 ms), each end its own direction: the exit's
// downloads, the edge's uploads; the probe and the light streams stay on
// QUIET, which carries next to none of the bulk.
func TestPairLinkMovesBulkOnly(t *testing.T) {
	angleCores(t)
	if testing.Short() {
		t.Skip("emulated link")
	}
	l := newAngleLink(t, angleRateBps, angleDelay)
	if !l.edge.Paired() || !l.exit.Paired() {
		t.Fatal("the link of two R1 binaries is no pair")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var dn atomic.Uint64
	l.download(ctx, 1, 2, &dn)
	l.upload(ctx, 1, 1)
	pr := l.probe(ctx, 2)
	time.Sleep(3 * time.Second)
	pr.on()
	d0, _, _ := l.pdown.stats()
	u0, _, _ := l.pup.stats()
	time.Sleep(2 * time.Second)
	moved := func(s *smux.Session) (bulk, light int) {
		cl := coreLinks.of(s)
		cl.flows.mu.Lock()
		defer cl.flows.mu.Unlock()
		for _, e := range cl.flows.ents {
			if e.st.Moved() {
				bulk++
			} else {
				light++
			}
		}
		return
	}
	eb, el := moved(l.edge)
	xb, xl := moved(l.exit)
	sd, _, _ := l.pdown.stats()
	su, _, _ := l.pup.stats()
	for i := range sd { // the last 2 s: past the moves
		sd[i] -= d0[i]
		su[i] -= u0[i]
	}
	t.Logf("edge: %d moved, %d not; exit: %d moved, %d not; over the last 2 s down QUIET/BULK %d/%d B, up %d/%d B",
		eb, el, xb, xl, sd[0], sd[1], su[0], su[1])
	// the edge: the upload moved; the downloads' and the probe's requests
	// did not. The exit: the downloads moved; the upload's acks and the
	// probe's echo did not
	if eb != 1 || xb != 2 {
		t.Fatalf("moved: edge %d (want the upload), exit %d (want the 2 downloads)", eb, xb)
	}
	if sd[0] > sd[1]/20 || su[0] > su[1]/20 {
		t.Fatalf("QUIET carried %d/%d B against BULK's %d/%d", sd[0], su[0], sd[1], su[1])
	}
	if p99, n := pr.pct(0.99); n == 0 || p99 > angleBound {
		t.Fatalf("probe p99 %v (%d)", p99, n)
	}
}

// One stream against sixteen (P10 (b)) on the pair: with early Done the
// user of one stream has its floor (C/2) at 10 and 100 Mbit/s; without it
// (TuneEarlyDone 0) the share it had is logged, for the report.
func TestPairLinkOneVs16(t *testing.T) {
	angleCores(t)
	if testing.Short() {
		t.Skip("emulated link, 40 s")
	}
	run := func(rate float64) (one, many float64) {
		l := newAngleLink(t, rate, angleDelay)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var a, b atomic.Uint64
		l.download(ctx, 1, 1, &a)
		l.download(ctx, 2, 16, &b)
		time.Sleep(3 * time.Second)
		r := angleRate(6*time.Second, &a, &b)
		return r[0], r[1]
	}
	for _, mb := range []float64{10, 100} {
		c := mb * mbit
		one, many := run(c)
		defer func(v int) { TuneEarlyDone = v }(TuneEarlyDone)
		TuneEarlyDone = 0
		one0, many0 := run(c)
		TuneEarlyDone = 1
		t.Logf("%3.0f Mbit/s: early Done: 1 stream %.3f of its floor (16: %.2f, total %.3f); without: %.3f (16: %.2f, total %.3f)",
			mb, one/(c/2), many*8/1e6, (one+many)/c, one0/(c/2), many0*8/1e6, (one0+many0)/c)
		if one < 0.9*c/2 {
			t.Fatalf("%.0f Mbit/s: the user of one stream got %.3f of its floor, want >= 0.9", mb, one/(c/2))
		}
		if one+many < 0.95*c {
			t.Fatalf("%.0f Mbit/s: the two took %.3f of the link", mb, (one+many)/c)
		}
	}
}

// With a peer of one connection (level 1, or without CapPair) the link is
// one connection, and works as before: a download, an upload and the probe.
func TestPairLinkFallback(t *testing.T) {
	angleCores(t)
	if testing.Short() {
		t.Skip("emulated link")
	}
	l := newAngleLinkCaps(t, angleRateBps, angleDelay, tlscarrier.LocalAuthHdr().Caps&^tlscarrier.CapPair)
	if l.edge.Paired() || l.exit.Paired() {
		t.Fatal("a pair without CapPair")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var dn atomic.Uint64
	l.download(ctx, 1, 2, &dn)
	up := l.upload(ctx, 1, 1)
	p := l.probe(ctx, 1)
	time.Sleep(time.Second)
	p.on()
	r := angleRate(3*time.Second, &dn, up)
	p50, n := p.pct(0.5)
	t.Logf("one connection: down %.2f up %.2f Mbit/s, probe p50 %v (%d)", r[0]*8/1e6, r[1]*8/1e6, p50, n)
	if r[0] < 0.8*angleRateBps || r[1] < 0.8*angleRateBps || n == 0 {
		t.Fatalf("one connection carried %.2f/%.2f Mbit/s, %d probes", r[0]*8/1e6, r[1]*8/1e6, n)
	}
}

// replyOf is pkt's reply: addresses and ports the other way round.
func replyOf(pkt []byte) []byte {
	r := append([]byte(nil), pkt...)
	if r[0]>>4 == 4 {
		copy(r[12:16], pkt[16:20])
		copy(r[16:20], pkt[12:16])
		ihl := int(r[0]&0x0f) * 4
		if r[9] == 6 || r[9] == 17 {
			copy(r[ihl:ihl+2], pkt[ihl+2:ihl+4])
			copy(r[ihl+2:ihl+4], pkt[ihl:ihl+2])
		}
		if r[9] == 1 {
			r[ihl] = 0 // echo reply
		}
		return r
	}
	copy(r[8:24], pkt[24:40])
	copy(r[24:40], pkt[8:24])
	if r[6] == 6 || r[6] == 17 {
		copy(r[40:42], pkt[42:44])
		copy(r[42:44], pkt[40:42])
	}
	return r
}

// v6UDP is an IPv6/UDP packet.
func v6UDP(sport, dport uint16) []byte {
	p := make([]byte, 60)
	p[0], p[6] = 0x60, 17
	p[8], p[23] = 0xfd, 1
	p[24], p[39] = 0xfd, 2
	binary.BigEndian.PutUint16(p[40:], sport)
	binary.BigEndian.PutUint16(p[42:], dport)
	return p
}

// The exit sends a reply on the link its request came on (pinned by
// arrival), not on one the reply's own hash picks; replyFlow is the reply's
// flow, IPv4 and IPv6, with ports and without; a link that dies hands the
// flow to the hash again.
func TestPairTunReplyByArrival(t *testing.T) {
	for _, req := range [][]byte{echoPacket(1, 1), udpPacketN(1, 4000, 200), v6UDP(5000, 53)} {
		h, ok := replyFlow(req)
		if !ok || h != flowHash(replyOf(req)) {
			t.Fatalf("replyFlow %x (%v), the reply's flow %x", h, ok, flowHash(replyOf(req)))
		}
	}
	s := &l3Set{toUser: true}
	links := []*l3Link{{id: 0x11111111}, {id: 0x22222222}, {id: 0x33333333}}
	for _, l := range links {
		s.add(l)
	}
	moved := 0
	for i := 0; i < 200; i++ {
		req := udpPacketN(byte(i), uint16(1000+i), 100)
		reply := flowHash(replyOf(req))
		hashPick := s.pickFlow(reply)
		in := links[i%3]
		s.noteArrival(in, req)
		if got := s.linkFor(reply); got != in {
			t.Fatalf("flow %d: reply on link %x, its request came on %x", i, got.id, in.id)
		}
		if hashPick != in {
			moved++
		}
	}
	if moved < 50 {
		t.Fatalf("the hash agreed with the arrival %d of 200 times: the test shows nothing", 200-moved)
	}
	links[0].dead.Store(true)
	req := udpPacketN(0, 1000, 100) // came on links[0]
	if got := s.linkFor(flowHash(replyOf(req))); got == links[0] || got == nil {
		t.Fatal("a reply kept a dead link")
	}
	// the edge (not toUser) pins as before
	e := &l3Set{}
	e.add(links[1])
	e.noteArrival(links[1], req)
	if len(e.arr) != 0 {
		t.Fatal("the edge pinned by arrival")
	}
}

// End to end over TLS carriers (RunIran and RunKharej): each link is a pair
// (two TCP connections accepted, one link at the exit, max_links counts
// links); a transfer that moves to BULK keeps its bytes in order; when one
// connection of a pair dies, the other goes with it, and the pool rebuilds
// the link.
func TestPairLinkE2E(t *testing.T) {
	angleCores(t)
	key := bytes.Repeat([]byte{0x5b}, 32)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	panel := echoPanel(t)
	raw, _ := net.Listen("tcp", "127.0.0.1:0")
	var mu sync.Mutex
	var conns []*closeConn
	ln2 := &wrapListener{Listener: raw, wrap: func(c net.Conn) net.Conn {
		cc := &closeConn{Conn: c}
		mu.Lock()
		conns = append(conns, cc)
		mu.Unlock()
		return cc
	}}
	srv := &tlscarrier.Server{SharedKey: key, Cert: testCert(t), BackendAddr: "127.0.0.1:1"}
	links := make(chan PoolStats, 1)
	go RunKharej(ctx, KharejConfig{Listener: ln2, Server: srv, Panel: panel, OnStart: func(f StatsFn) {
		go func() {
			for ctx.Err() == nil {
				select {
				case links <- f():
				default:
				}
				time.Sleep(20 * time.Millisecond)
			}
		}()
	}})
	port := freePort(t)
	go RunIran(ctx, IranConfig{
		Dialer: NewMTCPDialer(raw.Addr().String(), "lab.example.com", key, ""),
		Min:    2, Max: 2, PerLink: 50,
		ListenIP: "127.0.0.1", Ports: []string{port},
	})
	user := "127.0.0.1:" + port
	deadline := time.Now().Add(10 * time.Second)
	for {
		if c, err := net.Dial("tcp", user); err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("user port never opened")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// 2 links: 4 TCP connections, 2 links at the exit
	deadline = time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		n := len(conns)
		mu.Unlock()
		st := <-links
		if n == 4 && st.Links == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d TCP connections, %d links at the exit: want 4 and 2", n, st.Links)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// a transfer large enough to move to BULK keeps its bytes in order
	tcpEcho(t, user, 4<<20)
	// one half of a pair dies: the other half goes with it
	mu.Lock()
	victim := conns[1]
	mu.Unlock()
	victim.Conn.Close()
	deadline = time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		closed := 0
		for _, c := range conns[:4] {
			if c.closed.Load() {
				closed++
			}
		}
		mu.Unlock()
		if closed >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("one half of a pair closed, %d of the 4 connections closed: its other half lives on", closed)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// and the pool rebuilds it
	deadline = time.Now().Add(15 * time.Second)
	for {
		mu.Lock()
		n := len(conns)
		mu.Unlock()
		st := <-links
		if n >= 6 && st.Links == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not rebuilt: %d connections, %d links", n, st.Links)
		}
		time.Sleep(50 * time.Millisecond)
	}
	tcpEcho(t, user, 1<<20)
}

// closeConn records that the server closed a raw connection.
type closeConn struct {
	net.Conn
	closed atomic.Bool
}

func (c *closeConn) Close() error { c.closed.Store(true); return c.Conn.Close() }

type wrapListener struct {
	net.Listener
	wrap func(net.Conn) net.Conn
}

func (l *wrapListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return l.wrap(c), nil
}
