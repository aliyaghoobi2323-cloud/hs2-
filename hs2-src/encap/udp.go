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
	network := "udp"
	if ua.IP == nil || ua.IP.IsUnspecified() {
		// A wildcard bind: keep it AF-agnostic exactly as the carrier did.
		network = "udp"
	}
	return net.ListenUDP(network, ua)
}
