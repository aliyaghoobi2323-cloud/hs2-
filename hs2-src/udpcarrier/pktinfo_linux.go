//go:build linux

package udpcarrier

import (
	"net"

	"golang.org/x/sys/unix"
)

// Reply-source pinning for a wildcard UDP listener.
//
// A UDP socket bound to 0.0.0.0 answers with whatever source address the routing
// table prefers for the destination. On a multi-IP server that is the PRIMARY
// address — not necessarily the one the client sent to. The client's socket is
// connected to the address it dialed, so its kernel drops replies from any other
// source and the handshake silently never completes (and if the primary address
// is filtered upstream, the replies never even leave). With IP_PKTINFO we learn,
// per datagram, which local address it arrived on, and send the reply from that
// same address (ipi_spec_dst). Every multi-homed UDP server has to do this.

// pktinfoOOB is the control-message buffer: room for one IP_PKTINFO.
const pktinfoOOB = 64

// enablePktinfo turns on IP_PKTINFO for an IPv4 socket bound to the wildcard and
// reports whether per-datagram local addresses are now available. A socket bound
// to a specific address needs nothing: the kernel already replies from it.
func enablePktinfo(conn *net.UDPConn) bool {
	la, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || la.IP.To4() == nil || !la.IP.IsUnspecified() {
		return false
	}
	rc, err := conn.SyscallConn()
	if err != nil {
		return false
	}
	var serr error
	if err := rc.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_PKTINFO, 1)
	}); err != nil || serr != nil {
		return false
	}
	return true
}

// readWithDst reads one datagram and, when IP_PKTINFO is on, the local IPv4
// address the peer sent it to (nil otherwise).
func readWithDst(conn *net.UDPConn, buf, oob []byte) (int, *net.UDPAddr, net.IP, error) {
	n, oobn, _, addr, err := conn.ReadMsgUDP(buf, oob)
	if err != nil || oobn == 0 {
		return n, addr, nil, err
	}
	msgs, perr := unix.ParseSocketControlMessage(oob[:oobn])
	if perr != nil {
		return n, addr, nil, nil
	}
	for _, m := range msgs {
		// struct in_pktinfo { int ifindex; in_addr spec_dst; in_addr addr; };
		// addr is the header destination: the local IP the peer targeted.
		if m.Header.Level == unix.IPPROTO_IP && m.Header.Type == unix.IP_PKTINFO && len(m.Data) >= 12 {
			return n, addr, net.IPv4(m.Data[8], m.Data[9], m.Data[10], m.Data[11]), nil
		}
	}
	return n, addr, nil, nil
}

// writeFrom sends b to addr, from source address src when it is set.
func writeFrom(conn *net.UDPConn, b []byte, addr *net.UDPAddr, src net.IP) error {
	if src4 := src.To4(); src4 != nil {
		info := &unix.Inet4Pktinfo{}
		copy(info.Spec_dst[:], src4)
		_, _, err := conn.WriteMsgUDP(b, unix.PktInfo4(info), addr)
		return err
	}
	_, err := conn.WriteToUDP(b, addr)
	return err
}
