//go:build !linux

package engine

import "net"

func tcpCCInfo(tc *net.TCPConn) (ccInfo, bool) { return ccInfo{}, false }
func tcpCCReinit(tc *net.TCPConn) bool         { return false }
func ccBackRetryAll()                          {}
