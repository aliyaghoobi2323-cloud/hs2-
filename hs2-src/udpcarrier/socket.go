package udpcarrier

import "net"

// packetSocket is the listener's view of its underlying datagram socket,
// abstracted over the encapsulation so the demux/handshake code is written once
// and works for every transport encap provides.
//
// For a wildcard UDP socket it carries the per-datagram destination IP
// (IP_PKTINFO) so each reply leaves from the exact local address the peer
// targeted — the multi-homed behaviour the carrier has always had. For every
// other socket (a UDP bind to a specific IP, or a raw icmp/gre/ipip/ipx socket
// that already binds one local IP) there is nothing to pin: it is a plain
// ReadFrom/WriteTo PacketConn and the per-datagram dstIP is nil.
type packetSocket struct {
	pc  net.PacketConn
	udp *net.UDPConn // non-nil only when pktinfo is active on a wildcard UDP bind
	oob []byte       // pktinfo control-message scratch (serve loop only)

	// Batches (several datagrams per syscall), when the socket can: a raw
	// encapsulation's listener, or a UDP socket.
	raw interface {
		ReadBatch(func([]byte, net.Addr)) error
		WriteBatchTo([][]byte, net.Addr) error
	}
	uc *net.UDPConn
	ub *udpListenBatch
}

// newPacketSocket wraps pc, enabling reply-source pinning when pc is a wildcard
// UDP socket on a platform that supports IP_PKTINFO.
func newPacketSocket(pc net.PacketConn) *packetSocket {
	ps := &packetSocket{pc: pc}
	if uc, ok := pc.(*net.UDPConn); ok && enablePktinfo(uc) {
		ps.udp = uc
		ps.oob = make([]byte, pktinfoOOB)
	}
	switch c := pc.(type) {
	case interface {
		ReadBatch(func([]byte, net.Addr)) error
		WriteBatchTo([][]byte, net.Addr) error
	}:
		ps.raw = c
	case *net.UDPConn:
		ps.uc, ps.ub = c, newUDPListenBatch(c, ps.udp != nil)
	}
	return ps
}

// batched reports whether readBatch and writeBatchTo take several datagrams
// per syscall on this socket.
func (s *packetSocket) batched() bool { return s.raw != nil || s.ub != nil }

// readBatch reads the datagrams waiting in one call; fn gets each (the slice
// is reused after it returns), its source and, with pktinfo, the local IP it
// was sent to.
func (s *packetSocket) readBatch(fn func(b []byte, src net.Addr, dst net.IP)) error {
	if s.raw != nil {
		return s.raw.ReadBatch(func(b []byte, a net.Addr) { fn(b, a, nil) })
	}
	return s.ub.read(fn)
}

// writeBatchTo sends several datagrams to dst (from srcIP, as writeTo) in one
// call where the socket can, else one by one.
func (s *packetSocket) writeBatchTo(bs [][]byte, dst net.Addr, srcIP net.IP) error {
	switch {
	case s.raw != nil:
		return s.raw.WriteBatchTo(bs, dst)
	case s.ub != nil:
		if ua, ok := dst.(*net.UDPAddr); ok {
			if s.udp == nil {
				srcIP = nil // no pktinfo: the kernel picks, as writeTo
			}
			return udpWriteBatchTo(s.uc, bs, ua, srcIP)
		}
	}
	for _, b := range bs {
		if err := s.writeTo(b, dst, srcIP); err != nil {
			return err
		}
	}
	return nil
}

// readFrom reads one datagram and, when pktinfo is active, the local IP the peer
// sent it to (nil otherwise). src is the peer address, used as the demux key.
func (s *packetSocket) readFrom(buf []byte) (n int, src net.Addr, dstIP net.IP, err error) {
	if s.udp != nil {
		un, ua, dst, rerr := readWithDst(s.udp, buf, s.oob)
		var sa net.Addr
		if ua != nil {
			sa = ua
		}
		return un, sa, dst, rerr
	}
	n, src, err = s.pc.ReadFrom(buf)
	return n, src, nil, err
}

// writeTo sends b to dst. When pktinfo is active and srcIP is set, the datagram
// leaves from srcIP; otherwise the kernel picks the source (raw sockets are
// already bound to one local IP, so there is nothing to pin).
func (s *packetSocket) writeTo(b []byte, dst net.Addr, srcIP net.IP) error {
	if s.udp != nil && srcIP != nil {
		if ua, ok := dst.(*net.UDPAddr); ok {
			return writeFrom(s.udp, b, ua, srcIP)
		}
	}
	_, err := s.pc.WriteTo(b, dst)
	return err
}

func (s *packetSocket) localAddr() net.Addr { return s.pc.LocalAddr() }
func (s *packetSocket) close() error        { return s.pc.Close() }
