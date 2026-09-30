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
	return net.DialUDP("udp", laddr, ua)
}

func listenUDP(addr string) (net.PacketConn, error) {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	return net.ListenUDP(listenNetwork(ua), ua)
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
