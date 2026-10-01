package encap

import (
	"fmt"
	"net"
)

// The udp encapsulation is the ordinary case: a native UDP socket. *net.UDPConn
// already satisfies both net.Conn (dial) and net.PacketConn (listen), and the
// carrier keeps its existing multi-IP wildcard handling (pktinfo) by
// type-asserting *net.UDPConn, so udp behaves exactly as it did before encap
// existed.

// sockBuf is the send/receive buffer asked for on every carrier socket, UDP and
// raw. Without it a socket gets net.core.rmem_default (~208 KiB, the kernel
// counts ~2 KiB per datagram, so ~100 datagrams): at tens to hundreds of
// Mbit/s that is a few milliseconds, and any pause of the reading goroutine
// longer than that (GC, a busy CPU) overflows it. The kernel drops the excess
// silently (Udp RcvbufErrors) and the carrier reads it as path loss — rate
// backs off and FEC spends parity on a loss the path never had. Netns lab,
// 300 Mbit/s: ~1600 such drops per 20 s on the receiver. It is a ceiling, not
// an allocation: memory is used only while datagrams wait. As root it is
// forced past net.core.[rw]mem_max, so no system sysctl is needed.
const sockBuf = 4 << 20

func dialUDP(addr string, opt Options) (net.Conn, error) {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	var laddr *net.UDPAddr
	if opt.BindIP != "" {
		ip := net.ParseIP(opt.BindIP)
		if ip == nil {
			return nil, fmt.Errorf("encap udp: bad bind IP %q", opt.BindIP)
		}
		laddr = &net.UDPAddr{IP: ip}
	}
	c, err := net.DialUDP("udp", laddr, ua)
	if err != nil {
		return nil, err
	}
	setSockBufs(c, sockBuf)
	return c, nil
}

func listenUDP(addr string) (net.PacketConn, error) {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	c, err := net.ListenUDP(listenNetwork(ua), ua)
	if err != nil {
		return nil, err
	}
	setSockBufs(c, sockBuf)
	return c, nil
}

// listenNetwork picks the socket family for a UDP listen address. An IPv4 or
// empty host gets a plain IPv4 socket ("udp4"): Go would otherwise open a
// dual-stack IPv6 socket for a wildcard, on which IPv4 reply-source pinning
// (IP_PKTINFO) does not apply. The tunnel's endpoints are IPv4.
func listenNetwork(ua *net.UDPAddr) string {
	if ua.IP == nil || ua.IP.To4() != nil {
		return "udp4"
	}
	return "udp6"
}
