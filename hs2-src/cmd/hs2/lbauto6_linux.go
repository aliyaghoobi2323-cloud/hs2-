//go:build linux

package main

import (
	"syscall"
	"time"
)

// auto6Watch (b333) kicks the auto6 resolver at once when the kernel adds or
// removes an IPv6 address (rtnetlink RTMGRP_IPV6_IFADDR), so a change-over
// takes milliseconds instead of up to auto6Every; the periodic check stays as
// the fallback. Any failure just returns (the periodic check still runs).
func auto6Watch(stop <-chan struct{}) {
	const rtmgrpIPv6IfAddr = 0x100
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_ROUTE)
	if err != nil {
		return
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK, Groups: rtmgrpIPv6IfAddr}); err != nil {
		return
	}
	tv := syscall.NsecToTimeval(int64(time.Second))
	syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)
	buf := make([]byte, 8192)
	for {
		select {
		case <-stop:
			return
		default:
		}
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			if err == syscall.EAGAIN || err == syscall.EINTR || err == syscall.ENOBUFS {
				continue
			}
			return
		}
		if n > 0 {
			lbAutoKick()
		}
	}
}
