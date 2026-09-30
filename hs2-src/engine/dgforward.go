package engine

import (
	"context"
	"net"
	"sync"
	"time"
)

// Userspace port forwarder for the datagram tun.
//
// The datagram carrier is unreliable (FEC recovers most loss, the rest is
// ordinary packet loss), so user connections are NOT carried as a reliable
// mux over the carrier — that would need a reliability layer, which is exactly
// the TCP-in-TCP trap. Instead a user connection is proxied to the PEER'S TUN
// ADDRESS, so its packets ride the tunnel as ordinary IP packets and the
// endpoints' own TCP provides reliability end to end. There is no TCP inside
// the carrier.
//
//	iran (edge):  user connects to <user_listen_ip>:P  ->  dial <peer_tun_ip>:28443
//	                (kernel routes peer_tun_ip over the TUN, so it rides the pool)
//	kharej (exit): listen on <local_tun_ip>:28443      ->  dial <panel>
//
// Every user port P on the edge lands on the ONE on-tun port DgTunPort, and the
// exit hands everything arriving there to the panel. So the user ports are
// configured once, on the edge (forward_ports), and the exit needs only the panel
// address (expose) — the same split as the tcp transport. TCP and (optionally) UDP.
//
// The on-tun port is not the user/panel port on purpose. A panel almost always
// binds its port on 0.0.0.0 (all interfaces), and on Linux a bind of the SPECIFIC
// tun address <local_tun_ip>:P then fails with EADDRINUSE against that wildcard —
// even with SO_REUSEADDR — so an exit listening on the user port crash-looped.
// The tun address is private to the tunnel and 28443 is not a panel port, so the
// exit's listener stays clear of the panel.

// forwardTarget builds "host:port".
func forwardTarget(host, port string) string { return net.JoinHostPort(host, port) }

// DgTunPort is the single port the forwarder uses on the tun between the two
// servers. It is also the port the previous release used for the default user
// port 8443, so an edge that is not upgraded yet keeps working for that port.
const DgTunPort = "28443"

// runForwarder opens a TCP (and optionally UDP) listener on listenAddr and
// proxies every connection to dialAddr. Used on both ends of the datagram tun:
// the edge dials the peer's tun address, the exit dials the panel.
func runForwarder(ctx context.Context, listenAddr, dialAddr string, udp bool, logf func(string, ...any)) error {
	ln, err := ListenReuse(listenAddr)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go proxyTCP(ctx, c, dialAddr)
		}
	}()
	if udp {
		pc, err := net.ListenPacket("udp", listenAddr)
		if err != nil {
			ln.Close()
			return err
		}
		go func() { <-ctx.Done(); pc.Close() }()
		go proxyUDP(ctx, pc, dialAddr, logf)
	}
	return nil
}

// proxyTCP dials dialAddr and relays bytes both ways until either end closes.
func proxyTCP(ctx context.Context, user net.Conn, dialAddr string) {
	up, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", dialAddr)
	if err != nil {
		user.Close()
		return
	}
	relay(user, up)
}

// udpProxyIdle is how long a UDP flow with no traffic is kept before its
// upstream socket is closed.
const udpProxyIdle = 90 * time.Second

// proxyUDP forwards datagrams between clients and dialAddr, one upstream socket
// per client source address, so the return path finds its way back.
func proxyUDP(ctx context.Context, pc net.PacketConn, dialAddr string, logf func(string, ...any)) {
	ua, err := net.ResolveUDPAddr("udp", dialAddr)
	if err != nil {
		return
	}
	type flow struct {
		up   *net.UDPConn
		last time.Time
	}
	var mu sync.Mutex
	flows := map[string]*flow{}
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			mu.Lock()
			for k, f := range flows {
				if time.Since(f.last) > udpProxyIdle {
					f.up.Close()
					delete(flows, k)
				}
			}
			mu.Unlock()
		}
	}()
	buf := make([]byte, 65535)
	for {
		n, caddr, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		key := caddr.String()
		mu.Lock()
		f := flows[key]
		mu.Unlock()
		if f == nil {
			up, err := net.DialUDP("udp", nil, ua)
			if err != nil {
				continue
			}
			f = &flow{up: up, last: time.Now()}
			mu.Lock()
			flows[key] = f
			mu.Unlock()
			go func(f *flow, caddr net.Addr) {
				rb := make([]byte, 65535)
				for {
					f.up.SetReadDeadline(time.Now().Add(udpProxyIdle))
					m, err := f.up.Read(rb)
					if err != nil {
						break
					}
					pc.WriteTo(rb[:m], caddr)
					mu.Lock()
					f.last = time.Now()
					mu.Unlock()
				}
				f.up.Close()
				mu.Lock()
				if flows[key] == f {
					delete(flows, key)
				}
				mu.Unlock()
			}(f, caddr)
		}
		f.up.Write(buf[:n])
		mu.Lock()
		f.last = time.Now()
		mu.Unlock()
	}
}

// StartDgForwarders wires the userspace forwarders for the datagram tun on one
// side. On the edge (iran) it opens each user port in ports (forward_ports) and
// proxies it to the peer's tun address at DgTunPort. On the exit (kharej) it
// opens DgTunPort on the local tun address and proxies to panel (expose); the
// exit ignores ports and does nothing without a panel.
func StartDgForwarders(ctx context.Context, edge bool, ports []string, userListenIP, peerTunIP, localTunIP, panel string, udp bool, logf func(string, ...any)) error {
	if !edge {
		if panel == "" {
			return nil
		}
		if err := runForwarder(ctx, forwardTarget(localTunIP, DgTunPort), panel, udp, logf); err != nil {
			return err
		}
		logf("dg: tun port %s -> panel %s (all user ports)", DgTunPort, panel)
		return nil
	}
	for _, p := range ports {
		if err := runForwarder(ctx, forwardTarget(userListenIP, p), forwardTarget(peerTunIP, DgTunPort), udp, logf); err != nil {
			return err
		}
		logf("dg: user port %s open, forwarded over the tun to the panel", p)
	}
	return nil
}
