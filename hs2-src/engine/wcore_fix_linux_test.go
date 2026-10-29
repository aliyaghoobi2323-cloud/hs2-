//go:build linux

package engine

import (
	"io"
	"net"
	"testing"
	"time"
	"unsafe"

	"github.com/xtaci/smux"
	"golang.org/x/sys/unix"
)

// Item 6: tcpi_rcv_ooopack came with Linux 5.4; an older kernel fills a
// shorter TCP_INFO and the field reads 0, which the core took for "nothing
// arrived behind a hole" (a clean path) under any loss. Support is read from
// the length the kernel returned: a TCP_INFO that ends before the field
// leaves oooOK false, in tcpStats and in the runner's snapshot.
func TestWcoreFixOooSupportByLength(t *testing.T) {
	off := int(unsafe.Offsetof(unix.TCPInfo{}.Rcv_ooopack))
	for _, tc := range []struct {
		n    int
		want bool
	}{{unix.SizeofTCPInfo, true}, {off + 4, true}, {off, false}, {off - 8, false}, {104, false}, {-1, false}} {
		if got := tcpInfoHasOoo(tc.n); got != tc.want {
			t.Errorf("TCP_INFO of %d bytes (rcv_ooopack at %d): ooo %v, want %v", tc.n, off, got, tc.want)
		}
	}

	edge, exit := loopbackTCP(t)
	go io.Copy(io.Discard, edge)
	exit.Write(make([]byte, 64<<10))
	time.Sleep(20 * time.Millisecond)
	ts, ok := tcpStats(edge)
	if !ok {
		t.Skip("no TCP_INFO")
	}
	full := ts.oooOK // this kernel's own answer (5.4 or later: true)

	// a kernel before 5.4: the struct ends before tcpi_rcv_ooopack
	realp := tcpInfoRead.Load()
	real := *realp
	t.Cleanup(func() { tcpInfoRead.Store(realp) })
	short := func(fd int) (*unix.TCPInfo, int, error) {
		info, _, err := real(fd)
		if info != nil {
			short := *info
			b := (*[unix.SizeofTCPInfo]byte)(unsafe.Pointer(&short))
			clear(b[off:])
			return &short, off, err
		}
		return info, off, err
	}
	tcpInfoRead.Store(&short)
	ts, ok = tcpStats(edge)
	if !ok || ts.oooOK || ts.segsIn == 0 {
		t.Fatalf("a TCP_INFO short of rcv_ooopack: ok %v oooOK %v segsIn %d, want the rest read and oooOK false", ok, ts.oooOK, ts.segsIn)
	}
	cconn, sconn := net.Pipe()
	cfg := newSmuxConfig()
	cli, err := smux.Client(cconn, cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := smux.Server(sconn, newSmuxConfig())
	t.Cleanup(func() { cli.Close(); srv.Close() })
	// (the exit's core, whose loss signal this is, runs only with the lab
	// knob until step 3)
	up := WcoreUpload.Get()
	WcoreUpload.Set(1)
	t.Cleanup(func() { WcoreUpload.Set(up) })
	l := newWcLink(cli, cfg, edge, nil, true, nil)
	if s := l.snap(time.Now()); s.tcpNone || s.oooOK || s.minRTT == 0 {
		t.Fatalf("snapshot of a socket on a kernel before 5.4: tcpNone %v oooOK %v min_rtt %v, want the socket read and oooOK false", s.tcpNone, s.oooOK, s.minRTT)
	}
	tcpInfoRead.Store(realp)
	if s := l.snap(time.Now()); s.oooOK != full {
		t.Fatalf("snapshot with this kernel's own TCP_INFO: oooOK %v, want %v", s.oooOK, full)
	}
	t.Logf("this kernel reports rcv_ooopack: %v", full)
}
