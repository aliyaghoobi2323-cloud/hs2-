//go:build linux

package engine

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// tcpInfo (gc295) is unix.GetsockoptTCPInfo(fd, IPPROTO_TCP, TCP_INFO)
// returning the struct by value: the x/sys call returns a pointer to a new
// heap TCPInfo per call, and the sweepers, call picks and ul268's poll make
// ~8k calls/s on a busy side (95-150 MB/min, the gc295 storm profiles). Same
// syscall, same length argument, same result.
func tcpInfo(fd int) (unix.TCPInfo, error) {
	var info unix.TCPInfo
	l := uint32(unix.SizeofTCPInfo)
	_, _, e := unix.Syscall6(unix.SYS_GETSOCKOPT, uintptr(fd), uintptr(unix.IPPROTO_TCP), uintptr(unix.TCP_INFO),
		uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&l)), 0)
	if e != 0 {
		return info, e
	}
	return info, nil
}
