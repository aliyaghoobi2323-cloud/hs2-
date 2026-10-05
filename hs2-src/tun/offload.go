package tun

import (
	"encoding/binary"
	"errors"
)

// TCP segmentation and receive coalescing for a TUN opened with a virtio-net
// header (IFF_VNET_HDR) and TCP offloads (TUNSETOFFLOAD) — what lets the
// kernel and hs2 trade one large TCP packet instead of dozens of MTU-sized
// ones, one read() or write() each.
//
//   - Read (kernel → hs2): the kernel may hand over a TCP packet of up to 64
//     KB (gso_type TCPV4) to be cut into gso_size-byte segments, or any
//     packet whose transport checksum is left partial (NEEDS_CSUM). splitTCP
//     cuts the first into ordinary, fully checksummed IPv4 segments — exactly
//     the packets the kernel would have sent one by one — and completeCsum
//     finishes the second. Nothing on the wire changes: the other server gets
//     the same MTU-sized packets as before, from any version.
//   - Write (hs2 → kernel): coalesce merges consecutive in-order segments of
//     one TCP connection (same headers but the sequence, no flags but ACK and
//     a final PSH, equal sizes but the last) into one packet with gso_type
//     TCPV4 and a partial checksum, as the kernel's own GRO does; everything
//     else is written as it came.

// vnetHdrLen is the size of struct virtio_net_hdr (no mergeable buffers).
const vnetHdrLen = 10

const (
	vnetNeedsCsum = 1    // VIRTIO_NET_HDR_F_NEEDS_CSUM
	gsoNone       = 0    // VIRTIO_NET_HDR_GSO_NONE
	gsoTCPv4      = 1    // VIRTIO_NET_HDR_GSO_TCPV4
	gsoECN        = 0x80 // VIRTIO_NET_HDR_GSO_ECN
)

// vnetHdr is struct virtio_net_hdr; a TUN uses the host's byte order.
type vnetHdr struct {
	flags      uint8
	gsoType    uint8
	hdrLen     uint16
	gsoSize    uint16
	csumStart  uint16
	csumOffset uint16
}

func (h *vnetHdr) decode(b []byte) error {
	if len(b) < vnetHdrLen {
		return errShort
	}
	h.flags, h.gsoType = b[0], b[1]
	h.hdrLen = binary.NativeEndian.Uint16(b[2:])
	h.gsoSize = binary.NativeEndian.Uint16(b[4:])
	h.csumStart = binary.NativeEndian.Uint16(b[6:])
	h.csumOffset = binary.NativeEndian.Uint16(b[8:])
	return nil
}

func (h vnetHdr) encode(b []byte) {
	b[0], b[1] = h.flags, h.gsoType
	binary.NativeEndian.PutUint16(b[2:], h.hdrLen)
	binary.NativeEndian.PutUint16(b[4:], h.gsoSize)
	binary.NativeEndian.PutUint16(b[6:], h.csumStart)
	binary.NativeEndian.PutUint16(b[8:], h.csumOffset)
}

var (
	errShort    = errors.New("tun: short packet")
	errBadGSO   = errors.New("tun: malformed offload packet")
	errUnsuppGS = errors.New("tun: unsupported offload type")
)

// csumAdd adds b to a one's-complement sum of big-endian 16-bit words.
func csumAdd(sum uint32, b []byte) uint32 {
	for len(b) >= 8 {
		sum += uint32(binary.BigEndian.Uint16(b)) + uint32(binary.BigEndian.Uint16(b[2:])) +
			uint32(binary.BigEndian.Uint16(b[4:])) + uint32(binary.BigEndian.Uint16(b[6:]))
		sum = (sum & 0xffff) + (sum >> 16)
		b = b[8:]
	}
	for len(b) >= 2 {
		sum += uint32(binary.BigEndian.Uint16(b))
		b = b[2:]
	}
	if len(b) == 1 {
		sum += uint32(b[0]) << 8
	}
	return sum
}

