//go:build !linux

package engine

import (
	"net"
	"time"
)

func tcpBusy(*net.TCPConn) (int64, bool) { return 0, false }

func tcpTraceInfo(*net.TCPConn) string { return "" }

func tcpSRTT(*net.TCPConn) (time.Duration, bool) { return 0, false }

func tcpAckState(*net.TCPConn) (time.Duration, uint64, uint32, bool, bool) {
	return 0, 0, 0, false, false
}

func tcpDarkInfo(*net.TCPConn) (acked uint64, unacked uint32, backoff uint8, retrans uint32, ok bool) {
	return 0, 0, 0, 0, false
}

func tcpOutq(*net.TCPConn) (uint64, bool) { return 0, false }
