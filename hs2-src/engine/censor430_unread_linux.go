//go:build linux

package engine

import (
	"net"

	"golang.org/x/sys/unix"
)

// censorUnread (b430) is the bytes the kernel holds unread on tc (SIOCINQ).
// A close with any of them is sent as an RST (tcp_close: data was unread),
// not a FIN. -1 when it cannot be read.
func censorUnread(tc *net.TCPConn) int {
	if tc == nil {
		return -1
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		return -1
	}
	n := -1
	if rc.Control(func(fd uintptr) {
		if v, e := unix.IoctlGetInt(int(fd), unix.SIOCINQ); e == nil {
			n = v
		}
	}) != nil {
		return -1
	}
	return n
}
