package engine

import (
	"net"
	"testing"

	"golang.org/x/sys/unix"
)

func tcpCCName(t *testing.T, tc *net.TCPConn) string {
	rc, _ := tc.SyscallConn()
	var n string
	rc.Control(func(fd uintptr) { n, _ = unix.GetsockoptString(int(fd), unix.IPPROTO_TCP, unix.TCP_CONGESTION) })
	return n
}

// fx247 (audit246 L1): a link a failed reinit left on reno is put back on
// its algorithm by the next tick's retry, and forgotten once closed.
func TestFx247CCBackRetry(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	tc := c.(*net.TCPConn)
	want := tcpCCName(t, tc)
	if want == "" || want == "reno" {
		t.Skip("socket CC: " + want)
	}
	rc, _ := tc.SyscallConn()
	rc.Control(func(fd uintptr) { unix.SetsockoptString(int(fd), unix.IPPROTO_TCP, unix.TCP_CONGESTION, "reno") })
	ccBackNote(tc, want)
	if ccBackN.Load() != 1 {
		t.Fatalf("remembered %d", ccBackN.Load())
	}
	ccBackRetryAll()
	if g := tcpCCName(t, tc); g != want || ccBackN.Load() != 0 {
		t.Fatalf("CC %q (want %q), remembered %d", g, want, ccBackN.Load())
	}
	// a reinit leaves it on its algorithm, not reno
	if tcpCCReinit(tc) && tcpCCName(t, tc) != want {
		t.Fatalf("reinit left %q", tcpCCName(t, tc))
	}
	// a closed socket is forgotten
	ccBackNote(tc, "no-such-cc")
	c.Close()
	ccBackRetryAll()
	if ccBackN.Load() != 0 {
		t.Fatalf("closed socket still remembered")
	}
}
