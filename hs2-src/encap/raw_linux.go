//go:build linux

package encap

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// Raw-socket encapsulations on Linux. Each transport is one AF_INET SOCK_RAW
// socket of the kind's IP protocol, driven through Go's *net.IPConn so reads
// and writes use the runtime poller (deadlines work, no thread per socket).
//
// The kernel builds the outer IPv4 header on send (no IP_HDRINCL); on receive a
// raw socket delivers the whole IPv4 datagram, header included, which we parse
// ourselves — that also gives the listener the local address each peer
// targeted, so a wildcard listener can answer from it.

func init() {
	dialRawFn = dialRawLinux
	listenRawFn = listenRawLinux
}

// sockBuf is the send/receive buffer asked for on every raw socket. A raw
// socket gets the same default as UDP (~200 KiB), which a burst at tens of
// Mbit/s overruns; as root we force it past net.core.[rw]mem_max.
const sockBuf = 4 << 20

// rawPeerTTL / rawPeerMax bound the listener's per-peer state (the echo
// sequence to answer with, the local address the peer targeted).
const (
	rawPeerTTL   = 5 * time.Minute
	rawPeerMax   = 16384
	rawPeerSweep = time.Minute
)

func rawNetwork(proto int) string { return "ip4:" + strconv.Itoa(proto) }

func parseBindIP(s string) (net.IP, error) {
	if s == "" {
		return nil, nil
	}
	ip := net.ParseIP(s).To4()
	if ip == nil {
		return nil, fmt.Errorf("encap: bind IP %q is not an IPv4 address", s)
	}
	return ip, nil
}

// tuneRawSocket enlarges the buffers and installs the receive filter.
func tuneRawSocket(c *net.IPConn, prog []bpfInsn) error {
	rc, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	cerr := rc.Control(func(fd uintptr) {
		s := int(fd)
		if unix.SetsockoptInt(s, unix.SOL_SOCKET, unix.SO_RCVBUFFORCE, sockBuf) != nil {
			unix.SetsockoptInt(s, unix.SOL_SOCKET, unix.SO_RCVBUF, sockBuf)
		}
		if unix.SetsockoptInt(s, unix.SOL_SOCKET, unix.SO_SNDBUFFORCE, sockBuf) != nil {
			unix.SetsockoptInt(s, unix.SOL_SOCKET, unix.SO_SNDBUF, sockBuf)
		}
		filter := make([]unix.SockFilter, len(prog))
		for i, in := range prog {
			filter[i] = unix.SockFilter{Code: in.Code, Jt: in.Jt, Jf: in.Jf, K: in.K}
		}
		fprog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
		serr = unix.SetsockoptSockFprog(s, unix.SOL_SOCKET, unix.SO_ATTACH_FILTER, &fprog)
	})
	if cerr != nil {
		return cerr
	}
	if serr != nil {
		return fmt.Errorf("attach receive filter: %w", serr)
	}
	return nil
}

// randLinkID picks a non-zero 16-bit link id.
func randLinkID() uint16 {
	var b [2]byte
	for {
		rand.Read(b[:])
		if id := binary.BigEndian.Uint16(b[:]); id != 0 {
			return id
		}
	}
}

// bufPool holds packet-build scratch so concurrent writers (the carrier's
// pacer and its control sender) never serialise on one buffer.
var bufPool = sync.Pool{New: func() any { b := make([]byte, 0, 2048); return &b }}

// ---------------------------------------------------------------------------
// Dial side: a connected raw socket to one peer IP, one link id.

type rawConn struct {
	ipc     *net.IPConn
	f       *framer
	id      uint16
	seq     atomic.Uint32 // icmp echo sequence, like ping's
	laddr   *Addr
	raddr   *Addr
	raddrIP *net.IPAddr // the peer, for WriteToIP (the socket is unconnected)
}

