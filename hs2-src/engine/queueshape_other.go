//go:build !linux

package engine

import (
	"net"
)

// qShaper needs tc (Linux); elsewhere the controller never shapes.
type qShaper struct{ logf func(string, ...any) }

func newQShaper(logf func(string, ...any)) *qShaper { return &qShaper{logf: logf} }

func newQShaperOn(_ string, logf func(string, ...any)) *qShaper { return &qShaper{logf: logf} }

func (s *qShaper) apply(float64, []qKey) bool { return false }
func (s *qShaper) remove()                    {}
func (s *qShaper) zduRelease()                {}
func (s *qShaper) park([]qKey) bool           { return false }
func (s *qShaper) placed() bool               { return false }
func (s *qShaper) drops() uint64              { return 0 }
func (s *qShaper) stats() (uint64, uint64)    { return 0, 0 }

const qWire = 1.045

func qLinkDev(*net.TCPConn) string { return "" }

func qLinkSport(*net.TCPConn) uint16 { return 0 }
