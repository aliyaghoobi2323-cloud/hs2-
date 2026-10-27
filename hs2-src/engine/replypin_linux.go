//go:build linux

package engine

import (
	"net"
	"net/netip"

	"golang.org/x/sys/unix"
)

// replyPin makes a UDP user port bound to every address (user_listen_ip "",
// the installer's "all") answer each user from the address that user sent to.
// Without it the kernel picks the source the routing table prefers — on a
// server with several addresses the primary one — and a user of another
// address, whose socket is connected (game clients, xray), drops every reply.
// udpcarrier pins its own listener the same way (IP_PKTINFO); this does it for
// both socket families a wildcard user port has: dual-stack [::] (IPV6_PKTINFO,
// which carries IPv4 users as v4-mapped addresses in both directions) and
// IPv4-only 0.0.0.0 on a host with IPv6 off (IP_PKTINFO).
type replyPin struct {
	c  *net.UDPConn
	v6 bool // an AF_INET6 socket
}

// replyPinOOB is the control-message buffer of one read.
const replyPinOOB = 128

// pinReplies turns the pinning on for a wildcard UDP socket; nil for a socket
// bound to one address (the kernel answers from it already) or when the
// socket refuses the option.
func pinReplies(pc net.PacketConn) *replyPin {
	c, ok := pc.(*net.UDPConn)
	if !ok {
		return nil
	}
	la, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok || !la.IP.IsUnspecified() {
		return nil
	}
	v6 := la.IP.To4() == nil
	rc, err := c.SyscallConn()
	if err != nil {
		return nil
	}
	var serr error
	if err := rc.Control(func(fd uintptr) {
		if v6 {
			serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_RECVPKTINFO, 1)
		} else {
			serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_PKTINFO, 1)
		}
	}); err != nil || serr != nil {
		return nil
	}
	return &replyPin{c: c, v6: v6}
}

// readFrom reads one datagram with the local address its user sent it to
// (invalid when the kernel did not say).
func (r *replyPin) readFrom(buf, oob []byte) (int, *net.UDPAddr, netip.Addr, error) {
	n, oobn, _, addr, err := r.c.ReadMsgUDP(buf, oob)
	if err != nil || oobn == 0 {
		return n, addr, netip.Addr{}, err
	}
	msgs, perr := unix.ParseSocketControlMessage(oob[:oobn])
	if perr != nil {
		return n, addr, netip.Addr{}, nil
	}
	for _, m := range msgs {
		switch {
		case m.Header.Level == unix.IPPROTO_IPV6 && m.Header.Type == unix.IPV6_PKTINFO && len(m.Data) >= 16:
			// struct in6_pktinfo { struct in6_addr ipi6_addr; int ipi6_ifindex; }
			return n, addr, netip.AddrFrom16([16]byte(m.Data[:16])), nil
		case m.Header.Level == unix.IPPROTO_IP && m.Header.Type == unix.IP_PKTINFO && len(m.Data) >= 12:
			// struct in_pktinfo { int ipi_ifindex; in_addr ipi_spec_dst; in_addr ipi_addr; }
			return n, addr, netip.AddrFrom4([4]byte(m.Data[8:12])), nil
		}
	}
	return n, addr, netip.Addr{}, nil
}

// writeTo sends p to addr from src; with no src, or if the kernel refuses it
// (the address is gone), from whatever the kernel picks, as before.
func (r *replyPin) writeTo(p []byte, addr *net.UDPAddr, src netip.Addr) error {
	if src.IsValid() {
		var oob []byte
		if r.v6 {
			oob = unix.PktInfo6(&unix.Inet6Pktinfo{Addr: src.As16()})
		} else if src.Unmap().Is4() {
			oob = unix.PktInfo4(&unix.Inet4Pktinfo{Spec_dst: src.Unmap().As4()})
		}
		if oob != nil {
			if _, _, err := r.c.WriteMsgUDP(p, oob, addr); err == nil {
				return nil
			}
		}
	}
	_, err := r.c.WriteToUDP(p, addr)
	return err
}
