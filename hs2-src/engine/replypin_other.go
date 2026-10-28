//go:build !linux

package engine

import (
	"net"
	"net/netip"
)

// Reply-source pinning (replypin_linux.go) is Linux-only; elsewhere a wildcard
// UDP user port answers from the routing-preferred source. Production runs on
// Linux.
type replyPin struct{}

const replyPinOOB = 0

func pinReplies(pc net.PacketConn) *replyPin { return nil }

func (r *replyPin) readFrom(buf, oob []byte) (int, *net.UDPAddr, netip.Addr, error) {
	return 0, nil, netip.Addr{}, net.ErrClosed
}

func (r *replyPin) writeTo(p []byte, addr *net.UDPAddr, src netip.Addr) error { return net.ErrClosed }
