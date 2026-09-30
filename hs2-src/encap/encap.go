// Package encap is the pluggable datagram-transport layer under the hs2 UDP
// carrier. The carrier (udpcarrier) seals every frame with the core crypto
// (Noise IKpsk2 + ChaCha20-Poly1305 + adaptive FEC) and hands it here as one
// opaque datagram; encap only decides HOW that datagram crosses the wire:
//
//   - udp   — an ordinary UDP datagram (the default; native sockets)
//   - icmp  — the datagram as the payload of an ICMP Echo packet (raw socket)
//   - gre   — inside a GRE packet, IP protocol 47 (raw socket)
//   - ipip  — inside an IP-in-IP packet, IP protocol 4 (raw socket)
//   - ipx   — a raw IP protocol with a selectable number and light framing,
//     for paths that pass only one odd protocol
//
// Because the payload is always the carrier's sealed datagram, EVERY
// encapsulation gets the same encryption, the same authentication and the same
// FEC — the encapsulation is just the outer envelope. A restrictive path that
// drops UDP/TCP but forwards ICMP (a real case: some Iran<->abroad links pass
// only ICMP) can still carry the tunnel by choosing the icmp encapsulation.
//
// # The interface
//
// A transport is a datagram socket, so the two Go standard interfaces already
// fit exactly and the carrier needs no encapsulation-specific code:
//
//   - dial side  : net.Conn        (connected: Write/Read to one peer)
//   - listen side: net.PacketConn  (WriteTo/ReadFrom, many peers by address)
//
// Dial and Listen return those. Raw-socket transports (icmp/gre/ipip/ipx) bind
// to a specific local IP and, on the dial side, target a specific peer IP
// (like Backhaul's listen_ip/dst_ip), because a raw IP socket has no ports to
// demultiplex on. UDP keeps the full multi-IP wildcard behaviour it always had.
package encap

import (
	"fmt"
	"net"
	"sort"
	"strings"
)

// Kinds are the encapsulation names accepted by Dial/Listen and written into a
// config's "encap" field. udp is the default and needs no privileges; the rest
// need CAP_NET_RAW (the systemd unit and the installer grant it).
const (
	KindUDP  = "udp"
	KindICMP = "icmp"
	KindGRE  = "gre"
	KindIPIP = "ipip"
	KindIPX  = "ipx"
)

// Options configures a transport beyond its address. Fields not relevant to a
// kind are ignored, so one struct serves them all.
type Options struct {
	// BindIP is the local source IP (a config's bind_local_ip / listen_ip).
	// Empty lets the kernel choose (udp only; raw transports need it set when
	// the host has more than one address).
	BindIP string
	// ICMPRole is "client" or "server": an ICMP Echo tunnel sends requests
	// (type 8) from the client and replies (type 0) from the server, so a
	// stateful middlebox sees an ordinary ping exchange. Set by the carrier
	// side (dial=client, listen=server); ignored by other kinds.
	ICMPRole string
	// Proto is the IP protocol number for the ipx encapsulation (1..254,
	// avoiding the well-known ones the other kinds use; 0 = DefaultIPXProto).
	// Ignored otherwise.
	Proto int
	// Key keys the raw encapsulations' 16-bit framing magic (one per kind and
	// per direction), so a host's other traffic of the same IP protocol — a real
	// GRE/IPIP tunnel, the internet's pings — is discarded with one compare
	// instead of costing a handshake attempt, and so the magic is not a fixed
	// fingerprint shared by every deployment. The carrier passes a value
	// derived from the tunnel's shared secret. Empty uses an unkeyed default.
	// Ignored by udp.
	Key []byte
}

// DefaultIPXProto is the IP protocol the ipx encapsulation uses when none is
// configured: 253, reserved for experimentation (RFC 3692), which no kernel
// module claims.
const DefaultIPXProto = 253

