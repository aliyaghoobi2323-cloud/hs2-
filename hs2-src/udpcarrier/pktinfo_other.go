//go:build !linux

package udpcarrier

import "net"

// Reply-source pinning (IP_PKTINFO) is Linux-only; elsewhere a wildcard listener
// replies from the routing-preferred source. Bind the listener to a specific IP
// on multi-IP hosts. Production runs on Linux.

const pktinfoOOB = 0

func enablePktinfo(conn *net.UDPConn) bool { return false }

func readWithDst(conn *net.UDPConn, buf, oob []byte) (int, *net.UDPAddr, net.IP, error) {
	n, addr, err := conn.ReadFromUDP(buf)
	return n, addr, nil, err
}

func writeFrom(conn *net.UDPConn, b []byte, addr *net.UDPAddr, src net.IP) error {
	_, err := conn.WriteToUDP(b, addr)
	return err
}
