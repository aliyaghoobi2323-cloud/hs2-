//go:build linux

package engine

import (
	"net"
	"testing"
	"time"
)

// replyPin on its own, for both socket families a wildcard user port can
// have: dual-stack [::] (IPv4 users as v4-mapped) and IPv4-only 0.0.0.0 (a
// host with IPv6 off).
func TestReplyPinBothFamilies(t *testing.T) {
	for _, network := range []string{"udp", "udp4"} {
		t.Run(network, func(t *testing.T) {
			addr := ":0"
			if network == "udp4" {
				addr = "0.0.0.0:0"
			}
			pc, err := net.ListenPacket(network, addr)
			if err != nil {
				t.Fatal(err)
			}
			defer pc.Close()
			pin := pinReplies(pc)
			if pin == nil {
				t.Fatalf("no reply pinning on a wildcard %s socket (%s)", network, pc.LocalAddr())
			}
			port := pc.LocalAddr().(*net.UDPAddr).Port
			c, err := net.DialUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")}, &net.UDPAddr{IP: net.ParseIP("127.0.0.2"), Port: port})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.Write([]byte("ping"))
			buf, oob := make([]byte, 64), make([]byte, replyPinOOB)
			pc.SetReadDeadline(time.Now().Add(2 * time.Second))
			k, from, dst, err := pin.readFrom(buf, oob)
			if err != nil {
				t.Fatal(err)
			}
			if !dst.IsValid() || dst.Unmap().String() != "127.0.0.2" {
				t.Fatalf("the destination the user sent to: got %v, want 127.0.0.2", dst)
			}
			if err := pin.writeTo(buf[:k], from, dst); err != nil {
				t.Fatal(err)
			}
			c.SetReadDeadline(time.Now().Add(2 * time.Second))
			b := make([]byte, 64)
			if n, err := c.Read(b); err != nil || string(b[:n]) != "ping" {
				t.Fatalf("the connected user got %q, %v", b[:n], err)
			}
		})
	}
	// a socket on one address needs nothing: the kernel answers from it
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	if pinReplies(pc) != nil {
		t.Error("reply pinning on a socket bound to one address")
	}
}
