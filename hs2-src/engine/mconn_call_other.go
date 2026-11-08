//go:build !linux

package engine

import "net"

func tcpBusy(*net.TCPConn) (int64, bool) { return 0, false }
