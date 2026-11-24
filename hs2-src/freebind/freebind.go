// Package freebind lets hs2 bind a configured local address before the kernel
// has made it usable, so a boot never fails on a late address.
//
// b425 (prod 2026-11-20): the Iran server rebooted and t1 hs2 exited twice with
// "lb: listen on <v6>: bind: cannot assign requested address": the IPv6
// address was still tentative (DAD) and network-online.target does not wait
// for DAD. A late IPv4 address (DHCP, a script, a cloud agent) hits the same
// bind error. With IP_FREEBIND / IPV6_FREEBIND set before bind, the kernel
// accepts the bind for any address; the listener receives nothing until the
// address is up, then accepts as usual. No retry loop, no fatal exit.
//
// Only specific listen addresses get it: a wildcard ("", 0.0.0.0, ::) always
// binds. Dials with a fixed source address do NOT use it (a SYN must never
// leave from a tentative or absent address); their failure is an ordinary dial
// error the link pools already retry.
package freebind

import (
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Specific reports whether addr (host:port or a bare host) names one IP that
// is not a wildcard.
func Specific(addr string) bool {
	return specificIP(addr) != nil
}

func specificIP(addr string) net.IP {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	if i := strings.IndexByte(host, '%'); i >= 0 { // fe80::1%eth0
		host = host[:i]
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsUnspecified() {
		return nil
	}
	return ip
}

// Usable reports whether ip can be bound WITHOUT freebind right now (it is
// assigned and, for IPv6, past DAD). It binds a throwaway UDP socket on port 0.
var Usable = func(ip net.IP) bool {
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: ip})
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// Pending counts listen addresses bound with freebind that are not usable yet.
var Pending atomic.Int64

var (
	watchMu sync.Mutex
	watched = map[string]bool{} // IP string -> a watcher runs
)

// pollEvery / logf are vars for tests.
var (
	pollEvery = 500 * time.Millisecond
	logf      = log.Printf
)

// Watch is called after a listener on addr was bound (with freebind). When the
// address is not usable yet it logs that once and logs again once it becomes
// usable. One watcher per IP (TCP and UDP on the same IP share it).
func Watch(addr string) {
	ip := specificIP(addr)
	if ip == nil || Usable(ip) {
		return
	}
	key := ip.String()
	watchMu.Lock()
	if watched[key] {
		watchMu.Unlock()
		return
	}
	watched[key] = true
	watchMu.Unlock()
	Pending.Add(1)
	logf("listen: %s is not assigned on this host yet (IPv6 DAD or a late address); bound it anyway (freebind), it accepts as soon as the address is up", key)
	go func() {
		start := time.Now()
		for !Usable(ip) {
			time.Sleep(pollEvery)
		}
		Pending.Add(-1)
		watchMu.Lock()
		delete(watched, key)
		watchMu.Unlock()
		logf("listen: %s is now assigned (after %s); its listeners accept", key, time.Since(start).Round(100*time.Millisecond))
	}()
}