// csumFold folds a sum to 16 bits (not complemented).
func csumFold(sum uint32) uint16 {
	for sum > 0xffff {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return uint16(sum)
}

// pseudo4 is the IPv4 pseudo-header sum for a transport segment of length n.
func pseudo4(src, dst []byte, proto byte, n int) uint32 {
	sum := csumAdd(0, src[:4])
	sum = csumAdd(sum, dst[:4])
	return sum + uint32(proto) + uint32(n)
}

// ipv4Csum sets the header checksum of the IPv4 header h.
func ipv4Csum(h []byte) {
	h[10], h[11] = 0, 0
	binary.BigEndian.PutUint16(h[10:], ^csumFold(csumAdd(0, h)))
}

// completeCsum finishes a partial transport checksum (NEEDS_CSUM): the field
// at start+off holds the pseudo-header sum; the checksum covers pkt[start:].
func completeCsum(pkt []byte, start, off int) error {
	if start < 0 || off < 0 || start+off+2 > len(pkt) {
		return errBadGSO
	}
	c := ^csumFold(csumAdd(0, pkt[start:]))
	binary.BigEndian.PutUint16(pkt[start+off:], c)
	return nil
}

// TCP flags.
const (
	tcpFIN = 0x01
	tcpSYN = 0x02
	tcpRST = 0x04
	tcpPSH = 0x08
	tcpACK = 0x10
	tcpURG = 0x20
	tcpECE = 0x40
	tcpCWR = 0x80
)

// minGSOSize: an offload packet with smaller segments is refused (a sane
// kernel never sends one; it would cut 64 KB into thousands of packets).
const minGSOSize = 64

// splitTCP cuts an IPv4 TCP offload packet into segments of at most gsoSize
// payload bytes, each a complete packet with valid IP and TCP checksums,
// appended to out (one after another); ends gets each segment's end offset in
// out. The sequence advances per segment, the IP ID by one; FIN and PSH stay
// on the last segment only, CWR on the first only — as the kernel segments.
func splitTCP(pkt []byte, gsoSize int, out []byte, ends []int) ([]byte, []int, error) {
	if len(pkt) < 20 || pkt[0]>>4 != 4 || pkt[9] != 6 {
		return out, ends, errUnsuppGS
	}
	ihl := int(pkt[0]&0x0f) * 4
	if ihl < 20 || len(pkt) < ihl+20 {
		return out, ends, errBadGSO
	}
	thl := int(pkt[ihl+12]>>4) * 4
	hl := ihl + thl
	if thl < 20 || len(pkt) < hl || gsoSize < minGSOSize {
		return out, ends, errBadGSO
	}
	payload := pkt[hl:]
	seq := binary.BigEndian.Uint32(pkt[ihl+4:])
	id := binary.BigEndian.Uint16(pkt[4:])
	flags := pkt[ihl+13]
	for off, i := 0, 0; off < len(payload) || i == 0; i++ {
		n := min(gsoSize, len(payload)-off)
		start := len(out)
		out = append(out, pkt[:hl]...)
		out = append(out, payload[off:off+n]...)
		s := out[start:]
		binary.BigEndian.PutUint16(s[2:], uint16(hl+n))
		binary.BigEndian.PutUint16(s[4:], id+uint16(i))
		ipv4Csum(s[:ihl])
		t := s[ihl:]
		binary.BigEndian.PutUint32(t[4:], seq+uint32(off))
		f := flags
		if off+n < len(payload) {
			f &^= tcpFIN | tcpPSH
		}
		if i > 0 {
			f &^= tcpCWR
		}
		t[13] = f
		t[16], t[17] = 0, 0
		sum := pseudo4(s[12:16], s[16:20], 6, thl+n)
		binary.BigEndian.PutUint16(t[16:], ^csumFold(csumAdd(sum, t)))
		ends = append(ends, len(out))
		off += n
		if n == 0 {
			break
		}
	}
	return out, ends, nil
}

// groSeg is one packet's coalescing view: an IPv4 TCP segment carrying data
// and no flag but ACK (and PSH), or not mergeable.
type groSeg struct {
	ok       bool
	ihl, thl int
	seq, ack uint32
	payload  int
	psh      bool
	key      [12]byte // src, dst, ports
	tosTTLDF [3]byte
	win      uint16
	optsFrom int // options bytes are pkt[ihl+20 : ihl+thl]
}

func parseGRO(p []byte) (g groSeg) {
	if len(p) < 40 || p[0] != 0x45 || p[9] != 6 {
		return g // IPv4 without options, TCP
	}
	if int(binary.BigEndian.Uint16(p[2:])) != len(p) {
		return g
	}
	if frag := binary.BigEndian.Uint16(p[6:]); frag&0x3fff != 0 { // MF or an offset
		return g
	}
	ihl := 20
	thl := int(p[ihl+12]>>4) * 4
	if thl < 20 || len(p) < ihl+thl {
		return g
	}
	fl := p[ihl+13]
	if fl&^(tcpACK|tcpPSH) != 0 || fl&tcpACK == 0 {
		return g
	}
	g.payload = len(p) - ihl - thl
	if g.payload == 0 {
		return g
	}
	g.ok, g.ihl, g.thl = true, ihl, thl
	g.seq = binary.BigEndian.Uint32(p[ihl+4:])
	g.ack = binary.BigEndian.Uint32(p[ihl+8:])
	g.win = binary.BigEndian.Uint16(p[ihl+14:])
	g.psh = fl&tcpPSH != 0
	copy(g.key[:8], p[12:20])
	copy(g.key[8:], p[ihl:ihl+4])
	g.tosTTLDF = [3]byte{p[1], p[8], p[6] & 0x40}
	return g
}

// groMax: segments and bytes one coalesced packet may hold.
const (
	groMaxSegs  = 64
	groMaxBytes = 65535
)

// groItem is one packet to write: a packet as it came, or a run of segments
// of one connection to merge (segs indexes into the batch).
type groItem struct {
	segs    []int
	g       groSeg
	nextSeq uint32
	gso     int  // payload size of every segment but the last
	bytes   int  // IP length if merged
	closed  bool // a shorter segment or PSH ended it
}

// coalesce plans the writes for a batch of IP packets: items in arrival
// order, each one packet or a mergeable run of one connection's segments.
// Packets of one connection keep their order; different connections' do not
// matter to the kernel.
func coalesce(pkts [][]byte, items []groItem, open map[[12]byte]int) []groItem {
	clear(open)
	for i, p := range pkts {
		g := parseGRO(p)
		if !g.ok {
			// Any other TCP packet of a connection (IP options, flags, no
			// data) ends its open run: a later segment must not merge
			// across it, or the two would be written out of order.
			if len(p) >= 20 && p[0]>>4 == 4 && p[9] == 6 {
				if ihl := int(p[0]&0x0f) * 4; len(p) >= ihl+4 {
					var k [12]byte
					copy(k[:8], p[12:20])
					copy(k[8:], p[ihl:ihl+4])
					delete(open, k)
				}
			}
			items = append(items, groItem{segs: []int{i}})
			continue
		}
		if j, ok := open[g.key]; ok && mergeable(&items[j], g, pkts[items[j].segs[0]], p) {
			it := &items[j]
			it.segs = append(it.segs, i)
			it.nextSeq = g.seq + uint32(g.payload)
			it.bytes += g.payload
			if g.payload < it.gso || g.psh || len(it.segs) >= groMaxSegs {
				it.closed = true
				delete(open, g.key)
			}
			continue
		}
		items = append(items, groItem{segs: []int{i}, g: g, nextSeq: g.seq + uint32(g.payload),
			gso: g.payload, bytes: len(p), closed: g.psh})
		if g.psh {
			delete(open, g.key)
		} else {
			open[g.key] = len(items) - 1
		}
	}
	return items
}

// mergeable reports whether segment g (packet p) continues run it (first
// packet first).
func mergeable(it *groItem, g groSeg, first, p []byte) bool {
	f := it.g
	if it.closed || !f.ok || g.seq != it.nextSeq || g.ack != f.ack || g.win != f.win ||
		g.thl != f.thl || g.tosTTLDF != f.tosTTLDF || g.payload > it.gso ||
		it.bytes+g.payload > groMaxBytes {
		return false
	}
	// TCP options (timestamps) must be byte-equal, as the kernel's GRO wants.
	a, b := first[f.ihl+20:f.ihl+f.thl], p[g.ihl+20:g.ihl+g.thl]
	return string(a) == string(b)
}

// buildMerged writes run it's merged packet into out (vnet header first) and
// returns it.
func buildMerged(out []byte, pkts [][]byte, it *groItem) []byte {
	first := pkts[it.segs[0]]
	g := it.g
	hl := g.ihl + g.thl
	out = append(out[:0], make([]byte, vnetHdrLen)...)
	out = append(out, first[:hl]...)
	psh := false
	for _, i := range it.segs {
		p := pkts[i]
		out = append(out, p[hl:]...)
		psh = psh || p[g.ihl+13]&tcpPSH != 0
	}
	s := out[vnetHdrLen:]
	binary.BigEndian.PutUint16(s[2:], uint16(len(s)))
	ipv4Csum(s[:g.ihl])
	t := s[g.ihl:]
	if psh {
		t[13] |= tcpPSH
	}
	// Partial checksum: the pseudo-header sum, folded and not complemented;
	// the kernel (CHECKSUM_PARTIAL) completes it if it ever needs it.
	binary.BigEndian.PutUint16(t[16:], csumFold(pseudo4(s[12:16], s[16:20], 6, len(t))))
	vnetHdr{flags: vnetNeedsCsum, gsoType: gsoTCPv4, hdrLen: uint16(hl), gsoSize: uint16(it.gso),
		csumStart: uint16(g.ihl), csumOffset: 16}.encode(out)
	return out
}
