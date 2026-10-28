package engine

import (
	"context"
	"net"
	"testing"
	"time"
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
	// a BULK link's (not quiet), or the guard off: no guard
	var b owdLink
	b.qOut.Store(1)
	b.qOutAt.Store(ctrlNow())
	b.noteSocket(tcpStat{retrans: 1}, ctrlNow())
	b.noteSocket(tcpStat{retrans: 5, notsent: 1 << 20, pacingRate: 1}, ctrlNow())
	if _, ok := b.queue(); !ok {
		t.Fatal("a BULK link's reading guarded")
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
