package engine

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// R1 red team, second pass (method step 4; docs/architecture/wip/r1/
// redteam2/REPORT.fa.md): the two-way rule against a download whose app
// acknowledges in its own data (R2-1), and the idle QUIET link's one-way
// delay beat (Z10).

func ring(v ...uint32) []uint32 {
	r := make([]uint32, twoWayWin)
	for i := range r {
		r[i] = v[i%len(v)]
	}
	return r
}

// R2-1: QUIC (or any UDP) inside a VLESS connection, or a VPN over TCP
// inside one, acknowledges in its own data: its user sends 1 to 3 % of
// what it gets, in every tick, far under RTMax. The two-way rule read that
// as a call: neither the exit's hint nor the edge's move, so the download
// stayed on its QUIET link (cubic) at any rate. A call, also a lopsided
// one, stays two-way; a download and an upload acknowledged in their own
// data do not.
func TestRed2TwoWayAckedDownload(t *testing.T) {
	cases := []struct {
		name        string
		user, other []uint32 // bytes a move tick (100 ms)
		seen        int
		old, fixed  int // twoWayState with TwoWayRatio 0 and 16
	}{
		{"call 1.5/1.5 Mbit/s", ring(18750), ring(18750), 10, twoWayYes, twoWayYes},
		{"call held back (clumps)", ring(37500, 0), ring(0, 0, 0, 61440), 10, twoWayNo, twoWayNo},
		{"call, camera small: 0.25 up, 2.4 Mbit/s down", ring(3125), ring(30000), 10, twoWayYes, twoWayYes},
		{"call, camera small, held back: 0.25 up, 4 Mbit/s down in clumps", ring(3125), ring(90000, 0), 10, twoWayYes, twoWayYes},
		{"QUIC download 5 Mbit/s, acks 2 %", ring(1250), ring(62500), 10, twoWayYes, twoWayNo},
		{"QUIC download 40 Mbit/s, acks 2 %", ring(10000), ring(500000), 10, twoWayYes, twoWayNo},
		{"VPN over TCP, 10 Mbit/s down, acks 3 %", ring(3750), ring(125000), 10, twoWayYes, twoWayNo},
		{"QUIC upload 3.2 Mbit/s, acks 3 %", ring(40000), ring(1200), 10, twoWayYes, twoWayNo},
		{"QUIC download, young (3 ticks)", ring(1250), ring(62500), 3, twoWayPending, twoWayNo},
		{"call, young (3 ticks)", ring(18750), ring(18750), 3, twoWayPending, twoWayPending},
		{"plain download (request once)", ring(400, 0, 0, 0, 0, 0, 0, 0, 0, 0), ring(500000), 10, twoWayNo, twoWayNo},
	}
	for _, c := range cases {
		for _, k := range []struct {
			ratio, want int
		}{{0, c.old}, {16, c.fixed}} {
			withTune(t, TwoWayRatio, k.ratio)
			if got := twoWayState(c.user, c.other, twoWayWin-1, c.seen); got != k.want {
				t.Errorf("%s, ratio %d: state %d, want %d", c.name, k.ratio, got, k.want)
			}
		}
	}
}

// R2-1 in the realtime lane: a steady download of 3 Mbit/s acknowledged in
// its own data (the exit's side: it sends the download, gets the acks) got
// the hold (RTHold) as a call does once it turned to clumps; with the ratio
// it is never loosely a call, so its hold runs out. A call in clumps keeps
// it either way (TestRTHoldKeepsCall).
func TestRed2HoldAckedDownload(t *testing.T) {
	steady := func(n int) [][2]int {
		var v [][2]int
		for range n {
			v = append(v, [2]int{37500, 900})
		}
		return v
	}
	clumps := func(n int) [][2]int {
		var v [][2]int
		for i := range n {
			tx := 0
			if i%4 == 3 {
				tx = 60 << 10
			}
			v = append(v, [2]int{tx, 900})
		}
		return v
	}
	zeros := map[int]int{}
	for _, ratio := range []int{0, 16} {
		withTune(t, TwoWayRatio, ratio)
		withTune(t, RTHold, 20)
		var c flowClassifier
		var f flowStats
		var at time.Duration
		classRun(&c, &f, &at, steady(30))
		b := classRun(&c, &f, &at, clumps(60))
		for _, v := range b {
			if v == 0 {
				zeros[ratio]++
			}
		}
		t.Logf("ratio %d: the acked download in clumps for 6 s: %d of %d ticks without a realtime allowance", ratio, zeros[ratio], len(b))
	}
	if zeros[0] > 10 {
		t.Fatalf("without the ratio the acked download lost the hold (%d ticks without): the case is not shown", zeros[0])
	}
	if zeros[16] < 30 {
		t.Fatalf("with the ratio the acked download kept the realtime hold (%d of 60 ticks without)", zeros[16])
	}
}

