//go:build !linux

package engine

import "net"

func sockRcvbuf(net.Conn) (int, bool) { return 0, false }
