//go:build linux

package encap

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"testing"
	"time"
)

func listenT(t *testing.T, kind, addr string, opt Options) net.PacketConn {
	t.Helper()
	pc, err := Listen(kind, addr, opt)
	if err != nil {
		t.Fatalf("%s listen %s: %v", kind, addr, err)
	}
	t.Cleanup(func() { pc.Close() })
	return pc
}

func dialT(t *testing.T, kind, addr string, opt Options) net.Conn {
	t.Helper()
	c, err := Dial(kind, addr, opt)
	if err != nil {
		t.Fatalf("%s dial %s: %v", kind, addr, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func readFromT(t *testing.T, pc net.PacketConn, d time.Duration) ([]byte, net.Addr) {
	t.Helper()
	buf := make([]byte, 2048)
	pc.SetReadDeadline(time.Now().Add(d))
	n, a, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	return buf[:n], a
}

func readT(t *testing.T, c net.Conn, d time.Duration) []byte {
	t.Helper()
	buf := make([]byte, 2048)
	c.SetReadDeadline(time.Now().Add(d))
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return buf[:n]
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// Real raw sockets: every kind carries datagrams both ways, byte-exact, at
// sizes from empty to a full tunnel packet, with the link id preserved.
func TestRawSocketRoundTrip(t *testing.T) {
	needRawNetns(t)
	for _, k := range rawKinds {
		t.Run(k, func(t *testing.T) {
			opt := Options{Key: []byte("rt-key")}
			srv := listenT(t, k, "127.0.0.1:2096", opt)
			cli := dialT(t, k, "127.0.0.1:2096", opt)
			cliID := cli.LocalAddr().(*Addr).ID
			for i, n := range []int{0, 1, 17, 512, 1400, 1472} {
				msg := bytes.Repeat([]byte{byte(i + 1)}, n)
				if _, err := cli.Write(msg); err != nil {
					t.Fatalf("write %d: %v", n, err)
				}
				got, from := readFromT(t, srv, 2*time.Second)
				if !bytes.Equal(got, msg) {
					t.Fatalf("c2s %d bytes: got %d bytes", n, len(got))
				}
				fa := from.(*Addr)
				if fa.ID != cliID || !fa.IP.Equal(net.IPv4(127, 0, 0, 1)) {
					t.Fatalf("peer address %v, want 127.0.0.1#%d", fa, cliID)
				}
				reply := append([]byte("re:"), msg...)
				if _, err := srv.WriteTo(reply, from); err != nil {
					t.Fatalf("reply: %v", err)
				}
				if got := readT(t, cli, 2*time.Second); !bytes.Equal(got, reply) {
					t.Fatalf("s2c %d bytes: got %d", len(reply), len(got))
				}
			}
		})
	}
}

// Several links from one IP are told apart by their link id, and a reply
// reaches only the link it is addressed to — the kernel filter drops the rest
// before they wake the other links' readers.
func TestRawSocketLinksDemux(t *testing.T) {
	needRawNetns(t)
	for _, k := range rawKinds {
		t.Run(k, func(t *testing.T) {
			opt := Options{Key: []byte("demux")}
			srv := listenT(t, k, "127.0.0.1", opt)
			var clis []net.Conn
			addrs := map[string]net.Addr{}
			for i := 0; i < 4; i++ {
				c := dialT(t, k, "127.0.0.1", opt)
				clis = append(clis, c)
				c.Write([]byte(fmt.Sprintf("link-%d", i)))
				got, from := readFromT(t, srv, 2*time.Second)
				addrs[string(got)] = from
			}
			if len(addrs) != 4 {
				t.Fatalf("expected 4 distinct links, got %v", addrs)
			}
			seen := map[string]bool{}
			for _, a := range addrs {
				seen[a.String()] = true
			}
			if len(seen) != 4 {
				t.Fatalf("links share an address: %v", seen)
			}
			// Reply only to link 2.
			srv.WriteTo([]byte("for-2"), addrs["link-2"])
			if got := readT(t, clis[2], 2*time.Second); string(got) != "for-2" {
				t.Fatalf("link 2 got %q", got)
			}
			for _, i := range []int{0, 1, 3} {
				rc := clis[i].(*rawConn)
				rc.ipc.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
				buf := make([]byte, 2048)
				if n, _, _, _, err := rc.ipc.ReadMsgIP(buf, nil); !isTimeout(err) {
					t.Fatalf("link %d socket woke for another link's packet (n=%d err=%v)", i, n, err)
				}
			}
		})
	}
}

// A dialer keyed with another secret never reaches the listener: its packets
// fail the framing magic (in the kernel filter already).
func TestRawSocketForeignKeyIgnored(t *testing.T) {
	needRawNetns(t)
	for _, k := range rawKinds {
		t.Run(k, func(t *testing.T) {
			srv := listenT(t, k, "127.0.0.1", Options{Key: []byte("ours")})
			foreign := dialT(t, k, "127.0.0.1", Options{Key: []byte("theirs")})
			for i := 0; i < 20; i++ {
				foreign.Write([]byte("hello?"))
			}
			srv.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
			buf := make([]byte, 2048)
			if n, a, err := srv.ReadFrom(buf); !isTimeout(err) {
				t.Fatalf("listener accepted a foreign packet: %q from %v (%v)", buf[:n], a, err)
			}
		})
	}
}

// A wildcard listener answers from the local address each peer targeted, so
// a peer that dialed a secondary IP still accepts the reply (its connected
// socket takes nothing else) — the multi-IP server case.
func TestRawSocketWildcardRepliesFromTarget(t *testing.T) {
	needRawNetns(t)
	for _, k := range rawKinds {
		t.Run(k, func(t *testing.T) {
			srv := listenT(t, k, "0.0.0.0", Options{})
			cli := dialT(t, k, "127.0.0.2", Options{BindIP: "127.0.0.1"})
			cli.Write([]byte("ping"))
			_, from := readFromT(t, srv, 2*time.Second)
			srv.WriteTo([]byte("pong"), from)
			if got := readT(t, cli, 2*time.Second); string(got) != "pong" {
				t.Fatalf("got %q", got)
			}
		})
	}
}

// BindIP picks the source address the peer sees.
func TestRawSocketBindIP(t *testing.T) {
	needRawNetns(t)
	for _, k := range rawKinds {
		t.Run(k, func(t *testing.T) {
			srv := listenT(t, k, "127.0.0.1", Options{})
			cli := dialT(t, k, "127.0.0.1", Options{BindIP: "127.0.0.3"})
			cli.Write([]byte("x"))
			_, from := readFromT(t, srv, 2*time.Second)
			if ip := from.(*Addr).IP; !ip.Equal(net.IPv4(127, 0, 0, 3)) {
				t.Fatalf("source %v, want 127.0.0.3", ip)
			}
			if _, err := Dial(k, "127.0.0.1", Options{BindIP: "not-an-ip"}); err == nil {
				t.Fatal("bad bind IP accepted")
			}
		})
	}
}

// The icmp listener turns off the kernel's own echo replies, so the dialer
// hears only the listener — not its own datagrams echoed back.
func TestRawSocketICMPKernelSilent(t *testing.T) {
	needRawNetns(t)
	os.WriteFile(echoIgnorePath, []byte("0\n"), 0o644)
	srv := listenT(t, KindICMP, "127.0.0.1", Options{})
	if !EchoIgnored() {
		t.Fatal("icmp listener did not set icmp_echo_ignore_all")
	}
	cli := dialT(t, KindICMP, "127.0.0.1", Options{})
	for i := 0; i < 10; i++ {
		cli.Write([]byte("req"))
	}
	for i := 0; i < 10; i++ {
		readFromT(t, srv, 2*time.Second)
	}
	cli.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	buf := make([]byte, 2048)
	if n, err := cli.Read(buf); !isTimeout(err) {
		t.Fatalf("dialer received %q without the listener answering (kernel echo?) err=%v", buf[:n], err)
	}
}

// The icmp reply carries the sequence of the request it answers, the way a
// stateful middlebox expects an echo exchange to look.
func TestRawSocketICMPReplySequence(t *testing.T) {
	needRawNetns(t)
	srv := listenT(t, KindICMP, "127.0.0.1", Options{})
	cli := dialT(t, KindICMP, "127.0.0.1", Options{})
	rc := cli.(*rawConn)
	for i := 0; i < 5; i++ {
		cli.Write([]byte("q"))
		_, from := readFromT(t, srv, 2*time.Second)
		want := uint16(rc.seq.Load())
		srv.WriteTo([]byte("a"), from)
		rc.ipc.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 2048)
		n, _, _, _, err := rc.ipc.ReadMsgIP(buf, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _, tp, ok := rc.f.ipv4Payload(buf[:n])
		if !ok {
			t.Fatal("bad reply packet")
		}
		if _, seq, ok := rc.f.parse(tp); !ok || seq != want {
			t.Fatalf("reply seq %d, request seq %d", seq, want)
		}
	}
}
