package engine

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
	"github.com/xtaci/smux"
)

// The R1 round-3 judge's other fixes: the guard of a QUIET link's one-way
// delays (owd.go), the exit's receive buffer to a local panel (panelbuf.go)
// and the age limit of a user's UDP datagram at the edge (stream_iran.go).

// While this end's QUIET socket resends, or holds a backlog of its own, the
// queue the other end reads for this direction is none: from that beat
// until owdDirtyLook × owdLook and two round trips after it.
func TestOWDGuard(t *testing.T) {
	var o owdLink
	o.quiet = true
	now := ctrlNow()
	o.qOut.Store(int64(40 * time.Millisecond))
	o.qOutAt.Store(now)
	if q, ok := o.queue(); !ok || q != 40*time.Millisecond {
		t.Fatalf("clean: %v %v", q, ok)
	}
	ts := tcpStat{retrans: 10, rtt: 72000, pacingRate: 1e6}
	o.noteSocket(ts, now) // the first beat: a base, no resend seen
	if _, ok := o.queue(); !ok {
		t.Fatal("the first beat made it dirty")
	}
	ts.retrans = 11
	o.noteSocket(ts, now)
	if _, ok := o.queue(); ok {
		t.Fatal("a resend: still read")
	}
	hold := time.Duration(o.dirtyUntil.Load() - now)
	if want := owdDirtyLook*owdLook + 2*72*time.Millisecond; hold != want {
		t.Fatalf("held %v, want %v", hold, want)
	}
	// past the hold, a clean beat: read again
	o.dirtyUntil.Store(ctrlNow() - 1)
	o.qOutAt.Store(ctrlNow())
	o.noteSocket(ts, ctrlNow())
	if _, ok := o.queue(); !ok {
		t.Fatal("clean again: not read")
	}
	// a backlog of its own: 64 KB unsent at 1 MB/s is 64 ms past owdDirtyWait
	ts.notsent = 64 << 10
	o.noteSocket(ts, ctrlNow())
	if _, ok := o.queue(); ok {
		t.Fatal("a backlog: still read")
	}
	// a BULK link's own messages (the budget's) are guarded alike; a link
	// that trades none (α's version 4 aside) is not
	var o2 owdLink
	o2.own = true
	o2.qOut.Store(1)
	o2.qOutAt.Store(ctrlNow())
	o2.noteSocket(tcpStat{retrans: 1}, ctrlNow())
	o2.noteSocket(tcpStat{retrans: 5}, ctrlNow())
	if _, ok := o2.queue(); ok {
		t.Fatal("a BULK link's own reading on a resending socket: still read")
	}
	// OWDGuardBulk: 0 a BULK link's own messages are never none, 1 (the
	// default) for its resends, 2 also for its backlog: C3's mark at a low
	// pacing rate, 11.3 KiB at 7 Mbit/s, is 13 ms past owdDirtyWait (F3)
	for _, gb := range []int{0, 1, 2} {
		func() {
			defer func(v int) { OWDGuardBulk = v }(OWDGuardBulk)
			OWDGuardBulk = gb
			read := func(ts tcpStat) bool {
				var o3 owdLink
				o3.own = true
				o3.qOut.Store(1)
				o3.qOutAt.Store(ctrlNow())
				o3.noteSocket(tcpStat{retrans: 1}, ctrlNow())
				o3.noteSocket(ts, ctrlNow())
				_, ok := o3.queue()
				return ok
			}
			if ok := read(tcpStat{retrans: 3, rtt: 72000}); ok != (gb == 0) {
				t.Fatalf("OWDGuardBulk=%d: a BULK link's resends read %v", gb, ok)
			}
			if ok := read(tcpStat{retrans: 1, notsent: 11584, pacingRate: 7e6 / 8, rtt: 72000}); ok != (gb < 2) {
				t.Fatalf("OWDGuardBulk=%d: a BULK link's backlog read %v", gb, ok)
			}
		}()
	}
	var b owdLink
	b.qOut.Store(1)
	b.qOutAt.Store(ctrlNow())
	b.noteSocket(tcpStat{retrans: 1}, ctrlNow())
	b.noteSocket(tcpStat{retrans: 5, notsent: 1 << 20, pacingRate: 1}, ctrlNow())
	if _, ok := b.queue(); !ok {
		t.Fatal("a link of neither kind guarded")
	}
	defer func(v int) { TuneOWDGuard = v }(TuneOWDGuard)
	TuneOWDGuard = 0
	var g owdLink
	g.quiet = true
	g.qOut.Store(1)
	g.qOutAt.Store(ctrlNow())
	g.noteSocket(tcpStat{retrans: 1}, ctrlNow())
	g.noteSocket(tcpStat{retrans: 5}, ctrlNow())
	if _, ok := g.queue(); !ok {
		t.Fatal("HS2_TUNE_OWD_GUARD=0: guarded")
	}
}

func TestLocalTarget(t *testing.T) {
	for _, c := range []struct {
		a    string
		want bool
	}{
		{"127.0.0.1:5211", true}, {"127.9.9.9:1", true}, {"[::1]:443", true},
		{"[::ffff:127.0.0.1]:80", true}, {"192.0.2.77:80", false}, {"example.com:80", false}, {"nonsense", false},
	} {
		if got := localTarget(c.a); got != c.want {
			t.Fatalf("%s: %v, want %v", c.a, got, c.want)
		}
	}
	// one of the host's own addresses
	as, err := net.InterfaceAddrs()
	if err != nil {
		t.Skip(err)
	}
	for _, a := range as {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
			if !localTarget(net.JoinHostPort(ipn.IP.String(), "80")) {
				t.Fatalf("%s, the host's own, not local", ipn.IP)
			}
			return
		}
	}
}

