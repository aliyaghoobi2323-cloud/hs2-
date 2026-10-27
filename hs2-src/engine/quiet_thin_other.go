//go:build !linux

package engine

import "net"

// setThinTimeouts: no TCP_THIN_LINEAR_TIMEOUTS off Linux.
func setThinTimeouts(tc *net.TCPConn) bool { return false }
