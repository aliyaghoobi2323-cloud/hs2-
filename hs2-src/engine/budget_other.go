//go:build !linux

package engine

import "net"

// newOutQ: no SIOCOUTQ off Linux, so no in-flight budget (budget.go).
func newOutQ(tc *net.TCPConn) func() (int, error) { return nil }
