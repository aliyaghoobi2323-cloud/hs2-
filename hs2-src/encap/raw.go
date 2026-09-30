package encap

import (
	"errors"
	"net"
)

// Per-encapsulation wire overhead around each sealed datagram (transport header
// only; the 20-byte outer IP header is common to all and budgeted by the
// carrier). These are fixed by the framing in raw_linux.go.
const (
	icmpOverhead = 8 // ICMP echo header: type/code/checksum/id/seq
	greOverhead  = 4 // GRE: flags/version + protocol type
	ipipOverhead = 4 // our 4-byte magic+seq framing on IP proto 4
	ipxOverhead  = 4 // our 4-byte magic+seq framing on a chosen IP proto
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
