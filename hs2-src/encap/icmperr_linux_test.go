//go:build linux

package encap

import (
	"encoding/binary"
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// injectICMPUnreach sends an ICMP destination-unreachable (type 3, the given
// code) to dst, quoting an IP header of protocol proto from src to the tunnel
// peer — exactly what a router, an ip_gre/ipip module, or an off-path attacker
// who knows only the two IPs and the protocol would send. The quoted inner
// header is what the kernel matches a raw socket's error against.
func injectICMPUnreach(t *testing.T, from, to net.IP, proto int, code byte) {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_RAW, unix.IPPROTO_ICMP)
	if err != nil {
		t.Fatalf("icmp socket: %v", err)
	}
	defer unix.Close(fd)
	// quoted inner IP header (20 bytes) + 8 bytes of "original datagram".
	inner := make([]byte, 28)
	inner[0] = 0x45
	binary.BigEndian.PutUint16(inner[2:], 40)
	inner[8] = 64
	inner[9] = byte(proto)
	copy(inner[12:16], to.To4())   // the socket's local addr (the dialer)
	copy(inner[16:20], from.To4()) // the peer it talks to
	icmp := make([]byte, 8+len(inner))
	icmp[0], icmp[1] = 3, code // dest unreachable
	copy(icmp[8:], inner)
	binary.BigEndian.PutUint16(icmp[2:], inetChecksum(icmp))
	var sa unix.SockaddrInet4
	copy(sa.Addr[:], to.To4())
	if err := unix.Sendto(fd, icmp, 0, &sa); err != nil {
		t.Fatalf("send icmp: %v", err)
	}
}

// A raw-encap carrier survives ICMP errors aimed at its flow: after each of the
// hard codes (port-, protocol-, host- and admin-prohibited unreachable, and
// frag-needed) is injected, the dialer's socket still reads its data. A
// connected socket would have returned the error as fatal and killed the
// carrier from one packet; the unconnected dial socket ignores them.
func TestRawDialSurvivesICMPError(t *testing.T) {
	needRawNetns(t)
	// Route 127.0.0.2 <-> 127.0.0.1 so the injected ICMP's addresses match.
	for _, k := range rawKinds {
		t.Run(k, func(t *testing.T) {
			opt := Options{Key: []byte("icmp-err")}
			srv, err := Listen(k, "127.0.0.2", opt)
			if err != nil {
				t.Fatal(err)
			}
			defer srv.Close()
			cli, err := Dial(k, "127.0.0.2", opt) // dials from 127.0.0.1 by route
			if err != nil {
				t.Fatal(err)
			}
			defer cli.Close()
			rc := cli.(*rawConn)
			local := rc.laddr.IP
			if local.Equal(net.IPv4zero) {
				local = net.IPv4(127, 0, 0, 1)
			}

			// Baseline: data flows.
			send := func(msg string) {
				if _, err := cli.Write([]byte(msg)); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			recvOK := func(want string) {
				buf := make([]byte, 2048)
				srv.SetReadDeadline(time.Now().Add(2 * time.Second))
				n, _, err := srv.ReadFrom(buf)
				if err != nil || string(buf[:n]) != want {
					t.Fatalf("%s: server did not receive %q (n=%d err=%v)", k, want, n, err)
				}
			}
			send("before")
			recvOK("before")

			// Inject each hard ICMP error at the dialer, quoting its flow.
			for _, code := range []byte{3 /*port*/, 2 /*proto*/, 1 /*host*/, 13 /*admin*/, 4 /*frag-needed*/} {
				injectICMPUnreach(t, net.IPv4(127, 0, 0, 2), local, rc.f.proto, code)
			}
			time.Sleep(100 * time.Millisecond)

			// The carrier must still work: read on the dial socket does not fail,
			// and data still crosses in both directions.
			send("after-c2s")
			recvOK("after-c2s")
			if _, err := srv.WriteTo([]byte("after-s2c"), &Addr{IP: net.IPv4(127, 0, 0, 1), ID: rc.id, Kind: k}); err != nil {
				// wildcard reply-source: the server saw the peer already
			}
			buf := make([]byte, 2048)
			cli.SetReadDeadline(time.Now().Add(2 * time.Second))
			if n, err := cli.Read(buf); err != nil {
				t.Fatalf("%s: dial socket died after ICMP errors: %v", k, err)
			} else if string(buf[:n]) != "after-s2c" {
				t.Fatalf("%s: got %q after ICMP errors", k, buf[:n])
			}
		})
	}
}

// The unconnected dial socket ignores packets that are not from the peer: a
// packet of the same protocol, kind, magic and link id but from a DIFFERENT
// source IP is not delivered (the source-IP filter). Without it, an off-path
// host could inject carrier datagrams.
func TestRawDialSourceFiltered(t *testing.T) {
	needRawNetns(t)
	opt := Options{Key: []byte("srcfilter")}
	srv, err := Listen(KindGRE, "127.0.0.2", opt)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	cli, err := Dial(KindGRE, "127.0.0.2", opt)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	rc := cli.(*rawConn)

	// Forge a valid-looking server->client GRE packet but from 127.0.0.9.
	srvF, _ := newFramer(KindGRE, opt, false) // server direction framing
	pkt := srvF.build(nil, rc.id, 0, []byte("forged"))
	raw, err := net.DialIP("ip4:"+itoa(rc.f.proto), &net.IPAddr{IP: net.IPv4(127, 0, 0, 9)}, &net.IPAddr{IP: rc.laddr.IP})
	if err == nil {
		raw.Write(pkt)
		raw.Close()
	}
	cli.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
	buf := make([]byte, 2048)
	if n, err := cli.Read(buf); err == nil {
		t.Fatalf("dial socket accepted a packet from a foreign source: %q", buf[:n])
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [4]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
