//go:build !linux

package engine

import (
	"net"
	"time"
)

func tcpBusy(*net.TCPConn) (int64, bool) { return 0, false }

func tcpTraceInfo(*net.TCPConn) string { return "" }

func tcpSRTT(*net.TCPConn) (time.Duration, bool) { return 0, false }
