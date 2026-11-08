//go:build !linux

package engine

import (
	"net"
	"net/netip"
)

// qShaper needs tc (Linux); elsewhere the controller never shapes.
type qShaper struct{ logf func(string, ...any) }

func newQShaper(logf func(string, ...any)) *qShaper { return &qShaper{logf: logf} }

func newQShaperOn(_ string, logf func(string, ...any)) *qShaper { return &qShaper{logf: logf} }

func (s *qShaper) apply(float64, []netip.Addr) bool { return false }
func (s *qShaper) remove()                          {}

func qLinkDev(*net.TCPConn) string { return "" }
