package engine

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PROXY protocol on the user ports that have the `proxy` flag (§3.1, §3.14
// item 4): users that reach the edge through a local nginx/haproxy or a CDN
// all come from one address, and would all be one user (one user tag, one
// share). On such a port the proxy says where each connection really came
// from, in a PROXY protocol header (version 1 or 2) before the data, and the
// user tag is made from that address. The header is believed only from the
// addresses in proxy_from (loopback by default); a connection without a valid
// header, or from anywhere else, is closed — on a proxy port the connection's
// own address is the proxy's, never the user's.
//
// Spec: https://www.haproxy.org/download/2.9/doc/proxy-protocol.txt. Only
// what a stream user port needs is read: TCP over IPv4 or IPv6; LOCAL (the
// proxy's own health check), UNKNOWN and the other families keep the
// connection's own address. Version 2's TLVs are skipped.

const (
	proxyV1Max = 107             // a version 1 header, CRLF included
	proxyV2Hdr = 16              // version 2: signature, version/command, family, length
	proxyV2Max = 216             // ... and at most this many address and TLV bytes after it
	proxyWait  = 5 * time.Second // a proxy sends its header at once
)

// proxyV2Sig opens every version 2 header.
var proxyV2Sig = []byte("\r\n\r\n\x00\r\nQUIT\n")

var errProxyHeader = errors.New("no valid PROXY protocol header")

// readProxyHeader reads one PROXY protocol header (version 1 or 2) off r and
// returns the client's address. An invalid src (no error) means the header
// names none — UNKNOWN, LOCAL, another family — and the connection's own
// address stands. It reads at most proxyV1Max bytes of a version 1 header
// and proxyV2Hdr+proxyV2Max of a version 2 one; nothing past the header.
func readProxyHeader(r *bufio.Reader) (src netip.AddrPort, err error) {
	first, err := r.Peek(1)
	if err != nil {
		return netip.AddrPort{}, err
	}
	if first[0] == '\r' {
		return readProxyV2(r)
	}
	return readProxyV1(r)
}

// readProxyV1 reads "PROXY TCP4|TCP6|UNKNOWN src dst sport dport\r\n".
func readProxyV1(r *bufio.Reader) (netip.AddrPort, error) {
	line := make([]byte, 0, proxyV1Max)
	for {
		c, err := r.ReadByte()
		if err != nil {
			return netip.AddrPort{}, err
		}
		line = append(line, c)
		if c == '\n' {
			break
		}
		if len(line) == len("PROXY ") && string(line) != "PROXY " {
			return netip.AddrPort{}, errProxyHeader // no header at all: not one more byte
		}
		if len(line) == proxyV1Max {
			return netip.AddrPort{}, fmt.Errorf("%w: a version 1 header longer than %d bytes", errProxyHeader, proxyV1Max)
		}
	}
	s, ok := strings.CutSuffix(string(line), "\r\n")
	if !ok || !strings.HasPrefix(s, "PROXY ") {
		return netip.AddrPort{}, errProxyHeader
	}
	f := strings.Split(s, " ")
	if len(f) >= 2 && f[1] == "UNKNOWN" {
		return netip.AddrPort{}, nil // the proxy does not know: the connection's own address
	}
	if len(f) != 6 || f[1] != "TCP4" && f[1] != "TCP6" {
		return netip.AddrPort{}, errProxyHeader
	}
	ip, err1 := netip.ParseAddr(f[2])
	dst, err2 := netip.ParseAddr(f[3])
	port, err3 := strconv.ParseUint(f[4], 10, 16)
	_, err4 := strconv.ParseUint(f[5], 10, 16)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || ip.Zone() != "" ||
		ip.Is4() != (f[1] == "TCP4") || dst.Is4() != ip.Is4() {
		return netip.AddrPort{}, errProxyHeader
	}
	return netip.AddrPortFrom(ip, uint16(port)), nil
}

// readProxyV2 reads the binary header: signature, version and command,
// family and protocol, length, then the addresses and TLVs.
func readProxyV2(r *bufio.Reader) (netip.AddrPort, error) {
	h, err := r.Peek(proxyV2Hdr)
	if err != nil {
		return netip.AddrPort{}, err
	}
	n := int(binary.BigEndian.Uint16(h[14:]))
	if !bytes.Equal(h[:12], proxyV2Sig) || h[12]>>4 != 2 || h[12]&0x0f > 1 || n > proxyV2Max {
		return netip.AddrPort{}, errProxyHeader
	}
	cmd, fam := h[12]&0x0f, h[13]
	b := make([]byte, proxyV2Hdr+n)
	if _, err := io.ReadFull(r, b); err != nil {
		return netip.AddrPort{}, err
	}
	b = b[proxyV2Hdr:]
	if cmd == 0 { // LOCAL: the proxy's own connection
		return netip.AddrPort{}, nil
	}
	switch fam {
	case 0x11: // TCP over IPv4: src 4, dst 4, sport 2, dport 2
		if n < 12 {
			return netip.AddrPort{}, errProxyHeader
		}
		return netip.AddrPortFrom(netip.AddrFrom4([4]byte(b[:4])), binary.BigEndian.Uint16(b[8:])), nil
	case 0x21: // TCP over IPv6: src 16, dst 16, sport 2, dport 2
		if n < 36 {
			return netip.AddrPort{}, errProxyHeader
		}
		return netip.AddrPortFrom(netip.AddrFrom16([16]byte(b[:16])), binary.BigEndian.Uint16(b[32:])), nil
	}
	return netip.AddrPort{}, nil // another family (UNSPEC, UDP, unix): the connection's own address
}

// proxyFromDefault is where a PROXY header is believed from when proxy_from
// is not set: this server itself (a proxy on the edge).
var proxyFromDefault = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}

