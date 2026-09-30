//go:build !linux

package engine

import "net"

// TCP_INFO is Linux-only. Elsewhere per-link loss and send-side chrono stats are
// unavailable: soft-degrade and upload-pressure detection are off, and links are
// still healed on hard death. Production runs on Linux.
func tcpStats(tc *net.TCPConn) (tcpStat, bool) { return tcpStat{}, false }

func retransmits(tc *net.TCPConn) (uint64, bool) { return 0, false }
