package udpcarrier

import "github.com/hosseintaghipoursori-alt/hs2-tunnel/encap"

// CarrierOverhead is the most bytes the carrier adds around one tunnel packet
// on the wire: the datagram tag, wire sequence and send stamp (9), the FEC
// shard header and length prefix (fec.HeaderLen+2 = 11), the explicit
// sequence (8), and the sealed frame's header and 16-byte AEAD tag (28). A full-size data datagram
// is exactly innerMTU+CarrierOverhead bytes (TestCarrierOverhead measures it,
// parity included); nothing the carrier sends is larger.
const CarrierOverhead = 56

// PathMTU is the outer IPv4 MTU the carrier sizes for by default: Ethernet.
const PathMTU = 1500

// ipv4Header is the outer IPv4 header every encapsulation rides in.
const ipv4Header = 20

// InnerMTUFor is the largest tunnel MTU whose packets still fit one pathMTU
// IPv4 datagram, unfragmented, over the given encapsulation:
//
//	pathMTU - 20 (IPv4) - encap header - CarrierOverhead
//
// With a 1500-byte path that is 1416 for udp and gre, 1414 for icmp and 1420
// for ipip/ipx. The default tunnel MTU (DefaultInnerMTU, 1280) leaves room
// for paths below 1500 (PPPoE, an upstream tunnel) on every encapsulation.
func InnerMTUFor(kind string, pathMTU int) int {
	if pathMTU <= 0 {
		pathMTU = PathMTU
	}
	return pathMTU - ipv4Header - encap.Overhead(kind) - CarrierOverhead
}