func dialRawLinux(kind, addr string, opt Options) (net.Conn, error) {
	f, err := newFramer(kind, opt, true)
	if err != nil {
		return nil, err
	}
	ra, err := net.ResolveIPAddr("ip4", hostOf(addr))
	if err != nil {
		return nil, fmt.Errorf("encap %s: %w", f.kind, err)
	}
	bip, err := parseBindIP(opt.BindIP)
	if err != nil {
		return nil, err
	}
	// The dial socket is UNCONNECTED (net.ListenIP, not DialIP). A connected
	// raw socket reports ICMP errors for the flow as fatal read/write errors,
	// so one ICMP packet — a router's frag-needed, a stray port/protocol
	// unreachable from an ip_gre/ipip module on the server, or a spoofed one
	// from an off-path attacker who knows only the two IPs and the protocol —
	// would tear the carrier (and every pooled carrier to that server) down.
	// An unconnected socket ignores those errors; we restrict delivery to the
	// peer ourselves, with a source-IP match in the BPF filter and in Read.
	la := &net.IPAddr{IP: net.IPv4zero}
	if bip != nil {
		la = &net.IPAddr{IP: bip}
	}
	ipc, err := net.ListenIP(rawNetwork(f.proto), la)
	if err != nil {
		return nil, rawErr(f.kind, err)
	}
	id := randLinkID()
	srcIP := binary.BigEndian.Uint32(ra.IP.To4())
	if err := tuneRawSocket(ipc, f.recvFilter(id, srcIP)); err != nil {
		ipc.Close()
		return nil, fmt.Errorf("encap %s: %w", f.kind, err)
	}
	c := &rawConn{ipc: ipc, f: f, id: id, raddrIP: &net.IPAddr{IP: ra.IP.To4()},
		raddr: &Addr{IP: ra.IP.To4(), ID: id, Kind: f.kind}}
	local := net.IPv4zero
	if bip != nil {
		local = bip
	}
	c.laddr = &Addr{IP: local, ID: id, Kind: f.kind}
	var s [2]byte
	rand.Read(s[:])
	c.seq.Store(uint32(binary.BigEndian.Uint16(s[:])))
	return c, nil
}

func (c *rawConn) Read(b []byte) (int, error) {
	for {
		n, _, _, _, err := c.ipc.ReadMsgIP(b, nil)
		if err != nil {
			if softErr(err) {
				continue // an ICMP error for this socket; not the link dying
			}
			return 0, err
		}
		src, _, tp, ok := c.f.ipv4Payload(b[:n])
		if !ok || !src.Equal(c.raddrIP.IP) {
			continue // not from the peer (the socket is unconnected)
		}
		id, _, ok := c.f.parse(tp)
		if !ok || id != c.id {
			continue
		}
		return copy(b, tp[c.f.hdr:]), nil
	}
}

func (c *rawConn) Write(p []byte) (int, error) {
	bp := bufPool.Get().(*[]byte)
	pkt := c.f.build(*bp, c.id, uint16(c.seq.Add(1)), p)
	_, err := c.ipc.WriteToIP(pkt, c.raddrIP)
	*bp = pkt[:0]
	bufPool.Put(bp)
	if err != nil && !softErr(err) {
		return 0, err
	}
	return len(p), nil // a soft error (ENOBUFS, an ICMP error) drops this datagram
}

func (c *rawConn) Close() error                       { return c.ipc.Close() }
func (c *rawConn) LocalAddr() net.Addr                { return c.laddr }
func (c *rawConn) RemoteAddr() net.Addr               { return c.raddr }
func (c *rawConn) SetDeadline(t time.Time) error      { return c.ipc.SetDeadline(t) }
func (c *rawConn) SetReadDeadline(t time.Time) error  { return c.ipc.SetReadDeadline(t) }
func (c *rawConn) SetWriteDeadline(t time.Time) error { return c.ipc.SetWriteDeadline(t) }

// ---------------------------------------------------------------------------
// Listen side: one raw socket bound to a local IP (or the wildcard), serving
// every peer link; peers are told apart by (IP, link id).

type rawKey struct {
	ip [4]byte
	id uint16
}

