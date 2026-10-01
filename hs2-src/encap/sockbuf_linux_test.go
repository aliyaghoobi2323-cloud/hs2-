//go:build linux

package encap

import (
	"bytes"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

// sockOpt reads an integer SOL_SOCKET option from c.
func sockOpt(t *testing.T, c syscall.Conn, opt int) int {
	t.Helper()
	rc, err := c.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var v int
	var serr error
	if err := rc.Control(func(fd uintptr) {
		v, serr = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, opt)
	}); err != nil {
		t.Fatal(err)
	}
	if serr != nil {
		t.Fatal(serr)
	}
	return v
}

// Both ends of a udp carrier socket get the large buffers (the kernel reports
// twice the request), past net.core.[rw]mem_max when root.
func TestUDPSockBufs(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to force past net.core.rmem_max")
	}
	pc, err := Listen(KindUDP, "127.0.0.1:0", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	dc, err := Dial(KindUDP, pc.LocalAddr().String(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer dc.Close()
	for name, c := range map[string]syscall.Conn{"listen": pc.(syscall.Conn), "dial": dc.(syscall.Conn)} {
		if got := sockOpt(t, c, syscall.SO_RCVBUF); got < sockBuf {
			t.Errorf("%s: SO_RCVBUF = %d, want >= %d", name, got, sockBuf)
		}
		if got := sockOpt(t, c, syscall.SO_SNDBUF); got < sockBuf {
			t.Errorf("%s: SO_SNDBUF = %d, want >= %d", name, got, sockBuf)
		}
	}
}

// What the buffer is for: a reader that pauses must not lose datagrams. 1500
// carrier-sized datagrams (~20 ms of 800 Mbit/s) arrive while the listener is
// not reading; with the kernel default (~208 KiB) only ~100 of them survive,
// and the rest would be counted as path loss.
func TestUDPSockBufAbsorbsReaderPause(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to force past net.core.rmem_max")
	}
	pc, err := Listen(KindUDP, "127.0.0.1:0", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	dc, err := Dial(KindUDP, pc.LocalAddr().String(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer dc.Close()

	const n = 1500
	pkt := bytes.Repeat([]byte{0xa5}, 1332) // MTU 1280 + carrier overhead
	for i := 0; i < n; i++ {
		if _, err := dc.Write(pkt); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	got := 0
	buf := make([]byte, 2048)
	for {
		pc.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		if _, _, err := pc.ReadFrom(buf); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				break
			}
			t.Fatal(err)
		}
		got++
	}
	if got != n {
		t.Fatalf("received %d of %d datagrams sent while the reader paused", got, n)
	}
}
