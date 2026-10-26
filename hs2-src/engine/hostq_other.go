//go:build !linux

package engine

import "net"

// newHostSys: TCP_NOTSENT_LOWAT and SIOCOUTQNSD are Linux's; elsewhere the
// guard is today's send gate (newHostQ with no hostSys). Production runs
// on Linux.
func newHostSys(c net.Conn) hostSys { return nil }
