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
