package udpcarrier

import (
	"fmt"
	"net"
)

// localUDPAddr turns an optional source IP (a config's bind_local_ip) into the
// local address for a UDP dial. Empty means "let the kernel choose". An invalid
// IP is an error, never a silent fallback to the default source: on a multi-IP
// server the default may be exactly the address that is filtered, and quietly
// sending from it looks like a dead path instead of a config typo.
func localUDPAddr(bindIP string) (*net.UDPAddr, error) {
	if bindIP == "" {
		return nil, nil
	}
	ip := net.ParseIP(bindIP)
	if ip == nil {
		return nil, fmt.Errorf("udpcarrier: invalid source IP %q", bindIP)
	}
	return &net.UDPAddr{IP: ip}, nil
}

// listenNetwork picks the socket family for a listen address. An IPv4 or empty
// host gets a plain IPv4 socket ("udp4"): Go would otherwise open a dual-stack
// IPv6 socket for a wildcard, on which IPv4 reply-source pinning (IP_PKTINFO)
// does not apply. The tunnel's endpoints are IPv4.
func listenNetwork(ua *net.UDPAddr) string {
	if ua.IP == nil || ua.IP.To4() != nil {
		return "udp4"
	}
	return "udp6"
}