type rawPeer struct {
	addr  *Addr
	seq   uint16 // icmp: the peer's latest echo sequence, answered in replies
	local net.IP // the local address the peer targeted (wildcard bind)
	seen  time.Time
}

type rawPacketConn struct {
	ipc      *net.IPConn
	f        *framer
	wildcard bool
	laddr    *Addr

	mu        sync.Mutex
	peers     map[rawKey]*rawPeer
	lastSweep time.Time
}

func listenRawLinux(kind, addr string, opt Options) (net.PacketConn, error) {
	f, err := newFramer(kind, opt, false)
	if err != nil {
		return nil, err
	}
	host := hostOf(addr)
	var ip net.IP
	if host != "" {
		if ip = net.ParseIP(host).To4(); ip == nil {
			return nil, fmt.Errorf("encap %s: listen address %q is not an IPv4 address", f.kind, host)
		}
	}
	if ip == nil || ip.IsUnspecified() {
		if ip, err = parseBindIP(opt.BindIP); err != nil {
			return nil, err
		}
	}
	if ip == nil {
		ip = net.IPv4zero.To4()
	}
	if f.kind == KindICMP {
		// The server side receives echo REQUESTS; if the kernel also answered
		// them it would echo every sealed datagram straight back to the peer,
		// doubling the return path's traffic.
		if err := ignoreKernelEcho(); err != nil {
			return nil, err
		}
	}
	ipc, err := net.ListenIP(rawNetwork(f.proto), &net.IPAddr{IP: ip})
	if err != nil {
		return nil, rawErr(f.kind, err)
	}
	if err := tuneRawSocket(ipc, f.recvFilter(0, 0)); err != nil {
		ipc.Close()
		return nil, fmt.Errorf("encap %s: %w", f.kind, err)
	}
	return &rawPacketConn{
		ipc: ipc, f: f, wildcard: ip.IsUnspecified(),
		laddr: &Addr{IP: ip, Kind: f.kind},
		peers: make(map[rawKey]*rawPeer),
	}, nil
}

func (c *rawPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		n, _, _, _, err := c.ipc.ReadMsgIP(b, nil)
		if err != nil {
			if softErr(err) {
				continue
			}
			return 0, nil, err
		}
		src, dst, tp, ok := c.f.ipv4Payload(b[:n])
		if !ok {
			continue
		}
		id, seq, ok := c.f.parse(tp)
		if !ok {
			continue
		}
		a := c.notePeer(src, dst, id, seq)
		return copy(b, tp[c.f.hdr:]), a, nil
	}
}

// notePeer records what replying to a peer needs and returns its (shared,
// immutable) address.
func (c *rawPacketConn) notePeer(src, dst net.IP, id, seq uint16) *Addr {
	var k rawKey
	copy(k.ip[:], src)
	k.id = id
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.peers[k]
	if p == nil {
		if len(c.peers) >= rawPeerMax {
			c.sweepLocked(now, 10*time.Second)
			if len(c.peers) >= rawPeerMax {
				c.peers = make(map[rawKey]*rawPeer)
			}
		}
		p = &rawPeer{addr: &Addr{IP: append(net.IP(nil), src...), ID: id, Kind: c.f.kind}}
		c.peers[k] = p
	}
	p.seq, p.seen = seq, now
	if c.wildcard && !p.local.Equal(dst) {
		p.local = append(net.IP(nil), dst...)
	}
	if now.Sub(c.lastSweep) >= rawPeerSweep {
		c.lastSweep = now
		c.sweepLocked(now, rawPeerTTL)
	}
	return p.addr
}

func (c *rawPacketConn) sweepLocked(now time.Time, ttl time.Duration) {
	for k, p := range c.peers {
		if now.Sub(p.seen) > ttl {
			delete(c.peers, k)
		}
	}
}

