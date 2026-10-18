//go:build !linux

package engine

import "net"

// setMaxPacing: SO_MAX_PACING_RATE is Linux's (the pacing net and the
// window core's lab experiment); elsewhere nothing is capped.
func setMaxPacing(tc *net.TCPConn, rate uint64) bool { return false }