// Z10: an idle QUIET link's one-way delay loop sends nothing while no BULK
// rate is noted (OWDIdle), sends at once when one is, and parks again
// owdIdleAfter after the last; with OWDIdle 0 it beats every 0.5 to 1.5 s.
func TestRed2OWDIdleParks(t *testing.T) {
	run := func(idle int, steps func(a, b *owdLink)) {
		withTune(t, OWDIdle, idle)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		c1, c2 := net.Pipe()
		a, b := &owdLink{quiet: true}, &owdLink{quiet: true}
		done := make(chan struct{}, 2)
		go func() { owdLoop(ctx, c1, a); done <- struct{}{} }()
		go func() { owdLoop(ctx, c2, b); done <- struct{}{} }()
		steps(a, b)
		cancel()
		<-done
		<-done
	}
	run(0, func(a, b *owdLink) {
		time.Sleep(3500 * time.Millisecond)
		if n := a.msgs.Load() + b.msgs.Load(); n < 3 {
			t.Fatalf("OWDIdle 0: %d messages in 3.5 s, want the 1 s beat", n)
		}
		t.Logf("OWDIdle 0, idle 3.5 s: %d + %d messages", a.msgs.Load(), b.msgs.Load())
	})
	run(1, func(a, b *owdLink) {
		time.Sleep(2500 * time.Millisecond)
		if n := a.msgs.Load() + b.msgs.Load(); n != 0 {
			t.Fatalf("idle pool: %d messages in 2.5 s, want none", n)
		}
		t0 := time.Now()
		a.noteRate(1e6)
		b.noteRate(1e6)
		for a.msgs.Load() == 0 || b.msgs.Load() == 0 {
			if time.Since(t0) > 300*time.Millisecond {
				t.Fatalf("a busy pool: no message both ways 300 ms after the rate (%d, %d)", a.msgs.Load(), b.msgs.Load())
			}
			time.Sleep(5 * time.Millisecond)
		}
		first := time.Since(t0)
		// busy for 2 s: the beat follows the rate (owdTickOf)
		for range 40 {
			a.noteRate(1e6)
			b.noteRate(1e6)
			time.Sleep(50 * time.Millisecond)
		}
		busy := a.msgs.Load() + b.msgs.Load()
		// idle again: parked after owdIdleAfter and a beat
		time.Sleep(owdIdleAfter + 400*time.Millisecond)
		n0 := a.msgs.Load() + b.msgs.Load()
		time.Sleep(2 * time.Second)
		n1 := a.msgs.Load() + b.msgs.Load()
		t.Logf("first message both ways %v after the rate; %d messages in 2 s busy; %d in the 2 s after parking (parks %d, %d)",
			first.Round(time.Millisecond), busy, n1-n0, a.parks.Load(), b.parks.Load())
		if busy < 20 {
			t.Fatalf("busy: %d messages in 2 s at 1 MB/s (the beat owdTickOf: ~100 ms)", busy)
		}
		if n1 != n0 {
			t.Fatalf("idle again: %d messages after parking", n1-n0)
		}
	})
}

