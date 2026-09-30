//go:build linux

package engine

import (
	"net"

	"golang.org/x/sys/unix"
)

// retransmits reads the kernel's cumulative TCP retransmit count for a link via
// TCP_INFO. It is an activity-independent measure of path loss: a link moving
// data with a high retransmit fraction is genuinely degraded, whether or not its
// users are busy — unlike raw throughput, which also drops when users are idle.
func retransmits(tc *net.TCPConn) (uint64, bool) {
	if tc == nil {
		return 0, false
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		return 0, false
	}
	var total uint32
	var ok bool
	rc.Control(func(fd uintptr) {
		info, e := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
		if e != nil {
			return
		}
		total = info.Total_retrans
		ok = true
	})
	return uint64(total), ok
}

// sendPressure reads TCP_INFO and reports whether the socket has bytes queued
// that the network has not yet accepted (notsent_bytes) — i.e. the writer is
// held back by the path, the fingerprint of a per-connection throttle. With
// TCP_NOTSENT_LOWAT set low the kernel keeps only a small backlog, so any
// non-trivial notsent while sending means "wants to push more".
func sendPressure(tc *net.TCPConn) (bool, bool) {
	if tc == nil {
		return false, false
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		return false, false
	}
	var pressing, ok bool
	rc.Control(func(fd uintptr) {
		info, e := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
		if e != nil {
			return
		}
		ok = true
		pressing = info.Notsent_bytes >= notsentPressBytes
	})
	return pressing, ok
}