func (c *rawPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	a, ok := addr.(*Addr)
	if !ok || a.IP.To4() == nil {
		return 0, fmt.Errorf("encap %s: bad peer address %v", c.f.kind, addr)
	}
	var k rawKey
	copy(k.ip[:], a.IP.To4())
	k.id = a.ID
	var seq uint16
	var local net.IP
	c.mu.Lock()
	if pr := c.peers[k]; pr != nil {
		seq, local = pr.seq, pr.local
	}
	c.mu.Unlock()

	bp := bufPool.Get().(*[]byte)
	pkt := c.f.build(*bp, a.ID, seq, p)
	dst := &net.IPAddr{IP: a.IP}
	var err error
	if local != nil {
		// Answer from the address the peer targeted: its connected socket
		// accepts nothing else, and neither would a stateful middlebox.
		info := &unix.Inet4Pktinfo{}
		copy(info.Spec_dst[:], local.To4())
		_, _, err = c.ipc.WriteMsgIP(pkt, unix.PktInfo4(info), dst)
	} else {
		_, err = c.ipc.WriteToIP(pkt, dst)
	}
	*bp = pkt[:0]
	bufPool.Put(bp)
	if err != nil && !softErr(err) {
		return 0, err
	}
	return len(p), nil
}

func (c *rawPacketConn) Close() error                       { return c.ipc.Close() }
func (c *rawPacketConn) LocalAddr() net.Addr                { return c.laddr }
func (c *rawPacketConn) SetDeadline(t time.Time) error      { return c.ipc.SetDeadline(t) }
func (c *rawPacketConn) SetReadDeadline(t time.Time) error  { return c.ipc.SetReadDeadline(t) }
func (c *rawPacketConn) SetWriteDeadline(t time.Time) error { return c.ipc.SetWriteDeadline(t) }

// ---------------------------------------------------------------------------

const echoIgnorePath = "/proc/sys/net/ipv4/icmp_echo_ignore_all"

// ignoreKernelEcho makes this network namespace's kernel stop answering echo
// requests (net.ipv4.icmp_echo_ignore_all=1), which an icmp listener needs.
// Already set is fine; unable to set it is an error that says what to do.
func ignoreKernelEcho() error {
	if b, err := os.ReadFile(echoIgnorePath); err == nil && strings.TrimSpace(string(b)) == "1" {
		return nil
	}
	if err := os.WriteFile(echoIgnorePath, []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("encap icmp: the kernel would answer the tunnel's echo requests itself; "+
			"set net.ipv4.icmp_echo_ignore_all=1 (sysctl -w) or run as root: %w", err)
	}
	return nil
}

// EchoIgnored reports whether the kernel's automatic echo replies are off in
// this network namespace (for status/diagnostics).
func EchoIgnored() bool {
	b, err := os.ReadFile(echoIgnorePath)
	return err == nil && strings.TrimSpace(string(b)) == "1"
}

// softErr reports whether a raw-socket read/write error is a transient ICMP
// error the kernel reports on a CONNECTED raw socket (dest/host/net
// unreachable, port unreachable, a reset, a fragmentation-needed) rather than
// the socket dying. A connected raw socket delivers such an error — triggered
// by ANY host on the path, or a spoofed packet — as the result of the NEXT
// read or write; treating it as fatal would let one ICMP packet tear the
// carrier down. It is dropped and the operation retried instead.
func softErr(err error) bool {
	return errors.Is(err, unix.EHOSTUNREACH) || errors.Is(err, unix.ECONNREFUSED) ||
		errors.Is(err, unix.ENETUNREACH) || errors.Is(err, unix.ECONNRESET) ||
		errors.Is(err, unix.EHOSTDOWN) || errors.Is(err, unix.ENETDOWN) ||
		errors.Is(err, unix.EMSGSIZE) || errors.Is(err, unix.ENOBUFS) ||
		errors.Is(err, unix.ENOPROTOOPT) || errors.Is(err, unix.ENONET) ||
		errors.Is(err, unix.EPROTO)
}

func rawErr(kind string, err error) error {
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
		return fmt.Errorf("encap %s: a raw socket needs root or CAP_NET_RAW: %w", kind, err)
	}
	return fmt.Errorf("encap %s: %w", kind, err)
}
