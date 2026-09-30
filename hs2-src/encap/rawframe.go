package encap

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
)

// Framing for the raw-socket encapsulations (icmp/gre/ipip/ipx). This file is
// platform-independent — it only builds and parses bytes — so the wire format
// is unit-tested everywhere; raw_linux.go owns the sockets.
//
// The payload after the transport header is always one sealed carrier
// datagram, so the framing carries no secrets and needs no integrity of its
// own: the carrier's AEAD rejects anything forged or corrupted. It only has to
// (1) look like the protocol it claims to be, (2) say which link a packet
// belongs to, since raw IP has no ports, and (3) let a receiver throw away the
// host's other traffic of the same protocol cheaply.

// Addr is the address of one raw-encapsulation peer link: its IP plus the link
// id that tells apart several links from the same IP (the ICMP echo identifier,
// or the id in the hs2 framing). The listener demultiplexes peers by String().
type Addr struct {
	IP   net.IP
	ID   uint16
	Kind string
}

func (a *Addr) Network() string { return a.Kind }
func (a *Addr) String() string {
	if a == nil {
		return "<nil>"
	}
	return a.IP.String() + "#" + strconv.Itoa(int(a.ID))
}

// IP protocol numbers.
const (
	protoICMP = 1
	protoIPIP = 4
	protoGRE  = 47
)

const (
	icmpEchoReply   = 0
	icmpEchoRequest = 8
	greFlagsKey     = 0x2000 // K bit set, version 0 (RFC 2890)
	grePtypeIPv4    = 0x0800 // the protocol type every GRE-IPv4 tunnel carries
)

// framer builds and parses one kind's transport header, for one side (dial or
// listen). Everything it holds is fixed at construction, so it is safe for
// concurrent use.
type framer struct {
	kind    string
	proto   int  // IP protocol number of the raw socket
	hdr     int  // transport header length (the kind's Overhead)
	dial    bool // dial side: sends c2s, receives s2c
	txMagic uint16
	rxMagic uint16
	txType  byte // icmp: echo type sent
	rxType  byte // icmp: echo type accepted
}

func newFramer(kind string, opt Options, dial bool) (*framer, error) {
	k := normalize(kind)
	f := &framer{kind: k, dial: dial, hdr: Overhead(k)}
	switch k {
	case KindICMP:
		f.proto = protoICMP
		f.txType, f.rxType = icmpEchoReply, icmpEchoRequest
		if dial {
			f.txType, f.rxType = icmpEchoRequest, icmpEchoReply
		}
	case KindGRE:
		f.proto = protoGRE
	case KindIPIP:
		f.proto = protoIPIP
	case KindIPX:
		f.proto = opt.Proto
		if f.proto == 0 {
			f.proto = DefaultIPXProto
		}
		if !ValidIPXProto(f.proto) {
			return nil, fmt.Errorf("encap ipx: IP protocol %d is not usable for ipx (1..254, and not one the kernel handles)", f.proto)
		}
	default:
		return nil, fmt.Errorf("encap: %q is not a raw encapsulation", kind)
	}
	c2s, s2c := framingMagics(opt.Key, k, f.proto)
	f.txMagic, f.rxMagic = s2c, c2s
	if dial {
		f.txMagic, f.rxMagic = c2s, s2c
	}
	return f, nil
}

// framingMagics derives the two 16-bit framing magics for a kind (and, for
// ipx, its protocol number) from key: one per direction, never equal, so a
// side can never mistake its own packets — or a host echoing them back — for
// the peer's. HMAC keeps the key itself unrecoverable from the magic.
func framingMagics(key []byte, kind string, proto int) (c2s, s2c uint16) {
	m := func(dir string) uint16 {
		h := hmac.New(sha256.New, key)
		h.Write([]byte("hs2-encap-magic-v1\x00" + kind + "\x00" + strconv.Itoa(proto) + "\x00" + dir))
		return binary.BigEndian.Uint16(h.Sum(nil))
	}
	c2s, s2c = m("c2s"), m("s2c")
	if c2s == s2c {
		s2c ^= 0xffff
	}
	return c2s, s2c
}

// build writes the transport header for a packet to link id (and, for icmp,
// with echo sequence seq) followed by payload into dst, returning the packet.
func (f *framer) build(dst []byte, id, seq uint16, payload []byte) []byte {
	n := f.hdr + len(payload)
	if cap(dst) < n {
		dst = make([]byte, n)
	}
	b := dst[:n]
	copy(b[f.hdr:], payload)
	switch f.kind {
	case KindICMP:
		b[0], b[1] = f.txType, 0
		b[2], b[3] = 0, 0
		binary.BigEndian.PutUint16(b[4:], id)
		binary.BigEndian.PutUint16(b[6:], seq)
		binary.BigEndian.PutUint16(b[8:], f.txMagic)
		binary.BigEndian.PutUint16(b[2:], inetChecksum(b))
	case KindGRE:
		binary.BigEndian.PutUint16(b[0:], greFlagsKey)
		binary.BigEndian.PutUint16(b[2:], grePtypeIPv4)
		binary.BigEndian.PutUint16(b[4:], f.txMagic)
		binary.BigEndian.PutUint16(b[6:], id)
	default: // ipip, ipx
		binary.BigEndian.PutUint16(b[0:], f.txMagic)
		binary.BigEndian.PutUint16(b[2:], id)
	}
	return b
}

