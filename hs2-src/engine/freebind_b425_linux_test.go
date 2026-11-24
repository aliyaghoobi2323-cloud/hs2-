package engine

import (
	"net"
	"testing"
)

// b425: every engine listener path binds an address this host does not have
// (yet) instead of failing the start: the cold TCP listener (LB per-IP link
// listeners, forward ports, acme), the ZDU listener, and user UDP.
func TestB425ListenersBindUnassigned(t *testing.T) {
	const a4, a6 = "192.0.2.78:0", "[2001:db8::78]:0"
	for _, a := range []string{a4, a6} {
		ln, err := listenReuseRcvBufCold(a, 1<<20)
		if err != nil {
			t.Fatalf("cold %s: %v", a, err)
		}
		ln.Close()
		pc, err := listenPacketUDP(a)
		if err != nil {
			t.Fatalf("udp %s: %v", a, err)
		}
		pc.Close()
	}
	zduOn.Store(true)
	defer zduOn.Store(false)
	ln, err := ListenLink(a6)
	if err != nil {
		t.Fatalf("zdu link: %v", err)
	}
	ln.Close()
	pc, err := zduListenPacket(a4)
	if err != nil {
		t.Fatalf("zdu udp: %v", err)
	}
	pc.Close()
	if _, err := net.Listen("tcp", a4); err == nil {
		t.Fatal("192.0.2.78 is assigned here; the test proves nothing")
	}
}
