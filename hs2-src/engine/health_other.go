//go:build !linux

package engine

import "net"

// retransmits is Linux-only (TCP_INFO). On other platforms per-link loss is
// unavailable, so soft-degrade detection is disabled and links are still healed
// on hard death. Production runs on Linux.
func retransmits(tc *net.TCPConn) (uint64, bool) { return 0, false }
