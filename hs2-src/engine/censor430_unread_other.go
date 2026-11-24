//go:build !linux

package engine

import "net"

func censorUnread(tc *net.TCPConn) int { return -1 }