// countLink is rateLink (udp_hol_test.go) at loopback speed whose exit
// counts the UDP streams it accepted, and the datagrams by client id.
func countLink(t *testing.T, ctx context.Context, streams *atomic.Int64, seen *udpSeen) *mtcpLink {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	acc := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); acc <- c }()
	cc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	sc := <-acc
	cli, _, err := newSession(cc, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv, _, err := newSession(sc, true, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { <-ctx.Done(); cli.Close(); srv.Close(); cc.Close(); sc.Close() }()
	go func() {
		for {
			st, err := srv.AcceptStream()
			if err != nil {
				return
			}
			go func() {
				var k [3]byte
				if _, err := io.ReadFull(st, k[:]); err != nil || k[0] != kindUDPPort {
					io.Copy(io.Discard, st)
					return
				}
				streams.Add(1)
				buf := make([]byte, maxDatagram)
				for {
					p, err := readDatagram(st, buf)
					if err != nil {
						return
					}
					if len(p) >= 12 {
						seen.add(binary.BigEndian.Uint32(p), 0)
					}
				}
			}()
		}
	}()
	return &mtcpLink{sess: cli}
}

// Item 7: one client sends single datagrams from 3000 source ports (no
// spoofing: a new socket each) to a UDP user port. Each made a flow, a
// stream to the exit and a UDP socket there for udpIdle; with the caps the
// client gets UDPFlowsPerUser flows and another user's game still gets
// through.
func TestRed2UDPFlowFlood(t *testing.T) {
	for _, tc := range []struct {
		name          string
		total, perUsr int
	}{{"no caps (as before)", 0, 0}, {"caps 4096 / 256", 4096, 256}} {
		t.Run(tc.name, func(t *testing.T) {
			withTune(t, UDPFlowsMax, tc.total)
			withTune(t, UDPFlowsPerUser, tc.perUsr)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var streams atomic.Int64
			seen := &udpSeen{lats: map[uint32][]time.Duration{}}
			lm := NewLinkManager(nil, 1, 4, 8, nil)
			addManaged(lm, countLink(t, ctx, &streams, seen))
			pc, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer pc.Close()
			logs := make(chan string, 16)
			go serveUserUDP(ctx, pc, lm, 0, func(f string, a ...any) {
				select {
				case logs <- fmt.Sprintf(f, a...):
				default:
				}
			})
			addr := pc.LocalAddr().String()
			b := make([]byte, 16)
			binary.BigEndian.PutUint32(b, 9)
			for range 3000 {
				c, err := net.Dial("udp", addr)
				if err != nil {
					t.Fatal(err)
				}
				c.Write(b)
				c.Close()
			}
			// another user (127.0.0.2): a game's 100 datagrams
			var sent atomic.Int64
			_, port, _ := net.SplitHostPort(addr)
			game := func() {
				c, err := net.DialUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.2")}, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: atoiT(t, port)})
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				g := make([]byte, 64)
				binary.BigEndian.PutUint32(g, 7)
				for range 100 {
					c.Write(g)
					sent.Add(1)
					time.Sleep(5 * time.Millisecond)
				}
			}
			game()
			time.Sleep(500 * time.Millisecond)
			n, got := streams.Load(), len(seen.get(7))
			t.Logf("%s: %d UDP streams at the exit for 3000 source ports; the other user's game %d of %d datagrams; cap drops %d",
				tc.name, n, got, sent.Load(), udpCapDrops.Load())
			if tc.perUsr > 0 && n > int64(tc.perUsr)+1 {
				t.Fatalf("%d streams, past the cap of %d a user (and one for the game)", n, tc.perUsr)
			}
			if tc.perUsr == 0 && n < 2500 {
				t.Fatalf("without caps only %d streams for 3000 source ports (the case is not shown)", n)
			}
			if float64(got) < 0.95*float64(sent.Load()) {
				t.Fatalf("the other user's game got %d of %d datagrams", got, sent.Load())
			}
		})
	}
}

func atoiT(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// R2-3: a retiring QUIET link (QuietFollow) is closed only after it had no
// connection for QuietFollowGraceMs: a connection that just moved to BULK
// let its slot go, but its main stream still takes the exit's frames sent
// before the switch; closed at once, the group read a hole and ended.
func TestRed2FollowCloseGrace(t *testing.T) {
	const s = int64(time.Second)
	withTune(t, QuietFollowGraceMs, 5000)
	ql := &quietLink{}
	t0 := int64(1000 * s)
	if followCloseNow(ql, 2, t0) {
		t.Fatal("closed with connections on it")
	}
	if followCloseNow(ql, 0, t0) || followCloseNow(ql, 0, t0+4*s) {
		t.Fatal("closed within the grace after its last connection moved")
	}
	if followCloseNow(ql, 1, t0+4*s) || followCloseNow(ql, 0, t0+6*s) {
		t.Fatal("a connection came back: the grace starts again")
	}
	if !followCloseNow(ql, 0, t0+11*s+1) {
		t.Fatal("not closed after the grace")
	}
	withTune(t, QuietFollowGraceMs, 0)
	if !followCloseNow(&quietLink{}, 0, t0) {
		t.Fatal("grace 0: closed at once, as before")
	}
}

// Item 2: what the other server reports as its ceiling (kindInfo's u16
// MaxLinks, the largest over the live links) never takes the edge's QUIET
// count to 0 or past its own: 0 is "none heard" (its own count), anything
// else lands in [1, want] — a peer can lower it to one, no further.
func TestRed2FollowWantBounds(t *testing.T) {
	for want := 1; want <= 8; want++ {
		for _, bulk := range []int{0, 1, 5, 8, 300, 600} {
			for _, pm := range []int{-1, 0, 1, 2, bulk, bulk + 1, bulk + want, bulk + want + 5, 65535} {
				got := followWant(want, bulk, pm)
				if pm <= 0 && got != want {
					t.Fatalf("want %d bulk %d ceiling %d: %d, want its own count", want, bulk, pm, got)
				}
				if got < 1 || got > want {
					t.Fatalf("want %d bulk %d ceiling %d: %d QUIET links, outside [1, %d]", want, bulk, pm, got, want)
				}
			}
		}
	}
}