// panelDial to a loopback panel sets the receive buffer (Linux doubles what
// it is given); 0 leaves the kernel's autotuning.
func TestPanelDialRcvbuf(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	defer func(v int) { TunePanelRcvbuf = v }(TunePanelRcvbuf)
	for _, n := range []int{256 << 10, 0} {
		TunePanelRcvbuf = n
		c, err := panelDial(context.Background(), "tcp", ln.Addr().String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := sockRcvbuf(c)
		c.Close()
		if !ok {
			t.Skip("no SO_RCVBUF read here")
		}
		if n > 0 && (got < n || got > 2*n) {
			t.Fatalf("set %d: the socket holds %d", n, got)
		}
		if n == 0 && got == 2*(256<<10) {
			t.Fatalf("none set: the socket holds %d", got)
		}
	}
}

func TestUDPLate(t *testing.T) {
	defer func(v int) { UDPLateMs = v }(UDPLateMs)
	UDPLateMs = 150
	now := ctrlNow()
	if udpLate(now-int64(100*time.Millisecond), now) || !udpLate(now-int64(200*time.Millisecond), now) {
		t.Fatal("150 ms: 100 ms late, or 200 ms not")
	}
	UDPLateMs = 0
	if udpLate(now-int64(time.Hour), now) {
		t.Fatal("0: none dropped")
	}
}

// A connection that moved to a BULK link stays bulk there (MovedBulk),
// however little it moves: C1's quiet rule made 400 slow moved connections
// light, their frames bypassed the in-flight budget, and the link stayed at
// a tenth of the path (smoke C2). With the switch off, C1's rule holds.
func TestMovedStaysBulk(t *testing.T) {
	if QuietOff() || SchedOff() {
		t.Skip("β2 off")
	}
	defer func(v int) { MovedBulk = v }(MovedBulk)
	for _, on := range []int{1, 0} {
		MovedBulk = on
		up, dn := newAngleWire(angleRateBps, angleDelay), newAngleWire(angleRateBps, angleDelay)
		p := newClassPair(t, up, dn, classCaps(tlscarrier.CapBulk))
		st, err := p.edge.sess.OpenStreamSyn(smux.EncodeSynGroup(7, 2, smux.RoleMove, 99, 0))
		if err != nil {
			t.Fatal(err)
		}
		lf := linkFlows{bulkLink: true}
		e := lf.add(st)
		if Class(e.class.Load()) != ClassBulk {
			t.Fatalf("on %d: a move stream starts %v", on, Class(e.class.Load()))
		}
		now := time.Duration(time.Hour)
		for i := 0; i < 40; i++ { // 4 s with nothing sent: quiet by C1's rule
			now += classTick
			lf.tick(now)
		}
		got := Class(e.class.Load())
		if on == 1 && got != ClassBulk || on == 0 && got != ClassLight {
			t.Fatalf("MovedBulk %d: a quiet moved stream is %v", on, got)
		}
		up.close()
		dn.close()
	}
}

// A BULK link's socket gets a fixed send buffer (linkbuf.go): no growth to
// wait for, whatever the host's TCP memory.
func TestLinkSndbuf(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			defer c.Close()
			time.Sleep(time.Second)
		}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tc := c.(*net.TCPConn)
	before, ok := sockSndbuf(tc)
	if !ok {
		t.Skip("no SO_SNDBUF read here")
	}
	setLinkSndbuf(tc)
	after, _ := sockSndbuf(tc)
	if after < TuneLinkSndbuf {
		t.Fatalf("send buffer %d after setting %d (before %d)", after, TuneLinkSndbuf, before)
	}
}

// The budget takes the QUIET link's one-way queue of its direction only
// while its own round trip holds it: under 1% loss the QUIET link read 7 to
// 19 ms of its own resends on a BULK link whose srtt − min_rtt was 1 to 2
// ms (smoke C4), and the budget cut a link with no queue.
func TestBudgetQuietCheck(t *testing.T) {
	r := 72 * time.Millisecond
	defer func(v int) { BudgetQuietCheck = v }(BudgetQuietCheck)
	run := func() float64 {
		var c budgetCore
		in := budgetIn{On: true, Delivery: 90 * bMbit, MinRTT: r, SRTT: r + 1500*time.Microsecond, TxRate: 90 * bMbit,
			Blocked: true, MSS: 1448, DlvMax: 90 * bMbit, QOutOK: true, QOut: 19 * time.Millisecond}
		for i := 0; i < 30; i++ {
			c.step(in)
		}
		return c.b
	}
	BudgetQuietCheck = 1
	on := run()
	BudgetQuietCheck = 0
	off := run()
	want := 90 * bMbit * (r + budgetTarget()).Seconds()
	if on < 0.95*want {
		t.Fatalf("with the check: B %.0f KiB, want at least %.0f (no cut for a queue the link does not see)", on/1024, 0.95*want/1024)
	}
	if off >= on {
		t.Fatalf("without the check B %.0f KiB, with it %.0f: the QUIET reading made no cut", off/1024, on/1024)
	}
}