// ipxHandledProtos are the IP protocol numbers the Linux kernel has a handler
// for (or that are reserved). ipx must avoid them: a raw socket does not stop
// the kernel's own handler from also seeing the packet, so it would be
// processed twice — a real IPIP/GRE/ESP tunnel would eat it, and an unhandled
// but assigned number risks the kernel replying protocol-unreachable. Only
// numbers with no in-kernel consumer are safe to reuse for the tunnel.
var ipxHandledProtos = map[int]bool{
	0: true, 1: true /*ICMP*/, 2: true /*IGMP*/, 4: true /*IPIP*/, 6: true, /*TCP*/
	17: true /*UDP*/, 33: true /*DCCP*/, 41: true /*IPv6/6in4*/, 43: true, /*IPv6-Route*/
	44: true /*IPv6-Frag*/, 46: true /*RSVP*/, 47: true /*GRE*/, 50: true, /*ESP*/
	51: true /*AH*/, 58: true /*ICMPv6*/, 59: true, 60: true, 88: true, /*EIGRP*/
	89: true /*OSPF*/, 92: true /*MTP*/, 94: true /*KA/IPIP*/, 97: true, /*ETHERIP*/
	98: true /*ENCAP*/, 103: true /*PIM*/, 108: true /*IPComp*/, 112: true, /*VRRP*/
	115: true /*L2TP*/, 132: true /*SCTP*/, 136: true /*UDPLite*/, 137: true, /*MPLS-in-IP*/
	143: true /*Ethernet*/, 255: true, /*reserved*/
}

// ValidIPXProto reports whether p may be used by the ipx encapsulation: a
// protocol number in 1..254 that the kernel has no handler for and that no
// other encapsulation owns. The default (DefaultIPXProto, 253) is one of the
// RFC 3692 experimentation numbers, which are guaranteed unhandled.
func ValidIPXProto(p int) bool {
	if p < 1 || p > 254 {
		return false
	}
	return !ipxHandledProtos[p]
}

// Dial opens a connected datagram transport of the named kind to addr. For udp,
// addr is host:port; for raw kinds it is an IP (a :port suffix is accepted and
// ignored, so the same config "addr" works for every kind).
func Dial(kind, addr string, opt Options) (net.Conn, error) {
	switch normalize(kind) {
	case KindUDP:
		return dialUDP(addr, opt)
	case KindICMP:
		return dialRaw(KindICMP, addr, opt)
	case KindGRE:
		return dialRaw(KindGRE, addr, opt)
	case KindIPIP:
		return dialRaw(KindIPIP, addr, opt)
	case KindIPX:
		return dialRaw(KindIPX, addr, opt)
	default:
		return nil, fmt.Errorf("encap: unknown kind %q", kind)
	}
}

// Listen opens a listening datagram transport of the named kind. For udp, addr
// is host:port; for raw kinds it is the local IP to bind (opt.BindIP is used
// when addr has no IP, e.g. ":0").
func Listen(kind, addr string, opt Options) (net.PacketConn, error) {
	switch normalize(kind) {
	case KindUDP:
		return listenUDP(addr)
	case KindICMP:
		return listenRaw(KindICMP, addr, opt)
	case KindGRE:
		return listenRaw(KindGRE, addr, opt)
	case KindIPIP:
		return listenRaw(KindIPIP, addr, opt)
	case KindIPX:
		return listenRaw(KindIPX, addr, opt)
	default:
		return nil, fmt.Errorf("encap: unknown kind %q", kind)
	}
}

// Overhead is the number of bytes this encapsulation adds around each sealed
// datagram on the wire, so the carrier can size the inner MTU to avoid IP
// fragmentation. It is the transport header only; the 20-byte outer IP header
// is common to all and accounted for by the carrier's base budget.
func Overhead(kind string) int {
	switch normalize(kind) {
	case KindUDP:
		return 8 // UDP header
	case KindICMP:
		return icmpOverhead
	case KindGRE:
		return greOverhead
	case KindIPIP:
		return ipipOverhead
	case KindIPX:
		return ipxOverhead
	default:
		return 8
	}
}

// Raw reports whether a kind needs a raw socket (CAP_NET_RAW) rather than an
// ordinary UDP socket. The installer and the config checker use it to warn.
func Raw(kind string) bool { return normalize(kind) != KindUDP }

// Kinds lists the supported encapsulation names, for menus and help text.
func Kinds() []string {
	ks := []string{KindUDP, KindICMP, KindGRE, KindIPIP, KindIPX}
	sort.Strings(ks)
	return ks
}

// Valid reports whether kind is a supported encapsulation (empty = udp).
func Valid(kind string) bool {
	switch normalize(kind) {
	case KindUDP, KindICMP, KindGRE, KindIPIP, KindIPX:
		return true
	}
	return false
}

func normalize(kind string) string {
	k := strings.ToLower(strings.TrimSpace(kind))
	if k == "" {
		return KindUDP
	}
	return k
}

// hostOf strips an optional :port from addr and returns the host/IP. Raw
// transports have no ports, but accepting the suffix lets the same config
// "addr" (host:port) drive every kind.
func hostOf(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}
