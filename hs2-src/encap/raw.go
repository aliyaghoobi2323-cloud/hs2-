package encap

import (
	"errors"
	"net"
)

// Per-encapsulation wire overhead around each sealed datagram (transport header
// only; the 20-byte outer IP header is common to all and budgeted by the
// carrier). These are fixed by the framing in rawframe.go:
//
//	icmp : [type][code][csum:2][id:2][seq:2] [nonce:8]      echo header + obfs nonce
//	gre  : [flags/ver:2][ptype:2] [key = magic:2 | id:2]    RFC 2890 GRE with key
//	ipip : [magic:2][id:2]                                  on IP protocol 4
//	ipx  : [magic:2][id:2]                                  on a chosen IP protocol
//
// id names one link (one dialed transport) so a listener can tell apart several
// links from the same peer IP — raw IP has no ports; for icmp it is the echo
// identifier, which NAT rewrites consistently.
//
// icmp carries no fixed magic: a keyed directional prefix in the echo-sequence
// high byte tells the two directions apart and lets the kernel cheap-reject a
// host's unrelated ICMP, and an 8-byte per-packet nonce keys the mask over the
// structured header (obfs.go). gre/ipip/ipx keep the keyed framing magic.
const (
	icmpOverhead = 16 // 8-byte ICMP echo header + 8-byte obfuscation nonce
	greOverhead  = 8
	ipipOverhead = 4
	ipxOverhead  = 4
)

// errRawUnsupported is returned by the non-Linux build: raw ICMP/GRE/IPIP
// sockets are Linux-only. Production runs on Linux; udp works everywhere.
var errRawUnsupported = errors.New("encap: raw-socket encapsulations require Linux")

// dialRaw / listenRaw are implemented per-OS: raw_linux.go for Linux (real raw
// sockets), raw_other.go elsewhere (returns errRawUnsupported).
var (
	dialRawFn   func(kind, addr string, opt Options) (net.Conn, error)
	listenRawFn func(kind, addr string, opt Options) (net.PacketConn, error)
)

func dialRaw(kind, addr string, opt Options) (net.Conn, error) {
	if dialRawFn == nil {
		return nil, errRawUnsupported
	}
	return dialRawFn(kind, addr, opt)
}

func listenRaw(kind, addr string, opt Options) (net.PacketConn, error) {
	if listenRawFn == nil {
		return nil, errRawUnsupported
	}
	return listenRawFn(kind, addr, opt)
}
