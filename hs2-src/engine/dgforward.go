package engine

import (
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

// dgTunRcvBuf is the receive buffer, in bytes, set and locked on the
// forwarder's sockets that run OVER the tun (the edge's connection to
// <peer_tun_ip>:28443 and the exit's accepted end of it).
//
// Why: that TCP connection rides the datagram carriers, which deliver a packet
// FEC rebuilt after the ones behind it, and the real path reorders too, so its
// receiver routinely holds thousands of out-of-order segments. With the
// kernel's autotuned buffer, which starts small and grows only from what the
// application read in the last RTT, that out-of-order data overflows the
// buffer, and the kernel then clamps the advertised window to two segments
// (tcp_clamp_window) and regrows it slowly. The field measured exactly this:
// the in-tunnel connection rwnd-limited at ~4 segments per 80 ms RTT (~0.5
// Mbit/s per connection) with rcv_space ~30 KB, against 0.3-1 MB on healthy
// runs. A fixed, large buffer keeps the window open from the start. Memory is
// committed only as data actually queues, and 4 MB is below the 16 MB the
// autotuner may grow to anyway.
//
// Why 4 MB and not more: the field swept 4, 8 and 16 MB (2 flows, download
// then upload, the in-tunnel stall fixed in every case). Throughput did not
// move beyond run-to-run noise: download 114 / 112 / 117 Mbit/s, upload
// 171 / 153 / 165 (16 MB alone gave 193 and 136 on two identical runs). The
// larger buffers only cut the time the sender sat at the window limit (20-26%
// at 4 MB, 0.1-3.5% at 16 MB) without speeding it up, so the limit there is the
// carriers, the path and the CPU, not the window. Meanwhile every user
// connection is its own in-tunnel TCP connection, and the servers run hundreds
// of them on 2 GB of RAM: a larger ceiling only raises the worst case.
//
// HS2_TUN_RCVBUF overrides it in bytes; 0 restores the kernel's autotuning.
var dgTunRcvBuf = func() int {
	if v := strings.TrimSpace(os.Getenv("HS2_TUN_RCVBUF")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return 4 << 20
}()

// tunLeg says which side of a forwarder rides the tun, so only that socket
// gets the large receive buffer (the user and panel legs are local).
type tunLeg struct{ listen, dial bool }

// runForwarder opens a TCP (and optionally UDP) listener on listenAddr and
// proxies every connection to dialAddr. Used on both ends of the datagram tun:
// the edge dials the peer's tun address, the exit dials the panel. tun marks
// the leg that rides the tunnel (the exit's listener, the edge's dial).
func runForwarder(ctx context.Context, listenAddr, dialAddr string, udp bool, tun tunLeg, logf func(string, ...any)) error {
	lrb, drb := 0, 0
	if tun.listen {
		lrb = dgTunRcvBuf
	}
	if tun.dial {
		drb = dgTunRcvBuf
	}
	ln, err := listenReuseRcvBuf(listenAddr, lrb)
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
			go proxyTCP(ctx, c, dialAddr, drb)
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
// rcvbuf > 0 sets (locks) the dialed socket's receive buffer before connect,
// so the SYN advertises a window scale that fits it.
func proxyTCP(ctx context.Context, user net.Conn, dialAddr string, rcvbuf int) {
	up, err := forwardDialer(rcvbuf).DialContext(ctx, "tcp", dialAddr)
	if err != nil {
		user.Close()
		return
	}
	relay(user, up)
}

// forwardDialer returns the forwarder's dialer; rcvbuf > 0 sets (locks) the
// receive buffer on the socket before connect.
func forwardDialer(rcvbuf int) *net.Dialer {
	d := &net.Dialer{Timeout: 10 * time.Second}
	if rcvbuf > 0 {
		d.Control = func(network, address string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) { setRcvBuf(fd, rcvbuf) })
		}
	}
	return d
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
		if err := runForwarder(ctx, forwardTarget(localTunIP, DgTunPort), panel, udp, tunLeg{listen: true}, logf); err != nil {
			return err
		}
		logf("dg: tun port %s -> panel %s (all user ports)", DgTunPort, panel)
		return nil
	}
	for _, p := range ports {
		if err := runForwarder(ctx, forwardTarget(userListenIP, p), forwardTarget(peerTunIP, DgTunPort), udp, tunLeg{dial: true}, logf); err != nil {
			return err
		}
		logf("dg: user port %s open, forwarded over the tun to the panel", p)
	}
	return nil
}
