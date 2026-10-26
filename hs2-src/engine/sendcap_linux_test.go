//go:build linux

package engine

import (
	"io"
	"net"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// TestSendCapLinuxTCPInfo: C4-lite's reading of a real link socket: the
// delivery rate is there (dlvOK), the send segment, and the app-limited
// bit x/sys leaves out (a socket that sent one small write and waits is
// app-limited; one that keeps a full send queue is not); an older kernel's
// shorter TCP_INFO (tcpInfoRead) has no delivery rate, and C4-lite then
// sets no cap.
func TestSendCapLinuxTCPInfo(t *testing.T) {
	if off := unsafe.Offsetof(unix.TCPInfo{}.Rto); off != 8 {
		t.Fatalf("tcpi_rto at %d: the app-limited byte is not where tcpInfoAppLimited reads it", off)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	acc := make(chan net.Conn, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			acc <- c
		}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	srv := <-acc
	defer srv.Close()
	go io.Copy(io.Discard, srv)
	tc := c.(*net.TCPConn)
	// a small write, then nothing: app-limited
	c.Write(make([]byte, 100))
	time.Sleep(50 * time.Millisecond)
	st, ok := tcpStats(tc)
	if !ok || !st.dlvOK || st.sndMSS == 0 {
		t.Fatalf("TCP_INFO: ok %v dlvOK %v mss %d", ok, st.dlvOK, st.sndMSS)
	}
	if !st.appLimited {
		t.Fatalf("a socket that sent 100 B and waits reads not app-limited (delivery %d B/s)", st.deliveryRate)
	}
	// keep its send queue full for a while: not app-limited
	end := time.Now().Add(300 * time.Millisecond)
	buf := make([]byte, 256<<10)
	full := false
	for time.Now().Before(end) && !full {
		c.Write(buf)
		if st, _ := tcpStats(tc); !st.appLimited && st.deliveryRate > 0 {
			full = true
		}
	}
	if !full {
		t.Fatal("a socket with a full send queue reads app-limited throughout")
	}
	// an older kernel: TCP_INFO up to tcpi_min_rtt (4.6-4.8), no delivery rate
	old := func(fd int) (*unix.TCPInfo, int, error) {
		info, _, err := getsockoptTCPInfo(fd)
		return info, int(unsafe.Offsetof(unix.TCPInfo{}.Delivery_rate)), err
	}
	cur := tcpInfoRead.Load()
	tcpInfoRead.Store(&old)
	defer tcpInfoRead.Store(cur)
	if st, ok := tcpStats(tc); !ok || st.dlvOK || st.appLimited {
		t.Fatalf("an older kernel: ok %v dlvOK %v app-limited %v", ok, st.dlvOK, st.appLimited)
	}
	var sets []uint64
	set := func(_ *net.TCPConn, r uint64) bool { sets = append(sets, r); return true }
	pcur := wcPaceSet.Load()
	wcPaceSet.Store(&set)
	defer wcPaceSet.Store(pcur)
	var lines int
	f := func(string, ...any) { lines++ }
	coreLog.Store(&f)
	defer coreLog.Store(nil)
	sendcapNoDlv.Store(false)
	l := newSendCapLink(newCapSys(tc), nil)
	for i := 1; i <= 5; i++ {
		c.Write(buf)
		l.step(time.Duration(i)*100*time.Millisecond, uint64(i)*uint64(len(buf)))
	}
	if len(sets) != 0 || lines != 1 {
		t.Fatalf("an older kernel: caps %v, %d log lines", sets, lines)
	}
}
