//go:build linux

package main

import (
	"encoding/binary"
	"net"
	"syscall"
	"time"
	"unsafe"
)

// auto6Watch (b333) kicks the auto6 resolver at once when the kernel adds or
// removes an IPv6 address (rtnetlink RTMGRP_IPV6_IFADDR) or an IPv6 route
// (RTMGRP_IPV6_ROUTE: the default route moved), so a change-over takes
// milliseconds instead of up to auto6Every; the periodic check stays as the
// fallback. Any failure just returns (the periodic check still runs).
func auto6Watch(stop <-chan struct{}) {
	const rtmgrpIPv6IfAddr, rtmgrpIPv6Route = 0x100, 0x400
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_ROUTE)
	if err != nil {
		return
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK, Groups: rtmgrpIPv6IfAddr | rtmgrpIPv6Route}); err != nil {
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

// auto6Probe is the destination of the route lookup (Google's 2001:4860::/32,
// like `ip -6 route get 2001:4860::`): any global destination would do.
var auto6Probe = net.ParseIP("2001:4860::")

const (
	rtaDst       = 1
	rtaOif       = 4
	rtaPriority  = 6
	rtaMultipath = 9
	rtaTable     = 15
	rtTableMain  = 254
)

// auto6RouteGetIface is `ip -6 route get 2001:4860::`'s dev: one
// RTM_GETROUTE request (main routing decision, no source, no mark). "" on
// any failure or when the destination is unreachable.
func auto6RouteGetIface() string {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_ROUTE)
	if err != nil {
		return ""
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return ""
	}
	tv := syscall.NsecToTimeval(int64(time.Second))
	syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)

	const seq = 0x6a36
	req := make([]byte, syscall.SizeofNlMsghdr+syscall.SizeofRtMsg+syscall.SizeofRtAttr+16)
	ne := binary.NativeEndian
	ne.PutUint32(req[0:], uint32(len(req)))
	ne.PutUint16(req[4:], syscall.RTM_GETROUTE)
	ne.PutUint16(req[6:], syscall.NLM_F_REQUEST)
	ne.PutUint32(req[8:], seq)
	rt := req[syscall.SizeofNlMsghdr:]
	rt[0] = syscall.AF_INET6 // family
	rt[1] = 128              // dst_len
	at := rt[syscall.SizeofRtMsg:]
	ne.PutUint16(at[0:], syscall.SizeofRtAttr+16)
	ne.PutUint16(at[2:], rtaDst)
	copy(at[syscall.SizeofRtAttr:], auto6Probe.To16())
	if err := syscall.Sendto(fd, req, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return ""
	}
	buf := make([]byte, 16384)
	for i := 0; i < 4; i++ {
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return ""
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:n])
		if err != nil {
			return ""
		}
		for _, m := range msgs {
			if m.Header.Seq != seq {
				continue
			}
			switch m.Header.Type {
			case syscall.NLMSG_ERROR, syscall.NLMSG_DONE:
				return "" // e.g. ENETUNREACH: no IPv6 route
			case syscall.RTM_NEWROUTE:
				oif, _, _, ok := auto6RouteInfo(m)
				if !ok {
					return ""
				}
				return auto6IfName(oif)
			}
		}
	}
	return ""
}

// auto6MainDefaultIface is `ip -6 route show default`'s dev: the unicast
// default route of the main table with the lowest metric. "" = none.
func auto6MainDefaultIface() string {
	rib, err := syscall.NetlinkRIB(syscall.RTM_GETROUTE, syscall.AF_INET6)
	if err != nil {
		return ""
	}
	msgs, err := syscall.ParseNetlinkMessage(rib)
	if err != nil {
		return ""
	}
	best, bestM := 0, uint32(0)
	for _, m := range msgs {
		if m.Header.Type != syscall.RTM_NEWROUTE || len(m.Data) < syscall.SizeofRtMsg {
			continue
		}
		rtm := (*syscall.RtMsg)(unsafe.Pointer(&m.Data[0]))
		if rtm.Dst_len != 0 || rtm.Type != syscall.RTN_UNICAST {
			continue
		}
		oif, prio, table, ok := auto6RouteInfo(m)
		if table == 0 {
			table = uint32(rtm.Table)
		}
		if !ok || oif == 0 || table != rtTableMain {
			continue
		}
		if name := auto6IfName(oif); name == "" || auto6VirtName(name) {
			continue
		}
		if best == 0 || prio < bestM {
			best, bestM = oif, prio
		}
	}
	return auto6IfName(best)
}

// auto6RouteInfo reads RTA_OIF (or the first RTA_MULTIPATH nexthop's
// ifindex), RTA_PRIORITY and RTA_TABLE of one route message.
func auto6RouteInfo(m syscall.NetlinkMessage) (oif int, prio, table uint32, ok bool) {
	attrs, err := syscall.ParseNetlinkRouteAttr(&m)
	if err != nil {
		return 0, 0, 0, false
	}
	ne := binary.NativeEndian
	for _, a := range attrs {
		switch a.Attr.Type {
		case rtaOif:
			if len(a.Value) >= 4 {
				oif = int(ne.Uint32(a.Value))
			}
		case rtaPriority:
			if len(a.Value) >= 4 {
				prio = ne.Uint32(a.Value)
			}
		case rtaTable:
			if len(a.Value) >= 4 {
				table = ne.Uint32(a.Value)
			}
		case rtaMultipath:
			// struct rtnexthop { u16 len; u8 flags; u8 hops; s32 ifindex }
			if oif == 0 && len(a.Value) >= 8 {
				oif = int(int32(ne.Uint32(a.Value[4:])))
			}
		}
	}
	return oif, prio, table, oif > 0
}

func auto6IfName(index int) string {
	if index <= 0 {
		return ""
	}
	ifc, err := net.InterfaceByIndex(index)
	if err != nil {
		return ""
	}
	return ifc.Name
}
