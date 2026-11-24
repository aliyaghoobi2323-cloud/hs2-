package freebind

import "syscall"

const (
	ipFreebind   = 15 // IP_FREEBIND
	ipv6Freebind = 78 // IPV6_FREEBIND (Linux 4.15+)
)

// Set sets IP_FREEBIND and IPV6_FREEBIND on fd (best effort: an IPv4 socket
// rejects the IPv6 option; on a pre-4.15 kernel IP_FREEBIND alone also covers
// an IPv6 socket). It must run before bind (a ListenConfig Control does).
func Set(fd uintptr) {
	syscall.SetsockoptInt(int(fd), syscall.SOL_IP, ipFreebind, 1)
	syscall.SetsockoptInt(int(fd), syscall.SOL_IPV6, ipv6Freebind, 1)
}

// Get reports whether fd has freebind on (tests).
func Get(fd uintptr) bool {
	v, err := syscall.GetsockoptInt(int(fd), syscall.SOL_IP, ipFreebind)
	if err == nil && v != 0 {
		return true
	}
	v, err = syscall.GetsockoptInt(int(fd), syscall.SOL_IPV6, ipv6Freebind)
	return err == nil && v != 0
}
