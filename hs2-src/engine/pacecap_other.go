//go:build !linux

package engine

import "net"

// setMaxPacing: SO_MAX_PACING_RATE is Linux's (the window core's lab
// experiment only).
func setMaxPacing(tc *net.TCPConn, rate uint64) bool { return false }