// parse validates a received transport packet (the outer IP header already
// removed) and returns its link id, echo sequence (icmp) and whether it is one
// of ours in the expected direction. The payload is b[f.hdr:].
func (f *framer) parse(b []byte) (id, seq uint16, ok bool) {
	if len(b) < f.hdr {
		return 0, 0, false
	}
	switch f.kind {
	case KindICMP:
		if b[0] != f.rxType || b[1] != 0 || binary.BigEndian.Uint16(b[8:]) != f.rxMagic {
			return 0, 0, false
		}
		return binary.BigEndian.Uint16(b[4:]), binary.BigEndian.Uint16(b[6:]), true
	case KindGRE:
		if binary.BigEndian.Uint16(b[0:]) != greFlagsKey || binary.BigEndian.Uint16(b[4:]) != f.rxMagic {
			return 0, 0, false
		}
		return binary.BigEndian.Uint16(b[6:]), 0, true
	default:
		if binary.BigEndian.Uint16(b[0:]) != f.rxMagic {
			return 0, 0, false
		}
		return binary.BigEndian.Uint16(b[2:]), 0, true
	}
}

// ipv4Payload splits a received raw IPv4 packet (Linux raw sockets deliver the
// full datagram, header included) into its source, destination and transport
// payload. It rejects anything that is not a whole, unfragmented IPv4 packet
// of the framer's protocol — including a datagram the socket buffer truncated.
func (f *framer) ipv4Payload(pkt []byte) (src, dst net.IP, payload []byte, ok bool) {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return nil, nil, nil, false
	}
	ihl := int(pkt[0]&0x0f) * 4
	total := int(binary.BigEndian.Uint16(pkt[2:4]))
	if ihl < 20 || total < ihl || total > len(pkt) || int(pkt[9]) != f.proto {
		return nil, nil, nil, false
	}
	// A fragment (MF set or a non-zero offset) never reaches a raw socket from
	// the kernel — it reassembles first — but refuse one defensively.
	if frag := binary.BigEndian.Uint16(pkt[6:8]); frag&0x3fff != 0 {
		return nil, nil, nil, false
	}
	return net.IP(pkt[12:16]), net.IP(pkt[16:20]), pkt[ihl:total], true
}

// inetChecksum is the RFC 1071 Internet checksum of b (ICMP needs it computed
// by the sender: a raw IPPROTO_ICMP socket does not fill it in).
func inetChecksum(b []byte) uint16 {
	var sum uint32
	n := len(b)
	for i := 0; i+1 < n; i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if n%2 == 1 {
		sum += uint32(b[n-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

// bpfInsn is one classic-BPF instruction (the layout of struct sock_filter);
// raw_linux.go converts it for SO_ATTACH_FILTER.
type bpfInsn struct {
	Code uint16
	Jt   uint8
	Jf   uint8
	K    uint32
}

// Classic BPF opcodes used by the receive filter.
const (
	bpfLdxMsh = 0xb1 // BPF_LDX|BPF_B|BPF_MSH : X = 4*(P[k]&0xf)
	bpfLdwAbs = 0x20 // BPF_LD|BPF_W|BPF_ABS   : A = P[k:4]  (IP header is absolute)
	bpfLdhInd = 0x48 // BPF_LD|BPF_H|BPF_IND  : A = P[X+k:2]
	bpfLdbInd = 0x50 // BPF_LD|BPF_B|BPF_IND  : A = P[X+k:1]
	bpfJeqK   = 0x15 // BPF_JMP|BPF_JEQ|BPF_K
	bpfRetK   = 0x06 // BPF_RET|BPF_K
	bpfAccept = 0x40000
)

// recvFilter compiles the kernel-side receive filter for this framer: the
// socket only wakes up for packets of our kind, in the peer's direction, with
// the peer's magic — and, on the dial side, for our own link id AND from the
// peer's source IP (srcIP, big-endian; 0 skips it). The source check matters
// because the dial socket is UNCONNECTED (a connected raw socket takes ICMP
// errors as fatal read errors, so one spoofed packet could kill the carrier),
// so the kernel would otherwise deliver every packet of the protocol from any
// host. It is a cheap first cut; parse and Read verify everything in userspace.
func (f *framer) recvFilter(id uint16, srcIP uint32) []bpfInsn {
	type check struct {
		code uint16
		off  uint32
		val  uint32
	}
	var cs []check
	if f.dial && srcIP != 0 {
		cs = append(cs, check{bpfLdwAbs, 12, srcIP}) // IP source address
	}
	switch f.kind {
	case KindICMP:
		cs = append(cs, check{bpfLdbInd, 0, uint32(f.rxType)}, check{bpfLdhInd, 8, uint32(f.rxMagic)})
		if f.dial {
			cs = append(cs, check{bpfLdhInd, 4, uint32(id)})
		}
	case KindGRE:
		cs = append(cs, check{bpfLdhInd, 0, greFlagsKey}, check{bpfLdhInd, 4, uint32(f.rxMagic)})
		if f.dial {
			cs = append(cs, check{bpfLdhInd, 6, uint32(id)})
		}
	default:
		cs = append(cs, check{bpfLdhInd, 0, uint32(f.rxMagic)})
		if f.dial {
			cs = append(cs, check{bpfLdhInd, 2, uint32(id)})
		}
	}
	prog := []bpfInsn{{Code: bpfLdxMsh, K: 0}}
	for i, c := range cs {
		// On mismatch jump over the remaining checks and the accept to drop.
		jf := uint8((len(cs)-1-i)*2 + 1)
		prog = append(prog, bpfInsn{Code: c.code, K: c.off}, bpfInsn{Code: bpfJeqK, Jt: 0, Jf: jf, K: c.val})
	}
	return append(prog, bpfInsn{Code: bpfRetK, K: bpfAccept}, bpfInsn{Code: bpfRetK, K: 0})
}
