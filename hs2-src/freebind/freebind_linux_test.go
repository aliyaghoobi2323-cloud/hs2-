package freebind

import (
	"net"
	"syscall"
	"testing"
)

func fdFreebind(t *testing.T, sc syscall.Conn) bool {
	rc, err := sc.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var on bool
	rc.Control(func(fd uintptr) { on = Get(fd) })
	return on
}

// The prod bug: a bind on an address this host does not have (yet) must
// succeed, TCP and UDP, IPv4 and IPv6; a wildcard is left alone.
func TestListenUnassigned(t *testing.T) {
	logs := captureLogs(t, func(net.IP) bool { return true }) // no watcher goroutine
	_ = logs
	for _, a := range []string{"192.0.2.77:0", "[2001:db8::77]:0"} {
		if Usable(net.ParseIP("2001:db8::77")) {
			t.Skip("test address assigned?")
		}
		ln, err := Listen("tcp", a)
		if err != nil {
			t.Fatalf("Listen %s: %v", a, err)
		}
		if !fdFreebind(t, ln.(*net.TCPListener)) {
			t.Errorf("%s: freebind off", a)
		}
		ln.Close()
		pc, err := ListenPacket("udp", a)
		if err != nil {
			t.Fatalf("ListenPacket %s: %v", a, err)
		}
		if !fdFreebind(t, pc.(*net.UDPConn)) {
			t.Errorf("%s udp: freebind off", a)
		}
		pc.Close()
	}
	ln, err := Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// a plain bind of an unassigned address still fails (Usable's probe)
	if c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("192.0.2.77")}); err == nil {
		c.Close()
		t.Fatal("192.0.2.77 is assigned on this host")
	}
	// a wildcard gets no freebind
	w, err := Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if fdFreebind(t, w.(*net.TCPListener)) {
		t.Error("wildcard got freebind")
	}
}