// proxyAllowed reports whether a PROXY header from address from is believed:
// from must be in allow (none: proxyFromDefault).
func proxyAllowed(from netip.Addr, allow []netip.Prefix) bool {
	if len(allow) == 0 {
		allow = proxyFromDefault
	}
	from = from.Unmap()
	for _, p := range allow {
		if p.Contains(from) {
			return true
		}
	}
	return false
}

// ParseProxyFrom reads a proxy_from value: comma-separated addresses or
// prefixes ("10.0.0.5", "10.0.0.0/8", "::1"). Empty: nil (the default).
func ParseProxyFrom(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, raw := range strings.Split(s, ",") {
		e := strings.TrimSpace(raw)
		if e == "" {
			continue
		}
		if p, err := netip.ParsePrefix(e); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(e)
		if err != nil || a.Zone() != "" {
			return nil, fmt.Errorf("%q is not an address or a prefix (e.g. 127.0.0.1 or 10.0.0.0/8)", e)
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// proxiedConn is a user connection whose PROXY header was read: what the
// header reader buffered past it is read first, and RemoteAddr is the
// client's.
type proxiedConn struct {
	net.Conn
	r      *bufio.Reader
	client net.Addr
}

func (c *proxiedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
func (c *proxiedConn) RemoteAddr() net.Addr       { return c.client }

// SetLinger reaches the socket, so the wedge guard's reset (relayStream)
// still is one.
func (c *proxiedConn) SetLinger(sec int) error {
	if l, ok := c.Conn.(interface{ SetLinger(int) error }); ok {
		return l.SetLinger(sec)
	}
	return nil
}

// acceptProxied reads the PROXY header of a connection on a proxy port and
// returns the connection to relay, with the client's address. ok is false
// (and why says why) when it must be closed: from outside allow, or without a
// valid header within proxyWait.
func acceptProxied(c net.Conn, allow []netip.Prefix) (conn net.Conn, ok bool, why string) {
	peer := addrPortOf(c.RemoteAddr())
	if !proxyAllowed(peer.Addr(), allow) {
		return nil, false, fmt.Sprintf("a connection from %s, which is not in proxy_from", peer.Addr())
	}
	c.SetReadDeadline(time.Now().Add(proxyWait))
	r := bufio.NewReaderSize(c, 512)
	src, err := readProxyHeader(r)
	if err != nil {
		return nil, false, fmt.Sprintf("a connection from %s without a valid PROXY protocol header (%v)", peer.Addr(), err)
	}
	c.SetReadDeadline(time.Time{})
	client := c.RemoteAddr()
	if src.IsValid() {
		client = net.TCPAddrFromAddrPort(netip.AddrPortFrom(src.Addr().Unmap(), src.Port()))
	}
	return &proxiedConn{Conn: c, r: r, client: client}, true, ""
}

// addrPortOf is a net.Addr as an AddrPort (invalid: not an IP address).
func addrPortOf(a net.Addr) netip.AddrPort {
	if ta, ok := a.(*net.TCPAddr); ok {
		return ta.AddrPort()
	}
	if a == nil {
		return netip.AddrPort{}
	}
	ap, _ := netip.ParseAddrPort(a.String())
	return ap
}

// proxyRefusals logs the connections a proxy port closed, at most once a
// minute per port.
type proxyRefusals struct {
	mu   sync.Mutex
	last map[int]time.Time
}

func (p *proxyRefusals) note(port int, why string, logf func(string, ...any)) {
	if logf == nil {
		return
	}
	now := time.Now()
	p.mu.Lock()
	if p.last == nil {
		p.last = map[int]time.Time{}
	}
	if t, ok := p.last[port]; ok && now.Sub(t) < time.Minute {
		p.mu.Unlock()
		return
	}
	p.last[port] = now
	p.mu.Unlock()
	logf("user port %d (proxy): closed %s", port, why)
}
